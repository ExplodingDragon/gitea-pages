package page

import (
	"os"
	"path/filepath"
	"testing"
)

func setTestSites(t *testing.T, store *Store, rootURL string, rootTime, cursor int64, sites []Site) {
	t.Helper()
	if err := store.ReplaceSites(rootURL, rootTime, cursor, sites, true, 0); err != nil {
		t.Fatal(err)
	}
}

func TestStorePersistsPerSiteStateAndPendingReport(t *testing.T) {
	root := t.TempDir()
	s, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	site := Site{RepoID: 42, OwnerName: "alice", RepoName: "site", Enabled: true, PagesUpdatedUnixNano: 7, TarGzSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if err := s.Upsert(site); err != nil {
		t.Fatal(err)
	}
	candidate, err := s.WorkDir(site.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, ".gitea-pages-probe"), []byte("42 "+site.TarGzSHA256+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := s.Install(site.RepoID, site.TarGzSHA256, candidate); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(s.SiteDir(site.RepoID), "current"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0111 != 0111 {
		t.Fatalf("published site root cannot be traversed by an external Nginx user: %v", info.Mode())
	}
	if applied, err := s.SetResultForSite(site, site.TarGzSHA256, true, false, ""); err != nil || !applied {
		t.Fatal(err)
	}
	report := Result{RepoID: 42, AppliedPagesUpdatedUnixNano: 7, ActualTarGzSHA256: site.TarGzSHA256, ConfigApplied: true, DefaultPublished: true}
	if err := s.SavePendingReport(report); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkConfirmedReport(report); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTXTState("42/example.test", TXTState{LastSuccessUnixNano: 10, LastCheckUnixNano: 11, Valid: true}); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.SetTXTPolicy(false, 17); err != nil || !changed {
		t.Fatalf("save TXT policy: changed=%v err=%v", changed, err)
	}
	s.Close()

	s, err = OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	local, ok := s.Get(42)
	if !ok || !local.DefaultPublished || local.InstalledDigest != site.TarGzSHA256 {
		t.Fatalf("local site = %#v, ok=%v", local, ok)
	}
	reports := s.PendingReports()
	if len(reports) != 1 || reports[0] != report {
		t.Fatalf("pending reports = %#v", reports)
	}
	if state, ok := s.GetTXTState("42/example.test"); !ok || !state.Valid || state.LastSuccessUnixNano != 10 {
		t.Fatalf("TXT state = %#v, ok=%v", state, ok)
	}
	if enabled, updated, known := s.TXTPolicy(); !known || enabled || updated != 17 {
		t.Fatalf("TXT policy after restart: enabled=%v updated=%d known=%v", enabled, updated, known)
	}
	if changed, err := s.SetTXTPolicy(false, 17); err != nil || changed {
		t.Fatalf("unchanged TXT policy: changed=%v err=%v", changed, err)
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
	s, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, name := range []string{"work", "etcd"} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0700 {
			t.Fatalf("%s directory mode = %o, want 700", name, info.Mode().Perm())
		}
	}
	info, err := os.Stat(filepath.Join(root, "sites"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0111 == 0 {
		t.Fatalf("sites directory is not traversable: %o", info.Mode().Perm())
	}
}

func TestStoreUpsertClearsRoutesWhenAddressChanges(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first := Site{RepoID: 1, OwnerName: "alice", RepoName: "site", Enabled: true, PagesUpdatedUnixNano: 1}
	if err := s.Upsert(first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetResultForSite(first, "", true, true, ""); err != nil {
		t.Fatal(err)
	}
	changed := first
	changed.OwnerName = "bob"
	if err := s.Upsert(changed); err != nil {
		t.Fatal(err)
	}
	local, _ := s.Get(1)
	if local.DefaultPublished || local.CustomPublished {
		t.Fatalf("route flags survived address change: %#v", local)
	}
}

func TestOpenStoreClosesUnconfirmedPublication(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	digest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	site := Site{RepoID: 3, OwnerName: "alice", RepoName: "site", Enabled: true, PagesUpdatedUnixNano: 1, TarGzSHA256: digest}
	if err := store.Upsert(site); err != nil {
		t.Fatal(err)
	}
	candidate, err := store.WorkDir(site.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, ".gitea-pages-probe"), []byte("3 "+digest+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := store.Install(site.RepoID, digest, candidate); err != nil {
		t.Fatal(err)
	}
	if applied, err := store.SetResultForSite(site, digest, true, false, ""); err != nil || !applied {
		t.Fatalf("set candidate publication: applied=%v err=%v", applied, err)
	}
	store.Close()

	store, err = OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	local, ok := store.Get(site.RepoID)
	if !ok {
		t.Fatal("site state was lost")
	}
	if local.DefaultPublished || local.CustomPublished {
		t.Fatalf("unconfirmed publication survived restart: %#v", local)
	}
}

func TestCleanupUntrackedContent(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tracked := Site{RepoID: 1, OwnerName: "alice", RepoName: "site", Enabled: true}
	setTestSites(t, store, "http://pages.example", 1, 1, []Site{tracked})
	for _, repoID := range []int64{1, 2} {
		if err := os.MkdirAll(filepath.Join(store.SiteDir(repoID), "content-old"), 0755); err != nil {
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
	first, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := OpenStore(root); err == nil {
		second.Close()
		first.Close()
		t.Fatal("second Pages process acquired the same data root")
	}
	first.Close()
	third, err := OpenStore(root)
	if err != nil {
		t.Fatalf("data root stayed locked after close: %v", err)
	}
	third.Close()
}
