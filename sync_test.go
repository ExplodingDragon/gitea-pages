// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package page

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

type failingApplier struct{}

func (failingApplier) Apply(context.Context) error {
	return errors.New("apply failed")
}

func TestFullSyncCatchesUpdateDuringPagination(t *testing.T) {
	root, err := url.Parse("http://pages.example.test")
	if err != nil {
		t.Fatal(err)
	}
	all := make([]Site, 101)
	for i := range all {
		all[i] = Site{RepoID: int64(i + 1), OwnerName: "alice", RepoName: "site-" + strconv.Itoa(i+1), Enabled: true, PagesUpdatedUnixNano: int64(i + 1)}
	}
	var fullPages, catchups int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var response SitesResponse
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
				response.NextCursor = &Cursor{PagesUpdatedUnixNano: 100, RepoID: 100}
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
			response.Sites = []Site{changed}
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
	store, err := OpenStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := Config{RootURL: root, GiteaURL: base, Token: "test", SyncInterval: time.Minute, DeployQueueSize: 1, DeployConcurrency: 1}
	syncer := NewSyncer(cfg, NewClient(cfg), store, nil, nil)
	if err := syncer.syncAllSites(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fullPages != 2 || catchups != 1 {
		t.Fatalf("full pages = %d, catch-ups = %d", fullPages, catchups)
	}
	if cursor, initialized, _, _ := store.SyncState(); !initialized || cursor != 110 {
		t.Fatalf("sync state: cursor=%d initialized=%v", cursor, initialized)
	}
	if first, ok := store.GetSite(1); !ok || first.Site.RepoName != "renamed" || first.Site.PagesUpdatedUnixNano != 110 {
		t.Fatalf("site updated while paging = %#v, found=%v", first, ok)
	}
	count := 0
	store.ForEachSite(func(LocalSite) bool { count++; return true })
	if count != 101 {
		t.Fatalf("site count = %d", count)
	}
}

func TestFullSyncKeepsPreviousSnapshotWhenPaginationFails(t *testing.T) {
	root, err := url.Parse("http://pages.example.test")
	if err != nil {
		t.Fatal(err)
	}
	old := Site{RepoID: 1, OwnerName: "alice", RepoName: "old", Enabled: true, PagesUpdatedUnixNano: 7}
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
		_ = json.NewEncoder(w).Encode(SitesResponse{
			RootURL: root.String(), RootUpdatedUnixNano: 2, Until: 10,
			ArchiveLimits: testArchiveLimits(), RequireGiteaTXTRecord: true,
			NextCursor: &Cursor{PagesUpdatedUnixNano: 10, RepoID: 2},
			Sites:      []Site{{RepoID: 2, OwnerName: "alice", RepoName: "new", Enabled: true, PagesUpdatedUnixNano: 10}},
		})
	}))
	defer server.Close()
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	setTestSites(t, store, root.String(), 1, 7, []Site{old})
	cfg := Config{RootURL: root, GiteaURL: base, Token: "test", SyncInterval: time.Minute}
	syncer := NewSyncer(cfg, NewClient(cfg), store, nil, nil)
	if err := syncer.syncAllSites(context.Background()); err == nil {
		t.Fatal("full sync unexpectedly succeeded")
	}
	if cursor, initialized, gotRoot, rootTime := store.SyncState(); !initialized || cursor != 7 || gotRoot != root.String() || rootTime != 1 {
		t.Fatalf("previous sync state changed: cursor=%d initialized=%v root=%q root_time=%d", cursor, initialized, gotRoot, rootTime)
	}
	if got, ok := store.GetSite(old.RepoID); !ok || got.Site.RepoName != old.RepoName {
		t.Fatalf("previous site snapshot changed: %#v, found=%v", got, ok)
	}
	if _, ok := store.GetSite(2); ok {
		t.Fatal("partial full listing became visible")
	}
}

func TestIncrementalSyncKeepsCursorWhenConfigurationChangesBetweenPages(t *testing.T) {
	root, err := url.Parse("http://pages.example.test")
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		response := SitesResponse{
			RootURL: root.String(), RootUpdatedUnixNano: 1, ArchiveLimits: testArchiveLimits(),
			RequireGiteaTXTRecord: true, Until: 9,
		}
		if requests == 1 {
			response.Sites = []Site{{RepoID: 2, OwnerName: "alice", RepoName: "new", PagesUpdatedUnixNano: 8}}
			response.NextCursor = &Cursor{PagesUpdatedUnixNano: 8, RepoID: 2}
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
	store, err := OpenStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	setTestSites(t, store, root.String(), 1, 7, []Site{{RepoID: 1, OwnerName: "alice", RepoName: "old", PagesUpdatedUnixNano: 7}})
	cfg := Config{RootURL: root, GiteaURL: base, Token: "test", SyncInterval: time.Minute}
	syncer := NewSyncer(cfg, NewClient(cfg), store, nil, nil)
	var apiErr *APIError
	if err := syncer.syncOnce(t.Context()); !errors.As(err, &apiErr) || apiErr.Code != "state_changed" {
		t.Fatalf("sync error = %v", err)
	}
	if cursor, _, _, _ := store.SyncState(); cursor != 7 {
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
			if r.URL.Query().Get("since") == "9" {
				_ = json.NewEncoder(w).Encode(SitesResponse{RootURL: newRoot.String(), RootUpdatedUnixNano: 2, ArchiveLimits: testArchiveLimits(), RequireGiteaTXTRecord: true, Until: 9})
			} else if r.URL.Query().Get("after_time") == "" {
				_ = json.NewEncoder(w).Encode(SitesResponse{
					RootURL: oldRoot.String(), RootUpdatedUnixNano: 1, Until: 9,
					ArchiveLimits: testArchiveLimits(), RequireGiteaTXTRecord: true,
					Sites:      []Site{{RepoID: 2, OwnerName: "alice", RepoName: "intermediate", Enabled: true, PagesUpdatedUnixNano: 8}},
					NextCursor: &Cursor{PagesUpdatedUnixNano: 8, RepoID: 2},
				})
			} else {
				_ = json.NewEncoder(w).Encode(SitesResponse{RootURL: newRoot.String(), RootUpdatedUnixNano: 2, ArchiveLimits: testArchiveLimits(), RequireGiteaTXTRecord: true, Until: 9})
			}
		case "/api/internal/pages/sites":
			_ = json.NewEncoder(w).Encode(SitesResponse{
				RootURL: newRoot.String(), RootUpdatedUnixNano: 2, Until: 9,
				ArchiveLimits: testArchiveLimits(), RequireGiteaTXTRecord: true,
				Sites: []Site{{RepoID: 3, OwnerName: "alice", RepoName: "new", Enabled: true, PagesUpdatedUnixNano: 9}},
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
	store, err := OpenStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	setTestSites(t, store, oldRoot.String(), 1, 7, []Site{{RepoID: 1, OwnerName: "alice", RepoName: "old", Enabled: true, PagesUpdatedUnixNano: 7}})
	cfg := Config{RootURL: oldRoot, GiteaURL: base, Token: "test", SyncInterval: time.Minute}
	syncer := NewSyncer(cfg, NewClient(cfg), store, nil, nil)
	if err := syncer.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.GetSite(1); ok {
		t.Fatal("old registration snapshot was retained after successful full sync")
	}
	if _, ok := store.GetSite(2); ok {
		t.Fatal("site from mixed incremental registration was retained")
	}
	if site, ok := store.GetSite(3); !ok || site.Site.RepoName != "new" {
		t.Fatalf("new full snapshot missing: %#v, found=%v", site, ok)
	}
	if _, initialized, gotRoot, rootTime := store.SyncState(); !initialized || gotRoot != newRoot.String() || rootTime != 2 {
		t.Fatalf("new registration state missing: initialized=%v root=%q root_time=%d", initialized, gotRoot, rootTime)
	}
}

func TestPublishApplyFailureClosesCandidatePublication(t *testing.T) {
	root, err := url.Parse("http://pages.example.test")
	if err != nil {
		t.Fatal(err)
	}
	var report DeploymentResult
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
	store, err := OpenStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	site := Site{RepoID: 1, Enabled: true, OwnerName: "alice", RepoName: "docs", PagesUpdatedUnixNano: 1, TarGzSHA256: digest}
	setTestSites(t, store, root.String(), 1, 1, []Site{site})
	cfg := Config{GiteaURL: giteaURL, RootURL: root, Token: "test", Mode: ModeStandalone, DeployQueueSize: 1, DeployConcurrency: 1}
	syncer := NewSyncer(cfg, NewClient(cfg), store, nil, failingApplier{})
	if err := syncer.publishSite(context.Background(), site); err == nil {
		t.Fatal("publish unexpectedly succeeded")
	}
	local, ok := store.GetSite(site.RepoID)
	if !ok {
		t.Fatal("site state was removed")
	}
	if local.DefaultPublished || local.CustomPublished {
		t.Fatalf("candidate publication remained open: %#v", local)
	}
	if report.ConfigApplied || report.DefaultPublished || report.CustomPublished || report.ActualTarGzSHA256 != digest || report.ErrorCode != InstanceErrorRouteApplyFailed {
		t.Fatalf("unexpected failure report: %#v", report)
	}
}

func TestCustomDomainTXTGrace(t *testing.T) {
	store, err := OpenStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	syncer := NewSyncer(Config{DNSRefreshInterval: time.Minute}, nil, store, nil, nil)
	syncer.requireGiteaTXT.Store(true)
	syncer.txtLookup = func(context.Context, string, string) (TXTVerificationResult, error) {
		return TXTInvalid, errors.New("TXT value does not match")
	}
	now := time.Now()
	domain := Domain{
		ID: 1, Host: "docs.example.test", TXTToken: "token", TXTResult: DomainTXTInvalid,
		TXTLastSuccessUnixNano: now.Add(-time.Hour).UnixNano(),
	}
	key := "1/docs.example.test/token"
	if err := store.SetTXTState(key, TXTState{
		LastSuccessUnixNano: now.Add(-time.Hour).UnixNano(), LastCheckUnixNano: now.Add(-2 * time.Minute).UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	ready, err := syncer.customDomainAuthorized(t.Context(), Site{CustomDomain: &domain})
	if err != nil || !ready {
		t.Fatalf("recently valid domain was revoked: ready=%v err=%v", ready, err)
	}
	local, ok := store.GetTXTState(key)
	if !ok || local.LastSuccessUnixNano == 0 {
		t.Fatalf("local TXT grace was not retained: %#v", local)
	}

	domain.TXTLastSuccessUnixNano = now.Add(-25 * time.Hour).UnixNano()
	ready, err = syncer.customDomainAuthorized(t.Context(), Site{CustomDomain: &domain})
	if err == nil || ready {
		t.Fatalf("expired Gitea TXT authorization remained active: ready=%v err=%v", ready, err)
	}

	domain.TXTResult = DomainTXTValid
	domain.TXTLastSuccessUnixNano = now.UnixNano()
	if err := store.SetTXTState(key, TXTState{
		LastSuccessUnixNano: now.Add(time.Hour).UnixNano(), LastCheckUnixNano: now.Add(time.Hour).UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	ready, err = syncer.customDomainAuthorized(t.Context(), Site{CustomDomain: &domain})
	if err == nil || ready {
		t.Fatalf("future local TXT timestamps were trusted: ready=%v err=%v", ready, err)
	}
}

func TestPendingScanContinuouslyFillsBoundedQueue(t *testing.T) {
	store, err := OpenStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	setTestSites(t, store, "http://pages.example.test", 1, 3, []Site{
		{RepoID: 1, PagesUpdatedUnixNano: 1},
		{RepoID: 2, PagesUpdatedUnixNano: 2},
		{RepoID: 3, PagesUpdatedUnixNano: 3},
	})
	syncer := NewSyncer(Config{DeployQueueSize: 1, DeployConcurrency: 1}, nil, store, nil, nil)
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
	store, err := OpenStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	syncer := NewSyncer(Config{DeployQueueSize: 1, DeployConcurrency: 1}, nil, store, nil, nil)

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
