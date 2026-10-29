package page

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func newTestServer(t *testing.T, store *Store, root *url.URL) *Server {
	t.Helper()
	server, err := NewServer(store, root, "")
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestServerPublishesProjectAndHomepageRoutes(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SetRegistration("http://pages.example", 1); err != nil {
		t.Fatal(err)
	}
	site := Site{RepoID: 7, Enabled: true, OwnerName: "alice", RepoName: "docs", TarGzSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PagesUpdatedUnixNano: 1}
	if err := store.Upsert(site); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCursor(site.PagesUpdatedUnixNano); err != nil {
		t.Fatal(err)
	}
	candidate, err := store.WorkDir(7)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "index.html"), []byte("docs"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "download.unknown"), []byte("opaque"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "release..txt"), []byte("valid name"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, ".gitea-pages-probe"), []byte("7 "+site.TarGzSHA256+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := store.Install(7, site.TarGzSHA256, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetResultForSite(site, site.TarGzSHA256, true, true, ""); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("http://pages.example")
	handler := newTestServer(t, store, u).Handler()
	req := httptest.NewRequest(http.MethodGet, "https://alice.pages.example/docs/", nil)
	req.Host = "alice.pages.example"
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://alice.pages.example/docs?source=test", nil)
	req.Host = "alice.pages.example"
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusPermanentRedirect || resp.Header().Get("Location") != "http://alice.pages.example/docs/?source=test" {
		t.Fatalf("project root status=%d location=%q body=%q", resp.Code, resp.Header().Get("Location"), resp.Body.String())
	}
	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://alice.pages.example/docs/download.unknown", nil)
	req.Host = "alice.pages.example"
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || resp.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("unknown extension status=%d content-type=%q body=%q", resp.Code, resp.Header().Get("Content-Type"), resp.Body.String())
	}
	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://alice.pages.example/docs/release..txt", nil)
	req.Host = "alice.pages.example"
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || resp.Body.String() != "valid name" {
		t.Fatalf("double-dot filename status=%d body=%q", resp.Code, resp.Body.String())
	}
}

func TestServerHTTPSRedirectOnlyRecognizesKnownHosts(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SetRegistration("https://pages.example", 1); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("https://pages.example")
	server := newTestServer(t, store, u)

	request := func(host string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/check?source=test", nil)
		req.Host = host
		resp := httptest.NewRecorder()
		server.Handler().ServeHTTP(resp, req)
		return resp
	}
	if resp := request("unknown.example"); resp.Code != http.StatusNotFound || resp.Header().Get("Location") != "" {
		t.Fatalf("unknown host status=%d location=%q body=%q", resp.Code, resp.Header().Get("Location"), resp.Body.String())
	}
	if resp := request("pages.example"); resp.Code != http.StatusPermanentRedirect || resp.Header().Get("Location") != "https://pages.example/check?source=test" {
		t.Fatalf("root host status=%d location=%q body=%q", resp.Code, resp.Header().Get("Location"), resp.Body.String())
	}

	digest := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	site := Site{RepoID: 10, Enabled: true, OwnerName: "alice", RepoName: "docs", TarGzSHA256: digest, PagesUpdatedUnixNano: 1, CustomDomains: []Domain{{Host: "custom.example", State: "active"}}}
	setTestSites(t, store, "https://pages.example", 1, 1, []Site{site})
	candidate, err := store.WorkDir(site.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, ".gitea-pages-probe"), []byte("10 "+digest+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := store.Install(site.RepoID, digest, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetResultForSite(site, digest, false, true, ""); err != nil {
		t.Fatal(err)
	}
	server.UpdateSite(site.RepoID)
	if resp := request("custom.example"); resp.Code != http.StatusPermanentRedirect || resp.Header().Get("Location") != "https://custom.example/check?source=test" {
		t.Fatalf("published custom host status=%d location=%q body=%q", resp.Code, resp.Header().Get("Location"), resp.Body.String())
	}

	// A route update removes the old custom host from the O(1) host index. It
	// must stop being a redirect target immediately, just like the route itself.
	site.CustomDomains = nil
	if err := store.Upsert(site); err != nil {
		t.Fatal(err)
	}
	server.UpdateSite(site.RepoID)
	if resp := request("custom.example"); resp.Code != http.StatusNotFound || resp.Header().Get("Location") != "" {
		t.Fatalf("removed custom host status=%d location=%q body=%q", resp.Code, resp.Header().Get("Location"), resp.Body.String())
	}
}

func TestOwnerHostUsesGiteaAddressEncoding(t *testing.T) {
	for _, test := range []struct {
		owner, expected string
	}{
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

func TestServerDoesNotFollowEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SetRegistration("http://pages.example", 1); err != nil {
		t.Fatal(err)
	}
	digest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	site := Site{RepoID: 8, Enabled: true, OwnerName: "alice", RepoName: "docs", TarGzSHA256: digest, PagesUpdatedUnixNano: 1}
	if err := store.Upsert(site); err != nil {
		t.Fatal(err)
	}
	candidate, err := store.WorkDir(8)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, ".gitea-pages-probe"), []byte("8 "+digest+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(candidate, "index.html")); err != nil {
		t.Fatal(err)
	}
	if err := store.Install(8, digest, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetResultForSite(site, digest, true, true, ""); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("http://pages.example")
	req := httptest.NewRequest(http.MethodGet, "http://alice.pages.example/docs/", nil)
	req.Host = "alice.pages.example"
	resp := httptest.NewRecorder()
	newTestServer(t, store, u).Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("escaping symlink status=%d body=%q", resp.Code, resp.Body.String())
	}
}

func TestServerUsesSite404ForMissingFile(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SetRegistration("http://pages.example", 1); err != nil {
		t.Fatal(err)
	}
	digest := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	site := Site{RepoID: 9, Enabled: true, OwnerName: "alice", RepoName: "docs", TarGzSHA256: digest, PagesUpdatedUnixNano: 1}
	if err := store.Upsert(site); err != nil {
		t.Fatal(err)
	}
	candidate, err := store.WorkDir(9)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "index.html"), []byte("index"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "404.html"), []byte("custom missing"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, ".gitea-pages-probe"), []byte("9 "+digest+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := store.Install(9, digest, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetResultForSite(site, digest, true, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCursor(site.PagesUpdatedUnixNano); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("http://pages.example")
	req := httptest.NewRequest(http.MethodGet, "http://alice.pages.example/docs/missing", nil)
	req.Host = "alice.pages.example"
	resp := httptest.NewRecorder()
	newTestServer(t, store, u).Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound || resp.Body.String() != "custom missing" {
		t.Fatalf("site 404 status=%d body=%q", resp.Code, resp.Body.String())
	}
}

func TestServerUsesCustomPlatformResources(t *testing.T) {
	root := t.TempDir()
	custom := filepath.Join(root, "custom")
	if err := os.MkdirAll(filepath.Join(custom, "public", "errors"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custom, "public", "index.html"), []byte("custom home"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custom, "public", "errors", "404.html"), []byte("custom platform 404"), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SetRegistration("http://pages.example", 1); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("http://pages.example")
	s, err := NewServer(store, u, custom)
	if err != nil {
		t.Fatal(err)
	}
	for path, expected := range map[string]string{"/": "custom home", "/missing": "custom platform 404"} {
		req := httptest.NewRequest(http.MethodGet, "http://pages.example"+path, nil)
		req.Host = "pages.example"
		resp := httptest.NewRecorder()
		s.Handler().ServeHTTP(resp, req)
		if resp.Body.String() != expected {
			t.Fatalf("path %s body=%q, want %q", path, resp.Body.String(), expected)
		}
	}
}
