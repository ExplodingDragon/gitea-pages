// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

type testPublisher struct {
	publishErr error
}

func (*testPublisher) Restore(context.Context, Deployments) error { return nil }

func (p *testPublisher) Publish(_ context.Context, deployment Deployment) (Publication, error) {
	if p.publishErr != nil {
		return Publication{}, p.publishErr
	}
	return Publication{DefaultPublished: true, CustomPublished: deployment.CustomURL != nil}, nil
}

func (*testPublisher) Unpublish(context.Context, int64) error { return nil }

func newTestGiteaClient(cfg Config) *giteaClient {
	return &giteaClient{
		base: cfg.GiteaURL, token: cfg.Token,
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

func TestFullSyncCatchesUpdateDuringPagination(t *testing.T) {
	root, err := url.Parse("http://pages.example.test")
	if err != nil {
		t.Fatal(err)
	}
	all := make([]site, 101)
	for i := range all {
		all[i] = site{RepoID: int64(i + 1), OwnerName: "alice", RepoName: "site-" + strconv.Itoa(i+1), Enabled: true, PagesUpdatedUnixNano: int64(i + 1)}
	}
	var fullPages, catchups int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var response sitesResponse
		response.RootURL = root.String()
		response.RootUpdatedUnixNano = 1
		response.ArchiveLimits = testArchiveLimits()
		response.RequireGiteaTXTRecord = true
		switch r.URL.Path {
		case "/api/internal/pages/sites":
			fullPages++
			response.Until = 101
			if r.URL.Query().Get("after_time") == "" {
				response.Sites = all[:100]
				response.NextCursor = &cursor{PagesUpdatedUnixNano: 100, RepoID: 100}
			} else {
				response.Sites = all[100:]
			}
		case "/api/internal/pages/sites/updated":
			catchups++
			if r.URL.Query().Get("since") != "101" {
				t.Errorf("catch-up since = %q", r.URL.Query().Get("since"))
			}
			response.Until = 110
			changed := all[0]
			changed.RepoName = "renamed"
			changed.PagesUpdatedUnixNano = 110
			response.Sites = []site{changed}
		default:
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	cfg := Config{RootURL: root, GiteaURL: base, Token: "test", SyncInterval: time.Minute, DeployQueueSize: 1, DeployConcurrency: 1}
	syncer := newSyncer(cfg, newTestGiteaClient(cfg), store, &testPublisher{}, slog.Default())
	if err := syncer.syncAllSites(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fullPages != 2 || catchups != 1 {
		t.Fatalf("full pages = %d, catch-ups = %d", fullPages, catchups)
	}
	if cursor, initialized, _, _ := store.syncState(); !initialized || cursor != 110 {
		t.Fatalf("sync state: cursor=%d initialized=%v", cursor, initialized)
	}
	if first, ok := store.getSite(1); !ok || first.Site.RepoName != "renamed" || first.Site.PagesUpdatedUnixNano != 110 {
		t.Fatalf("site updated while paging = %#v, found=%v", first, ok)
	}
	count := 0
	store.forEachSite(func(localSite) bool { count++; return true })
	if count != 101 {
		t.Fatalf("site count = %d", count)
	}
}

func TestFullSyncKeepsPreviousSnapshotWhenPaginationFails(t *testing.T) {
	root, err := url.Parse("http://pages.example.test")
	if err != nil {
		t.Fatal(err)
	}
	old := site{RepoID: 1, OwnerName: "alice", RepoName: "old", Enabled: true, PagesUpdatedUnixNano: 7}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/pages/sites" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("after_time") != "" {
			http.Error(w, "listing failed", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sitesResponse{
			RootURL: root.String(), RootUpdatedUnixNano: 2, Until: 10,
			ArchiveLimits: testArchiveLimits(), RequireGiteaTXTRecord: true,
			NextCursor: &cursor{PagesUpdatedUnixNano: 10, RepoID: 2},
			Sites:      []site{{RepoID: 2, OwnerName: "alice", RepoName: "new", Enabled: true, PagesUpdatedUnixNano: 10}},
		})
	}))
	defer server.Close()
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	setTestSites(t, store, root.String(), 7, []site{old})
	cfg := Config{RootURL: root, GiteaURL: base, Token: "test", SyncInterval: time.Minute}
	syncer := newSyncer(cfg, newTestGiteaClient(cfg), store, &testPublisher{}, slog.Default())
	if err := syncer.syncAllSites(context.Background()); err == nil {
		t.Fatal("full sync unexpectedly succeeded")
	}
	if cursor, initialized, gotRoot, rootTime := store.syncState(); !initialized || cursor != 7 || gotRoot != root.String() || rootTime != 1 {
		t.Fatalf("previous sync state changed: cursor=%d initialized=%v root=%q root_time=%d", cursor, initialized, gotRoot, rootTime)
	}
	if got, ok := store.getSite(old.RepoID); !ok || got.Site.RepoName != old.RepoName {
		t.Fatalf("previous site snapshot changed: %#v, found=%v", got, ok)
	}
	if _, ok := store.getSite(2); ok {
		t.Fatal("partial full listing became visible")
	}
}

func TestIncrementalSyncKeepsCursorWhenConfigurationChangesBetweenPages(t *testing.T) {
	root, err := url.Parse("http://pages.example.test")
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		response := sitesResponse{
			RootURL: root.String(), RootUpdatedUnixNano: 1, ArchiveLimits: testArchiveLimits(),
			RequireGiteaTXTRecord: true, Until: 9,
		}
		if requests == 1 {
			response.Sites = []site{{RepoID: 2, OwnerName: "alice", RepoName: "new", PagesUpdatedUnixNano: 8}}
			response.NextCursor = &cursor{PagesUpdatedUnixNano: 8, RepoID: 2}
		} else {
			response.ArchiveLimits.MaxFileBytes--
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	setTestSites(t, store, root.String(), 7, []site{{RepoID: 1, OwnerName: "alice", RepoName: "old", PagesUpdatedUnixNano: 7}})
	cfg := Config{RootURL: root, GiteaURL: base, Token: "test", SyncInterval: time.Minute}
	syncer := newSyncer(cfg, newTestGiteaClient(cfg), store, &testPublisher{}, slog.Default())
	var apiErr *apiError
	if err := syncer.syncOnce(t.Context()); !errors.As(err, &apiErr) || apiErr.Code != "state_changed" {
		t.Fatalf("sync error = %v", err)
	}
	if cursor, _, _, _ := store.syncState(); cursor != 7 {
		t.Fatalf("cursor advanced to %d", cursor)
	}
}

func TestIncrementalRootChangeOnLaterPageStartsFullSync(t *testing.T) {
	oldRoot, err := url.Parse("http://pages.example.test")
	if err != nil {
		t.Fatal(err)
	}
	newRoot, err := url.Parse("https://pages.example.test")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/internal/pages/sites/updated":
			switch {
			case r.URL.Query().Get("since") == "9":
				_ = json.NewEncoder(w).Encode(sitesResponse{RootURL: newRoot.String(), RootUpdatedUnixNano: 2, ArchiveLimits: testArchiveLimits(), RequireGiteaTXTRecord: true, Until: 9})
			case r.URL.Query().Get("after_time") == "":
				_ = json.NewEncoder(w).Encode(sitesResponse{
					RootURL: oldRoot.String(), RootUpdatedUnixNano: 1, Until: 9,
					ArchiveLimits: testArchiveLimits(), RequireGiteaTXTRecord: true,
					Sites:      []site{{RepoID: 2, OwnerName: "alice", RepoName: "intermediate", Enabled: true, PagesUpdatedUnixNano: 8}},
					NextCursor: &cursor{PagesUpdatedUnixNano: 8, RepoID: 2},
				})
			default:
				_ = json.NewEncoder(w).Encode(sitesResponse{RootURL: newRoot.String(), RootUpdatedUnixNano: 2, ArchiveLimits: testArchiveLimits(), RequireGiteaTXTRecord: true, Until: 9})
			}
		case "/api/internal/pages/sites":
			_ = json.NewEncoder(w).Encode(sitesResponse{
				RootURL: newRoot.String(), RootUpdatedUnixNano: 2, Until: 9,
				ArchiveLimits: testArchiveLimits(), RequireGiteaTXTRecord: true,
				Sites: []site{{RepoID: 3, OwnerName: "alice", RepoName: "new", Enabled: true, PagesUpdatedUnixNano: 9}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	setTestSites(t, store, oldRoot.String(), 7, []site{{RepoID: 1, OwnerName: "alice", RepoName: "old", Enabled: true, PagesUpdatedUnixNano: 7}})
	cfg := Config{RootURL: oldRoot, GiteaURL: base, Token: "test", SyncInterval: time.Minute}
	syncer := newSyncer(cfg, newTestGiteaClient(cfg), store, &testPublisher{}, slog.Default())
	if err := syncer.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.getSite(1); ok {
		t.Fatal("old registration snapshot was retained after successful full sync")
	}
	if _, ok := store.getSite(2); ok {
		t.Fatal("site from mixed incremental registration was retained")
	}
	if site, ok := store.getSite(3); !ok || site.Site.RepoName != "new" {
		t.Fatalf("new full snapshot missing: %#v, found=%v", site, ok)
	}
	if _, initialized, gotRoot, rootTime := store.syncState(); !initialized || gotRoot != newRoot.String() || rootTime != 2 {
		t.Fatalf("new registration state missing: initialized=%v root=%q root_time=%d", initialized, gotRoot, rootTime)
	}
}

func TestPublishFailureRetainsConfirmedContent(t *testing.T) {
	root, err := url.Parse("http://pages.example.test")
	if err != nil {
		t.Fatal(err)
	}
	var report deploymentResult
	reportServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/internal/pages/results" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Errorf("decode report: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer reportServer.Close()
	giteaURL, err := url.Parse(reportServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	oldDigest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	newDigest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	targetSite := site{RepoID: 1, Enabled: true, OwnerName: "alice", RepoName: "docs", PagesUpdatedUnixNano: 1, TarGzSHA256: newDigest}
	setTestSites(t, store, root.String(), 1, []site{targetSite})
	oldCandidate, err := store.workDir(targetSite.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldCandidate, "old.html"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.prepareContent(targetSite.RepoID, oldDigest, oldCandidate); err != nil {
		t.Fatal(err)
	}
	if applied, err := store.saveDeploymentResult(targetSite, oldDigest, true, false); err != nil || !applied {
		t.Fatalf("save old publication: applied=%v err=%v", applied, err)
	}
	newCandidate, err := store.workDir(targetSite.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newCandidate, "new.html"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	content, err := store.prepareContent(targetSite.RepoID, newDigest, newCandidate)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{GiteaURL: giteaURL, RootURL: root, Token: "test", DeployQueueSize: 1, DeployConcurrency: 1}
	syncer := newSyncer(cfg, newTestGiteaClient(cfg), store, &testPublisher{publishErr: errors.New("publish failed")}, slog.Default())
	if err := syncer.publishSite(context.Background(), targetSite, content); err == nil {
		t.Fatal("publish unexpectedly succeeded")
	}
	local, ok := store.getSite(targetSite.RepoID)
	if !ok {
		t.Fatal("site state was removed")
	}
	if !local.DefaultPublished || local.CustomPublished || local.InstalledDigest != oldDigest {
		t.Fatalf("confirmed publication was not retained: %#v", local)
	}
	if _, ok := store.content(targetSite.RepoID, oldDigest); !ok {
		t.Fatal("confirmed content was removed")
	}
	if _, ok := store.content(targetSite.RepoID, newDigest); ok {
		t.Fatal("failed candidate content remains")
	}
	if report.ConfigApplied || report.DefaultPublished || report.CustomPublished || report.ActualTarGzSHA256 != oldDigest || report.ErrorCode != instanceErrorRouteApplyFailed {
		t.Fatalf("unexpected failure report: %#v", report)
	}
}

func TestCustomDomainTXTGrace(t *testing.T) {
	store, err := openStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	syncer := newSyncer(Config{DNSRefreshInterval: time.Minute}, nil, store, &testPublisher{}, slog.Default())
	syncer.requireGiteaTXT.Store(true)
	syncer.txtLookup = func(context.Context, string, string) (txtVerificationResult, error) {
		return txtInvalid, errors.New("TXT value does not match")
	}
	now := time.Now()
	domain := domain{
		ID: 1, Host: "docs.example.test", TXTToken: "token", TXTResult: domaintxtInvalid,
		TXTLastSuccessUnixNano: now.Add(-time.Hour).UnixNano(),
	}
	key := "1/docs.example.test/token"
	if err := store.setTXTState(key, txtState{
		LastSuccessUnixNano: now.Add(-time.Hour).UnixNano(), LastCheckUnixNano: now.Add(-2 * time.Minute).UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	ready, err := syncer.customDomainAuthorized(t.Context(), site{CustomDomain: &domain})
	if err != nil || !ready {
		t.Fatalf("recently valid domain was revoked: ready=%v err=%v", ready, err)
	}
	local, ok := store.getTXTState(key)
	if !ok || local.LastSuccessUnixNano == 0 {
		t.Fatalf("local TXT grace was not retained: %#v", local)
	}

	domain.TXTLastSuccessUnixNano = now.Add(-25 * time.Hour).UnixNano()
	ready, err = syncer.customDomainAuthorized(t.Context(), site{CustomDomain: &domain})
	if err == nil || ready {
		t.Fatalf("expired Gitea TXT authorization remained active: ready=%v err=%v", ready, err)
	}

	domain.TXTResult = domaintxtValid
	domain.TXTLastSuccessUnixNano = now.UnixNano()
	if err := store.setTXTState(key, txtState{
		LastSuccessUnixNano: now.Add(time.Hour).UnixNano(), LastCheckUnixNano: now.Add(time.Hour).UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	ready, err = syncer.customDomainAuthorized(t.Context(), site{CustomDomain: &domain})
	if err == nil || ready {
		t.Fatalf("future local TXT timestamps were trusted: ready=%v err=%v", ready, err)
	}
}

func TestPendingScanContinuouslyFillsBoundedQueue(t *testing.T) {
	store, err := openStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	setTestSites(t, store, "http://pages.example.test", 3, []site{
		{RepoID: 1, PagesUpdatedUnixNano: 1},
		{RepoID: 2, PagesUpdatedUnixNano: 2},
		{RepoID: 3, PagesUpdatedUnixNano: 3},
	})
	syncer := newSyncer(Config{DeployQueueSize: 1, DeployConcurrency: 1}, nil, store, &testPublisher{}, slog.Default())
	ctx, cancel := context.WithCancel(t.Context())
	syncer.workers.Add(1)
	go syncer.scanPendingSites(ctx)
	syncer.requestPendingScan()

	received := make(map[int64]struct{}, 3)
	for len(received) < 3 {
		select {
		case repoID := <-syncer.queue:
			received[repoID] = struct{}{}
		case <-time.After(time.Second):
			t.Fatalf("bounded queue stopped after repository IDs %v", received)
		}
	}
	cancel()
	syncer.workers.Wait()
}

func TestEnqueueSiteKeepsUpdateDuringDeployment(t *testing.T) {
	store, err := openStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	syncer := newSyncer(Config{DeployQueueSize: 1, DeployConcurrency: 1}, nil, store, &testPublisher{}, slog.Default())

	syncer.enqueueSite(1)
	if repoID := <-syncer.queue; repoID != 1 {
		t.Fatalf("queued repository ID = %d, want 1", repoID)
	}
	syncer.enqueueSite(1)
	syncer.finishDeployment(1)

	select {
	case repoID := <-syncer.queue:
		if repoID != 1 {
			t.Fatalf("queued repository ID = %d, want 1", repoID)
		}
	case <-time.After(time.Second):
		t.Fatal("site update received during deployment was not queued again")
	}
}
