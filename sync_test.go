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
		response.TXTVerificationEnabled = true
		switch r.URL.Path {
		case "/api/internal/pages/sites":
			fullPages++
			response.Until = 101
			response.NextSince = 101
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
			response.NextSince = 110
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
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := Config{RootURL: root, GiteaURL: base, Token: "test", SyncInterval: time.Minute, DeployQueueSize: 1, DeployConcurrency: 1}
	syncer := NewSyncer(cfg, NewClient(cfg), store, nil, nil)
	if err := syncer.full(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fullPages != 2 || catchups != 1 {
		t.Fatalf("full pages = %d, catch-ups = %d", fullPages, catchups)
	}
	if cursor, initialized, _, _ := store.State(); !initialized || cursor != 110 {
		t.Fatalf("sync state: cursor=%d initialized=%v", cursor, initialized)
	}
	if first, ok := store.Get(1); !ok || first.Site.RepoName != "renamed" || first.Site.PagesUpdatedUnixNano != 110 {
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
			NextCursor: &Cursor{PagesUpdatedUnixNano: 10, RepoID: 2},
			Sites:      []Site{{RepoID: 2, OwnerName: "alice", RepoName: "new", Enabled: true, PagesUpdatedUnixNano: 10}},
		})
	}))
	defer server.Close()
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	setTestSites(t, store, root.String(), 1, 7, []Site{old})
	cfg := Config{RootURL: root, GiteaURL: base, Token: "test", SyncInterval: time.Minute}
	syncer := NewSyncer(cfg, NewClient(cfg), store, nil, nil)
	if err := syncer.full(context.Background()); err == nil {
		t.Fatal("full sync unexpectedly succeeded")
	}
	if cursor, initialized, gotRoot, rootTime := store.State(); !initialized || cursor != 7 || gotRoot != root.String() || rootTime != 1 {
		t.Fatalf("previous sync state changed: cursor=%d initialized=%v root=%q root_time=%d", cursor, initialized, gotRoot, rootTime)
	}
	if got, ok := store.Get(old.RepoID); !ok || got.Site.RepoName != old.RepoName {
		t.Fatalf("previous site snapshot changed: %#v, found=%v", got, ok)
	}
	if _, ok := store.Get(2); ok {
		t.Fatal("partial full listing became visible")
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
				_ = json.NewEncoder(w).Encode(SitesResponse{RootURL: newRoot.String(), RootUpdatedUnixNano: 2, Until: 9, NextSince: 9})
			} else if r.URL.Query().Get("after_time") == "" {
				_ = json.NewEncoder(w).Encode(SitesResponse{
					RootURL: oldRoot.String(), RootUpdatedUnixNano: 1, Until: 9, NextSince: 9,
					Sites:      []Site{{RepoID: 2, OwnerName: "alice", RepoName: "intermediate", Enabled: true, PagesUpdatedUnixNano: 8}},
					NextCursor: &Cursor{PagesUpdatedUnixNano: 8, RepoID: 2},
				})
			} else {
				_ = json.NewEncoder(w).Encode(SitesResponse{RootURL: newRoot.String(), RootUpdatedUnixNano: 2, Until: 9, NextSince: 9})
			}
		case "/api/internal/pages/sites":
			_ = json.NewEncoder(w).Encode(SitesResponse{
				RootURL: newRoot.String(), RootUpdatedUnixNano: 2, Until: 9, NextSince: 9,
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
	store, err := OpenStore(t.TempDir())
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
	if _, ok := store.Get(1); ok {
		t.Fatal("old registration snapshot was retained after successful full sync")
	}
	if _, ok := store.Get(2); ok {
		t.Fatal("site from mixed incremental registration was retained")
	}
	if site, ok := store.Get(3); !ok || site.Site.RepoName != "new" {
		t.Fatalf("new full snapshot missing: %#v, found=%v", site, ok)
	}
	if _, initialized, gotRoot, rootTime := store.State(); !initialized || gotRoot != newRoot.String() || rootTime != 2 {
		t.Fatalf("new registration state missing: initialized=%v root=%q root_time=%d", initialized, gotRoot, rootTime)
	}
}

func TestPublishApplyFailureClosesCandidatePublication(t *testing.T) {
	root, err := url.Parse("http://pages.example.test")
	if err != nil {
		t.Fatal(err)
	}
	var report Result
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
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	site := Site{RepoID: 1, Enabled: true, OwnerName: "alice", RepoName: "docs", PagesUpdatedUnixNano: 1, TarGzSHA256: digest}
	setTestSites(t, store, root.String(), 1, 1, []Site{site})
	cfg := Config{GiteaURL: giteaURL, RootURL: root, Token: "test", Mode: ModeStandalone, DeployQueueSize: 1, DeployConcurrency: 1}
	syncer := NewSyncer(cfg, NewClient(cfg), store, nil, failingApplier{})
	if err := syncer.publishCurrent(context.Background(), site); err == nil {
		t.Fatal("publish unexpectedly succeeded")
	}
	local, ok := store.Get(site.RepoID)
	if !ok {
		t.Fatal("site state was removed")
	}
	if local.DefaultPublished || local.CustomPublished {
		t.Fatalf("candidate publication remained open: %#v", local)
	}
	if local.ErrorCode != "route_apply_failed" {
		t.Fatalf("error code = %q", local.ErrorCode)
	}
	if report.ConfigApplied || report.DefaultPublished || report.CustomPublished || report.ActualTarGzSHA256 != digest {
		t.Fatalf("unexpected failure report: %#v", report)
	}
}
