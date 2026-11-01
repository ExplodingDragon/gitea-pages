// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gitea.dev/page/internal/artifact"
	"gitea.dev/page/internal/gitea"
)

const testEtcdQuotaBytes = int64(64 * 1024 * 1024)

func testArchiveLimits() artifact.Limits {
	return artifact.Limits{
		MaxTarGzBytes: 100 * 1024 * 1024,
		MaxSiteBytes:  512 * 1024 * 1024,
		MaxFileBytes:  100 * 1024 * 1024,
		MaxEntries:    10_000,
	}
}

func setTestSites(t *testing.T, store *Store, rootURL string, cursor int64, sites []gitea.Site) {
	t.Helper()
	listing := make(map[int64]gitea.Site, len(sites))
	for _, site := range sites {
		listing[site.RepoID] = site
	}
	if err := store.ReplaceSites(t.Context(), rootURL, 1, cursor, listing, testArchiveLimits(), true); err != nil {
		t.Fatal(err)
	}
}

func TestStorePersistsPerSiteStateAndPendingReport(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	site := gitea.Site{RepoID: 42, OwnerName: "alice", RepoName: "site", Enabled: true, PagesUpdatedUnixNano: 7, TarGzSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if err := s.UpsertSite(t.Context(), site); err != nil {
		t.Fatal(err)
	}
	candidate, err := s.WorkDir(site.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "index.html"), []byte("page"), 0o644); err != nil {
		t.Fatal(err)
	}
	content, err := s.InstallContent(site.RepoID, site.TarGzSHA256, candidate)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(content.Root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 != 0o111 {
		t.Fatalf("published site root cannot be traversed by the web service: %v", info.Mode())
	}
	if applied, err := s.SaveDeploymentResult(t.Context(), site, site.TarGzSHA256, true, false); err != nil || !applied {
		t.Fatal(err)
	}
	report := gitea.DeploymentResult{RepoID: 42, AppliedPagesUpdatedUnixNano: 7, ActualTarGzSHA256: site.TarGzSHA256, ConfigApplied: true, DefaultPublished: true}
	if err := s.SavePendingReport(t.Context(), report); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkConfirmedReport(t.Context(), report); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.SetSyncConfig(t.Context(), testArchiveLimits(), false); err != nil || !changed {
		t.Fatalf("save synchronized configuration: changed=%v err=%v", changed, err)
	}
	if err := s.SetTXTState(t.Context(), "42/example.test", TXTState{LastSuccessUnixNano: 10, LastCheckUnixNano: 11}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(root, testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	local, ok := s.Site(42)
	if !ok || !local.DefaultPublished || local.InstalledDigest != site.TarGzSHA256 {
		t.Fatalf("local site = %#v, ok=%v", local, ok)
	}
	reports := s.PendingReports()
	if len(reports) != 1 || reports[0] != report {
		t.Fatalf("pending reports = %#v", reports)
	}
	if state, ok := s.TXTState("42/example.test"); !ok || state.LastSuccessUnixNano != 10 {
		t.Fatalf("TXT state = %#v, ok=%v", state, ok)
	}
	if syncState := s.SyncState(); syncState.ArchiveLimits != testArchiveLimits() || syncState.RequireGiteaTXT {
		t.Fatalf("synchronized configuration after restart: limits=%#v require_txt=%v", syncState.ArchiveLimits, syncState.RequireGiteaTXT)
	}
	if changed, err := s.SetSyncConfig(t.Context(), testArchiveLimits(), false); err != nil || changed {
		t.Fatalf("unchanged synchronized configuration: changed=%v err=%v", changed, err)
	}
	if err := s.ClearPendingReport(t.Context(), 42, report); err != nil {
		t.Fatal(err)
	}
	if len(s.PendingReports()) != 0 {
		t.Fatal("pending report was not removed")
	}
}

func TestStoreScansSitesWithPendingReportState(t *testing.T) {
	store, err := Open(t.TempDir(), testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sites := []gitea.Site{
		{RepoID: 1, OwnerName: "alice", RepoName: "one", PagesUpdatedUnixNano: 1},
		{RepoID: 2, OwnerName: "alice", RepoName: "two", PagesUpdatedUnixNano: 2},
	}
	setTestSites(t, store, "http://pages.example", 2, sites)
	if err := store.SavePendingReport(t.Context(), gitea.DeploymentResult{RepoID: 2, AppliedPagesUpdatedUnixNano: 2}); err != nil {
		t.Fatal(err)
	}

	visited := make(map[int64]bool)
	if err := store.ScanSites(t.Context(), func(site SiteState, pendingReport bool) error {
		visited[site.Site.RepoID] = pendingReport
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(visited) != 2 || visited[1] || !visited[2] {
		t.Fatalf("scanned sites = %#v", visited)
	}
}

func TestOpenStorePreparesPrivateDirectories(t *testing.T) {
	root := t.TempDir()
	abandonedWork := filepath.Join(root, "work", "42-abandoned", "content")
	if err := os.MkdirAll(abandonedWork, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abandonedWork, "index.html"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root, testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, name := range []string{"work", "etcd"} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s directory mode = %o, want 700", name, info.Mode().Perm())
		}
	}
	workEntries, err := os.ReadDir(filepath.Join(root, "work"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workEntries) != 0 {
		t.Fatalf("deployment work directory contains stale entries: %v", workEntries)
	}
	info, err := os.Stat(filepath.Join(root, "sites"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("sites directory is not traversable: %o", info.Mode().Perm())
	}
}

func TestStoreUpsertClearsRoutesWhenAddressChanges(t *testing.T) {
	s, err := Open(t.TempDir(), testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first := gitea.Site{RepoID: 1, OwnerName: "alice", RepoName: "site", Enabled: true, PagesUpdatedUnixNano: 1}
	if err := s.UpsertSite(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDeploymentResult(t.Context(), first, "", true, true); err != nil {
		t.Fatal(err)
	}
	changed := first
	changed.OwnerName = "bob"
	if err := s.UpsertSite(t.Context(), changed); err != nil {
		t.Fatal(err)
	}
	local, _ := s.Site(1)
	if local.DefaultPublished || local.CustomPublished {
		t.Fatalf("route flags survived address change: %#v", local)
	}
}

func TestSetRegistrationPersistsClosedRoutesForNewRoot(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root, testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	targetSite := gitea.Site{RepoID: 1, OwnerName: "alice", RepoName: "site", Enabled: true, PagesUpdatedUnixNano: 1}
	setTestSites(t, store, "http://old.pages.example", 1, []gitea.Site{targetSite})
	if _, err := store.SaveDeploymentResult(t.Context(), targetSite, "", true, true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRegistration(t.Context(), "http://new.pages.example", 2); err != nil {
		t.Fatal(err)
	}
	local, ok := store.Site(targetSite.RepoID)
	if !ok || local.DefaultPublished || local.CustomPublished {
		t.Fatalf("new root retained old routes in memory: local=%#v found=%v", local, ok)
	}
	var persisted SiteState
	if ok, err := store.records.Get(t.Context(), siteKey(targetSite.RepoID), &persisted); err != nil || !ok || persisted.DefaultPublished || persisted.CustomPublished {
		t.Fatalf("new root retained old persisted routes: local=%#v found=%v err=%v", persisted, ok, err)
	}
	store.Close()

	store, err = Open(root, testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	local, ok = store.Site(targetSite.RepoID)
	if !ok || local.DefaultPublished || local.CustomPublished {
		t.Fatalf("new root retained old routes: local=%#v found=%v", local, ok)
	}
	syncState := store.SyncState()
	if syncState.RootURL != "http://new.pages.example" || syncState.RootUpdatedUnixNano != 2 {
		t.Fatalf("registration after restart: root=%q time=%d", syncState.RootURL, syncState.RootUpdatedUnixNano)
	}
}

func TestOpenStoreClosesUnconfirmedPublication(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root, testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	digest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	site := gitea.Site{RepoID: 3, OwnerName: "alice", RepoName: "site", Enabled: true, PagesUpdatedUnixNano: 1, TarGzSHA256: digest}
	if err := store.UpsertSite(t.Context(), site); err != nil {
		t.Fatal(err)
	}
	candidate, err := store.WorkDir(site.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "index.html"), []byte("page"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InstallContent(site.RepoID, digest, candidate); err != nil {
		t.Fatal(err)
	}
	if applied, err := store.SaveDeploymentResult(t.Context(), site, digest, true, false); err != nil || !applied {
		t.Fatalf("set candidate publication: applied=%v err=%v", applied, err)
	}
	store.Close()

	store, err = Open(root, testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	local, ok := store.Site(site.RepoID)
	if !ok {
		t.Fatal("site state was lost")
	}
	if local.DefaultPublished || local.CustomPublished {
		t.Fatalf("unconfirmed publication survived restart: %#v", local)
	}
}

func TestRemoveUntrackedContent(t *testing.T) {
	store, err := Open(t.TempDir(), testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tracked := gitea.Site{RepoID: 1, OwnerName: "alice", RepoName: "site", Enabled: true}
	setTestSites(t, store, "http://pages.example", 1, []gitea.Site{tracked})
	for _, repoID := range []int64{1, 2} {
		if err := os.MkdirAll(filepath.Join(store.siteDir(repoID), "content-old"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RemoveUntrackedContent(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.siteDir(1)); err != nil {
		t.Fatalf("tracked site directory was removed: %v", err)
	}
	if _, err := os.Stat(store.siteDir(2)); !os.IsNotExist(err) {
		t.Fatalf("untracked site directory remains: %v", err)
	}
}

func TestOpenStoreLocksDataRoot(t *testing.T) {
	root := t.TempDir()
	first, err := Open(root, testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Open(root, testEtcdQuotaBytes); err == nil {
		second.Close()
		first.Close()
		t.Fatal("second Pages process acquired the same data root")
	}
	first.Close()
	third, err := Open(root, testEtcdQuotaBytes)
	if err != nil {
		t.Fatalf("data root stayed locked after close: %v", err)
	}
	third.Close()
}

func TestStoreWriteUsesCallerContext(t *testing.T) {
	store, err := Open(t.TempDir(), testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = store.UpsertSite(ctx, gitea.Site{RepoID: 1, PagesUpdatedUnixNano: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("store write after cancellation = %v", err)
	}
}

func TestOpenStoreForcesFullSyncAfterInterruptedSnapshot(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root, testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	oldSite := gitea.Site{RepoID: 1, OwnerName: "alice", RepoName: "old", Enabled: true, PagesUpdatedUnixNano: 1}
	setTestSites(t, store, "http://pages.example", 1, []gitea.Site{oldSite})
	if err := os.MkdirAll(store.siteDir(oldSite.RepoID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.records.Put(context.Background(), fullSyncProgressKey, true); err != nil {
		t.Fatal(err)
	}
	partial := SiteState{Site: gitea.Site{RepoID: 2, OwnerName: "bob", RepoName: "partial", Enabled: true, PagesUpdatedUnixNano: 2}}
	if err := store.records.Put(context.Background(), siteKey(2), partial); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store, err = Open(root, testEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if syncState := store.SyncState(); syncState.Initialized || syncState.Cursor != 0 {
		t.Fatalf("interrupted snapshot was trusted: initialized=%v cursor=%d", syncState.Initialized, syncState.Cursor)
	}
	if _, ok := store.Site(1); ok {
		t.Fatal("old site metadata was loaded from an interrupted snapshot")
	}
	if _, ok := store.Site(2); ok {
		t.Fatal("partial site metadata was loaded from an interrupted snapshot")
	}
	if _, err := os.Stat(store.siteDir(oldSite.RepoID)); err != nil {
		t.Fatalf("installed content was removed while recovering metadata: %v", err)
	}

	current := oldSite
	current.RepoName = "current"
	current.PagesUpdatedUnixNano = 3
	setTestSites(t, store, "http://pages.example", 3, []gitea.Site{current})
	if _, ok := store.Site(1); !ok {
		t.Fatal("completed full snapshot was not installed")
	}
	var marker bool
	if ok, err := store.records.Get(context.Background(), fullSyncProgressKey, &marker); err != nil || ok {
		t.Fatalf("full-sync recovery marker remains: ok=%v err=%v", ok, err)
	}
	var partialRecord SiteState
	if ok, err := store.records.Get(context.Background(), siteKey(2), &partialRecord); err != nil || ok {
		t.Fatalf("partial snapshot record remains: ok=%v err=%v", ok, err)
	}
}
