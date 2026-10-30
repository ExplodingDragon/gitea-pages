// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package page

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// LocalSite combines a Gitea site fact with this instance's active result.
type LocalSite struct {
	Site                Site              `json:"site"`
	InstalledDigest     string            `json:"installed_digest"`
	DefaultPublished    bool              `json:"default_published"`
	CustomPublished     bool              `json:"custom_published"`
	LastAttemptUnixNano int64             `json:"last_attempt_unix_nano,omitempty"`
	ConfirmedResult     *DeploymentResult `json:"confirmed_result,omitempty"`
}

// TXTState records the most recent local ownership check for a domain.
type TXTState struct {
	LastSuccessUnixNano int64 `json:"last_success_unix_nano"`
	LastCheckUnixNano   int64 `json:"last_check_unix_nano"`
}

type localState struct {
	Cursor              int64
	Initialized         bool
	RootURL             string
	RootUpdatedUnixNano int64
	ArchiveLimits       ArchiveLimits
	RequireGiteaTXT     bool
	Sites               map[int64]LocalSite
	PendingReports      map[int64]DeploymentResult
	TXTChecks           map[string]TXTState
}

const (
	defaultEtcdQuotaBytes = int64(2 * 1024 * 1024 * 1024)

	storeMetaKey         = "/gitea-pages/meta"
	fullSyncProgressKey  = "/gitea-pages/full-sync-in-progress"
	storedSitesPrefix    = "/gitea-pages/sites/"
	storedReportsPrefix  = "/gitea-pages/reports/"
	storedTXTStatePrefix = "/gitea-pages/txt/"
)

type storeMeta struct {
	Cursor              int64         `json:"cursor"`
	Initialized         bool          `json:"initialized"`
	RootURL             string        `json:"root_url"`
	RootUpdatedUnixNano int64         `json:"root_updated_unix_nano"`
	ArchiveLimits       ArchiveLimits `json:"archive_limits"`
	RequireGiteaTXT     bool          `json:"require_gitea_txt_record"`
}

// Store keeps local synchronization facts in embedded etcd and site files on disk.
type Store struct {
	root  string
	kv    *EmbeddedKV
	lock  *os.File
	mu    sync.RWMutex
	state localState
}

// OpenStore locks and restores one Pages data root.
func OpenStore(root string, quotaBytes int64) (*Store, error) {
	if quotaBytes <= 0 {
		return nil, errors.New("embedded etcd quota must be positive")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root, "pages.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("pages data root is already in use: %w", err)
	}
	s := &Store{
		root: root,
		lock: lock,
		state: localState{
			Sites:          make(map[int64]LocalSite),
			PendingReports: make(map[int64]DeploymentResult),
			TXTChecks:      make(map[string]TXTState),
		},
	}
	opened := false
	defer func() {
		if !opened {
			s.Close()
		}
	}()
	for _, dir := range []string{filepath.Join(root, "sites"), filepath.Join(root, "work")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	if err := os.Chmod(filepath.Join(root, "work"), 0o700); err != nil {
		return nil, err
	}
	kv, err := StartEmbeddedKV(root, quotaBytes)
	if err != nil {
		return nil, err
	}
	s.kv = kv
	var meta storeMeta
	ok, err := kv.Get(context.Background(), storeMetaKey, &meta)
	if err != nil {
		return nil, err
	}
	if ok {
		s.state.Cursor, s.state.Initialized, s.state.RootURL, s.state.RootUpdatedUnixNano = meta.Cursor, meta.Initialized, meta.RootURL, meta.RootUpdatedUnixNano
		s.state.ArchiveLimits, s.state.RequireGiteaTXT = meta.ArchiveLimits, meta.RequireGiteaTXT
	}
	var fullSyncMarkerPresent bool
	fullSyncMarkerPresent, err = kv.Get(context.Background(), fullSyncProgressKey, new(bool))
	if err != nil {
		return nil, err
	}
	if fullSyncMarkerPresent {
		s.state.Cursor = 0
		s.state.Initialized = false
	}
	if !fullSyncMarkerPresent {
		if err := kv.ListPrefix(context.Background(), storedSitesPrefix, func(key string, value []byte) error {
			var site LocalSite
			if err := json.Unmarshal(value, &site); err != nil {
				return fmt.Errorf("read site %s: %w", key, err)
			}
			repoID, err := strconv.ParseInt(strings.TrimPrefix(key, storedSitesPrefix), 10, 64)
			if err != nil || repoID <= 0 || site.Site.RepoID != repoID {
				return fmt.Errorf("read site %s: invalid repository ID", key)
			}
			s.state.Sites[repoID] = site
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if err := kv.ListPrefix(context.Background(), storedReportsPrefix, func(key string, value []byte) error {
		var result DeploymentResult
		if err := json.Unmarshal(value, &result); err != nil {
			return fmt.Errorf("read pending report %s: %w", key, err)
		}
		repoID, err := strconv.ParseInt(strings.TrimPrefix(key, storedReportsPrefix), 10, 64)
		if err != nil || repoID <= 0 || result.RepoID != repoID {
			return fmt.Errorf("read pending report %s: invalid repository ID", key)
		}
		s.state.PendingReports[repoID] = result
		return nil
	}); err != nil {
		return nil, err
	}
	if err := kv.ListPrefix(context.Background(), storedTXTStatePrefix, func(key string, value []byte) error {
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
		return nil, err
	}
	// The current pointer is the actual installed content after a crash. If a
	// file switch completed before its etcd write, recover the target digest;
	// if the pointer is damaged, close the route and let deployment retry.
	for repoID, local := range s.state.Sites {
		changed := false
		if local.Site.Enabled && isSHA256Digest(local.Site.TarGzSHA256) && s.InstalledMatches(local.Site.RepoID, local.Site.TarGzSHA256) && local.InstalledDigest != local.Site.TarGzSHA256 {
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
			confirmed := local.ConfirmedResult
			if confirmed == nil || confirmed.RepoID != local.Site.RepoID ||
				confirmed.AppliedPagesUpdatedUnixNano != local.Site.PagesUpdatedUnixNano ||
				confirmed.AppliedRootUpdatedUnixNano != s.state.RootUpdatedUnixNano ||
				!confirmed.ConfigApplied || confirmed.ActualTarGzSHA256 != local.InstalledDigest ||
				confirmed.DefaultPublished != local.DefaultPublished || confirmed.CustomPublished != local.CustomPublished ||
				local.Site.CustomDomain != nil && (!archiveLimitsKnown(s.state.ArchiveLimits) || confirmed.AppliedRequireGiteaTXTRecord != s.state.RequireGiteaTXT) {
				local.DefaultPublished = false
				local.CustomPublished = false
				changed = true
			}
		}
		if changed {
			s.state.Sites[repoID] = local
			if err := s.saveSiteLocked(local.Site.RepoID, local); err != nil {
				return nil, err
			}
		}
		_ = s.cleanupOldContent(local.Site.RepoID, local.InstalledDigest)
	}
	if !fullSyncMarkerPresent {
		_ = s.CleanupUntrackedContent()
	}
	opened = true
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

func (s *Store) metaLocked() storeMeta {
	return storeMeta{
		Cursor:              s.state.Cursor,
		Initialized:         s.state.Initialized,
		RootURL:             s.state.RootURL,
		RootUpdatedUnixNano: s.state.RootUpdatedUnixNano,
		ArchiveLimits:       s.state.ArchiveLimits,
		RequireGiteaTXT:     s.state.RequireGiteaTXT,
	}
}

func (s *Store) saveMeta(meta storeMeta) error {
	return s.kv.Put(context.Background(), storeMetaKey, meta)
}

func (s *Store) SetSyncConfig(limits ArchiveLimits, requireGiteaTXT bool) (bool, error) {
	if _, err := limits.maxTarBytes(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	known := archiveLimitsKnown(s.state.ArchiveLimits)
	txtChanged := !known || s.state.RequireGiteaTXT != requireGiteaTXT
	if known && s.state.ArchiveLimits == limits && !txtChanged {
		return false, nil
	}
	if txtChanged {
		if err := s.kv.DeletePrefix(context.Background(), storedTXTStatePrefix); err != nil {
			return false, err
		}
	}
	meta := s.metaLocked()
	meta.ArchiveLimits, meta.RequireGiteaTXT = limits, requireGiteaTXT
	if err := s.saveMeta(meta); err != nil {
		return false, err
	}
	s.state.ArchiveLimits, s.state.RequireGiteaTXT = limits, requireGiteaTXT
	if txtChanged {
		s.state.TXTChecks = make(map[string]TXTState)
	}
	return txtChanged, nil
}

func (s *Store) SyncConfig() (limits ArchiveLimits, requireGiteaTXT, known bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.ArchiveLimits, s.state.RequireGiteaTXT, archiveLimitsKnown(s.state.ArchiveLimits)
}

func archiveLimitsKnown(limits ArchiveLimits) bool {
	return limits.MaxTarGzBytes > 0 && limits.MaxSiteBytes > 0 && limits.MaxFileBytes > 0 && limits.MaxEntries > 0
}

func siteKey(repoID int64) string {
	return storedSitesPrefix + strconv.FormatInt(repoID, 10)
}

func reportKey(repoID int64) string {
	return storedReportsPrefix + strconv.FormatInt(repoID, 10)
}

func txtKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return storedTXTStatePrefix + hex.EncodeToString(h[:])
}

func (s *Store) saveSiteLocked(repoID int64, site LocalSite) error {
	return s.kv.Put(context.Background(), siteKey(repoID), site)
}

// ReplaceSites commits a completed full listing as one in-memory snapshot. The
// existing deployment facts are carried over for repository IDs that remain
// in the listing; a changed route starts deployment again. Keeping the old
// map until all pages have been fetched lets request handlers continue serving
// the last complete snapshot when a listing or persistence operation fails.
func (s *Store) ReplaceSites(rootURL string, rootTime, cursor int64, sites []Site, limits ArchiveLimits, requireGiteaTXT bool) error {
	if _, err := limits.maxTarBytes(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	txtChanged := !archiveLimitsKnown(s.state.ArchiveLimits) || s.state.RequireGiteaTXT != requireGiteaTXT
	if txtChanged {
		if err := s.kv.DeletePrefix(context.Background(), storedTXTStatePrefix); err != nil {
			return err
		}
	}

	previous := s.state.Sites
	next := make(map[int64]LocalSite, len(sites))
	for _, site := range sites {
		local := previous[site.RepoID]
		if local.Site.RepoID != 0 && routeStateChanged(local.Site, site) {
			local.DefaultPublished = false
			local.CustomPublished = false
		}
		local.Site = site
		next[site.RepoID] = local
	}
	meta := storeMeta{
		Cursor:              cursor,
		Initialized:         true,
		RootURL:             rootURL,
		RootUpdatedUnixNano: rootTime,
		ArchiveLimits:       limits,
		RequireGiteaTXT:     requireGiteaTXT,
	}
	if err := s.persistSiteSetLocked(next, meta); err != nil {
		return err
	}
	s.state.Sites = next
	s.state.Cursor, s.state.Initialized = cursor, true
	s.state.RootURL, s.state.RootUpdatedUnixNano = rootURL, rootTime
	s.state.ArchiveLimits, s.state.RequireGiteaTXT = limits, requireGiteaTXT
	if txtChanged {
		s.state.TXTChecks = make(map[string]TXTState)
	}
	return nil
}

func (s *Store) persistSiteSetLocked(next map[int64]LocalSite, meta storeMeta) error {
	if err := s.kv.Put(context.Background(), fullSyncProgressKey, true); err != nil {
		return err
	}
	if err := s.kv.DeletePrefix(context.Background(), storedSitesPrefix); err != nil {
		return err
	}
	for _, site := range next {
		if err := s.saveSiteLocked(site.Site.RepoID, site); err != nil {
			return err
		}
	}
	if err := s.saveMeta(meta); err != nil {
		return err
	}
	return s.kv.Delete(context.Background(), fullSyncProgressKey)
}

func (s *Store) SetRegistration(rootURL string, rootTime int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta := s.metaLocked()
	meta.RootURL, meta.RootUpdatedUnixNano = rootURL, rootTime
	if s.state.RootURL != "" && (s.state.RootURL != rootURL || s.state.RootUpdatedUnixNano != rootTime) {
		next := maps.Clone(s.state.Sites)
		for key, local := range next {
			local.DefaultPublished = false
			local.CustomPublished = false
			next[key] = local
		}
		if err := s.persistSiteSetLocked(next, meta); err != nil {
			return err
		}
		s.state.Sites = next
	} else if err := s.saveMeta(meta); err != nil {
		return err
	}
	s.state.RootURL, s.state.RootUpdatedUnixNano = rootURL, rootTime
	return nil
}

func (s *Store) UpsertSite(site Site) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.state.Sites[site.RepoID]
	if old.Site.RepoID != 0 && routeStateChanged(old.Site, site) {
		old.DefaultPublished = false
		old.CustomPublished = false
	}
	old.Site = site
	if err := s.saveSiteLocked(site.RepoID, old); err != nil {
		return err
	}
	s.state.Sites[site.RepoID] = old
	return nil
}

func (s *Store) GetSite(repoID int64) (LocalSite, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.state.Sites[repoID]
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

// SaveDeploymentResult records the result only when it still describes the
// current synchronized site. A stale worker therefore cannot reopen an older
// route after a newer site update has arrived.
func (s *Store) SaveDeploymentResult(site Site, digest string, defaultPublished, customPublished bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.state.Sites[site.RepoID]
	if !ok {
		return false, nil
	}
	if v.Site.PagesUpdatedUnixNano != site.PagesUpdatedUnixNano || v.Site.TarGzSHA256 != site.TarGzSHA256 || routeStateChanged(v.Site, site) {
		return false, nil
	}
	v.InstalledDigest, v.DefaultPublished, v.CustomPublished = digest, defaultPublished, customPublished
	v.LastAttemptUnixNano = time.Now().UnixNano()
	if err := s.saveSiteLocked(site.RepoID, v); err != nil {
		return false, err
	}
	s.state.Sites[site.RepoID] = v
	return true, nil
}

// SavePendingReport stores the newest result for a repository until Gitea
// confirms it. A single result per repository is enough because Gitea accepts
// a result only when its page timestamp still matches; a newer result replaces
// an older one and will be reported with the current deployment state.
func (s *Store) SavePendingReport(result DeploymentResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if result.RepoID <= 0 {
		return errors.New("invalid report repository")
	}
	if err := s.kv.Put(context.Background(), reportKey(result.RepoID), result); err != nil {
		return err
	}
	if s.state.PendingReports == nil {
		s.state.PendingReports = make(map[int64]DeploymentResult)
	}
	s.state.PendingReports[result.RepoID] = result
	return nil
}

func (s *Store) PendingReports() []DeploymentResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	results := make([]DeploymentResult, 0, len(s.state.PendingReports))
	for _, result := range s.state.PendingReports {
		results = append(results, result)
	}
	return results
}

func (s *Store) PendingReport(repoID int64) (DeploymentResult, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result, ok := s.state.PendingReports[repoID]
	return result, ok
}

func (s *Store) MarkConfirmedReport(result DeploymentResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := result.RepoID
	local, ok := s.state.Sites[key]
	if !ok || local.Site.PagesUpdatedUnixNano != result.AppliedPagesUpdatedUnixNano {
		return nil
	}
	local.ConfirmedResult = &result
	if err := s.saveSiteLocked(result.RepoID, local); err != nil {
		return err
	}
	s.state.Sites[key] = local
	return nil
}

func (s *Store) ClearPendingReport(repoID int64, expected DeploymentResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.state.PendingReports[repoID]; !ok || current != expected {
		return nil
	}
	if err := s.kv.Delete(context.Background(), reportKey(repoID)); err != nil {
		return err
	}
	delete(s.state.PendingReports, repoID)
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
		return errors.New("empty TXT state key")
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
	meta := s.metaLocked()
	meta.Cursor, meta.Initialized = cursor, true
	if err := s.saveMeta(meta); err != nil {
		return err
	}
	s.state.Cursor, s.state.Initialized = cursor, true
	return nil
}

func (s *Store) SyncState() (cursor int64, initialized bool, rootURL string, rootTime int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Cursor, s.state.Initialized, s.state.RootURL, s.state.RootUpdatedUnixNano
}
