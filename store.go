package page

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type LocalSite struct {
	Site                Site    `json:"site"`
	InstalledDigest     string  `json:"installed_digest"`
	DefaultPublished    bool    `json:"default_published"`
	CustomPublished     bool    `json:"custom_published"`
	ErrorCode           string  `json:"error_code,omitempty"`
	LastAttemptUnixNano int64   `json:"last_attempt_unix_nano,omitempty"`
	Confirmed           *Result `json:"confirmed,omitempty"`
}
type TXTState struct {
	LastSuccessUnixNano int64 `json:"last_success_unix_nano"`
	LastCheckUnixNano   int64 `json:"last_check_unix_nano"`
	Valid               bool  `json:"valid"`
}
type localState struct {
	Cursor              int64                `json:"cursor"`
	Initialized         bool                 `json:"initialized"`
	RootURL             string               `json:"root_url"`
	RootUpdatedUnixNano int64                `json:"root_updated_unix_nano"`
	TXTPolicyKnown      bool                 `json:"txt_policy_known"`
	TXTPolicyEnabled    bool                 `json:"txt_policy_enabled"`
	TXTPolicyUpdated    int64                `json:"txt_policy_updated"`
	Sites               map[string]LocalSite `json:"sites"`
	PendingReports      map[string]Result    `json:"pending_reports,omitempty"`
	TXTChecks           map[string]TXTState  `json:"txt_checks,omitempty"`
}

type storeMeta struct {
	Cursor              int64  `json:"cursor"`
	Initialized         bool   `json:"initialized"`
	RootURL             string `json:"root_url"`
	RootUpdatedUnixNano int64  `json:"root_updated_unix_nano"`
	TXTPolicyKnown      bool   `json:"txt_policy_known"`
	TXTPolicyEnabled    bool   `json:"txt_policy_enabled"`
	TXTPolicyUpdated    int64  `json:"txt_policy_updated"`
}
type Store struct {
	root  string
	kv    *EmbeddedKV
	lock  *os.File
	mu    sync.RWMutex
	state localState
}

func OpenStore(root string, quota ...int64) (*Store, error) {
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root, "pages.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("pages data root is already in use: %w", err)
	}
	for _, dir := range []string{filepath.Join(root, "sites"), filepath.Join(root, "work")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
			return nil, err
		}
	}
	if err := os.Chmod(filepath.Join(root, "work"), 0700); err != nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return nil, err
	}
	backendQuota := int64(2 * 1024 * 1024 * 1024)
	if len(quota) > 0 {
		backendQuota = quota[0]
	}
	kv, err := StartEmbeddedKV(root, backendQuota)
	if err != nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return nil, err
	}
	s := &Store{root: root, kv: kv, lock: lock, state: localState{Sites: make(map[string]LocalSite), PendingReports: make(map[string]Result), TXTChecks: make(map[string]TXTState)}}
	var meta storeMeta
	ok, err := kv.Get(context.Background(), "/gitea-pages/meta", &meta)
	if err != nil {
		s.Close()
		return nil, err
	}
	if ok {
		s.state.Cursor, s.state.Initialized, s.state.RootURL, s.state.RootUpdatedUnixNano = meta.Cursor, meta.Initialized, meta.RootURL, meta.RootUpdatedUnixNano
		s.state.TXTPolicyKnown, s.state.TXTPolicyEnabled, s.state.TXTPolicyUpdated = meta.TXTPolicyKnown, meta.TXTPolicyEnabled, meta.TXTPolicyUpdated
	}
	if err := kv.ListPrefix(context.Background(), "/gitea-pages/sites/", func(key string, value []byte) error {
		var site LocalSite
		if err := json.Unmarshal(value, &site); err != nil {
			return fmt.Errorf("read site %s: %w", key, err)
		}
		s.state.Sites[strings.TrimPrefix(key, "/gitea-pages/sites/")] = site
		return nil
	}); err != nil {
		s.Close()
		return nil, err
	}
	if err := kv.ListPrefix(context.Background(), "/gitea-pages/reports/", func(key string, value []byte) error {
		var result Result
		if err := json.Unmarshal(value, &result); err != nil {
			return fmt.Errorf("read pending report %s: %w", key, err)
		}
		s.state.PendingReports[strings.TrimPrefix(key, "/gitea-pages/reports/")] = result
		return nil
	}); err != nil {
		s.Close()
		return nil, err
	}
	if err := kv.ListPrefix(context.Background(), "/gitea-pages/txt/", func(key string, value []byte) error {
		var record struct {
			Key   string   `json:"key"`
			State TXTState `json:"state"`
		}
		if err := json.Unmarshal(value, &record); err != nil {
			return fmt.Errorf("read TXT state %s: %w", key, err)
		}
		if record.Key != "" {
			s.state.TXTChecks[record.Key] = record.State
		}
		return nil
	}); err != nil {
		s.Close()
		return nil, err
	}
	// The current pointer is the actual installed content after a crash. If a
	// file switch completed before its etcd write, recover the target digest;
	// if the pointer is damaged, close the route and let deployment retry.
	for key, local := range s.state.Sites {
		changed := false
		if local.Site.Enabled && validDigest(local.Site.TarGzSHA256) && s.InstalledMatches(local.Site.RepoID, local.Site.TarGzSHA256) && local.InstalledDigest != local.Site.TarGzSHA256 {
			local.InstalledDigest = local.Site.TarGzSHA256
			local.DefaultPublished = false
			local.CustomPublished = false
			changed = true
		} else if local.InstalledDigest != "" && !s.InstalledMatches(local.Site.RepoID, local.InstalledDigest) {
			local.InstalledDigest = ""
			local.DefaultPublished = false
			local.CustomPublished = false
			changed = true
		}
		if local.DefaultPublished || local.CustomPublished {
			confirmed := local.Confirmed
			if confirmed == nil || confirmed.RepoID != local.Site.RepoID ||
				confirmed.AppliedPagesUpdatedUnixNano != local.Site.PagesUpdatedUnixNano ||
				confirmed.AppliedRootUpdatedUnixNano != s.state.RootUpdatedUnixNano ||
				!confirmed.ConfigApplied || confirmed.ActualTarGzSHA256 != local.InstalledDigest ||
				confirmed.DefaultPublished != local.DefaultPublished || confirmed.CustomPublished != local.CustomPublished {
				local.DefaultPublished = false
				local.CustomPublished = false
				changed = true
			}
		}
		if changed {
			s.state.Sites[key] = local
			if err := s.saveSiteLocked(local.Site.RepoID, local); err != nil {
				s.Close()
				return nil, err
			}
		}
		_ = s.cleanupOldContent(local.Site.RepoID, local.InstalledDigest)
	}
	_ = s.CleanupUntrackedContent()
	return s, nil
}

func (s *Store) Close() {
	if s.kv != nil {
		s.kv.Close()
		s.kv = nil
	}
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		_ = s.lock.Close()
		s.lock = nil
	}
}

// routingSnapshot copies only the facts needed to build a request-time route
// index or an Nginx configuration. Reports and TXT checks are deliberately
// excluded so route rebuild cost grows only with the site set.
func (s *Store) routingSnapshot() (initialized bool, rootURL string, sites []LocalSite) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sites = make([]LocalSite, 0, len(s.state.Sites))
	for _, site := range s.state.Sites {
		sites = append(sites, site)
	}
	return s.state.Initialized, s.state.RootURL, sites
}

func (s *Store) saveMetaLocked() error {
	return s.saveMeta(storeMeta{
		Cursor:              s.state.Cursor,
		Initialized:         s.state.Initialized,
		RootURL:             s.state.RootURL,
		RootUpdatedUnixNano: s.state.RootUpdatedUnixNano,
		TXTPolicyKnown:      s.state.TXTPolicyKnown,
		TXTPolicyEnabled:    s.state.TXTPolicyEnabled,
		TXTPolicyUpdated:    s.state.TXTPolicyUpdated,
	})
}

func (s *Store) saveMeta(meta storeMeta) error {
	return s.kv.Put(context.Background(), "/gitea-pages/meta", meta)
}

func (s *Store) SetTXTPolicy(enabled bool, updated int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := !s.state.TXTPolicyKnown || s.state.TXTPolicyEnabled != enabled || s.state.TXTPolicyUpdated != updated
	if !changed {
		return false, nil
	}
	meta := storeMeta{
		Cursor:              s.state.Cursor,
		Initialized:         s.state.Initialized,
		RootURL:             s.state.RootURL,
		RootUpdatedUnixNano: s.state.RootUpdatedUnixNano,
		TXTPolicyKnown:      true,
		TXTPolicyEnabled:    enabled,
		TXTPolicyUpdated:    updated,
	}
	if err := s.saveMeta(meta); err != nil {
		return false, err
	}
	s.state.TXTPolicyKnown, s.state.TXTPolicyEnabled, s.state.TXTPolicyUpdated = true, enabled, updated
	return true, nil
}

func (s *Store) TXTPolicy() (enabled bool, updated int64, known bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.TXTPolicyEnabled, s.state.TXTPolicyUpdated, s.state.TXTPolicyKnown
}

func siteKey(repoID int64) string {
	return "/gitea-pages/sites/" + strconv.FormatInt(repoID, 10)
}

func localSiteKey(repoID int64) string {
	return strconv.FormatInt(repoID, 10)
}

func reportKey(repoID int64) string {
	return "/gitea-pages/reports/" + strconv.FormatInt(repoID, 10)
}

func txtKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return "/gitea-pages/txt/" + hex.EncodeToString(h[:])
}

func (s *Store) saveSiteLocked(repoID int64, site LocalSite) error {
	return s.kv.Put(context.Background(), siteKey(repoID), site)
}

func (s *Store) deletePrefixLocked(prefix string) error {
	keys := make([]string, 0)
	if err := s.kv.ListPrefix(context.Background(), prefix, func(key string, _ []byte) error {
		keys = append(keys, key)
		return nil
	}); err != nil {
		return err
	}
	for _, key := range keys {
		if err := s.kv.Delete(context.Background(), key); err != nil {
			return err
		}
	}
	return nil
}

// ReplaceSites commits a completed full listing as one in-memory snapshot. The
// existing deployment facts are carried over for repository IDs that remain
// in the listing; a changed route starts deployment again. Keeping the old
// map until all pages have been fetched lets request handlers continue serving
// the last complete snapshot when a listing or persistence operation fails.
func (s *Store) ReplaceSites(rootURL string, rootTime, cursor int64, sites []Site, txtEnabled bool, txtUpdated int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	previous := s.state.Sites
	next := make(map[string]LocalSite, len(sites))
	for _, site := range sites {
		local := previous[localSiteKey(site.RepoID)]
		if local.Site.RepoID != 0 && routeStateChanged(local.Site, site) {
			local.DefaultPublished = false
			local.CustomPublished = false
			local.ErrorCode = ""
		}
		local.Site = site
		next[localSiteKey(site.RepoID)] = local
	}
	meta := storeMeta{
		Cursor:              cursor,
		Initialized:         true,
		RootURL:             rootURL,
		RootUpdatedUnixNano: rootTime,
		TXTPolicyKnown:      true,
		TXTPolicyEnabled:    txtEnabled,
		TXTPolicyUpdated:    txtUpdated,
	}
	if err := s.persistSiteSetLocked(next, meta); err != nil {
		return err
	}
	s.state.Sites = next
	s.state.Cursor, s.state.Initialized = cursor, true
	s.state.RootURL, s.state.RootUpdatedUnixNano = rootURL, rootTime
	s.state.TXTPolicyKnown, s.state.TXTPolicyEnabled, s.state.TXTPolicyUpdated = true, txtEnabled, txtUpdated
	return nil
}

func (s *Store) persistSiteSetLocked(next map[string]LocalSite, meta storeMeta) error {
	previous := s.state.Sites
	for _, site := range next {
		if err := s.saveSiteLocked(site.Site.RepoID, site); err != nil {
			s.restoreSiteSetLocked(previous)
			return err
		}
	}
	for key, site := range previous {
		if _, ok := next[key]; ok {
			continue
		}
		if err := s.kv.Delete(context.Background(), siteKey(site.Site.RepoID)); err != nil {
			s.restoreSiteSetLocked(previous)
			return err
		}
	}
	if err := s.saveMeta(meta); err != nil {
		s.restoreSiteSetLocked(previous)
		_ = s.saveMeta(storeMeta{
			Cursor:              s.state.Cursor,
			Initialized:         s.state.Initialized,
			RootURL:             s.state.RootURL,
			RootUpdatedUnixNano: s.state.RootUpdatedUnixNano,
			TXTPolicyKnown:      s.state.TXTPolicyKnown,
			TXTPolicyEnabled:    s.state.TXTPolicyEnabled,
			TXTPolicyUpdated:    s.state.TXTPolicyUpdated,
		})
		return err
	}
	return nil
}

func (s *Store) restoreSiteSetLocked(previous map[string]LocalSite) {
	if err := s.deletePrefixLocked("/gitea-pages/sites/"); err != nil {
		return
	}
	for _, site := range previous {
		if err := s.saveSiteLocked(site.Site.RepoID, site); err != nil {
			return
		}
	}
}

func (s *Store) SetRegistration(rootURL string, rootTime int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.state
	previous.Sites = cloneLocalSites(s.state.Sites)
	if s.state.RootURL != "" && (s.state.RootURL != rootURL || s.state.RootUpdatedUnixNano != rootTime) {
		for key, local := range s.state.Sites {
			local.DefaultPublished = false
			local.CustomPublished = false
			s.state.Sites[key] = local
			if err := s.saveSiteLocked(local.Site.RepoID, local); err != nil {
				s.restoreSiteSetLocked(previous.Sites)
				s.state = previous
				return err
			}
		}
	}
	s.state.RootURL, s.state.RootUpdatedUnixNano = rootURL, rootTime
	if err := s.saveMetaLocked(); err != nil {
		s.restoreSiteSetLocked(previous.Sites)
		s.state = previous
		return err
	}
	return nil
}

func (s *Store) Upsert(site Site) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.state.Sites[localSiteKey(site.RepoID)]
	if old.Site.RepoID != 0 && routeStateChanged(old.Site, site) {
		old.DefaultPublished = false
		old.CustomPublished = false
		old.ErrorCode = ""
	}
	old.Site = site
	if err := s.saveSiteLocked(site.RepoID, old); err != nil {
		return err
	}
	s.state.Sites[localSiteKey(site.RepoID)] = old
	return nil
}

func (s *Store) Get(repoID int64) (LocalSite, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.state.Sites[localSiteKey(repoID)]
	return v, ok
}

// ForEachSite visits the current local site facts without creating another
// copy of the complete site map. Returning false stops the scan early.
func (s *Store) ForEachSite(fn func(LocalSite) bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, site := range s.state.Sites {
		if !fn(site) {
			return
		}
	}
}

func (s *Store) SetResultForSite(site Site, digest string, defaultPublished, customPublished bool, errCode string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.state.Sites[localSiteKey(site.RepoID)]
	if !ok {
		return false, nil
	}
	if v.Site.PagesUpdatedUnixNano != site.PagesUpdatedUnixNano || v.Site.TarGzSHA256 != site.TarGzSHA256 || routeStateChanged(v.Site, site) {
		return false, nil
	}
	v.InstalledDigest, v.DefaultPublished, v.CustomPublished, v.ErrorCode = digest, defaultPublished, customPublished, errCode
	v.LastAttemptUnixNano = time.Now().UnixNano()
	if err := s.saveSiteLocked(site.RepoID, v); err != nil {
		return false, err
	}
	s.state.Sites[localSiteKey(site.RepoID)] = v
	return true, nil
}

// SavePendingReport stores the newest result for a repository until Gitea
// confirms it. A single result per repository is enough because Gitea accepts
// a result only when its page timestamp still matches; a newer result replaces
// an older one and will be reported with the current deployment state.
func (s *Store) SavePendingReport(result Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if result.RepoID <= 0 {
		return fmt.Errorf("invalid report repository")
	}
	if err := s.kv.Put(context.Background(), reportKey(result.RepoID), result); err != nil {
		return err
	}
	if s.state.PendingReports == nil {
		s.state.PendingReports = make(map[string]Result)
	}
	s.state.PendingReports[strconv.FormatInt(result.RepoID, 10)] = result
	return nil
}

func (s *Store) PendingReports() []Result {
	s.mu.RLock()
	defer s.mu.RUnlock()
	results := make([]Result, 0, len(s.state.PendingReports))
	for _, result := range s.state.PendingReports {
		results = append(results, result)
	}
	return results
}

func (s *Store) PendingReport(repoID int64) (Result, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result, ok := s.state.PendingReports[strconv.FormatInt(repoID, 10)]
	return result, ok
}

func (s *Store) MarkConfirmedReport(result Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := localSiteKey(result.RepoID)
	local, ok := s.state.Sites[key]
	if !ok || local.Site.PagesUpdatedUnixNano != result.AppliedPagesUpdatedUnixNano {
		return nil
	}
	local.Confirmed = &result
	if err := s.saveSiteLocked(result.RepoID, local); err != nil {
		return err
	}
	s.state.Sites[key] = local
	return nil
}

func (s *Store) ClearPendingReport(repoID int64, expected Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strconv.FormatInt(repoID, 10)
	if current, ok := s.state.PendingReports[key]; !ok || !reflect.DeepEqual(current, expected) {
		return nil
	}
	if err := s.kv.Delete(context.Background(), reportKey(repoID)); err != nil {
		return err
	}
	delete(s.state.PendingReports, key)
	return nil
}

func (s *Store) GetTXTState(key string) (TXTState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.state.TXTChecks[key]
	return state, ok
}

func (s *Store) SetTXTState(key string, state TXTState) error {
	if key == "" {
		return fmt.Errorf("empty TXT state key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record := struct {
		Key   string   `json:"key"`
		State TXTState `json:"state"`
	}{key, state}
	if err := s.kv.Put(context.Background(), txtKey(key), record); err != nil {
		return err
	}
	if s.state.TXTChecks == nil {
		s.state.TXTChecks = make(map[string]TXTState)
	}
	s.state.TXTChecks[key] = state
	return nil
}

func (s *Store) SetCursor(cursor int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta := storeMeta{
		Cursor:              cursor,
		Initialized:         true,
		RootURL:             s.state.RootURL,
		RootUpdatedUnixNano: s.state.RootUpdatedUnixNano,
		TXTPolicyKnown:      s.state.TXTPolicyKnown,
		TXTPolicyEnabled:    s.state.TXTPolicyEnabled,
		TXTPolicyUpdated:    s.state.TXTPolicyUpdated,
	}
	if err := s.saveMeta(meta); err != nil {
		return err
	}
	s.state.Cursor, s.state.Initialized = cursor, true
	return nil
}

func (s *Store) State() (cursor int64, initialized bool, rootURL string, rootTime int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Cursor, s.state.Initialized, s.state.RootURL, s.state.RootUpdatedUnixNano
}

func (s *Store) SiteDir(repoID int64) string {
	return filepath.Join(s.root, "sites", strconv.FormatInt(repoID, 10))
}

// RemoveContent removes the installed files but keeps the repository record.
// Gitea keeps repository tombstones so a later repository with the same name
// cannot inherit the old local deployment state.
func (s *Store) RemoveContent(repoID int64) error {
	dir := s.SiteDir(repoID)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "current" || strings.HasPrefix(entry.Name(), "content-") {
			if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) WorkDir(repoID int64) (string, error) {
	return os.MkdirTemp(filepath.Join(s.root, "work"), fmt.Sprintf("%d-", repoID))
}

func (s *Store) Install(repoID int64, digest, candidate string) error {
	if !validDigest(digest) {
		return fmt.Errorf("invalid digest")
	}
	dir := s.SiteDir(repoID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	final := filepath.Join(dir, "content-"+digest)
	if err := os.RemoveAll(final); err != nil {
		return err
	}
	if err := os.Rename(candidate, final); err != nil {
		return err
	}
	// Nginx may run as a different user and must be able to traverse the
	// published site root. ExtractTarGz already normalizes its child modes.
	if err := os.Chmod(final, 0755); err != nil {
		return err
	}
	current := filepath.Join(dir, "current")
	tmp := filepath.Join(dir, ".current.tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink("content-"+digest, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, current); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// Switching current is the installation boundary. Retaining an old
	// content directory when cleanup cannot finish keeps the new publication
	// usable; startup and the next installation retry this best-effort cleanup.
	_ = s.cleanupOldContent(repoID, digest)
	return nil
}

func (s *Store) cleanupOldContent(repoID int64, digest string) error {
	if !validDigest(digest) {
		return nil
	}
	dir := s.SiteDir(repoID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cleanupErr error
	keep := "content-" + digest
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "content-") || entry.Name() == keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil && cleanupErr == nil {
			cleanupErr = err
		}
	}
	return cleanupErr
}

// CleanupUntrackedContent retries removal of stale content and site directories
// whose repository record is no longer in the completed Gitea listing. It runs
// at startup and after a successful full sync, so a transient filesystem error
// can recover without adding another persistent cleanup queue.
func (s *Store) CleanupUntrackedContent() error {
	s.mu.RLock()
	tracked := make(map[int64]struct{}, len(s.state.Sites))
	trackedSites := make([]LocalSite, 0, len(s.state.Sites))
	for _, site := range s.state.Sites {
		tracked[site.Site.RepoID] = struct{}{}
		trackedSites = append(trackedSites, site)
	}
	s.mu.RUnlock()
	var cleanupErr error
	for _, site := range trackedSites {
		if err := s.cleanupOldContent(site.Site.RepoID, site.InstalledDigest); err != nil && cleanupErr == nil {
			cleanupErr = err
		}
	}

	entries, err := os.ReadDir(filepath.Join(s.root, "sites"))
	if err != nil {
		if os.IsNotExist(err) {
			return cleanupErr
		}
		if cleanupErr == nil {
			cleanupErr = err
		}
		return cleanupErr
	}
	for _, entry := range entries {
		repoID, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil || repoID <= 0 {
			continue
		}
		if _, ok := tracked[repoID]; ok {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.root, "sites", entry.Name())); err != nil && cleanupErr == nil {
			cleanupErr = err
		}
	}
	return cleanupErr
}

func (s *Store) InstalledMatches(repoID int64, digest string) bool {
	if !validDigest(digest) {
		return false
	}
	link, err := os.Readlink(filepath.Join(s.SiteDir(repoID), "current"))
	if err != nil {
		return false
	}
	if link != "content-"+digest {
		return false
	}
	info, err := os.Stat(filepath.Join(s.SiteDir(repoID), "current", ".gitea-pages-probe"))
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	b, err := os.ReadFile(filepath.Join(s.SiteDir(repoID), "current", ".gitea-pages-probe"))
	return err == nil && string(b) == strconv.FormatInt(repoID, 10)+" "+digest+"\n"
}

func validDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func routeStateChanged(old, current Site) bool {
	return old.OwnerName != current.OwnerName || old.RepoName != current.RepoName || old.Deleted != current.Deleted || old.Enabled != current.Enabled || !reflect.DeepEqual(old.CustomDomains, current.CustomDomains)
}

func cloneLocalSites(sites map[string]LocalSite) map[string]LocalSite {
	clone := make(map[string]LocalSite, len(sites))
	for key, site := range sites {
		clone[key] = site
	}
	return clone
}
