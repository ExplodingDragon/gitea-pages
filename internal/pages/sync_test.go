// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pages

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	pageconfig "gitea.dev/page/internal/config"
	"gitea.dev/page/internal/gitea"
	"gitea.dev/page/internal/storage"
	"github.com/miekg/dns"
)

type testPublisher struct {
	publishErr error
}

func (*testPublisher) restore(context.Context, deploymentSource) error { return nil }

func (p *testPublisher) publish(_ context.Context, deployment siteDeployment) (publicationResult, error) {
	if p.publishErr != nil {
		return publicationResult{}, p.publishErr
	}
	return publicationResult{defaultPublished: true, customPublished: deployment.customURL != nil}, nil
}

func (*testPublisher) unpublish(context.Context, int64) error { return nil }

func TestFullSyncCatchesUpdateDuringPagination(t *testing.T) {
	root, err := url.Parse("http://pages.example.test")
	if err != nil {
		t.Fatal(err)
	}
	all := make([]gitea.Site, 101)
	for i := range all {
		all[i] = gitea.Site{RepoID: int64(i + 1), OwnerName: "alice", RepoName: "site-" + strconv.Itoa(i+1), Enabled: true, PagesUpdatedUnixNano: int64(i + 1)}
	}
	var fullPages, catchups int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var response gitea.SitesResponse
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
				response.NextCursor = &gitea.Cursor{PagesUpdatedUnixNano: 100, RepoID: 100}
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
			response.Sites = []gitea.Site{changed}
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
	store, err := storage.Open(t.TempDir(), testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := pageconfig.Config{RootURL: root, GiteaURL: base, Token: "test", SyncInterval: time.Minute, DeployQueueSize: 1, DeployConcurrency: 1}
	syncer := newTestSyncer(cfg, newTestGiteaClient(cfg), store, &testPublisher{})
	if err := syncer.syncAllSites(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fullPages != 2 || catchups != 1 {
		t.Fatalf("full pages = %d, catch-ups = %d", fullPages, catchups)
	}
	if syncState := store.SyncState(); !syncState.Initialized || syncState.Cursor != 110 {
		t.Fatalf("sync state: cursor=%d initialized=%v", syncState.Cursor, syncState.Initialized)
	}
	if first, ok := store.Site(1); !ok || first.Site.RepoName != "renamed" || first.Site.PagesUpdatedUnixNano != 110 {
		t.Fatalf("site updated while paging = %#v, found=%v", first, ok)
	}
	count := 0
	store.RangeSites(func(storage.SiteState) bool { count++; return true })
	if count != 101 {
		t.Fatalf("site count = %d", count)
	}
}

func TestFullSyncKeepsPreviousSnapshotWhenPaginationFails(t *testing.T) {
	root, err := url.Parse("http://pages.example.test")
	if err != nil {
		t.Fatal(err)
	}
	old := gitea.Site{RepoID: 1, OwnerName: "alice", RepoName: "old", Enabled: true, PagesUpdatedUnixNano: 7}
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
		_ = json.NewEncoder(w).Encode(gitea.SitesResponse{
			RootURL: root.String(), RootUpdatedUnixNano: 2, Until: 10,
			ArchiveLimits: testArchiveLimits(), RequireGiteaTXTRecord: true,
			NextCursor: &gitea.Cursor{PagesUpdatedUnixNano: 10, RepoID: 2},
			Sites:      []gitea.Site{{RepoID: 2, OwnerName: "alice", RepoName: "new", Enabled: true, PagesUpdatedUnixNano: 10}},
		})
	}))
	defer server.Close()
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(t.TempDir(), testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	setTestSites(t, store, root.String(), 7, []gitea.Site{old})
	cfg := pageconfig.Config{RootURL: root, GiteaURL: base, Token: "test", SyncInterval: time.Minute}
	syncer := newTestSyncer(cfg, newTestGiteaClient(cfg), store, &testPublisher{})
	if err := syncer.syncAllSites(context.Background()); err == nil {
		t.Fatal("full sync unexpectedly succeeded")
	}
	if syncState := store.SyncState(); !syncState.Initialized || syncState.Cursor != 7 || syncState.RootURL != root.String() || syncState.RootUpdatedUnixNano != 1 {
		t.Fatalf("previous sync state changed: cursor=%d initialized=%v root=%q root_time=%d", syncState.Cursor, syncState.Initialized, syncState.RootURL, syncState.RootUpdatedUnixNano)
	}
	if got, ok := store.Site(old.RepoID); !ok || got.Site.RepoName != old.RepoName {
		t.Fatalf("previous site snapshot changed: %#v, found=%v", got, ok)
	}
	if _, ok := store.Site(2); ok {
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
		response := gitea.SitesResponse{
			RootURL: root.String(), RootUpdatedUnixNano: 1, ArchiveLimits: testArchiveLimits(),
			RequireGiteaTXTRecord: true, Until: 9,
		}
		if requests == 1 {
			response.Sites = []gitea.Site{{RepoID: 2, OwnerName: "alice", RepoName: "new", PagesUpdatedUnixNano: 8}}
			response.NextCursor = &gitea.Cursor{PagesUpdatedUnixNano: 8, RepoID: 2}
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
	store, err := storage.Open(t.TempDir(), testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	setTestSites(t, store, root.String(), 7, []gitea.Site{{RepoID: 1, OwnerName: "alice", RepoName: "old", PagesUpdatedUnixNano: 7}})
	cfg := pageconfig.Config{RootURL: root, GiteaURL: base, Token: "test", SyncInterval: time.Minute}
	syncer := newTestSyncer(cfg, newTestGiteaClient(cfg), store, &testPublisher{})
	var apiErr *gitea.APIError
	if err := syncer.syncOnce(t.Context()); !errors.As(err, &apiErr) || apiErr.Code != "state_changed" {
		t.Fatalf("sync error = %v", err)
	}
	if cursor := store.SyncState().Cursor; cursor != 7 {
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
				_ = json.NewEncoder(w).Encode(gitea.SitesResponse{RootURL: newRoot.String(), RootUpdatedUnixNano: 2, ArchiveLimits: testArchiveLimits(), RequireGiteaTXTRecord: true, Until: 9})
			case r.URL.Query().Get("after_time") == "":
				_ = json.NewEncoder(w).Encode(gitea.SitesResponse{
					RootURL: oldRoot.String(), RootUpdatedUnixNano: 1, Until: 9,
					ArchiveLimits: testArchiveLimits(), RequireGiteaTXTRecord: true,
					Sites:      []gitea.Site{{RepoID: 2, OwnerName: "alice", RepoName: "intermediate", Enabled: true, PagesUpdatedUnixNano: 8}},
					NextCursor: &gitea.Cursor{PagesUpdatedUnixNano: 8, RepoID: 2},
				})
			default:
				_ = json.NewEncoder(w).Encode(gitea.SitesResponse{RootURL: newRoot.String(), RootUpdatedUnixNano: 2, ArchiveLimits: testArchiveLimits(), RequireGiteaTXTRecord: true, Until: 9})
			}
		case "/api/internal/pages/sites":
			_ = json.NewEncoder(w).Encode(gitea.SitesResponse{
				RootURL: newRoot.String(), RootUpdatedUnixNano: 2, Until: 9,
				ArchiveLimits: testArchiveLimits(), RequireGiteaTXTRecord: true,
				Sites: []gitea.Site{{RepoID: 3, OwnerName: "alice", RepoName: "new", Enabled: true, PagesUpdatedUnixNano: 9}},
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
	store, err := storage.Open(t.TempDir(), testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	setTestSites(t, store, oldRoot.String(), 7, []gitea.Site{{RepoID: 1, OwnerName: "alice", RepoName: "old", Enabled: true, PagesUpdatedUnixNano: 7}})
	cfg := pageconfig.Config{RootURL: oldRoot, GiteaURL: base, Token: "test", SyncInterval: time.Minute}
	syncer := newTestSyncer(cfg, newTestGiteaClient(cfg), store, &testPublisher{})
	if err := syncer.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Site(1); ok {
		t.Fatal("old registration snapshot was retained after successful full sync")
	}
	if _, ok := store.Site(2); ok {
		t.Fatal("site from mixed incremental registration was retained")
	}
	if site, ok := store.Site(3); !ok || site.Site.RepoName != "new" {
		t.Fatalf("new full snapshot missing: %#v, found=%v", site, ok)
	}
	if syncState := store.SyncState(); !syncState.Initialized || syncState.RootURL != newRoot.String() || syncState.RootUpdatedUnixNano != 2 {
		t.Fatalf("new registration state missing: initialized=%v root=%q root_time=%d", syncState.Initialized, syncState.RootURL, syncState.RootUpdatedUnixNano)
	}
}

func TestPublishFailureRetainsConfirmedContent(t *testing.T) {
	root, err := url.Parse("http://pages.example.test")
	if err != nil {
		t.Fatal(err)
	}
	var report gitea.DeploymentResult
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
	store, err := storage.Open(t.TempDir(), testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	oldDigest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	newDigest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	targetSite := gitea.Site{RepoID: 1, Enabled: true, OwnerName: "alice", RepoName: "docs", PagesUpdatedUnixNano: 1, TarGzSHA256: newDigest}
	setTestSites(t, store, root.String(), 1, []gitea.Site{targetSite})
	oldCandidate, err := store.WorkDir(targetSite.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldCandidate, "old.html"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InstallContent(targetSite.RepoID, oldDigest, oldCandidate); err != nil {
		t.Fatal(err)
	}
	if applied, err := store.SaveDeploymentResult(t.Context(), targetSite, oldDigest, true, false); err != nil || !applied {
		t.Fatalf("save old publication: applied=%v err=%v", applied, err)
	}
	newCandidate, err := store.WorkDir(targetSite.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newCandidate, "new.html"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	content, err := store.InstallContent(targetSite.RepoID, newDigest, newCandidate)
	if err != nil {
		t.Fatal(err)
	}
	cfg := pageconfig.Config{GiteaURL: giteaURL, RootURL: root, Token: "test", DeployQueueSize: 1, DeployConcurrency: 1}
	syncer := newTestSyncer(cfg, newTestGiteaClient(cfg), store, &testPublisher{publishErr: errors.New("publish failed")})
	if err := syncer.deployer.publishSite(context.Background(), targetSite, content, true); err == nil {
		t.Fatal("publish unexpectedly succeeded")
	}
	local, ok := store.Site(targetSite.RepoID)
	if !ok {
		t.Fatal("site state was removed")
	}
	if !local.DefaultPublished || local.CustomPublished || local.InstalledDigest != oldDigest {
		t.Fatalf("confirmed publication was not retained: %#v", local)
	}
	if _, ok := store.InstalledContent(targetSite.RepoID, oldDigest); !ok {
		t.Fatal("confirmed content was removed")
	}
	if _, ok := store.InstalledContent(targetSite.RepoID, newDigest); ok {
		t.Fatal("failed candidate content remains")
	}
	if report.ConfigApplied || report.DefaultPublished || report.CustomPublished || report.ActualTarGzSHA256 != oldDigest || report.ErrorCode != gitea.ErrorRouteApplyFailed {
		t.Fatalf("unexpected failure report: %#v", report)
	}
}

func TestDeploymentFailureReportsAfterStoreFailure(t *testing.T) {
	root, err := url.Parse("http://pages.example.test")
	if err != nil {
		t.Fatal(err)
	}
	var report gitea.DeploymentResult
	reportServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/internal/pages/results" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Errorf("decode report: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "report_failed"})
	}))
	defer reportServer.Close()
	giteaURL, err := url.Parse(reportServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(t.TempDir(), testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	targetSite := gitea.Site{RepoID: 1, Enabled: true, OwnerName: "alice", RepoName: "docs", PagesUpdatedUnixNano: 1}
	setTestSites(t, store, root.String(), 1, []gitea.Site{targetSite})
	store.Close()

	cfg := pageconfig.Config{GiteaURL: giteaURL, RootURL: root, Token: "test", DeployQueueSize: 1, DeployConcurrency: 1}
	syncer := newTestSyncer(cfg, newTestGiteaClient(cfg), store, &testPublisher{})
	err = syncer.deployer.reportDeploymentFailure(t.Context(), targetSite, true, gitea.ErrorContentInstallFailed, errors.New("disk full"))
	if err == nil {
		t.Fatal("deployment failure did not retain the local storage error")
	}
	if !strings.Contains(err.Error(), "save deployment failure") {
		t.Fatalf("deployment failure did not retain the local storage error: %v", err)
	}
	var apiErr *gitea.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "report_failed" {
		t.Fatalf("deployment failure did not retain the report error: %v", err)
	}
	if report.RepoID != targetSite.RepoID || report.ConfigApplied || report.DefaultPublished || report.CustomPublished || report.ErrorCode != gitea.ErrorContentInstallFailed {
		t.Fatalf("unexpected failure report: %#v", report)
	}
}

func TestCustomDomainTXTGrace(t *testing.T) {
	store, err := storage.Open(t.TempDir(), testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resolverAddress := startTestDNSServer(t, dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(request)
		for _, question := range request.Question {
			if question.Qtype == dns.TypeTXT {
				response.Answer = append(response.Answer, &dns.TXT{
					Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60},
					Txt: []string{"different"},
				})
			}
		}
		_ = writer.WriteMsg(response)
	}))
	syncer := newTestSyncer(pageconfig.Config{DNSRefreshInterval: time.Minute, DNSResolver: resolverAddress}, nil, store, &testPublisher{})
	now := time.Now()
	domain := gitea.Domain{
		ID: 1, Host: "docs.example.test", TXTToken: "token", TXTResult: "invalid",
		TXTLastSuccessUnixNano: now.Add(-time.Hour).UnixNano(),
	}
	key := "1/docs.example.test/token"
	if err := store.SetTXTState(t.Context(), key, storage.TXTState{
		LastSuccessUnixNano: now.Add(-time.Hour).UnixNano(), LastCheckUnixNano: now.Add(-2 * time.Minute).UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	ready, err := syncer.deployer.customDomainAuthorized(t.Context(), gitea.Site{CustomDomain: &domain}, true)
	if err != nil || !ready {
		t.Fatalf("recently valid domain was revoked: ready=%v err=%v", ready, err)
	}
	local, ok := store.TXTState(key)
	if !ok || local.LastSuccessUnixNano == 0 {
		t.Fatalf("local TXT grace was not retained: %#v", local)
	}

	domain.TXTLastSuccessUnixNano = now.Add(-25 * time.Hour).UnixNano()
	ready, err = syncer.deployer.customDomainAuthorized(t.Context(), gitea.Site{CustomDomain: &domain}, true)
	if err == nil || ready {
		t.Fatalf("expired Gitea TXT authorization remained active: ready=%v err=%v", ready, err)
	}

	domain.TXTResult = gitea.DomainTXTValid
	domain.TXTLastSuccessUnixNano = now.UnixNano()
	if err := store.SetTXTState(t.Context(), key, storage.TXTState{
		LastSuccessUnixNano: now.Add(time.Hour).UnixNano(), LastCheckUnixNano: now.Add(time.Hour).UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	ready, err = syncer.deployer.customDomainAuthorized(t.Context(), gitea.Site{CustomDomain: &domain}, true)
	if err == nil || ready {
		t.Fatalf("future local TXT timestamps were trusted: ready=%v err=%v", ready, err)
	}
}

func TestPendingScanContinuouslyFillsBoundedQueue(t *testing.T) {
	store, err := storage.Open(t.TempDir(), testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	setTestSites(t, store, "http://pages.example.test", 3, []gitea.Site{
		{RepoID: 1, PagesUpdatedUnixNano: 1},
		{RepoID: 2, PagesUpdatedUnixNano: 2},
		{RepoID: 3, PagesUpdatedUnixNano: 3},
	})
	syncer := newTestSyncer(pageconfig.Config{DeployQueueSize: 1, DeployConcurrency: 1}, nil, store, &testPublisher{})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	scanDone := make(chan error, 1)
	go func() { scanDone <- syncer.deployer.scanPendingSites(ctx) }()

	received := make(map[int64]struct{}, 3)
	for len(received) < 3 {
		repoID, ok := syncer.deployer.queue.next(ctx)
		if !ok {
			t.Fatalf("bounded queue stopped after repository IDs %v", received)
		}
		received[repoID] = struct{}{}
		syncer.deployer.queue.finish(repoID)
	}
	if err := <-scanDone; err != nil {
		t.Fatal(err)
	}
}
