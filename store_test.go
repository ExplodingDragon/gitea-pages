// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package page

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func setTestSites(t *testing.T, store *Store, rootURL string, rootTime, cursor int64, sites []Site) {
	t.Helper()
	if err := store.ReplaceSites(rootURL, rootTime, cursor, sites, testArchiveLimits(), true); err != nil {
		t.Fatal(err)
	}
}

func TestStorePersistsPerSiteStateAndPendingReport(t *testing.T) {
	root := t.TempDir()
	s, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	site := Site{RepoID: 42, OwnerName: "alice", RepoName: "site", Enabled: true, PagesUpdatedUnixNano: 7, TarGzSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if err := s.UpsertSite(site); err != nil {
		t.Fatal(err)
	}
	candidate, err := s.WorkDir(site.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, ".gitea-pages-probe"), []byte("42 "+site.TarGzSHA256+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Install(site.RepoID, site.TarGzSHA256, candidate); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(s.SiteDir(site.RepoID), "current"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 != 0o111 {
		t.Fatalf("published site root cannot be traversed by an external Nginx user: %v", info.Mode())
	}
	if applied, err := s.SaveDeploymentResult(site, site.TarGzSHA256, true, false); err != nil || !applied {
		t.Fatal(err)
	}
	report := DeploymentResult{RepoID: 42, AppliedPagesUpdatedUnixNano: 7, ActualTarGzSHA256: site.TarGzSHA256, ConfigApplied: true, DefaultPublished: true}
	if err := s.SavePendingReport(report); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkConfirmedReport(report); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.SetSyncConfig(testArchiveLimits(), false); err != nil || !changed {
		t.Fatalf("save synchronized configuration: changed=%v err=%v", changed, err)
	}
	if err := s.SetTXTState("42/example.test", TXTState{LastSuccessUnixNano: 10, LastCheckUnixNano: 11}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	local, ok := s.GetSite(42)
	if !ok || !local.DefaultPublished || local.InstalledDigest != site.TarGzSHA256 {
		t.Fatalf("local site = %#v, ok=%v", local, ok)
	}
	reports := s.PendingReports()
	if len(reports) != 1 || reports[0] != report {
		t.Fatalf("pending reports = %#v", reports)
	}
	if state, ok := s.GetTXTState("42/example.test"); !ok || state.LastSuccessUnixNano != 10 {
		t.Fatalf("TXT state = %#v, ok=%v", state, ok)
	}
	if limits, requireTXT, known := s.SyncConfig(); !known || requireTXT || limits != testArchiveLimits() {
		t.Fatalf("synchronized configuration after restart: limits=%#v require_txt=%v known=%v", limits, requireTXT, known)
	}
	if changed, err := s.SetSyncConfig(testArchiveLimits(), false); err != nil || changed {
		t.Fatalf("unchanged synchronized configuration: changed=%v err=%v", changed, err)
	}
	if err := s.ClearPendingReport(42, report); err != nil {
		t.Fatal(err)
	}
	if len(s.PendingReports()) != 0 {
		t.Fatal("pending report was not removed")
	}
}

func TestOpenStoreProtectsPrivateDirectories(t *testing.T) {
	root := t.TempDir()
	s, err := OpenStore(root, defaultEtcdQuotaBytes)
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
	info, err := os.Stat(filepath.Join(root, "sites"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("sites directory is not traversable: %o", info.Mode().Perm())
	}
}

func TestStoreUpsertClearsRoutesWhenAddressChanges(t *testing.T) {
	s, err := OpenStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first := Site{RepoID: 1, OwnerName: "alice", RepoName: "site", Enabled: true, PagesUpdatedUnixNano: 1}
	if err := s.UpsertSite(first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDeploymentResult(first, "", true, true); err != nil {
		t.Fatal(err)
	}
	changed := first
	changed.OwnerName = "bob"
	if err := s.UpsertSite(changed); err != nil {
		t.Fatal(err)
	}
	local, _ := s.GetSite(1)
	if local.DefaultPublished || local.CustomPublished {
		t.Fatalf("route flags survived address change: %#v", local)
	}
}

func TestSetRegistrationPersistsClosedRoutesForNewRoot(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	site := Site{RepoID: 1, OwnerName: "alice", RepoName: "site", Enabled: true, PagesUpdatedUnixNano: 1}
	setTestSites(t, store, "http://old.pages.example", 1, 1, []Site{site})
	if _, err := store.SaveDeploymentResult(site, "", true, true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRegistration("http://new.pages.example", 2); err != nil {
		t.Fatal(err)
	}
	local, ok := store.GetSite(site.RepoID)
	if !ok || local.DefaultPublished || local.CustomPublished {
		t.Fatalf("new root retained old routes in memory: local=%#v found=%v", local, ok)
	}
	var persisted LocalSite
	if ok, err := store.kv.Get(t.Context(), siteKey(site.RepoID), &persisted); err != nil || !ok || persisted.DefaultPublished || persisted.CustomPublished {
		t.Fatalf("new root retained old persisted routes: local=%#v found=%v err=%v", persisted, ok, err)
	}
	store.Close()

	store, err = OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	local, ok = store.GetSite(site.RepoID)
	if !ok || local.DefaultPublished || local.CustomPublished {
		t.Fatalf("new root retained old routes: local=%#v found=%v", local, ok)
	}
	_, _, rootURL, rootTime := store.SyncState()
	if rootURL != "http://new.pages.example" || rootTime != 2 {
		t.Fatalf("registration after restart: root=%q time=%d", rootURL, rootTime)
	}
}

func TestOpenStoreClosesUnconfirmedPublication(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	digest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	site := Site{RepoID: 3, OwnerName: "alice", RepoName: "site", Enabled: true, PagesUpdatedUnixNano: 1, TarGzSHA256: digest}
	if err := store.UpsertSite(site); err != nil {
		t.Fatal(err)
	}
	candidate, err := store.WorkDir(site.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, ".gitea-pages-probe"), []byte("3 "+digest+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Install(site.RepoID, digest, candidate); err != nil {
		t.Fatal(err)
	}
	if applied, err := store.SaveDeploymentResult(site, digest, true, false); err != nil || !applied {
		t.Fatalf("set candidate publication: applied=%v err=%v", applied, err)
	}
	store.Close()

	store, err = OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	local, ok := store.GetSite(site.RepoID)
	if !ok {
		t.Fatal("site state was lost")
	}
	if local.DefaultPublished || local.CustomPublished {
		t.Fatalf("unconfirmed publication survived restart: %#v", local)
	}
}

func TestCleanupUntrackedContent(t *testing.T) {
	store, err := OpenStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tracked := Site{RepoID: 1, OwnerName: "alice", RepoName: "site", Enabled: true}
	setTestSites(t, store, "http://pages.example", 1, 1, []Site{tracked})
	for _, repoID := range []int64{1, 2} {
		if err := os.MkdirAll(filepath.Join(store.SiteDir(repoID), "content-old"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CleanupUntrackedContent(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.SiteDir(1)); err != nil {
		t.Fatalf("tracked site directory was removed: %v", err)
	}
	if _, err := os.Stat(store.SiteDir(2)); !os.IsNotExist(err) {
		t.Fatalf("untracked site directory remains: %v", err)
	}
}

func TestOpenStoreLocksDataRoot(t *testing.T) {
	root := t.TempDir()
	first, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := OpenStore(root, defaultEtcdQuotaBytes); err == nil {
		second.Close()
		first.Close()
		t.Fatal("second Pages process acquired the same data root")
	}
	first.Close()
	third, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatalf("data root stayed locked after close: %v", err)
	}
	third.Close()
}

func TestOpenStoreForcesFullSyncAfterInterruptedSnapshot(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	oldSite := Site{RepoID: 1, OwnerName: "alice", RepoName: "old", Enabled: true, PagesUpdatedUnixNano: 1}
	setTestSites(t, store, "http://pages.example", 1, 1, []Site{oldSite})
	if err := os.MkdirAll(store.SiteDir(oldSite.RepoID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.kv.Put(context.Background(), fullSyncProgressKey, true); err != nil {
		t.Fatal(err)
	}
	partial := LocalSite{Site: Site{RepoID: 2, OwnerName: "bob", RepoName: "partial", Enabled: true, PagesUpdatedUnixNano: 2}}
	if err := store.kv.Put(context.Background(), siteKey(2), partial); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store, err = OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if cursor, initialized, _, _ := store.SyncState(); initialized || cursor != 0 {
		t.Fatalf("interrupted snapshot was trusted: initialized=%v cursor=%d", initialized, cursor)
	}
	if _, ok := store.GetSite(1); ok {
		t.Fatal("old site metadata was loaded from an interrupted snapshot")
	}
	if _, ok := store.GetSite(2); ok {
		t.Fatal("partial site metadata was loaded from an interrupted snapshot")
	}
	if _, err := os.Stat(store.SiteDir(oldSite.RepoID)); err != nil {
		t.Fatalf("installed content was removed while recovering metadata: %v", err)
	}

	current := oldSite
	current.RepoName = "current"
	current.PagesUpdatedUnixNano = 3
	setTestSites(t, store, "http://pages.example", 1, 3, []Site{current})
	if _, ok := store.GetSite(1); !ok {
		t.Fatal("completed full snapshot was not installed")
	}
	var marker bool
	if ok, err := store.kv.Get(context.Background(), fullSyncProgressKey, &marker); err != nil || ok {
		t.Fatalf("full-sync recovery marker remains: ok=%v err=%v", ok, err)
	}
	var partialRecord LocalSite
	if ok, err := store.kv.Get(context.Background(), siteKey(2), &partialRecord); err != nil || ok {
		t.Fatalf("partial snapshot record remains: ok=%v err=%v", ok, err)
	}
}
