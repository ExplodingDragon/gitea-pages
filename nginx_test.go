// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package page

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testNginxConfig(t *testing.T, root string) Config {
	t.Helper()
	u, err := url.Parse("https://pages.example")
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		RootURL:             u,
		DataRoot:            root,
		NginxRoot:           filepath.Join(root, "nginx"),
		NginxHTTP01Root:     filepath.Join(root, "http01"),
		NginxTestCommand:    []string{"sh", "-c", "test -f \"$1\"", "--", "{candidate}"},
		NginxReloadCommand:  []string{"sh", "-c", "exit 0"},
		NginxCommandTimeout: time.Second,
	}
}

func TestNginxRenderUsesDefaultCertificateForPlatformHost(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := testNginxConfig(t, root)
	p, err := NewNginxPublisher(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "nginx", "public", "index.html")); err != nil {
		t.Fatalf("platform home was not exported: %v", err)
	}
	certDir := filepath.Join(root, "nginx", "certs", "default")
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fullchain.pem", "privkey.pem"} {
		if err := os.WriteFile(filepath.Join(certDir, name), []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config, err := p.render(map[string][]nginxLocation{
		"pages.example":       {{base: "/", root: "/srv/pages", marker: "/srv/route-state/root"}},
		"alice.pages.example": {{base: "/demo/", root: "/srv/demo", marker: "/srv/route-state/demo"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(config, `certs/default/fullchain.pem`) {
		t.Fatalf("platform host did not use default certificate directory:\n%s", config)
	}
	if strings.Contains(config, `certs/alice.pages.example/fullchain.pem`) {
		t.Fatalf("owner host must use the default wildcard certificate:\n%s", config)
	}
	if !strings.Contains(config, `if (!-f "/srv/route-state/root")`) {
		t.Fatalf("route marker check missing:\n%s", config)
	}
	if !strings.Contains(config, `location = /demo { return 308 /demo/$is_args$args; }`) {
		t.Fatalf("project root does not redirect to its slash form while preserving query parameters:\n%s", config)
	}
	if !strings.Contains(config, "location = / {") || !strings.Contains(config, "@gitea-pages/error404") {
		t.Fatalf("platform home or 404 route missing:\n%s", config)
	}
	for _, directive := range []string{"etag off;", "if_modified_since off;", "add_header X-Content-Type-Options nosniff always;"} {
		if !strings.Contains(config, directive) {
			t.Fatalf("generated server is missing %q:\n%s", directive, config)
		}
	}
	for _, directive := range []string{
		`listen 80 default_server; server_name ""; return 404;`,
		`listen 443 ssl default_server; server_name "";`,
		`ssl_reject_handshake on;`,
		`return 404;`,
	} {
		if !strings.Contains(config, directive) {
			t.Fatalf("generated config is missing unknown-host fallback %q:\n%s", directive, config)
		}
	}
}

func TestNginxExportRemovesStaleCustomAssets(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	customRoot := filepath.Join(root, "custom")
	asset := filepath.Join(customRoot, "public", "assets", "obsolete.css")
	if err := os.MkdirAll(filepath.Dir(asset), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asset, []byte("obsolete"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testNginxConfig(t, root)
	cfg.CustomRoot = customRoot
	if _, err := NewNginxPublisher(cfg, store); err != nil {
		t.Fatal(err)
	}
	exported := filepath.Join(cfg.NginxRoot, "public", "assets", "obsolete.css")
	if _, err := os.Stat(exported); err != nil {
		t.Fatalf("custom asset was not exported: %v", err)
	}
	if err := os.Remove(asset); err != nil {
		t.Fatal(err)
	}
	if _, err := NewNginxPublisher(cfg, store); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(exported); !os.IsNotExist(err) {
		t.Fatalf("stale custom asset remains available, err=%v", err)
	}
}

func TestNginxRenderKeepsHTTPChallengeWithoutCertificate(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p, err := NewNginxPublisher(testNginxConfig(t, root), store)
	if err != nil {
		t.Fatal(err)
	}
	config, err := p.render(map[string][]nginxLocation{"custom.example": nil})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(config, "server {") != 3 || !strings.Contains(config, "listen 443 ssl default_server") || !strings.Contains(config, "/.well-known/acme-challenge/") {
		t.Fatalf("expected HTTP/HTTPS fallback and challenge servers while TLS certificate is missing:\n%s", config)
	}
	if !strings.Contains(config, `listen 80 default_server; server_name ""; return 404;`) {
		t.Fatalf("unknown HTTP hosts are not rejected:\n%s", config)
	}
	if !strings.Contains(config, `listen 443 ssl default_server; server_name ""; ssl_reject_handshake on;`) {
		t.Fatalf("unknown HTTPS SNI is not rejected while the default certificate is missing:\n%s", config)
	}
	if !strings.HasSuffix(config, "}\n") {
		t.Fatalf("unfinished nginx server block:\n%s", config)
	}
}

func TestNginxHTTPSFallbackRejectsUnknownSNIWithoutDefaultCertificate(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := testNginxConfig(t, root)
	p, err := NewNginxPublisher(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	customCertDir := filepath.Join(cfg.NginxRoot, "certs", "custom.example")
	if err := os.MkdirAll(customCertDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fullchain.pem", "privkey.pem"} {
		if err := os.WriteFile(filepath.Join(customCertDir, name), []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config, err := p.render(map[string][]nginxLocation{"custom.example": nil})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(config, "listen 443 ssl; server_name \"custom.example\"") {
		t.Fatalf("custom HTTPS server was not generated:\n%s", config)
	}
	if !strings.Contains(config, `listen 443 ssl default_server; server_name ""; ssl_reject_handshake on;`) {
		t.Fatalf("unknown HTTPS SNI fallback was not generated without a default certificate:\n%s", config)
	}
}

func TestNginxHTTPModeDoesNotNeedChallengeDirectory(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := testNginxConfig(t, root)
	cfg.RootURL, _ = url.Parse("http://pages.example")
	cfg.NginxHTTP01Root = ""
	p, err := NewNginxPublisher(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	config, err := p.render(map[string][]nginxLocation{"pages.example": nil})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(config, "acme-challenge") || strings.Contains(config, "listen 443") || !strings.Contains(config, "location = / {") {
		t.Fatalf("unexpected HTTP mode configuration:\n%s", config)
	}
}

func TestNginxApplyRollsBackAfterReloadFailure(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	digest := strings.Repeat("a", 64)
	site := Site{RepoID: 7, Enabled: true, OwnerName: "alice", RepoName: "demo", TarGzSHA256: digest, PagesUpdatedUnixNano: 1}
	setTestSites(t, store, "http://pages.example", 1, 1, []Site{site})
	cfg := testNginxConfig(t, root)
	cfg.RootURL, _ = url.Parse("http://pages.example")
	p, err := NewNginxPublisher(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveDeploymentResult(site, digest, true, false); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(filepath.Join(cfg.NginxRoot, "pages.conf"))
	if err != nil {
		t.Fatal(err)
	}
	oldReady, err := os.ReadFile(filepath.Join(p.cfg.DataRoot, "route-state", "config-ready"))
	if err != nil {
		t.Fatal(err)
	}
	oldSnapshotDigest := strings.SplitN(string(oldReady), "\n", 2)[0]
	readyPath := filepath.Join(cfg.DataRoot, "route-state", "config-ready")
	cfg.NginxReloadCommand = []string{"sh", "-c", "if [ -f \"$1\" ]; then exit 2; fi; exit 1", "--", readyPath}
	p.cfg = cfg
	setTestSites(t, store, "http://pages.example", 1, 1, []Site{{RepoID: 7, Enabled: true, OwnerName: "alice", RepoName: "renamed", TarGzSHA256: digest, PagesUpdatedUnixNano: 2}})
	if err := p.Apply(context.Background()); err == nil {
		t.Fatal("reload failure unexpectedly succeeded")
	}
	got, err := os.ReadFile(filepath.Join(cfg.NginxRoot, "pages.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(old) {
		t.Fatalf("configuration was not restored after reload failure")
	}
	gotReady, err := os.ReadFile(filepath.Join(cfg.DataRoot, "route-state", "config-ready"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotReady) != string(oldReady) {
		t.Fatalf("config-ready was not restored after reload failure")
	}
	if _, err := os.Stat(filepath.Join(cfg.NginxRoot, "config", "snapshots", oldSnapshotDigest, "pages.conf")); err != nil {
		t.Fatalf("old snapshot was removed after reload failure: %v", err)
	}
}

func TestNginxApplyRepublishesMissingPagesConfig(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	digest := strings.Repeat("a", 64)
	site := Site{RepoID: 7, Enabled: true, OwnerName: "alice", RepoName: "demo", TarGzSHA256: digest, PagesUpdatedUnixNano: 1}
	setTestSites(t, store, "https://pages.example", 1, 1, []Site{site})
	if _, err := store.SaveDeploymentResult(site, digest, true, false); err != nil {
		t.Fatal(err)
	}
	cfg := testNginxConfig(t, root)
	p, err := NewNginxPublisher(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(cfg.NginxRoot, "pages.conf")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(context.Background()); err != nil {
		t.Fatalf("Apply did not restore a missing pages.conf: %v", err)
	}
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("restored pages.conf is not a regular file: %s", info.Mode())
	}
	if body, err := os.ReadFile(target); err != nil || len(body) == 0 {
		t.Fatalf("restored pages.conf is empty or unreadable: err=%v", err)
	}
}

func TestNginxApplyActivatesRouteWithoutReloadWhenConfigIsUnchanged(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	digest := strings.Repeat("a", 64)
	site := Site{RepoID: 7, Enabled: true, OwnerName: "alice", RepoName: "demo", TarGzSHA256: digest, PagesUpdatedUnixNano: 1}
	setTestSites(t, store, "http://pages.example", 1, 1, []Site{site})
	cfg := testNginxConfig(t, root)
	cfg.RootURL, _ = url.Parse("http://pages.example")
	testLog := filepath.Join(root, "nginx-tests")
	reloadLog := filepath.Join(root, "nginx-reloads")
	cfg.NginxTestCommand = []string{"sh", "-c", fmt.Sprintf("echo test >> %q; test -f \"$1\"", testLog), "--", "{candidate}"}
	cfg.NginxReloadCommand = []string{"sh", "-c", "echo reload >> \"$1\"", "--", reloadLog}
	p, err := NewNginxPublisher(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(cfg.NginxRoot, "pages.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), `location /demo/`) {
		t.Fatalf("unpublished site route stub is missing:\n%s", config)
	}
	marker := p.routeMarker(ownerHost(site.OwnerName, cfg.RootURL.Hostname()), "/demo/", site.RepoID)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unpublished site has an active route marker, err=%v", err)
	}
	if _, err := os.Stat(testLog); err != nil {
		t.Fatalf("initial nginx config test did not run: %v", err)
	}
	if _, err := store.SaveDeploymentResult(site, digest, true, false); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("published site route marker was not created: %v", err)
	}
	tests, err := os.ReadFile(testLog)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(tests), "test"); got != 1 {
		t.Fatalf("marker-only update ran nginx config test %d times, want 1", got)
	}
	reloads, err := os.ReadFile(reloadLog)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(reloads), "reload"); got != 1 {
		t.Fatalf("marker-only update ran nginx reload %d times, want 1", got)
	}
}

func TestNginxApplyReloadsWhenCertificateChanges(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	digest := strings.Repeat("a", 64)
	site := Site{RepoID: 7, Enabled: true, OwnerName: "alice", RepoName: "demo", TarGzSHA256: digest, PagesUpdatedUnixNano: 1}
	setTestSites(t, store, "https://pages.example", 1, 1, []Site{site})
	if _, err := store.SaveDeploymentResult(site, digest, true, false); err != nil {
		t.Fatal(err)
	}
	cfg := testNginxConfig(t, root)
	certDir := filepath.Join(cfg.NginxRoot, "certs", "default")
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fullchain.pem", "privkey.pem"} {
		if err := os.WriteFile(filepath.Join(certDir, name), []byte("initial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reloadLog := filepath.Join(root, "reloads")
	cfg.NginxReloadCommand = []string{"sh", "-c", "echo reload >> \"$1\"", "--", reloadLog}
	p, err := NewNginxPublisher(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	failMarker := filepath.Join(root, "fail-once")
	readyPath := filepath.Join(cfg.DataRoot, "route-state", "config-ready")
	cfg.NginxReloadCommand = []string{"sh", "-c", "if [ -f \"$3\" ]; then exit 2; fi; if [ ! -f \"$1\" ]; then touch \"$1\"; exit 1; fi; echo reload >> \"$2\"", "--", failMarker, reloadLog, readyPath}
	p.cfg = cfg
	oldReady, err := os.ReadFile(filepath.Join(cfg.DataRoot, "route-state", "config-ready"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "fullchain.pem"), []byte("rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(context.Background()); err == nil {
		t.Fatal("reload failure unexpectedly succeeded")
	}
	if body, err := os.ReadFile(filepath.Join(certDir, "fullchain.pem")); err != nil || string(body) != "rotated" {
		t.Fatalf("new certificate material was not retained after reload failure: err=%v body=%q", err, body)
	}
	gotReady, err := os.ReadFile(filepath.Join(cfg.DataRoot, "route-state", "config-ready"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotReady) != string(oldReady) {
		t.Fatalf("ready state changed after reload failure: got %q want %q", gotReady, oldReady)
	}
	if err := p.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	reloads, err := os.ReadFile(reloadLog)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(reloads), "reload"); got != 2 {
		t.Fatalf("certificate update triggered %d reloads, want 2", got)
	}
}

func TestNginxApplyCleansObsoleteSnapshots(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	digest := strings.Repeat("a", 64)
	site := Site{RepoID: 7, Enabled: true, OwnerName: "alice", RepoName: "demo", TarGzSHA256: digest, PagesUpdatedUnixNano: 1}
	setTestSites(t, store, "http://pages.example", 1, 1, []Site{site})
	if _, err := store.SaveDeploymentResult(site, digest, true, false); err != nil {
		t.Fatal(err)
	}
	cfg := testNginxConfig(t, root)
	cfg.RootURL, _ = url.Parse("http://pages.example")
	p, err := NewNginxPublisher(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldReady, err := os.ReadFile(filepath.Join(p.cfg.DataRoot, "route-state", "config-ready"))
	if err != nil {
		t.Fatal(err)
	}
	oldDigest := strings.SplitN(string(oldReady), "\n", 2)[0]
	setTestSites(t, store, "http://pages.example", 1, 1, []Site{{RepoID: 7, Enabled: true, OwnerName: "alice", RepoName: "renamed", TarGzSHA256: digest, PagesUpdatedUnixNano: 2}})
	if err := p.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	newReady, err := os.ReadFile(filepath.Join(p.cfg.DataRoot, "route-state", "config-ready"))
	if err != nil {
		t.Fatal(err)
	}
	newDigest := strings.SplitN(string(newReady), "\n", 2)[0]
	if oldDigest == newDigest {
		t.Fatal("route update reused the old snapshot digest")
	}
	if _, err := os.Stat(filepath.Join(p.cfg.NginxRoot, "config", "snapshots", oldDigest)); !os.IsNotExist(err) {
		t.Fatalf("obsolete snapshot still exists, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(p.cfg.NginxRoot, "config", "snapshots", newDigest, "pages.conf")); err != nil {
		t.Fatalf("current snapshot was removed: %v", err)
	}
}

func TestNginxNoChangeRetriesSnapshotCleanup(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	digest := strings.Repeat("a", 64)
	site := Site{RepoID: 7, Enabled: true, OwnerName: "alice", RepoName: "demo", TarGzSHA256: digest, PagesUpdatedUnixNano: 1}
	setTestSites(t, store, "https://pages.example", 1, 1, []Site{site})
	if _, err := store.SaveDeploymentResult(site, digest, true, false); err != nil {
		t.Fatal(err)
	}
	p, err := NewNginxPublisher(testNginxConfig(t, root), store)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(p.cfg.NginxRoot, "config", "snapshots", strings.Repeat("b", 64))
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "pages.conf"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	p.removeSnapshot = func(path string) error {
		attempts++
		if attempts == 1 {
			return os.ErrPermission
		}
		return os.RemoveAll(path)
	}
	if err := p.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("failed cleanup should leave stale snapshot for retry: %v", err)
	}
	if err := p.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("retry did not remove stale snapshot, err=%v", err)
	}
}

func TestNginxCustomTemplate(t *testing.T) {
	root := t.TempDir()
	custom := filepath.Join(root, "custom", "nginx")
	if err := os.MkdirAll(custom, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(filepath.Dir(custom), "public"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(custom), "public", "index.html"), []byte("custom Pages home"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custom, "pages.conf.tmpl"), []byte("# before\n{{ managed_config }}\n# after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := testNginxConfig(t, root)
	cfg.CustomRoot = filepath.Dir(custom)
	p, err := NewNginxPublisher(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	config, err := p.render(map[string][]nginxLocation{"pages.example": nil})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(config, "# before") || !strings.HasSuffix(config, "# after\n") {
		t.Fatalf("custom nginx template was not applied: %q", config)
	}
	home, err := os.ReadFile(filepath.Join(cfg.NginxRoot, "public", "index.html"))
	if err != nil || string(home) != "custom Pages home" {
		t.Fatalf("custom platform home export = %q, err=%v", home, err)
	}
}
