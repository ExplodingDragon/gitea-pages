// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// TestSyncDeployAndServe exercises the complete standalone path through the
// public module boundary: a site is listed by Gitea, its tar.gz is downloaded,
// checked and installed, deployment is reported, and the installed site is
// then served by the real HTTP handler. A second listing/update confirms that
// a later Gitea timestamp replaces the published content.
func TestSyncDeployAndServe(t *testing.T) {
	firstArchive := archiveBytes(t, "first page")
	secondArchive := archiveBytes(t, "updated page")
	firstDigest := archiveDigest(firstArchive)
	secondDigest := archiveDigest(secondArchive)

	rootURL, err := url.Parse("http://pages.example.test")
	if err != nil {
		t.Fatal(err)
	}
	firstSite := site{RepoID: 42, Enabled: true, OwnerName: "alice", RepoName: "docs", PagesUpdatedUnixNano: 10, TarGzSHA256: firstDigest}
	secondSite := firstSite
	secondSite.PagesUpdatedUnixNano = 20
	secondSite.TarGzSHA256 = secondDigest

	var mu sync.Mutex
	updated := false
	var reports []deploymentResult
	reportCh := make(chan deploymentResult, 4)
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer page-token" {
			http.Error(w, "missing page token", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		currentUpdated := updated
		mu.Unlock()
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/internal/pages/instance":
			var req registerRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode registration: %v", err)
			}
			_ = json.NewEncoder(w).Encode(registerResponse{RootURL: req.RootURL, RootUpdatedUnixNano: 1})
		case r.Method == http.MethodGet && (r.URL.Path == "/api/internal/pages/sites" || r.URL.Path == "/api/internal/pages/sites/updated"):
			full := r.URL.Path == "/api/internal/pages/sites" && r.URL.Query().Get("since") == ""
			w.Header().Set("Content-Type", "application/json")
			response := sitesResponse{RootURL: rootURL.String(), RootUpdatedUnixNano: 1, ArchiveLimits: testArchiveLimits(), RequireGiteaTXTRecord: true, Until: 10}
			if full {
				response.Sites = []site{firstSite}
			} else if currentUpdated {
				response.Sites = []site{secondSite}
				response.Until = 20
			}
			if err := json.NewEncoder(w).Encode(response); err != nil {
				t.Errorf("encode site response: %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/internal/pages/sites/42/content/"+firstDigest:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(firstArchive)
		case r.Method == http.MethodGet && r.URL.Path == "/api/internal/pages/sites/42/content/"+secondDigest:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(secondArchive)
		case r.Method == http.MethodPost && r.URL.Path == "/api/internal/pages/results":
			var result deploymentResult
			if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
				t.Fatalf("decode deployment result: %v", err)
			}
			mu.Lock()
			reports = append(reports, result)
			mu.Unlock()
			reportCh <- result
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer gitea.Close()

	dataRoot := t.TempDir()
	store, err := openStore(dataRoot, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	giteaURL, err := url.Parse(gitea.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Mode:              ModeStandalone,
		GiteaURL:          giteaURL,
		RootURL:           rootURL,
		Token:             "page-token",
		DataRoot:          dataRoot,
		EtcdQuotaBytes:    defaultEtcdQuotaBytes,
		HTTPListen:        listener.Addr().String(),
		DeployQueueSize:   1,
		DeployConcurrency: 1,
		DeployTimeout:     time.Minute,
		SyncInterval:      time.Minute,
	}
	publisher, err := NewStandalonePublisher(cfg)
	if err != nil {
		t.Fatal(err)
	}
	webServer := &http.Server{Handler: publisher.Handler(false)}
	serveDone := make(chan error, 1)
	go func() { serveDone <- webServer.Serve(listener) }()
	defer func() {
		_ = webServer.Shutdown(context.Background())
		if err := <-serveDone; err != nil && err != http.ErrServerClosed {
			t.Errorf("website server: %v", err)
		}
	}()

	syncer := newSyncer(cfg, newTestGiteaClient(cfg), store, publisher, slog.Default())
	if err := syncer.syncAllSites(context.Background()); err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	syncer.startWorkers(workerCtx)
	defer func() {
		cancel()
		syncer.workers.Wait()
	}()
	syncer.requestPendingScan()
	waitForReport(t, reportCh, firstDigest)
	assertSiteBody(t, listener.Addr().String(), "alice.pages.example.test", "first page")

	mu.Lock()
	updated = true
	mu.Unlock()
	if err := syncer.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	syncer.requestPendingScan()
	waitForReport(t, reportCh, secondDigest)
	assertSiteBody(t, listener.Addr().String(), "alice.pages.example.test", "updated page")

	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 2 {
		t.Fatalf("deployment reports = %d, want 2", len(reports))
	}
	for _, result := range reports {
		if !result.ConfigApplied || !result.DefaultPublished || result.CustomPublished || result.ErrorCode != "" {
			t.Fatalf("unexpected deployment result: %#v", result)
		}
	}
}

func archiveBytes(t *testing.T, body string) []byte {
	t.Helper()
	f := makeArchive(t, testTarEntry{name: "assets/page.txt", kind: tar.TypeReg, body: body, mode: 0o755})
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func archiveDigest(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func waitForReport(t *testing.T, reports <-chan deploymentResult, digest string) {
	t.Helper()
	select {
	case report := <-reports:
		if report.ActualTarGzSHA256 != digest {
			t.Fatalf("deployment report digest = %q, want %q", report.ActualTarGzSHA256, digest)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for deployment report %s", digest)
	}
}

func assertSiteBody(t *testing.T, address, host, want string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	defer client.Transport.(*http.Transport).CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, "http://"+address+"/docs/assets/page.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, []byte(want)) {
		t.Fatalf("site response status=%d body=%q, want 200/%q", resp.StatusCode, body, want)
	}
}
