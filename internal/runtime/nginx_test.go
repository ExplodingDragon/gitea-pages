// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
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
		RootURL: u, DataRoot: root,
		NginxRoot: filepath.Join(root, "nginx"), NginxHTTP01Root: filepath.Join(root, "http01"),
		NginxTestCommand:   []string{"sh", "-c", "test -f \"$1\"", "--", "{candidate}"},
		NginxReloadCommand: []string{"sh", "-c", "exit 0"}, NginxCommandTimeout: time.Second,
	}
}

func newTestNginxPublisher(t *testing.T, cfg Config) *NginxPublisher {
	t.Helper()
	publisher, err := NewNginxPublisher(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return publisher
}

func TestNginxRenderUsesRouteMarkersAndDefaultCertificate(t *testing.T) {
	root := t.TempDir()
	publisher := newTestNginxPublisher(t, testNginxConfig(t, root))
	certDir := filepath.Join(root, "nginx", "certs", "default")
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fullchain.pem", "privkey.pem"} {
		if err := os.WriteFile(filepath.Join(certDir, name), []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config, err := publisher.render(map[string][]nginxLocation{
		"pages.example":       {{base: "/", root: "/srv/pages", marker: "/srv/routes/root"}},
		"alice.pages.example": {{base: "/demo/", root: "/srv/demo", marker: "/srv/routes/demo"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`certs/default/fullchain.pem`, `location = /demo/.gitea-pages-probe`,
		`alias "/srv/routes/demo"`, `location = /demo { return 308 /demo/$is_args$args; }`,
		`etag off;`, `add_header X-Content-Type-Options nosniff always;`,
		`listen 443 ssl default_server; server_name ""; ssl_reject_handshake on;`,
	} {
		if !strings.Contains(config, expected) {
			t.Fatalf("generated config is missing %q:\n%s", expected, config)
		}
	}
	if strings.Contains(config, `certs/alice.pages.example/fullchain.pem`) {
		t.Fatalf("owner host did not use the default wildcard certificate:\n%s", config)
	}
}

func TestNginxExportRemovesStaleCustomAssets(t *testing.T) {
	root := t.TempDir()
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
	newTestNginxPublisher(t, cfg)
	exported := filepath.Join(cfg.NginxRoot, "public", "assets", "obsolete.css")
	if _, err := os.Stat(exported); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(asset); err != nil {
		t.Fatal(err)
	}
	newTestNginxPublisher(t, cfg)
	if _, err := os.Stat(exported); !os.IsNotExist(err) {
		t.Fatalf("stale custom asset remains, err=%v", err)
	}
}

func TestNginxRenderKeepsHTTPChallengeWithoutCertificate(t *testing.T) {
	publisher := newTestNginxPublisher(t, testNginxConfig(t, t.TempDir()))
	config, err := publisher.render(map[string][]nginxLocation{"custom.example": nil})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"/.well-known/acme-challenge/", `listen 80 default_server; server_name ""; return 404;`, `ssl_reject_handshake on;`} {
		if !strings.Contains(config, expected) {
			t.Fatalf("generated config is missing %q:\n%s", expected, config)
		}
	}
}

func TestNginxHTTPModeDoesNotNeedChallengeDirectory(t *testing.T) {
	cfg := testNginxConfig(t, t.TempDir())
	cfg.RootURL, _ = url.Parse("http://pages.example")
	cfg.NginxHTTP01Root = ""
	publisher := newTestNginxPublisher(t, cfg)
	config, err := publisher.render(map[string][]nginxLocation{"pages.example": nil})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(config, "acme-challenge") || strings.Contains(config, "listen 443") {
		t.Fatalf("unexpected HTTP mode configuration:\n%s", config)
	}
}

func TestNginxApplyRollsBackConfigAfterReloadFailure(t *testing.T) {
	root := t.TempDir()
	cfg := testNginxConfig(t, root)
	cfg.RootURL, _ = url.Parse("http://pages.example")
	publisher := newTestNginxPublisher(t, cfg)
	first := testDeployment(t, 7, strings.Repeat("a", 64), "http://alice.pages.example/demo/", "", map[string]string{"index.html": "one"})
	publisher.deployments[first.RepositoryID] = ActiveDeployment{Deployment: first, Publication: Publication{DefaultPublished: true}}
	if err := switchCurrentContent(first); err != nil {
		t.Fatal(err)
	}
	if err := publisher.apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	oldConfig, err := os.ReadFile(filepath.Join(cfg.NginxRoot, "pages.conf"))
	if err != nil {
		t.Fatal(err)
	}
	oldReady, err := os.ReadFile(filepath.Join(cfg.DataRoot, "route-state", "config-ready"))
	if err != nil {
		t.Fatal(err)
	}
	publisher.cfg.NginxReloadCommand = []string{"sh", "-c", "exit 1"}
	updated := first
	updatedURL, _ := url.Parse("http://alice.pages.example/renamed/")
	updated.DefaultURL = *updatedURL
	publisher.deployments[first.RepositoryID] = ActiveDeployment{Deployment: updated, Publication: Publication{DefaultPublished: true}}
	if err := publisher.apply(t.Context()); err == nil {
		t.Fatal("reload failure unexpectedly succeeded")
	}
	if config, err := os.ReadFile(filepath.Join(cfg.NginxRoot, "pages.conf")); err != nil || string(config) != string(oldConfig) {
		t.Fatalf("configuration changed after failure: err=%v", err)
	}
	if ready, err := os.ReadFile(filepath.Join(cfg.DataRoot, "route-state", "config-ready")); err != nil || string(ready) != string(oldReady) {
		t.Fatalf("ready state changed after failure: err=%v", err)
	}
}

func TestNginxPublishRestoresPreviousDeploymentAfterApplyFailure(t *testing.T) {
	root := t.TempDir()
	cfg := testNginxConfig(t, root)
	cfg.RootURL, _ = url.Parse("http://pages.example")
	publisher := newTestNginxPublisher(t, cfg)
	old := testDeployment(t, 11, strings.Repeat("a", 64), "http://alice.pages.example/docs/", "http://old.example/", map[string]string{"index.html": "old"})
	if err := publisher.Restore(t.Context(), testDeployments{{Deployment: old, Publication: Publication{DefaultPublished: true, CustomPublished: true}}}); err != nil {
		t.Fatal(err)
	}
	oldLink, err := os.Readlink(filepath.Join(filepath.Dir(old.Content.Path()), "current"))
	if err != nil {
		t.Fatal(err)
	}

	updated := testDeployment(t, 11, strings.Repeat("b", 64), "http://alice.pages.example/renamed/", "http://new.example/", map[string]string{"index.html": "new"})
	publisher.cfg.NginxReloadCommand = []string{"sh", "-c", "exit 1"}
	if _, err := publisher.Publish(t.Context(), updated); err == nil {
		t.Fatal("publish unexpectedly succeeded")
	}
	active := publisher.deployments[old.RepositoryID]
	if active.Content.Digest() != old.Content.Digest() || active.DefaultURL.String() != old.DefaultURL.String() {
		t.Fatalf("active deployment after rollback = %#v", active)
	}
	if _, ok := publisher.challengeHosts["old.example"]; !ok {
		t.Fatal("old challenge host was not restored")
	}
	if _, ok := publisher.challengeHosts["new.example"]; ok {
		t.Fatal("new challenge host remained after rollback")
	}
	link, err := os.Readlink(filepath.Join(filepath.Dir(updated.Content.Path()), "current"))
	if err != nil || link != oldLink {
		t.Fatalf("current link after rollback = %q, err=%v, want %q", link, err, oldLink)
	}
}

func TestNginxMarkerUpdateDoesNotReloadUnchangedConfig(t *testing.T) {
	root := t.TempDir()
	cfg := testNginxConfig(t, root)
	cfg.RootURL, _ = url.Parse("http://pages.example")
	testLog, reloadLog := filepath.Join(root, "tests"), filepath.Join(root, "reloads")
	cfg.NginxTestCommand = []string{"sh", "-c", fmt.Sprintf("echo test >> %q; test -f \"$1\"", testLog), "--", "{candidate}"}
	cfg.NginxReloadCommand = []string{"sh", "-c", "echo reload >> \"$1\"", "--", reloadLog}
	publisher := newTestNginxPublisher(t, cfg)
	deployment := testDeployment(t, 8, strings.Repeat("b", 64), "http://alice.pages.example/demo/", "", map[string]string{"asset.js": "asset"})
	if err := switchCurrentContent(deployment); err != nil {
		t.Fatal(err)
	}
	publisher.deployments[8] = ActiveDeployment{Deployment: deployment}
	if err := publisher.apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	publisher.deployments[8] = ActiveDeployment{Deployment: deployment, Publication: Publication{DefaultPublished: true}}
	if err := publisher.apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]int{testLog: 1, reloadLog: 1} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(string(body), "\n"); got != want {
			t.Fatalf("%s operations=%d, want %d", path, got, want)
		}
	}
	marker := publisher.routeMarker("alice.pages.example", "/demo/", 8)
	body, err := os.ReadFile(marker)
	if err != nil || string(body) != "8 "+deployment.Content.Digest()+"\n" {
		t.Fatalf("route marker=%q, err=%v", body, err)
	}
}

func TestNginxApplyRepublishesMissingConfigAndCleansSnapshots(t *testing.T) {
	root := t.TempDir()
	publisher := newTestNginxPublisher(t, testNginxConfig(t, root))
	if err := publisher.apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(publisher.cfg.NginxRoot, "pages.conf")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(publisher.cfg.NginxRoot, "config", "snapshots", strings.Repeat("b", 64))
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "pages.conf"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := publisher.apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(target); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("pages.conf was not restored: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale snapshot remains, err=%v", err)
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
	cfg := testNginxConfig(t, root)
	cfg.CustomRoot = filepath.Dir(custom)
	publisher := newTestNginxPublisher(t, cfg)
	config, err := publisher.render(map[string][]nginxLocation{"pages.example": nil})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(config, "# before") || !strings.HasSuffix(config, "# after\n") {
		t.Fatalf("custom template was not applied: %q", config)
	}
	home, err := os.ReadFile(filepath.Join(cfg.NginxRoot, "public", "index.html"))
	if err != nil || string(home) != "custom Pages home" {
		t.Fatalf("custom platform home=%q err=%v", home, err)
	}
}
