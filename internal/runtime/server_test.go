// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

type testDeployments []ActiveDeployment

func (d testDeployments) ForEach(fn func(ActiveDeployment) error) error {
	for _, deployment := range d {
		if err := fn(deployment); err != nil {
			return err
		}
	}
	return nil
}

func newTestStandalonePublisher(t *testing.T, scheme, customRoot string, deployments ...ActiveDeployment) *StandalonePublisher {
	t.Helper()
	root, err := url.Parse(scheme + "://pages.example")
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := NewStandalonePublisher(Config{RootURL: root, CustomRoot: customRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Restore(t.Context(), testDeployments(deployments)); err != nil {
		t.Fatal(err)
	}
	return publisher
}

func testDeployment(t *testing.T, repoID int64, digest, defaultAddress, customAddress string, files map[string]string) Deployment {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	defaultURL, err := url.Parse(defaultAddress)
	if err != nil {
		t.Fatal(err)
	}
	deployment := Deployment{RepositoryID: repoID, DefaultURL: *defaultURL, Content: Content{digest: digest, root: root}}
	if customAddress != "" {
		customURL, err := url.Parse(customAddress)
		if err != nil {
			t.Fatal(err)
		}
		deployment.CustomURL = customURL
	}
	return deployment
}

func requestStandalone(t *testing.T, publisher *StandalonePublisher, target, host string, secure bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = host
	resp := httptest.NewRecorder()
	publisher.Handler(secure).ServeHTTP(resp, req)
	return resp
}

func TestStandalonePublisherServesIndexlessContentAndProbe(t *testing.T) {
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	deployment := testDeployment(t, 7, digest, "http://alice.pages.example/docs/", "", map[string]string{
		"assets/app.js": "app", "download.unknown": "opaque", "release..txt": "valid name",
	})
	publisher := newTestStandalonePublisher(t, "http", "", ActiveDeployment{Deployment: deployment, Publication: Publication{DefaultPublished: true}})

	if resp := requestStandalone(t, publisher, "http://alice.pages.example/docs/", "alice.pages.example", false); resp.Code != http.StatusNotFound {
		t.Fatalf("indexless root status=%d body=%q", resp.Code, resp.Body.String())
	}
	if resp := requestStandalone(t, publisher, "http://alice.pages.example/docs/assets/app.js", "alice.pages.example", false); resp.Code != http.StatusOK || resp.Body.String() != "app" {
		t.Fatalf("asset status=%d body=%q", resp.Code, resp.Body.String())
	}
	if resp := requestStandalone(t, publisher, "http://alice.pages.example/docs/.gitea-pages-probe", "alice.pages.example", false); resp.Code != http.StatusOK || resp.Body.String() != "7 "+digest+"\n" {
		t.Fatalf("probe status=%d body=%q", resp.Code, resp.Body.String())
	}
	if resp := requestStandalone(t, publisher, "http://alice.pages.example/docs/download.unknown", "alice.pages.example", false); resp.Code != http.StatusOK || resp.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("unknown file status=%d type=%q", resp.Code, resp.Header().Get("Content-Type"))
	}
	if resp := requestStandalone(t, publisher, "http://alice.pages.example/docs/release..txt", "alice.pages.example", false); resp.Code != http.StatusOK || resp.Body.String() != "valid name" {
		t.Fatalf("double-dot file status=%d body=%q", resp.Code, resp.Body.String())
	}
}

func TestStandalonePublisherReplacesRepositoryRoutes(t *testing.T) {
	digest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	old := testDeployment(t, 8, digest, "https://alice.pages.example/docs/", "https://custom.example/", map[string]string{"index.html": "old"})
	publisher := newTestStandalonePublisher(t, "https", "", ActiveDeployment{Deployment: old, Publication: Publication{DefaultPublished: true, CustomPublished: true}})
	if resp := requestStandalone(t, publisher, "http://custom.example/check", "custom.example", false); resp.Code != http.StatusPermanentRedirect {
		t.Fatalf("custom redirect status=%d", resp.Code)
	}

	updated := old
	updatedURL, _ := url.Parse("https://alice.pages.example/renamed/")
	updated.DefaultURL = *updatedURL
	updated.CustomURL = nil
	publisher.routeIndex.mu.Lock()
	publisher.setDeploymentLocked(updated, Publication{DefaultPublished: true})
	publisher.routeIndex.mu.Unlock()
	if resp := requestStandalone(t, publisher, "http://custom.example/check", "custom.example", false); resp.Code != http.StatusNotFound {
		t.Fatalf("removed custom host status=%d", resp.Code)
	}
	if resp := requestStandalone(t, publisher, "https://alice.pages.example/renamed/", "alice.pages.example", true); resp.Code != http.StatusOK || resp.Body.String() != "old" {
		t.Fatalf("renamed route status=%d body=%q", resp.Code, resp.Body.String())
	}
}

func TestStandalonePublisherDoesNotFollowEscapingSymlink(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	deployment := testDeployment(t, 9, "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "http://alice.pages.example/docs/", "", nil)
	if err := os.Symlink(outside, filepath.Join(deployment.Content.Path(), "index.html")); err != nil {
		t.Fatal(err)
	}
	publisher := newTestStandalonePublisher(t, "http", "", ActiveDeployment{Deployment: deployment, Publication: Publication{DefaultPublished: true}})
	if resp := requestStandalone(t, publisher, "http://alice.pages.example/docs/", "alice.pages.example", false); resp.Code != http.StatusNotFound {
		t.Fatalf("escaping symlink status=%d body=%q", resp.Code, resp.Body.String())
	}
}

func TestStandalonePublisherUsesSiteAndPlatformResources(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom")
	if err := os.MkdirAll(filepath.Join(custom, "public", "errors"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custom, "public", "index.html"), []byte("custom home"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custom, "public", "errors", "404.html"), []byte("platform missing"), 0o644); err != nil {
		t.Fatal(err)
	}
	deployment := testDeployment(t, 10, "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", "http://alice.pages.example/docs/", "", map[string]string{"404.html": "site missing"})
	publisher := newTestStandalonePublisher(t, "http", custom, ActiveDeployment{Deployment: deployment, Publication: Publication{DefaultPublished: true}})
	if resp := requestStandalone(t, publisher, "http://alice.pages.example/docs/missing", "alice.pages.example", false); resp.Code != http.StatusNotFound || resp.Body.String() != "site missing" {
		t.Fatalf("site 404 status=%d body=%q", resp.Code, resp.Body.String())
	}
	if resp := requestStandalone(t, publisher, "http://pages.example/", "pages.example", false); resp.Code != http.StatusOK || resp.Body.String() != "custom home" {
		t.Fatalf("platform home status=%d body=%q", resp.Code, resp.Body.String())
	}
}

func TestOwnerHostUsesGiteaAddressEncoding(t *testing.T) {
	for _, test := range []struct{ owner, expected string }{
		{"alice", "alice.pages.example.test"},
		{"team.docs", "x-25q6w3iheo.pages.example.test"},
		{"team_docs", "x-25q6w51cps.pages.example.test"},
		{"x-pages", "x-1xpbv8j4.pages.example.test"},
	} {
		if actual := ownerHost(test.owner, "pages.example.test"); actual != test.expected {
			t.Fatalf("ownerHost(%q)=%q, want %q", test.owner, actual, test.expected)
		}
	}
}

func TestDefaultSiteRoute(t *testing.T) {
	for _, test := range []struct{ repository, host, base string }{
		{repository: ".pages", host: "alice.pages.example.test", base: "/"},
		{repository: ".PAGES", host: "alice.pages.example.test", base: "/"},
		{repository: "Docs", host: "alice.pages.example.test", base: "/docs/"},
	} {
		host, base, ok := defaultSiteRoute(site{OwnerName: "Alice", RepoName: test.repository}, "pages.example.test")
		if !ok || host != test.host || base != test.base {
			t.Fatalf("defaultSiteRoute(%q)=(%q, %q, %v), want (%q, %q, true)", test.repository, host, base, ok, test.host, test.base)
		}
	}
}
