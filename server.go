package page

import (
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Server struct {
	store       *Store
	root        *url.URL
	secure      bool
	challengeMu *sync.RWMutex
	challenges  map[string]string
	routeIndex  *routeIndex
	resources   platformResources
}

type routeIndex struct {
	mu     sync.RWMutex
	routes map[string]routeEntry
	byRepo map[int64][]string
	byHost map[string]int
}

type routeEntry struct {
	site LocalSite
	base string
}

func (s *Server) WithHTTPS() *Server {
	return &Server{store: s.store, root: s.root, secure: true, challengeMu: s.challengeMu, challenges: s.challenges, routeIndex: s.routeIndex, resources: s.resources}
}

func (s *Server) SetChallenge(host, token, keyAuthorization string) {
	s.challengeMu.Lock()
	s.challenges[normalizeHost(host)+"/"+token] = keyAuthorization
	s.challengeMu.Unlock()
}

func (s *Server) ClearChallenge(host, token string) {
	s.challengeMu.Lock()
	delete(s.challenges, normalizeHost(host)+"/"+token)
	s.challengeMu.Unlock()
}

func NewServer(store *Store, root *url.URL, customRoot string) (*Server, error) {
	resources, err := loadPlatformResources(customRoot)
	if err != nil {
		return nil, err
	}
	s := &Server{store: store, root: root, challengeMu: &sync.RWMutex{}, challenges: make(map[string]string), routeIndex: &routeIndex{routes: make(map[string]routeEntry), byRepo: make(map[int64][]string), byHost: make(map[string]int)}, resources: resources}
	s.RebuildRoutes()
	return s, nil
}

func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

// RebuildRoutes creates the request-time index from the persisted state. It is
// intentionally rebuilt after synchronization or deployment changes, rather
// than scanning every repository for every request.
func (s *Server) RebuildRoutes() {
	initialized, rootURL, sites := s.store.routingSnapshot()
	routes := make(map[string]routeEntry, len(sites))
	byRepo := make(map[int64][]string, len(sites))
	byHost := make(map[string]int, len(sites))
	if initialized && rootURL == s.root.String() {
		for _, v := range sites {
			for key, entry := range s.siteRoutes(v) {
				routes[key] = entry
				byRepo[v.Site.RepoID] = append(byRepo[v.Site.RepoID], key)
				byHost[routeHost(key)]++
			}
		}
	}
	s.routeIndex.mu.Lock()
	s.routeIndex.routes = routes
	s.routeIndex.byRepo = byRepo
	s.routeIndex.byHost = byHost
	s.routeIndex.mu.Unlock()
}

// UpdateSite changes only the route keys belonging to one repository. A
// large instance therefore does not rebuild every unrelated route when one
// repository changes or finishes deployment.
func (s *Server) UpdateSite(repoID int64) {
	local, ok := s.store.Get(repoID)
	_, initialized, rootURL, _ := s.store.State()
	s.routeIndex.mu.Lock()
	defer s.routeIndex.mu.Unlock()
	for _, key := range s.routeIndex.byRepo[repoID] {
		if entry, exists := s.routeIndex.routes[key]; exists && entry.site.Site.RepoID == repoID {
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
	if !ok || !initialized || rootURL != s.root.String() {
		return
	}
	for key, entry := range s.siteRoutes(local) {
		s.routeIndex.routes[key] = entry
		s.routeIndex.byRepo[repoID] = append(s.routeIndex.byRepo[repoID], key)
		s.routeIndex.byHost[routeHost(key)]++
	}
}

func (s *Server) siteRoutes(v LocalSite) map[string]routeEntry {
	if v.Site.Deleted || !v.Site.Enabled || v.InstalledDigest == "" {
		return nil
	}
	routes := make(map[string]routeEntry, 1+len(v.Site.CustomDomains))
	for _, d := range v.Site.CustomDomains {
		if d.State == "active" && v.CustomPublished {
			routes[routeKey(normalizeHost(d.Host), "/")] = routeEntry{site: v, base: "/"}
		}
	}
	if v.DefaultPublished {
		host := ownerHost(v.Site.OwnerName, s.root.Hostname())
		if host != "" {
			base := "/"
			if !strings.EqualFold(v.Site.RepoName, ".pages") {
				base = "/" + url.PathEscape(strings.ToLower(v.Site.RepoName)) + "/"
			}
			routes[routeKey(host, base)] = routeEntry{site: v, base: base}
		}
	}
	return routes
}

func routeKey(host, base string) string { return host + "\x00" + base }

func routeHost(key string) string {
	host, _, _ := strings.Cut(key, "\x00")
	return host
}

func (s *Server) lookupRoute(host, requestPath string) (LocalSite, string, bool) {
	s.routeIndex.mu.RLock()
	defer s.routeIndex.mu.RUnlock()
	if v, ok := s.routeIndex.routes[routeKey(host, "/")]; ok {
		// A custom domain and an owner homepage both use the root route. A
		// project route below is more specific and is checked first.
		if strings.HasPrefix(requestPath, "/") {
			segment := strings.TrimPrefix(requestPath, "/")
			if i := strings.IndexByte(segment, '/'); i >= 0 {
				segment = segment[:i]
			}
			if segment != "" {
				base := "/" + url.PathEscape(strings.ToLower(segment)) + "/"
				if project, exists := s.routeIndex.routes[routeKey(host, base)]; exists {
					return project.site, project.base, true
				}
			}
		}
		return v.site, v.base, true
	}
	segment := strings.TrimPrefix(requestPath, "/")
	if i := strings.IndexByte(segment, '/'); i >= 0 {
		segment = segment[:i]
	}
	if segment == "" {
		return LocalSite{}, "", false
	}
	base := "/" + url.PathEscape(strings.ToLower(segment)) + "/"
	v, ok := s.routeIndex.routes[routeKey(host, base)]
	return v.site, v.base, ok
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	host := normalizeHost(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if s.root.Scheme == "https" && !s.secure && !strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/") && s.knownHost(host) {
		u := *r.URL
		u.Scheme = "https"
		u.Host = host
		http.Redirect(w, r, u.String(), http.StatusPermanentRedirect)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/") {
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
		s.platform(w, r)
		return
	}
	if host == normalizeHost(s.root.Hostname()) && r.URL.Path == "/" {
		s.platformHome(w, r)
		return
	}
	match, base, found := s.lookupRoute(host, r.URL.Path)
	if !found {
		if s.knownHost(host) {
			s.platform(w, r)
		} else {
			plainNotFound(w)
		}
		return
	}
	if base != "/" && r.URL.Path == strings.TrimSuffix(base, "/") {
		u := *r.URL
		u.Path = base
		u.RawPath = ""
		http.Redirect(w, r, u.String(), http.StatusPermanentRedirect)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, base)
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
	currentRoot := filepath.Join(s.store.SiteDir(match.Site.RepoID), "current")
	resolvedRoot, err := filepath.EvalSymlinks(currentRoot)
	if err != nil || !within(resolvedRoot, s.store.SiteDir(match.Site.RepoID)) {
		plainNotFound(w)
		return
	}
	file := filepath.Join(currentRoot, filepath.FromSlash(path.Clean("/" + rel)[1:]))
	resolvedFile, err := filepath.EvalSymlinks(file)
	if err != nil || !within(resolvedFile, resolvedRoot) {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		} else {
			s.serveSiteNotFound(w, r, resolvedRoot)
		}
		return
	}
	f, err := os.Open(resolvedFile)
	if err != nil {
		if os.IsNotExist(err) {
			s.serveSiteNotFound(w, r, resolvedRoot)
		} else {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", siteContentType(filepath.Base(resolvedFile)))
	http.ServeContent(w, r, filepath.Base(resolvedFile), time.Time{}, f)
}

func (s *Server) knownHost(host string) bool {
	if host == normalizeHost(s.root.Hostname()) {
		return true
	}
	s.routeIndex.mu.RLock()
	defer s.routeIndex.mu.RUnlock()
	return s.routeIndex.byHost[host] > 0
}

// siteContentType returns the type from the file extension without reading
// file contents. Keeping this table in the program makes standalone and nginx
// serving independent of the host's MIME database; unknown extensions are
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

func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

func (s *Server) serveSiteNotFound(w http.ResponseWriter, r *http.Request, root string) {
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, "404.html"))
	if err != nil || !within(resolved, root) {
		plainNotFound(w)
		return
	}
	f, err := os.Open(resolved)
	if err != nil {
		plainNotFound(w)
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", siteContentType(filepath.Base(resolved)))
	w.WriteHeader(http.StatusNotFound)
	http.ServeContent(w, r, filepath.Base(resolved), time.Time{}, f)
}

func plainNotFound(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "404 Not Found", http.StatusNotFound)
}

func (s *Server) platformHome(w http.ResponseWriter, r *http.Request) {
	s.servePlatformFile(w, r, "index.html", http.StatusOK)
}

func (s *Server) platform(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) servePlatformFile(w http.ResponseWriter, r *http.Request, name string, status int) {
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

func ownerHost(owner, root string) string {
	label, err := ownerDNSLabel(strings.ToLower(owner))
	if err != nil || root == "" {
		return ""
	}
	return label + "." + normalizeHost(root)
}

const ownerAlphabet = "-._0123456789abcdefghijklmnopqrstuvwxyz"
const ownerDirectAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz-"

func ownerDNSLabel(owner string) (string, error) {
	if owner == "" || len(owner) > 40 {
		return "", fmt.Errorf("invalid owner")
	}
	direct := !strings.HasPrefix(owner, "x-") && owner[0] != '-' && owner[len(owner)-1] != '-'
	for i := 0; i < len(owner); i++ {
		c := owner[i]
		if !strings.ContainsRune(ownerAlphabet, rune(c)) {
			return "", fmt.Errorf("invalid owner")
		}
		if !strings.ContainsRune(ownerDirectAlphabet, rune(c)) {
			direct = false
		}
	}
	if direct {
		return owner, nil
	}
	number := new(big.Int)
	forty := big.NewInt(40)
	for i := 0; i < len(owner); i++ {
		number.Mul(number, forty)
		number.Add(number, big.NewInt(int64(strings.IndexByte(ownerAlphabet, owner[i])+1)))
	}
	return "x-" + number.Text(36), nil
}

func within(p, root string) bool {
	p, _ = filepath.Abs(p)
	root, _ = filepath.Abs(root)
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
