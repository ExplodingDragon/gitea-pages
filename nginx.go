package page

// The nginx publisher owns generated files, while nginx owns the listening
// sockets. A candidate is checked and reloaded atomically so stopping Pages
// does not stop an already working nginx instance.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type NginxPublisher struct {
	cfg            Config
	store          *Store
	resources      platformResources
	removeSnapshot func(string) error
}
type nginxLocation struct{ base, root, marker string }
type nginxMarker struct{ path, body string }

func NewNginxPublisher(cfg Config, store *Store) (*NginxPublisher, error) {
	if err := os.MkdirAll(cfg.NginxRoot, 0755); err != nil {
		return nil, err
	}
	if cfg.NginxHTTP01Root != "" {
		if info, err := os.Lstat(cfg.NginxHTTP01Root); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("nginx HTTP-01 directory must not be a symbolic link")
		} else if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err := os.MkdirAll(cfg.NginxHTTP01Root, 0755); err != nil {
			return nil, err
		}
		realHTTP01, err := filepath.EvalSymlinks(cfg.NginxHTTP01Root)
		if err != nil {
			return nil, err
		}
		realData, err := filepath.EvalSymlinks(cfg.DataRoot)
		if err != nil {
			return nil, err
		}
		realNginx, err := filepath.EvalSymlinks(cfg.NginxRoot)
		if err != nil {
			return nil, err
		}
		if withinPath(realData, realHTTP01) || withinPath(realNginx, realHTTP01) {
			return nil, fmt.Errorf("nginx HTTP-01 directory contains Pages data or output")
		}
		// An interrupted lego order is restarted as a fresh order. Remove its
		// old challenge files before publishing this process's configuration.
		entries, err := os.ReadDir(cfg.NginxHTTP01Root)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if err := os.RemoveAll(filepath.Join(cfg.NginxHTTP01Root, entry.Name())); err != nil {
				return nil, err
			}
		}
	}
	resources, err := loadPlatformResources(cfg.CustomRoot)
	if err != nil {
		return nil, err
	}
	p := &NginxPublisher{cfg: cfg, store: store, resources: resources, removeSnapshot: os.RemoveAll}
	if err := p.exportPublic(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *NginxPublisher) exportPublic() error {
	root := filepath.Join(p.cfg.NginxRoot, "public")
	currentAssets := make(map[string]struct{})
	for name, body := range p.resources.files {
		if name != "index.html" && name != "errors/404.html" && !strings.HasPrefix(name, "assets/") {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err := writeMarker(target, body, 0644); err != nil {
			return err
		}
		if strings.HasPrefix(name, "assets/") {
			currentAssets[filepath.Clean(target)] = struct{}{}
		}
	}

	assetsRoot := filepath.Join(root, "assets")
	directories := make([]string, 0)
	if err := filepath.WalkDir(assetsRoot, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name != assetsRoot {
				directories = append(directories, name)
			}
			return nil
		}
		if _, ok := currentAssets[filepath.Clean(name)]; ok {
			return nil
		}
		return os.Remove(name)
	}); err != nil && !os.IsNotExist(err) {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		_ = os.Remove(directories[i])
	}
	return nil
}

func (p *NginxPublisher) challengeDir(host string) string {
	host = normalizeHost(host)
	if host == "" || strings.ContainsAny(host, "/\\") {
		return ""
	}
	return filepath.Join(p.cfg.NginxHTTP01Root, host)
}

func (p *NginxPublisher) SetChallenge(host, token, key string) {
	if p.cfg.NginxHTTP01Root == "" || token == "" || token != filepath.Base(token) || strings.ContainsAny(token, `/\\`) {
		return
	}
	d := p.challengeDir(host)
	if d == "" {
		return
	}
	_ = os.MkdirAll(d, 0755)
	_ = writeMarker(filepath.Join(d, token), []byte(key), 0644)
}

func (p *NginxPublisher) ClearChallenge(host, token string) {
	if p.cfg.NginxHTTP01Root == "" || token == "" || token != filepath.Base(token) || strings.ContainsAny(token, `/\\`) {
		return
	}
	if d := p.challengeDir(host); d != "" {
		_ = os.Remove(filepath.Join(d, token))
	}
}

func (p *NginxPublisher) Apply(ctx context.Context) error {
	// Keep the certificate set stable from rendering through reload. Otherwise
	// a concurrent renewal could change a certificate after it was included in
	// the ready-state digest but before nginx loaded the generated config.
	certificateFilesMu.Lock()
	defer certificateFilesMu.Unlock()

	initialized, rootURL, sites := p.store.routingSnapshot()
	// An interrupted full listing or an instance root change has no trusted
	// route set. The generated entry must therefore contain no site routes.
	if !initialized || rootURL != p.cfg.RootURL.String() {
		sites = nil
	}
	hosts := make(map[string][]nginxLocation)
	markers := make(map[string]nginxMarker)
	for _, site := range sites {
		if site.Site.Deleted || !site.Site.Enabled {
			continue
		}
		// An active custom Host needs an HTTP challenge endpoint before it has
		// a certificate or a published site route. The challenge file itself is
		// created only while the authorized ACME flow is running.
		for _, domain := range site.Site.CustomDomains {
			if domain.State == "active" {
				host := normalizeHost(domain.Host)
				if _, ok := hosts[host]; !ok {
					hosts[host] = nil
				}
			}
		}
		host := ownerHost(site.Site.OwnerName, p.cfg.RootURL.Hostname())
		if host != "" {
			loc := p.siteLocation(site, host)
			hosts[host] = append(hosts[host], loc)
			if site.InstalledDigest != "" && site.DefaultPublished {
				markers[loc.marker] = nginxMarker{loc.marker, markerBody(host, loc.base, site.Site.RepoID)}
			}
		}
		for _, domain := range site.Site.CustomDomains {
			if domain.State != "active" {
				continue
			}
			host := normalizeHost(domain.Host)
			loc := nginxLocation{"/", p.store.SiteDir(site.Site.RepoID) + "/current/", p.routeMarker(host, "/", site.Site.RepoID)}
			hosts[host] = append(hosts[host], loc)
			if site.InstalledDigest != "" && site.CustomPublished {
				markers[loc.marker] = nginxMarker{loc.marker, markerBody(host, "/", site.Site.RepoID)}
			}
		}
	}
	rootHost := normalizeHost(p.cfg.RootURL.Hostname())
	if _, ok := hosts[rootHost]; !ok {
		hosts[rootHost] = nil
	}
	config, err := p.render(hosts)
	if err != nil {
		return err
	}
	certificateDigest, err := p.certificateDigest()
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(config))
	digest := hex.EncodeToString(sum[:])
	readyState := digest + "\n" + certificateDigest + "\n"
	target := filepath.Join(p.cfg.NginxRoot, "pages.conf")
	targetMatches := false
	if info, e := os.Lstat(target); e == nil && info.Mode().IsRegular() {
		if old, readErr := os.ReadFile(target); readErr == nil {
			targetMatches = string(old) == config
		}
	}
	markerDir := filepath.Join(p.cfg.DataRoot, "route-state")
	if targetMatches && p.readConfirmedReadyState() == readyState {
		if err := os.MkdirAll(markerDir, 0755); err != nil {
			return err
		}
		previousMarkers, err := snapshotMarkers(markerDir)
		if err != nil {
			return err
		}
		if err := writeMarkers(markerDir, markers, true); err != nil {
			_ = restoreMarkers(markerDir, previousMarkers)
			return fmt.Errorf("write nginx route markers: %w", err)
		}
		p.cleanupSnapshots(digest)
		return nil
	}
	confDir := filepath.Join(p.cfg.NginxRoot, "config", "snapshots", digest)
	if err := os.MkdirAll(confDir, 0755); err != nil {
		return err
	}
	candidate := filepath.Join(confDir, "pages.conf")
	if err := os.WriteFile(candidate, []byte(config), 0644); err != nil {
		return err
	}
	if err := runConfigured(ctx, p.cfg.NginxTestCommand, candidate, p.cfg.NginxCommandTimeout); err != nil {
		return fmt.Errorf("nginx config test: %w", err)
	}
	if err := os.MkdirAll(markerDir, 0755); err != nil {
		return err
	}
	previousMarkers, err := snapshotMarkers(markerDir)
	if err != nil {
		return err
	}
	readyPath := filepath.Join(markerDir, "config-ready")
	previousReady, previousReadyExists, err := readOptional(readyPath)
	if err != nil {
		return err
	}
	oldTarget, oldExists, err := readOptional(target)
	if err != nil {
		return err
	}
	// The generated nginx config checks config-ready before serving a route.
	// Close that gate while nginx loads the candidate. This keeps the old
	// running configuration from observing new route markers, and leaves the
	// new configuration closed if the process stops before the commit below.
	if err := os.Remove(readyPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := atomicCopy(candidate, target, 0644); err != nil {
		_ = restoreFile(readyPath, previousReady, previousReadyExists, 0644)
		return err
	}
	if err := runConfigured(ctx, p.cfg.NginxReloadCommand, target, p.cfg.NginxCommandTimeout); err != nil {
		// Certificate files are intentionally kept. The ready-state digest still
		// describes the old running configuration, so the next Apply retries the
		// new certificate material without introducing a second certificate
		// transaction.
		_ = restoreFile(target, oldTarget, oldExists, 0644)
		_ = restoreMarkers(markerDir, previousMarkers)
		_ = restoreFile(readyPath, previousReady, previousReadyExists, 0644)
		return fmt.Errorf("nginx reload: %w", err)
	}
	// The reload has installed the candidate. Commit the route markers and the
	// readiness gate afterwards, so a crash during reload cannot publish a
	// configuration that nginx has not accepted.
	if err := writeMarkers(markerDir, markers, true); err != nil {
		_ = os.Remove(readyPath)
		return fmt.Errorf("write nginx route markers: %w", err)
	}
	if err := writeMarker(readyPath, []byte(readyState), 0644); err != nil {
		_ = os.Remove(readyPath)
		return fmt.Errorf("write nginx ready state: %w", err)
	}
	p.cleanupSnapshots(digest)
	return nil
}

// cleanupSnapshots removes only Pages-generated snapshot directories that are
// no longer current. A cleanup error is deliberately reported and ignored:
// the new configuration has already passed nginx's checks and reload, so a
// housekeeping failure must not turn a successful publication into an error.
// The next Apply retries the same cleanup, including the no-change path above.
func (p *NginxPublisher) cleanupSnapshots(currentDigest string) {
	root := filepath.Join(p.cfg.NginxRoot, "config", "snapshots")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pages nginx snapshot cleanup: %v\n", err)
		return
	}

	keep := map[string]struct{}{currentDigest: {}}
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != sha256.Size*2 || !isHexDigest(name) {
			continue
		}
		if _, ok := keep[name]; ok || entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(root, name, "pages.conf")
		info, err := os.Lstat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := p.removeSnapshot(filepath.Join(root, name)); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "pages nginx snapshot cleanup %s: %v\n", name, err)
		}
	}
}

func isHexDigest(value string) bool {
	for _, r := range value {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func (p *NginxPublisher) confirmedConfigDigest() string {
	b, err := os.ReadFile(filepath.Join(p.cfg.DataRoot, "route-state", "config-ready"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
}

func (p *NginxPublisher) readConfirmedReadyState() string {
	b, err := os.ReadFile(filepath.Join(p.cfg.DataRoot, "route-state", "config-ready"))
	if err != nil {
		return ""
	}
	return string(b)
}

func (p *NginxPublisher) certificateDigest() (string, error) {
	root := filepath.Join(p.cfg.NginxRoot, "certs")
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) && path == root {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		_, _ = hash.Write([]byte(relative))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(body)
		_, _ = hash.Write([]byte{0})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (p *NginxPublisher) siteLocation(site LocalSite, host string) nginxLocation {
	base := "/"
	if !strings.EqualFold(site.Site.RepoName, ".pages") {
		base = "/" + strings.ToLower(site.Site.RepoName) + "/"
	}
	return nginxLocation{base, p.store.SiteDir(site.Site.RepoID) + "/current/", p.routeMarker(host, base, site.Site.RepoID)}
}

func (p *NginxPublisher) routeMarker(host, base string, repoID int64) string {
	sum := sha256.Sum256([]byte(host + "\x00" + base + "\x00" + strconv.FormatInt(repoID, 10)))
	return filepath.Join(p.cfg.DataRoot, "route-state", "route-"+hex.EncodeToString(sum[:]))
}

func markerBody(host, base string, repoID int64) string {
	return host + " " + base + " " + strconv.FormatInt(repoID, 10) + "\n"
}

func (p *NginxPublisher) render(hosts map[string][]nginxLocation) (string, error) {
	var managed strings.Builder
	managed.WriteString("# Generated by gitea-pages. Include this file from the nginx http context.\n")
	managed.WriteString("# Pages owns the default servers below; use this file with a dedicated Pages nginx listener.\n")
	p.writeFallbackServers(&managed)
	names := make([]string, 0, len(hosts))
	for host := range hosts {
		names = append(names, host)
	}
	sort.Strings(names)
	for _, host := range names {
		locations := hosts[host]
		sort.Slice(locations, func(i, j int) bool { return locations[i].base < locations[j].base })
		p.writeServer(&managed, host, locations)
	}
	root := p.cfg.CustomRoot
	if root == "" {
		return managed.String(), nil
	}
	template, err := readCustomFile(filepath.Join(root, "nginx", "pages.conf.tmpl"))
	if err != nil {
		if os.IsNotExist(err) {
			return managed.String(), nil
		}
		return "", err
	}
	if strings.Count(template, "{{ managed_config }}") != 1 {
		return "", fmt.Errorf("nginx template must contain exactly one {{ managed_config }}")
	}
	server, err := readCustomOptional(filepath.Join(root, "nginx", "server.inc"))
	if err != nil {
		return "", err
	}
	location, err := readCustomOptional(filepath.Join(root, "nginx", "location.inc"))
	if err != nil {
		return "", err
	}
	managedText := strings.ReplaceAll(managed.String(), "server {", "server {\n"+server)
	// The generated static locations all carry the index directive.  Inserting
	// at that stable point keeps operator location.inc out of the ACME and
	// platform locations, whose behavior must remain under Pages control.
	managedText = strings.ReplaceAll(managedText, "index index.html;", "index index.html;\n"+location)
	return strings.Replace(template, "{{ managed_config }}", managedText, 1), nil
}

func (p *NginxPublisher) writeFallbackServers(b *strings.Builder) {
	fmt.Fprintln(b, `server { listen 80 default_server; server_name ""; return 404; }`)
	if p.cfg.RootURL.Scheme != "https" {
		return
	}
	// ssl_reject_handshake does not need a certificate and is available since
	// nginx 1.19.4. It makes the default HTTPS server reject unknown SNI
	// during the handshake, including while the Pages default certificate is
	// still being obtained.
	fmt.Fprintln(b, `server { listen 443 ssl default_server; server_name ""; ssl_reject_handshake on; }`)
}

func readCustomFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 16*1024*1024 {
		return "", fmt.Errorf("custom nginx resource %s is not a regular file within the size limit", path)
	}
	b, err := os.ReadFile(path)
	return string(b), err
}

func readCustomOptional(path string) (string, error) {
	b, err := readCustomFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	return b, err
}

func (p *NginxPublisher) writeServer(b *strings.Builder, host string, locations []nginxLocation) {
	qhost := nginxQuote(host)
	fmt.Fprintf(b, "server { listen 80; server_name %s; etag off; if_modified_since off; add_header X-Content-Type-Options nosniff always; add_header Cache-Control 'no-cache, must-revalidate' always;\n", qhost)
	if p.cfg.RootURL.Scheme == "https" {
		qroot := nginxQuote(p.cfg.NginxHTTP01Root + "/" + host + "/")
		fmt.Fprintf(b, "location ^~ /.well-known/acme-challenge/ { alias %s; }\n", qroot)
	}
	fmt.Fprintln(b, "location = /@gitea-pages/ready { add_header Cache-Control 'no-store' always; return 204; }")
	if p.cfg.RootURL.Scheme == "https" {
		fmt.Fprintln(b, "location / { return 308 https://$host$request_uri; }")
	} else {
		p.writePlatformLocations(b, host)
		writeLocations(b, locations, filepath.Join(p.cfg.DataRoot, "route-state", "config-ready"))
	}
	fmt.Fprintln(b, "}")
	if p.cfg.RootURL.Scheme != "https" {
		return
	}
	certDir := filepath.Join(p.cfg.NginxRoot, "certs", "default")
	rootHost := normalizeHost(p.cfg.RootURL.Hostname())
	if host != rootHost && !strings.HasSuffix(host, "."+rootHost) {
		certDir = filepath.Join(p.cfg.NginxRoot, "certs", host)
	}
	if _, err := os.Stat(filepath.Join(certDir, "fullchain.pem")); err != nil {
		return
	}
	if _, err := os.Stat(filepath.Join(certDir, "privkey.pem")); err != nil {
		return
	}
	fmt.Fprintf(b, "server { listen 443 ssl; server_name %s; etag off; if_modified_since off; add_header X-Content-Type-Options nosniff always; add_header Cache-Control 'no-cache, must-revalidate' always;\n", qhost)
	fmt.Fprintf(b, "ssl_certificate %s; ssl_certificate_key %s;\n", nginxQuote(filepath.Join(certDir, "fullchain.pem")), nginxQuote(filepath.Join(certDir, "privkey.pem")))
	fmt.Fprintln(b, "location = /@gitea-pages/ready { add_header Cache-Control 'no-store' always; return 204; }")
	p.writePlatformLocations(b, host)
	writeLocations(b, locations, filepath.Join(p.cfg.DataRoot, "route-state", "config-ready"))
	fmt.Fprintln(b, "}")
}

func (p *NginxPublisher) writePlatformLocations(b *strings.Builder, host string) {
	public := filepath.Join(p.cfg.NginxRoot, "public")
	fmt.Fprintf(b, "location ^~ /@gitea-pages/assets/ { alias %s; }\n", nginxQuote(filepath.Join(public, "assets")+"/"))
	fmt.Fprintf(b, "error_page 404 /@gitea-pages/error404; location = /@gitea-pages/error404 { internal; add_header Cache-Control 'no-store' always; alias %s; }\n", nginxQuote(filepath.Join(public, "errors", "404.html")))
	if host == normalizeHost(p.cfg.RootURL.Hostname()) {
		fmt.Fprintf(b, "location = / { root %s; try_files /index.html =404; }\n", nginxQuote(public))
	}
}

func writeLocations(b *strings.Builder, locations []nginxLocation, configMarker string) {
	if len(locations) == 0 {
		fmt.Fprintln(b, "location / { return 404; }")
		return
	}
	hasRoot := false
	for _, l := range locations {
		if l.base == "/" {
			hasRoot = true
			fmt.Fprintf(b, "location / { if (!-f %s) { return 404; } if (!-f %s) { return 404; } root %s; index index.html; }\n", nginxQuote(configMarker), nginxQuote(l.marker), nginxQuote(l.root))
		} else {
			base := strings.TrimSuffix(l.base, "/")
			fmt.Fprintf(b, "location = %s { return 308 %s$is_args$args; }\n", base, base+"/")
			fmt.Fprintf(b, "location %s { if (!-f %s) { return 404; } if (!-f %s) { return 404; } alias %s; index index.html; }\n", l.base, nginxQuote(configMarker), nginxQuote(l.marker), nginxQuote(l.root))
		}
	}
	if !hasRoot {
		fmt.Fprintln(b, "location / { return 404; }")
	}
}

func runConfigured(parent context.Context, command []string, candidate string, timeout time.Duration) error {
	args := make([]string, len(command))
	for i, v := range command {
		args[i] = strings.ReplaceAll(v, "{candidate}", candidate)
	}
	if len(args) == 0 || args[0] == "" {
		return fmt.Errorf("empty configured command")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func nginxQuote(s string) string { return strconv.Quote(s) }
func readOptional(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return b, err == nil, err
}

func atomicCopy(src, target string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func restoreFile(path string, data []byte, exists bool, mode os.FileMode) error {
	if !exists {
		return os.Remove(path)
	}
	return os.WriteFile(path, data, mode)
}

func snapshotMarkers(dir string) (map[string][]byte, error) {
	r := map[string][]byte{}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			r[e.Name()] = b
		}
	}
	return r, nil
}

func writeMarkers(dir string, markers map[string]nginxMarker, removeStale bool) error {
	keep := map[string]struct{}{"config-ready": {}}
	for _, m := range markers {
		name := filepath.Base(m.path)
		keep[name] = struct{}{}
		if err := writeMarker(m.path, []byte(m.body), 0644); err != nil {
			return err
		}
	}
	if !removeStale {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			if _, ok := keep[e.Name()]; !ok {
				if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
		}
	}
	return nil
}

func writeMarker(path string, body []byte, mode os.FileMode) error {
	if err := os.WriteFile(path+".tmp", body, mode); err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		_ = os.Remove(path + ".tmp")
		return err
	}
	return nil
}

func restoreMarkers(dir string, previous map[string][]byte) error {
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			if _, ok := previous[e.Name()]; !ok {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	for name, b := range previous {
		if err := writeMarker(filepath.Join(dir, name), b, 0644); err != nil {
			return err
		}
	}
	return nil
}
