// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pages

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	pageconfig "gitea.dev/page/internal/config"
	"gitea.dev/page/internal/gitea"
	"gitea.dev/page/internal/storage"
)

type testDeployments []activeDeployment

func (d testDeployments) forEach(fn func(activeDeployment) error) error {
	for _, deployment := range d {
		if err := fn(deployment); err != nil {
			return err
		}
	}
	return nil
}

func newTestHTTPPublisher(t *testing.T, scheme, customRoot string, deployments ...activeDeployment) *httpPublisher {
	t.Helper()
	root, err := url.Parse(scheme + "://pages.example")
	if err != nil {
		t.Fatal(err)
	}
	mode := pageconfig.HTTPSDisabled
	if scheme == "https" {
		mode = pageconfig.HTTPSPages
	}
	publisher, err := newHTTPPublisher(pageconfig.Config{RootURL: root, CustomRoot: customRoot, HTTPSMode: mode})
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.restore(t.Context(), testDeployments(deployments)); err != nil {
		t.Fatal(err)
	}
	return publisher
}

func testDeployment(t *testing.T, repoID int64, digest, defaultAddress, customAddress string, files map[string]string) siteDeployment {
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
	deployment := siteDeployment{repoID: repoID, defaultURL: *defaultURL, content: storage.Content{Digest: digest, Root: root}}
	if customAddress != "" {
		customURL, err := url.Parse(customAddress)
		if err != nil {
			t.Fatal(err)
		}
		deployment.customURL = customURL
	}
	return deployment
}

func requestHTTPPublisher(t *testing.T, publisher *httpPublisher, target, host string, secure bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = host
	resp := httptest.NewRecorder()
	publisher.Handler(secure).ServeHTTP(resp, req)
	return resp
}

func TestHTTPPublisherServesIndexlessContentAndProbe(t *testing.T) {
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	deployment := testDeployment(t, 7, digest, "http://alice.pages.example/docs/", "", map[string]string{
		"assets/app.js": "app", "download.unknown": "opaque", "release..txt": "valid name", "guide/index.html": "guide",
	})
	publisher := newTestHTTPPublisher(t, "http", "", activeDeployment{siteDeployment: deployment, publicationResult: publicationResult{defaultPublished: true}})

	if resp := requestHTTPPublisher(t, publisher, "http://alice.pages.example/docs/", "alice.pages.example", false); resp.Code != http.StatusNotFound {
		t.Fatalf("indexless root status=%d body=%q", resp.Code, resp.Body.String())
	}
	resp := requestHTTPPublisher(t, publisher, "http://alice.pages.example/docs/assets/app.js", "alice.pages.example", false)
	if resp.Code != http.StatusOK || resp.Body.String() != "app" || resp.Header().Get("ETag") != `"`+digest+`"` {
		t.Fatalf("asset status=%d body=%q", resp.Code, resp.Body.String())
	}
	request := httptest.NewRequest(http.MethodGet, "http://alice.pages.example/docs/assets/app.js", nil)
	request.Host = "alice.pages.example"
	request.Header.Set("If-None-Match", resp.Header().Get("ETag"))
	resp = httptest.NewRecorder()
	publisher.Handler(false).ServeHTTP(resp, request)
	if resp.Code != http.StatusNotModified || resp.Body.Len() != 0 {
		t.Fatalf("conditional asset status=%d body=%q", resp.Code, resp.Body.String())
	}
	resp = requestHTTPPublisher(t, publisher, "http://alice.pages.example/docs/guide?lang=en", "alice.pages.example", false)
	location, err := url.Parse(resp.Header().Get("Location"))
	if err != nil || resp.Code != http.StatusPermanentRedirect || location.Path != "/docs/guide/" || location.RawQuery != "lang=en" {
		t.Fatalf("directory redirect status=%d location=%q err=%v", resp.Code, resp.Header().Get("Location"), err)
	}
	if resp := requestHTTPPublisher(t, publisher, "http://alice.pages.example/docs/guide/", "alice.pages.example", false); resp.Code != http.StatusOK || resp.Body.String() != "guide" {
		t.Fatalf("directory index status=%d body=%q", resp.Code, resp.Body.String())
	}
	if resp := requestHTTPPublisher(t, publisher, "http://alice.pages.example/docs/.gitea-pages-probe", "alice.pages.example", false); resp.Code != http.StatusOK || resp.Body.String() != "7 "+digest+"\n" {
		t.Fatalf("probe status=%d body=%q", resp.Code, resp.Body.String())
	}
	if resp := requestHTTPPublisher(t, publisher, "http://alice.pages.example/docs/download.unknown", "alice.pages.example", false); resp.Code != http.StatusOK || resp.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("unknown file status=%d type=%q", resp.Code, resp.Header().Get("Content-Type"))
	}
	if resp := requestHTTPPublisher(t, publisher, "http://alice.pages.example/docs/release..txt", "alice.pages.example", false); resp.Code != http.StatusOK || resp.Body.String() != "valid name" {
		t.Fatalf("double-dot file status=%d body=%q", resp.Code, resp.Body.String())
	}
}

func TestHTTPPublisherReplacesRepositoryRoutes(t *testing.T) {
	digest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	old := testDeployment(t, 8, digest, "https://alice.pages.example/docs/", "https://custom.example/", map[string]string{"index.html": "old"})
	publisher := newTestHTTPPublisher(t, "https", "", activeDeployment{siteDeployment: old, publicationResult: publicationResult{defaultPublished: true, customPublished: true}})
	if resp := requestHTTPPublisher(t, publisher, "http://custom.example/check", "custom.example", false); resp.Code != http.StatusPermanentRedirect {
		t.Fatalf("custom redirect status=%d", resp.Code)
	}

	updated := old
	updatedURL, _ := url.Parse("https://alice.pages.example/renamed/")
	updated.defaultURL = *updatedURL
	updated.customURL = nil
	publisher.routeIndex.mu.Lock()
	publisher.setDeploymentLocked(updated, publicationResult{defaultPublished: true})
	publisher.routeIndex.mu.Unlock()
	if resp := requestHTTPPublisher(t, publisher, "http://custom.example/check", "custom.example", false); resp.Code != http.StatusNotFound {
		t.Fatalf("removed custom host status=%d", resp.Code)
	}
	if resp := requestHTTPPublisher(t, publisher, "https://alice.pages.example/renamed/", "alice.pages.example", true); resp.Code != http.StatusOK || resp.Body.String() != "old" {
		t.Fatalf("renamed route status=%d body=%q", resp.Code, resp.Body.String())
	}
}

func TestHTTPPublisherProxyServesPlainHTTP(t *testing.T) {
	deployment := testDeployment(t, 11, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "https://alice.pages.example/docs/", "", map[string]string{"index.html": "proxied"})
	root, err := url.Parse("https://pages.example")
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := newHTTPPublisher(pageconfig.Config{RootURL: root, HTTPSMode: pageconfig.HTTPSProxy})
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.restore(t.Context(), testDeployments{{siteDeployment: deployment, publicationResult: publicationResult{defaultPublished: true}}}); err != nil {
		t.Fatal(err)
	}
	resp := requestHTTPPublisher(t, publisher, "http://alice.pages.example/docs/", "alice.pages.example", false)
	if resp.Code != http.StatusOK || resp.Body.String() != "proxied" {
		t.Fatalf("proxy origin response: status=%d body=%q", resp.Code, resp.Body.String())
	}
	publisher.SetChallenge("alice.pages.example", "token", "challenge")
	resp = requestHTTPPublisher(t, publisher, "http://alice.pages.example/.well-known/acme-challenge/token", "alice.pages.example", false)
	if resp.Code != http.StatusNotFound || resp.Body.String() == "challenge" {
		t.Fatalf("proxy origin exposed Pages HTTP-01 state: status=%d body=%q", resp.Code, resp.Body.String())
	}
}

func TestHTTPPublisherProxyConfirmsDeploymentOverHTTP(t *testing.T) {
	root, err := url.Parse("https://pages.example")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := newHTTPPublisher(pageconfig.Config{RootURL: root, HTTPSMode: pageconfig.HTTPSProxy, HTTPListen: listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: publisher.Handler(false)}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
		if err := <-done; err != nil && err != http.ErrServerClosed {
			t.Errorf("serve HTTP origin: %v", err)
		}
	})

	deployment := testDeployment(t, 12, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", "https://alice.pages.example/docs/", "", map[string]string{"index.html": "proxied"})
	publication, err := publisher.publish(t.Context(), deployment)
	if err != nil {
		t.Fatal(err)
	}
	if !publication.defaultPublished || publication.problem != publicationProblemNone {
		t.Fatalf("unexpected proxy publication: %#v", publication)
	}
}

func TestHTTPPublisherDoesNotFollowEscapingSymlink(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	deployment := testDeployment(t, 9, "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "http://alice.pages.example/docs/", "", nil)
	if err := os.Symlink(outside, filepath.Join(deployment.content.Root, "index.html")); err != nil {
		t.Fatal(err)
	}
	publisher := newTestHTTPPublisher(t, "http", "", activeDeployment{siteDeployment: deployment, publicationResult: publicationResult{defaultPublished: true}})
	if resp := requestHTTPPublisher(t, publisher, "http://alice.pages.example/docs/", "alice.pages.example", false); resp.Code != http.StatusNotFound {
		t.Fatalf("escaping symlink status=%d body=%q", resp.Code, resp.Body.String())
	}
}

func TestHTTPPublisherUsesSiteAndPlatformResources(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom")
	if err := os.MkdirAll(filepath.Join(custom, "errors"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custom, "index.html"), []byte("custom home"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custom, "errors", "404.html"), []byte("platform missing"), 0o644); err != nil {
		t.Fatal(err)
	}
	deployment := testDeployment(t, 10, "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", "http://alice.pages.example/docs/", "", map[string]string{"404.html": "site missing"})
	publisher := newTestHTTPPublisher(t, "http", custom, activeDeployment{siteDeployment: deployment, publicationResult: publicationResult{defaultPublished: true}})
	if resp := requestHTTPPublisher(t, publisher, "http://alice.pages.example/docs/missing", "alice.pages.example", false); resp.Code != http.StatusNotFound || resp.Body.String() != "site missing" {
		t.Fatalf("site 404 status=%d body=%q", resp.Code, resp.Body.String())
	}
	if resp := requestHTTPPublisher(t, publisher, "http://pages.example/", "pages.example", false); resp.Code != http.StatusOK || resp.Body.String() != "custom home" {
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
		host, base, ok := defaultSiteRoute(gitea.Site{OwnerName: "Alice", RepoName: test.repository}, "pages.example.test")
		if !ok || host != test.host || base != test.base {
			t.Fatalf("defaultSiteRoute(%q)=(%q, %q, %v), want (%q, %q, true)", test.repository, host, base, ok, test.host, test.base)
		}
	}
}
