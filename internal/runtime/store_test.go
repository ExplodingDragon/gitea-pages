// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func setTestSites(t *testing.T, store *store, rootURL string, cursor int64, sites []site) {
	t.Helper()
	if err := store.replaceSites(rootURL, 1, cursor, sites, testArchiveLimits(), true); err != nil {
		t.Fatal(err)
	}
}

func TestStorePersistsPerSiteStateAndPendingReport(t *testing.T) {
	root := t.TempDir()
	s, err := openStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	site := site{RepoID: 42, OwnerName: "alice", RepoName: "site", Enabled: true, PagesUpdatedUnixNano: 7, TarGzSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if err := s.upsertSite(site); err != nil {
		t.Fatal(err)
	}
	candidate, err := s.workDir(site.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "index.html"), []byte("page"), 0o644); err != nil {
		t.Fatal(err)
	}
	content, err := s.prepareContent(site.RepoID, site.TarGzSHA256, candidate)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(content.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 != 0o111 {
		t.Fatalf("published site root cannot be traversed by an external Nginx user: %v", info.Mode())
	}
	if applied, err := s.saveDeploymentResult(site, site.TarGzSHA256, true, false); err != nil || !applied {
		t.Fatal(err)
	}
	report := deploymentResult{RepoID: 42, AppliedPagesUpdatedUnixNano: 7, ActualTarGzSHA256: site.TarGzSHA256, ConfigApplied: true, DefaultPublished: true}
	if err := s.savePendingReport(report); err != nil {
		t.Fatal(err)
	}
	if err := s.markConfirmedReport(report); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.setSyncConfig(testArchiveLimits(), false); err != nil || !changed {
		t.Fatalf("save synchronized configuration: changed=%v err=%v", changed, err)
	}
	if err := s.setTXTState("42/example.test", txtState{LastSuccessUnixNano: 10, LastCheckUnixNano: 11}); err != nil {
		t.Fatal(err)
	}
	s.close()

	s, err = openStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	local, ok := s.getSite(42)
	if !ok || !local.DefaultPublished || local.InstalledDigest != site.TarGzSHA256 {
		t.Fatalf("local site = %#v, ok=%v", local, ok)
	}
	reports := s.pendingReports()
	if len(reports) != 1 || reports[0] != report {
		t.Fatalf("pending reports = %#v", reports)
	}
	if state, ok := s.getTXTState("42/example.test"); !ok || state.LastSuccessUnixNano != 10 {
		t.Fatalf("TXT state = %#v, ok=%v", state, ok)
	}
	if limits, requireTXT, known := s.syncConfig(); !known || requireTXT || limits != testArchiveLimits() {
		t.Fatalf("synchronized configuration after restart: limits=%#v require_txt=%v known=%v", limits, requireTXT, known)
	}
	if changed, err := s.setSyncConfig(testArchiveLimits(), false); err != nil || changed {
		t.Fatalf("unchanged synchronized configuration: changed=%v err=%v", changed, err)
	}
	if err := s.clearPendingReport(42, report); err != nil {
		t.Fatal(err)
	}
	if len(s.pendingReports()) != 0 {
		t.Fatal("pending report was not removed")
	}
}

func TestOpenStoreProtectsPrivateDirectories(t *testing.T) {
	root := t.TempDir()
	s, err := openStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
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
	s, err := openStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	first := site{RepoID: 1, OwnerName: "alice", RepoName: "site", Enabled: true, PagesUpdatedUnixNano: 1}
	if err := s.upsertSite(first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.saveDeploymentResult(first, "", true, true); err != nil {
		t.Fatal(err)
	}
	changed := first
	changed.OwnerName = "bob"
	if err := s.upsertSite(changed); err != nil {
		t.Fatal(err)
	}
	local, _ := s.getSite(1)
	if local.DefaultPublished || local.CustomPublished {
		t.Fatalf("route flags survived address change: %#v", local)
	}
}

func TestSetRegistrationPersistsClosedRoutesForNewRoot(t *testing.T) {
	root := t.TempDir()
	store, err := openStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	targetSite := site{RepoID: 1, OwnerName: "alice", RepoName: "site", Enabled: true, PagesUpdatedUnixNano: 1}
	setTestSites(t, store, "http://old.pages.example", 1, []site{targetSite})
	if _, err := store.saveDeploymentResult(targetSite, "", true, true); err != nil {
		t.Fatal(err)
	}
	if err := store.setRegistration("http://new.pages.example", 2); err != nil {
		t.Fatal(err)
	}
	local, ok := store.getSite(targetSite.RepoID)
	if !ok || local.DefaultPublished || local.CustomPublished {
		t.Fatalf("new root retained old routes in memory: local=%#v found=%v", local, ok)
	}
	var persisted localSite
	if ok, err := store.kv.get(t.Context(), siteKey(targetSite.RepoID), &persisted); err != nil || !ok || persisted.DefaultPublished || persisted.CustomPublished {
		t.Fatalf("new root retained old persisted routes: local=%#v found=%v err=%v", persisted, ok, err)
	}
	store.close()

	store, err = openStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	local, ok = store.getSite(targetSite.RepoID)
	if !ok || local.DefaultPublished || local.CustomPublished {
		t.Fatalf("new root retained old routes: local=%#v found=%v", local, ok)
	}
	_, _, rootURL, rootTime := store.syncState()
	if rootURL != "http://new.pages.example" || rootTime != 2 {
		t.Fatalf("registration after restart: root=%q time=%d", rootURL, rootTime)
	}
}

func TestOpenStoreClosesUnconfirmedPublication(t *testing.T) {
	root := t.TempDir()
	store, err := openStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	digest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	site := site{RepoID: 3, OwnerName: "alice", RepoName: "site", Enabled: true, PagesUpdatedUnixNano: 1, TarGzSHA256: digest}
	if err := store.upsertSite(site); err != nil {
		t.Fatal(err)
	}
	candidate, err := store.workDir(site.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "index.html"), []byte("page"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.prepareContent(site.RepoID, digest, candidate); err != nil {
		t.Fatal(err)
	}
	if applied, err := store.saveDeploymentResult(site, digest, true, false); err != nil || !applied {
		t.Fatalf("set candidate publication: applied=%v err=%v", applied, err)
	}
	store.close()

	store, err = openStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	local, ok := store.getSite(site.RepoID)
	if !ok {
		t.Fatal("site state was lost")
	}
	if local.DefaultPublished || local.CustomPublished {
		t.Fatalf("unconfirmed publication survived restart: %#v", local)
	}
}

func TestCleanupUntrackedContent(t *testing.T) {
	store, err := openStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	tracked := site{RepoID: 1, OwnerName: "alice", RepoName: "site", Enabled: true}
	setTestSites(t, store, "http://pages.example", 1, []site{tracked})
	for _, repoID := range []int64{1, 2} {
		if err := os.MkdirAll(filepath.Join(store.siteDir(repoID), "content-old"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.cleanupUntrackedContent(); err != nil {
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
	first, err := openStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := openStore(root, defaultEtcdQuotaBytes); err == nil {
		second.close()
		first.close()
		t.Fatal("second Pages process acquired the same data root")
	}
	first.close()
	third, err := openStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatalf("data root stayed locked after close: %v", err)
	}
	third.close()
}

func TestOpenStoreForcesFullSyncAfterInterruptedSnapshot(t *testing.T) {
	root := t.TempDir()
	store, err := openStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	oldSite := site{RepoID: 1, OwnerName: "alice", RepoName: "old", Enabled: true, PagesUpdatedUnixNano: 1}
	setTestSites(t, store, "http://pages.example", 1, []site{oldSite})
	if err := os.MkdirAll(store.siteDir(oldSite.RepoID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.kv.put(context.Background(), fullSyncProgressKey, true); err != nil {
		t.Fatal(err)
	}
	partial := localSite{Site: site{RepoID: 2, OwnerName: "bob", RepoName: "partial", Enabled: true, PagesUpdatedUnixNano: 2}}
	if err := store.kv.put(context.Background(), siteKey(2), partial); err != nil {
		t.Fatal(err)
	}
	store.close()

	store, err = openStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	if cursor, initialized, _, _ := store.syncState(); initialized || cursor != 0 {
		t.Fatalf("interrupted snapshot was trusted: initialized=%v cursor=%d", initialized, cursor)
	}
	if _, ok := store.getSite(1); ok {
		t.Fatal("old site metadata was loaded from an interrupted snapshot")
	}
	if _, ok := store.getSite(2); ok {
		t.Fatal("partial site metadata was loaded from an interrupted snapshot")
	}
	if _, err := os.Stat(store.siteDir(oldSite.RepoID)); err != nil {
		t.Fatalf("installed content was removed while recovering metadata: %v", err)
	}

	current := oldSite
	current.RepoName = "current"
	current.PagesUpdatedUnixNano = 3
	setTestSites(t, store, "http://pages.example", 3, []site{current})
	if _, ok := store.getSite(1); !ok {
		t.Fatal("completed full snapshot was not installed")
	}
	var marker bool
	if ok, err := store.kv.get(context.Background(), fullSyncProgressKey, &marker); err != nil || ok {
		t.Fatalf("full-sync recovery marker remains: ok=%v err=%v", ok, err)
	}
	var partialRecord localSite
	if ok, err := store.kv.get(context.Background(), siteKey(2), &partialRecord); err != nil || ok {
		t.Fatalf("partial snapshot record remains: ok=%v err=%v", ok, err)
	}
}
