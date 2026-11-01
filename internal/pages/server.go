// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pages

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	pageconfig "gitea.dev/page/internal/config"
)

// httpPublisher maps confirmed deployments to the built-in HTTP handlers.
type httpPublisher struct {
	cfg         pageconfig.Config
	root        *url.URL
	challengeMu sync.RWMutex
	challenges  map[string]string
	routeIndex  routeIndex
	resources   platformResources
}

type routeIndex struct {
	mu     sync.RWMutex
	routes map[string]routeEntry
	byRepo map[int64][]string
	byHost map[string]int
}

type routeEntry struct {
	deployment siteDeployment
	base       string
}

func (s *httpPublisher) SetChallenge(host, token, keyAuthorization string) {
	s.challengeMu.Lock()
	s.challenges[normalizeHost(host)+"/"+token] = keyAuthorization
	s.challengeMu.Unlock()
}

func (s *httpPublisher) ClearChallenge(host, token string) {
	s.challengeMu.Lock()
	delete(s.challenges, normalizeHost(host)+"/"+token)
	s.challengeMu.Unlock()
}

// newHTTPPublisher creates the built-in publisher.
func newHTTPPublisher(cfg pageconfig.Config) (*httpPublisher, error) {
	if cfg.RootURL == nil || cfg.RootURL.Hostname() == "" || cfg.RootURL.Scheme != "http" && cfg.RootURL.Scheme != "https" {
		return nil, errors.New("pages root URL must use HTTP or HTTPS")
	}
	resources, err := loadPlatformResources(cfg.CustomRoot)
	if err != nil {
		return nil, err
	}
	s := &httpPublisher{
		cfg:        cfg,
		root:       cfg.RootURL,
		challenges: make(map[string]string),
		routeIndex: routeIndex{
			routes: make(map[string]routeEntry),
			byRepo: make(map[int64][]string),
			byHost: make(map[string]int),
		},
		resources: resources,
	}
	return s, nil
}

// Handler returns a request handler for one listener. The secure flag records
// whether that listener already terminated TLS, while both handlers share the
// same route and challenge state.
func (s *httpPublisher) Handler(secure bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.serveHTTP(w, r, secure)
	})
}

func (s *httpPublisher) removeRepositoryLocked(repoID int64) {
	for _, key := range s.routeIndex.byRepo[repoID] {
		if entry, exists := s.routeIndex.routes[key]; exists && entry.deployment.repoID == repoID {
			delete(s.routeIndex.routes, key)
			host := routeHost(key)
			if s.routeIndex.byHost[host] <= 1 {
				delete(s.routeIndex.byHost, host)
			} else {
				s.routeIndex.byHost[host]--
			}
		}
	}
	delete(s.routeIndex.byRepo, repoID)
}

func (s *httpPublisher) setDeploymentLocked(deployment siteDeployment, publication publicationResult) {
	s.removeRepositoryLocked(deployment.repoID)
	for key, entry := range deploymentRoutes(deployment, publication) {
		s.routeIndex.routes[key] = entry
		s.routeIndex.byRepo[deployment.repoID] = append(s.routeIndex.byRepo[deployment.repoID], key)
		s.routeIndex.byHost[routeHost(key)]++
	}
}

func deploymentRoutes(deployment siteDeployment, publication publicationResult) map[string]routeEntry {
	routes := make(map[string]routeEntry, 2)
	if publication.defaultPublished {
		host, base := deployment.defaultURL.Hostname(), deployment.defaultURL.EscapedPath()
		if base == "" {
			base = "/"
		}
		if !strings.HasSuffix(base, "/") {
			base += "/"
		}
		routes[routeKey(normalizeHost(host), base)] = routeEntry{deployment: deployment, base: base}
	}
	if publication.customPublished && deployment.customURL != nil {
		routes[routeKey(normalizeHost(deployment.customURL.Hostname()), "/")] = routeEntry{deployment: deployment, base: "/"}
	}
	return routes
}

func (s *httpPublisher) restore(_ context.Context, deployments deploymentSource) error {
	routes := make(map[string]routeEntry)
	byRepo := make(map[int64][]string)
	byHost := make(map[string]int)
	if err := deployments.forEach(func(active activeDeployment) error {
		for key, entry := range deploymentRoutes(active.siteDeployment, active.publicationResult) {
			routes[key] = entry
			byRepo[active.repoID] = append(byRepo[active.repoID], key)
			byHost[routeHost(key)]++
		}
		return nil
	}); err != nil {
		return err
	}
	s.routeIndex.mu.Lock()
	s.routeIndex.routes, s.routeIndex.byRepo, s.routeIndex.byHost = routes, byRepo, byHost
	s.routeIndex.mu.Unlock()
	return nil
}

func (s *httpPublisher) publish(ctx context.Context, deployment siteDeployment) (publicationResult, error) {
	s.routeIndex.mu.Lock()
	old := make(map[string]routeEntry, len(s.routeIndex.byRepo[deployment.repoID]))
	for _, key := range s.routeIndex.byRepo[deployment.repoID] {
		old[key] = s.routeIndex.routes[key]
	}
	requested := publicationResult{defaultPublished: true, customPublished: deployment.customURL != nil}
	s.setDeploymentLocked(deployment, requested)
	s.routeIndex.mu.Unlock()

	restore := func() {
		s.routeIndex.mu.Lock()
		s.removeRepositoryLocked(deployment.repoID)
		for key, entry := range old {
			s.routeIndex.routes[key] = entry
			s.routeIndex.byRepo[deployment.repoID] = append(s.routeIndex.byRepo[deployment.repoID], key)
			s.routeIndex.byHost[routeHost(key)]++
		}
		s.routeIndex.mu.Unlock()
	}
	httpAddress, err := localListenAddress(s.cfg.HTTPListen)
	if err != nil {
		restore()
		return publicationResult{}, err
	}
	deploymentAddress := httpAddress
	probeScheme := "http"
	if s.cfg.HTTPSMode == pageconfig.HTTPSPages {
		deploymentAddress, err = localListenAddress(s.cfg.HTTPSListen)
		if err != nil {
			restore()
			return publicationResult{}, err
		}
		probeScheme = "https"
	}
	if err := probeHTTP(ctx, probeScheme, deploymentAddress, s.root.Hostname(), "/@gitea-pages/ready", http.StatusNoContent, ""); err != nil {
		restore()
		return publicationResult{}, err
	}
	result := publicationResult{}
	if err := probeDeploymentURL(ctx, probeScheme, deploymentAddress, deployment.defaultURL, deployment.repoID, deployment.content.Digest); err == nil {
		result.defaultPublished = true
	} else {
		result.problem = publicationProblemEntryProbeFailed
	}
	if deployment.customURL != nil {
		if err := probeDeploymentURL(ctx, probeScheme, deploymentAddress, *deployment.customURL, deployment.repoID, deployment.content.Digest); err == nil {
			result.customPublished = true
		} else {
			result.problem = publicationProblemEntryProbeFailed
		}
	}
	s.routeIndex.mu.Lock()
	s.setDeploymentLocked(deployment, result)
	s.routeIndex.mu.Unlock()
	return result, nil
}

func (s *httpPublisher) unpublish(_ context.Context, repoID int64) error {
	s.routeIndex.mu.Lock()
	s.removeRepositoryLocked(repoID)
	s.routeIndex.mu.Unlock()
	return nil
}

func routeKey(host, base string) string { return host + "\x00" + base }

func routeHost(key string) string {
	host, _, _ := strings.Cut(key, "\x00")
	return host
}

func (s *httpPublisher) lookupRoute(host, requestPath string) (routeEntry, bool) {
	s.routeIndex.mu.RLock()
	defer s.routeIndex.mu.RUnlock()
	segment := strings.TrimPrefix(requestPath, "/")
	if i := strings.IndexByte(segment, '/'); i >= 0 {
		segment = segment[:i]
	}
	if segment != "" {
		base := "/" + url.PathEscape(strings.ToLower(segment)) + "/"
		if project, ok := s.routeIndex.routes[routeKey(host, base)]; ok {
			return project, true
		}
	}
	root, ok := s.routeIndex.routes[routeKey(host, "/")]
	return root, ok
}

func (s *httpPublisher) serveHTTP(w http.ResponseWriter, r *http.Request, secure bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	host := normalizeHost(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if s.cfg.HTTPSMode == pageconfig.HTTPSPages && !secure && !strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/") && s.knownHost(host) {
		u := *r.URL
		u.Scheme = "https"
		u.Host = host
		http.Redirect(w, r, u.String(), http.StatusPermanentRedirect)
		return
	}
	if s.cfg.HTTPSMode == pageconfig.HTTPSPages && strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/") {
		token := strings.TrimPrefix(r.URL.Path, "/.well-known/acme-challenge/")
		s.challengeMu.RLock()
		body, ok := s.challenges[host+"/"+token]
		s.challengeMu.RUnlock()
		if ok {
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
			return
		}
	}
	if strings.HasPrefix(r.URL.Path, "/@gitea-pages/") {
		if !s.knownHost(host) {
			plainNotFound(w)
			return
		}
		s.servePlatformRoute(w, r)
		return
	}
	if host == normalizeHost(s.root.Hostname()) && r.URL.Path == "/" {
		s.servePlatformFile(w, r, "index.html", http.StatusOK)
		return
	}
	match, found := s.lookupRoute(host, r.URL.Path)
	if !found {
		if s.knownHost(host) {
			s.servePlatformRoute(w, r)
		} else {
			plainNotFound(w)
		}
		return
	}
	s.serveSite(w, r, match)
}

func (s *httpPublisher) serveSite(w http.ResponseWriter, r *http.Request, match routeEntry) {
	if match.base != "/" && r.URL.Path == strings.TrimSuffix(match.base, "/") {
		u := *r.URL
		u.Path = match.base
		u.RawPath = ""
		http.Redirect(w, r, u.String(), http.StatusPermanentRedirect)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, match.base)
	if rel == ".gitea-pages-probe" {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(strconv.FormatInt(match.deployment.repoID, 10) + " " + match.deployment.content.Digest + "\n"))
		return
	}
	if strings.HasSuffix(r.URL.Path, "/") {
		if rel == "" {
			rel = "index.html"
		} else {
			rel += "index.html"
		}
	} else if rel == "" {
		rel = "index.html"
	}
	for _, segment := range strings.Split(rel, "/") {
		if segment == ".." {
			plainNotFound(w)
			return
		}
	}
	contentRoot := match.deployment.content.Root
	resolvedRoot, err := filepath.EvalSymlinks(contentRoot)
	if err != nil || filepath.Clean(resolvedRoot) != filepath.Clean(contentRoot) {
		plainNotFound(w)
		return
	}
	file := filepath.Join(contentRoot, filepath.FromSlash(path.Clean("/" + rel)[1:]))
	resolvedFile, err := filepath.EvalSymlinks(file)
	if err != nil || !pathWithin(resolvedFile, resolvedRoot) {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		} else {
			s.serveSiteNotFound(w, r, resolvedRoot, match.deployment.content.Digest)
		}
		return
	}
	f, err := os.Open(resolvedFile)
	if err != nil {
		if os.IsNotExist(err) {
			s.serveSiteNotFound(w, r, resolvedRoot, match.deployment.content.Digest)
		} else {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if info.IsDir() {
		_ = f.Close()
		u := *r.URL
		u.Path += "/"
		u.RawPath = ""
		http.Redirect(w, r, u.String(), http.StatusPermanentRedirect)
		return
	}
	serveLocalFile(w, r, f, resolvedFile, match.deployment.content.Digest, http.StatusOK)
}

func (s *httpPublisher) knownHost(host string) bool {
	if host == normalizeHost(s.root.Hostname()) {
		return true
	}
	s.routeIndex.mu.RLock()
	defer s.routeIndex.mu.RUnlock()
	return s.routeIndex.byHost[host] > 0
}

// siteContentType returns the type from the file extension without reading
// file contents. Keeping this table in the program makes serving independent
// of the host's MIME database; unknown extensions are
// deliberately treated as opaque downloads.
func siteContentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".json", ".map":
		return "application/json"
	case ".xml":
		return "application/xml"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".csv":
		return "text/csv; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".wasm":
		return "application/wasm"
	case ".pdf":
		return "application/pdf"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".otf":
		return "font/otf"
	case ".mp3":
		return "audio/mpeg"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".ogg":
		return "audio/ogg"
	case ".wav":
		return "audio/wav"
	default:
		return "application/octet-stream"
	}
}

func pathWithin(path, root string) bool {
	path, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return false
	}
	root, err = filepath.Abs(filepath.Clean(root))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

func (s *httpPublisher) serveSiteNotFound(w http.ResponseWriter, r *http.Request, root, digest string) {
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, "404.html"))
	if err != nil || !pathWithin(resolved, root) {
		plainNotFound(w)
		return
	}
	f, err := os.Open(resolved)
	if err != nil {
		plainNotFound(w)
		return
	}
	serveLocalFile(w, r, f, resolved, digest, http.StatusNotFound)
}

func serveLocalFile(w http.ResponseWriter, r *http.Request, file *os.File, name, digest string, status int) {
	defer func() { _ = file.Close() }()
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", siteContentType(filepath.Base(name)))
	if digest != "" {
		etag := `"` + digest + `"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	http.ServeContent(w, r, filepath.Base(name), time.Time{}, file)
}

func plainNotFound(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "404 Not Found", http.StatusNotFound)
}

func (s *httpPublisher) servePlatformRoute(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/@gitea-pages/ready" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/@gitea-pages/assets/") {
		name := strings.TrimPrefix(r.URL.Path, "/@gitea-pages/")
		if strings.Contains(name, "..") {
			plainNotFound(w)
			return
		}
		if _, ok := s.resources.files[name]; ok {
			s.servePlatformFile(w, r, name, http.StatusOK)
			return
		}
	}
	s.servePlatformFile(w, r, "errors/404.html", http.StatusNotFound)
}

func (s *httpPublisher) servePlatformFile(w http.ResponseWriter, r *http.Request, name string, status int) {
	data, ok := s.resources.files[name]
	if !ok {
		plainNotFound(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", siteContentType(name))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}
