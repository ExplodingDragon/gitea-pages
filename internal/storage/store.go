// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package storage

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

	"gitea.dev/page/internal/artifact"
	"gitea.dev/page/internal/gitea"
)

// SiteState combines a Gitea site fact with this instance's active result.
type SiteState struct {
	Site                gitea.Site              `json:"site"`
	InstalledDigest     string                  `json:"installed_digest"`
	DefaultPublished    bool                    `json:"default_published"`
	CustomPublished     bool                    `json:"custom_published"`
	LastAttemptUnixNano int64                   `json:"last_attempt_unix_nano,omitempty"`
	ConfirmedResult     *gitea.DeploymentResult `json:"confirmed_result,omitempty"`
}

// TXTState records the most recent local ownership check for a domain.
type TXTState struct {
	LastSuccessUnixNano int64 `json:"last_success_unix_nano"`
	LastCheckUnixNano   int64 `json:"last_check_unix_nano"`
}

// SyncState is the synchronization position and instance-wide configuration
// confirmed by Gitea.
type SyncState struct {
	Cursor              int64           `json:"cursor"`
	Initialized         bool            `json:"initialized"`
	RootURL             string          `json:"root_url"`
	RootUpdatedUnixNano int64           `json:"root_updated_unix_nano"`
	ArchiveLimits       artifact.Limits `json:"archive_limits"`
	RequireGiteaTXT     bool            `json:"require_gitea_txt_record"`
}

type storeSnapshot struct {
	sync           SyncState
	sites          map[int64]SiteState
	pendingReports map[int64]gitea.DeploymentResult
	txtChecks      map[string]TXTState
}

const (
	storeMetaKey         = "/gitea-pages/meta"
	fullSyncProgressKey  = "/gitea-pages/full-sync-in-progress"
	storedSitesPrefix    = "/gitea-pages/sites/"
	storedReportsPrefix  = "/gitea-pages/reports/"
	storedTXTStatePrefix = "/gitea-pages/txt/"
)

// Store keeps local synchronization facts in embedded etcd and site files on disk.
type Store struct {
	root     string
	records  *State
	lock     *os.File
	mu       sync.RWMutex
	snapshot storeSnapshot
}

// Open locks and restores one Pages data root.
func Open(root string, quotaBytes int64) (*Store, error) {
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
		snapshot: storeSnapshot{
			sites:          make(map[int64]SiteState),
			pendingReports: make(map[int64]gitea.DeploymentResult),
			txtChecks:      make(map[string]TXTState),
		},
	}
	opened := false
	defer func() {
		if !opened {
			s.Close()
		}
	}()
	if err := os.MkdirAll(filepath.Join(root, "sites"), 0o755); err != nil {
		return nil, err
	}
	workRoot := filepath.Join(root, "work")
	if err := os.RemoveAll(workRoot); err != nil {
		return nil, fmt.Errorf("clean deployment work directory: %w", err)
	}
	if err := os.Mkdir(workRoot, 0o700); err != nil {
		return nil, err
	}
	kv, err := openState(root, quotaBytes)
	if err != nil {
		return nil, err
	}
	s.records = kv
	ctx := context.Background()
	fullSyncMarkerPresent, err := s.restoreSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.reconcileInstalledContent(ctx, fullSyncMarkerPresent); err != nil {
		return nil, err
	}
	opened = true
	return s, nil
}

func (s *Store) restoreSnapshot(ctx context.Context) (bool, error) {
	var meta SyncState
	ok, err := s.records.Get(ctx, storeMetaKey, &meta)
	if err != nil {
		return false, err
	}
	if ok {
		s.snapshot.sync = meta
	}
	var fullSyncMarkerPresent bool
	fullSyncMarkerPresent, err = s.records.Get(ctx, fullSyncProgressKey, new(bool))
	if err != nil {
		return false, err
	}
	if fullSyncMarkerPresent {
		s.snapshot.sync.Cursor = 0
		s.snapshot.sync.Initialized = false
	}
	if !fullSyncMarkerPresent {
		if err := s.records.Range(ctx, storedSitesPrefix, func(key string, value []byte) error {
			repoID, site, err := decodeSiteRecord(key, value)
			if err != nil {
				return err
			}
			s.snapshot.sites[repoID] = site
			return nil
		}); err != nil {
			return false, err
		}
	}
	if err := s.records.Range(ctx, storedReportsPrefix, func(key string, value []byte) error {
		var result gitea.DeploymentResult
		if err := json.Unmarshal(value, &result); err != nil {
			return fmt.Errorf("read pending report %s: %w", key, err)
		}
		repoID, err := strconv.ParseInt(strings.TrimPrefix(key, storedReportsPrefix), 10, 64)
		if err != nil || repoID <= 0 || result.RepoID != repoID {
			return fmt.Errorf("read pending report %s: invalid repository ID", key)
		}
		s.snapshot.pendingReports[repoID] = result
		return nil
	}); err != nil {
		return false, err
	}
	if err := s.records.Range(ctx, storedTXTStatePrefix, func(key string, value []byte) error {
		var record struct {
			Key   string   `json:"key"`
			State TXTState `json:"state"`
		}
		if err := json.Unmarshal(value, &record); err != nil {
			return fmt.Errorf("read TXT state %s: %w", key, err)
		}
		if record.Key != "" {
			s.snapshot.txtChecks[record.Key] = record.State
		}
		return nil
	}); err != nil {
		return false, err
	}
	return fullSyncMarkerPresent, nil
}

func (s *Store) reconcileInstalledContent(ctx context.Context, fullSyncMarkerPresent bool) error {
	for repoID, local := range s.snapshot.sites {
		changed := false
		if local.InstalledDigest != "" && !s.hasInstalledContent(local.Site.RepoID, local.InstalledDigest) {
			local.InstalledDigest = ""
			local.DefaultPublished = false
			local.CustomPublished = false
			changed = true
		}
		if local.DefaultPublished || local.CustomPublished {
			confirmed := local.ConfirmedResult
			if confirmed == nil || confirmed.RepoID != local.Site.RepoID ||
				confirmed.AppliedPagesUpdatedUnixNano != local.Site.PagesUpdatedUnixNano ||
				confirmed.AppliedRootUpdatedUnixNano != s.snapshot.sync.RootUpdatedUnixNano ||
				!confirmed.ConfigApplied || confirmed.ActualTarGzSHA256 != local.InstalledDigest ||
				confirmed.DefaultPublished != local.DefaultPublished || confirmed.CustomPublished != local.CustomPublished ||
				local.Site.CustomDomain != nil && (!archiveLimitsKnown(s.snapshot.sync.ArchiveLimits) || confirmed.AppliedRequireGiteaTXTRecord != s.snapshot.sync.RequireGiteaTXT) {
				local.DefaultPublished = false
				local.CustomPublished = false
				changed = true
			}
		}
		if changed {
			s.snapshot.sites[repoID] = local
			if err := s.saveSiteLocked(ctx, local.Site.RepoID, local); err != nil {
				return err
			}
		}
		_ = s.RemoveSupersededContent(local.Site.RepoID, local.InstalledDigest)
	}
	if !fullSyncMarkerPresent {
		_ = s.RemoveUntrackedContent()
	}
	return nil
}

// State returns the durable state owned by the Store. Its lifetime ends when
// the Store is closed.
func (s *Store) State() *State { return s.records }

// Errors reports a terminal embedded state failure.
func (s *Store) Errors() <-chan error { return s.records.errors() }

func (s *Store) Close() {
	if s.records != nil {
		s.records.close()
	}
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		_ = s.lock.Close()
		s.lock = nil
	}
}

func (s *Store) syncStateLocked() SyncState {
	return s.snapshot.sync
}

func (s *Store) saveSyncState(ctx context.Context, state SyncState) error {
	return s.records.Put(ctx, storeMetaKey, state)
}

func (s *Store) SetSyncConfig(ctx context.Context, limits artifact.Limits, requireGiteaTXT bool) (bool, error) {
	if err := limits.Validate(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	known := archiveLimitsKnown(s.snapshot.sync.ArchiveLimits)
	txtChanged := !known || s.snapshot.sync.RequireGiteaTXT != requireGiteaTXT
	if known && s.snapshot.sync.ArchiveLimits == limits && !txtChanged {
		return false, nil
	}
	if txtChanged {
		if err := s.records.deletePrefix(ctx, storedTXTStatePrefix); err != nil {
			return false, err
		}
	}
	state := s.syncStateLocked()
	state.ArchiveLimits, state.RequireGiteaTXT = limits, requireGiteaTXT
	if err := s.saveSyncState(ctx, state); err != nil {
		return false, err
	}
	s.snapshot.sync.ArchiveLimits, s.snapshot.sync.RequireGiteaTXT = limits, requireGiteaTXT
	if txtChanged {
		s.snapshot.txtChecks = make(map[string]TXTState)
	}
	return txtChanged, nil
}

func archiveLimitsKnown(limits artifact.Limits) bool {
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

func (s *Store) saveSiteLocked(ctx context.Context, repoID int64, site SiteState) error {
	return s.records.Put(ctx, siteKey(repoID), site)
}

// ReplaceSites commits a completed full listing as one in-memory snapshot. The
// existing deployment facts are carried over for repository IDs that remain
// in the listing; a changed route starts deployment again. Keeping the old
// map until all pages have been fetched lets request handlers continue serving
// the last complete snapshot when a listing or persistence operation fails.
func (s *Store) ReplaceSites(ctx context.Context, rootURL string, rootTime, cursor int64, sites map[int64]gitea.Site, limits artifact.Limits, requireGiteaTXT bool) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	txtChanged := !archiveLimitsKnown(s.snapshot.sync.ArchiveLimits) || s.snapshot.sync.RequireGiteaTXT != requireGiteaTXT
	if txtChanged {
		if err := s.records.deletePrefix(ctx, storedTXTStatePrefix); err != nil {
			return err
		}
	}

	previous := s.snapshot.sites
	next := make(map[int64]SiteState, len(sites))
	for _, site := range sites {
		local := previous[site.RepoID]
		if local.Site.RepoID != 0 && publicationRouteChanged(local.Site, site) {
			local.DefaultPublished = false
			local.CustomPublished = false
		}
		local.Site = site
		next[site.RepoID] = local
	}
	state := SyncState{
		Cursor:              cursor,
		Initialized:         true,
		RootURL:             rootURL,
		RootUpdatedUnixNano: rootTime,
		ArchiveLimits:       limits,
		RequireGiteaTXT:     requireGiteaTXT,
	}
	if err := s.persistSiteSetLocked(ctx, next, state); err != nil {
		return err
	}
	s.snapshot.sites = next
	s.snapshot.sync = state
	if txtChanged {
		s.snapshot.txtChecks = make(map[string]TXTState)
	}
	return nil
}

func (s *Store) persistSiteSetLocked(ctx context.Context, next map[int64]SiteState, state SyncState) error {
	if err := s.records.Put(ctx, fullSyncProgressKey, true); err != nil {
		return err
	}
	if err := s.records.deletePrefix(ctx, storedSitesPrefix); err != nil {
		return err
	}
	for _, site := range next {
		if err := s.saveSiteLocked(ctx, site.Site.RepoID, site); err != nil {
			return err
		}
	}
	if err := s.saveSyncState(ctx, state); err != nil {
		return err
	}
	return s.records.Delete(ctx, fullSyncProgressKey)
}

func (s *Store) SetRegistration(ctx context.Context, rootURL string, rootTime int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.syncStateLocked()
	state.RootURL, state.RootUpdatedUnixNano = rootURL, rootTime
	if s.snapshot.sync.RootURL != "" && (s.snapshot.sync.RootURL != rootURL || s.snapshot.sync.RootUpdatedUnixNano != rootTime) {
		next := maps.Clone(s.snapshot.sites)
		for key, local := range next {
			local.DefaultPublished = false
			local.CustomPublished = false
			next[key] = local
		}
		if err := s.persistSiteSetLocked(ctx, next, state); err != nil {
			return err
		}
		s.snapshot.sites = next
	} else if err := s.saveSyncState(ctx, state); err != nil {
		return err
	}
	s.snapshot.sync.RootURL, s.snapshot.sync.RootUpdatedUnixNano = rootURL, rootTime
	return nil
}

func (s *Store) UpsertSite(ctx context.Context, site gitea.Site) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.snapshot.sites[site.RepoID]
	if old.Site.RepoID != 0 && publicationRouteChanged(old.Site, site) {
		old.DefaultPublished = false
		old.CustomPublished = false
	}
	old.Site = site
	if err := s.saveSiteLocked(ctx, site.RepoID, old); err != nil {
		return err
	}
	s.snapshot.sites[site.RepoID] = old
	return nil
}

func (s *Store) Site(repoID int64) (SiteState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.snapshot.sites[repoID]
	return v, ok
}

// RangeSites visits the current local site facts without creating another
// copy of the complete site map. Returning false stops the scan early.
func (s *Store) RangeSites(fn func(SiteState) bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, site := range s.snapshot.sites {
		if !fn(site) {
			return
		}
	}
}

// ScanSites reads persisted site records in fixed-size etcd pages without
// holding the in-memory snapshot lock while the caller waits for queue space.
func (s *Store) ScanSites(ctx context.Context, fn func(SiteState, bool) error) error {
	return s.records.Range(ctx, storedSitesPrefix, func(key string, value []byte) error {
		_, site, err := decodeSiteRecord(key, value)
		if err != nil {
			return err
		}
		_, pendingReport := s.PendingReport(site.Site.RepoID)
		return fn(site, pendingReport)
	})
}

func decodeSiteRecord(key string, value []byte) (int64, SiteState, error) {
	var site SiteState
	if err := json.Unmarshal(value, &site); err != nil {
		return 0, SiteState{}, fmt.Errorf("read site %s: %w", key, err)
	}
	repoID, err := strconv.ParseInt(strings.TrimPrefix(key, storedSitesPrefix), 10, 64)
	if err != nil || repoID <= 0 || site.Site.RepoID != repoID {
		return 0, SiteState{}, fmt.Errorf("read site %s: invalid repository ID", key)
	}
	return repoID, site, nil
}

// SaveDeploymentResult records the result only when it still describes the
// current synchronized site. A stale worker therefore cannot reopen an older
// route after a newer site update has arrived.
func (s *Store) SaveDeploymentResult(ctx context.Context, site gitea.Site, digest string, defaultPublished, customPublished bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.snapshot.sites[site.RepoID]
	if !ok {
		return false, nil
	}
	if v.Site.PagesUpdatedUnixNano != site.PagesUpdatedUnixNano || v.Site.TarGzSHA256 != site.TarGzSHA256 || publicationRouteChanged(v.Site, site) {
		return false, nil
	}
	v.InstalledDigest, v.DefaultPublished, v.CustomPublished = digest, defaultPublished, customPublished
	v.LastAttemptUnixNano = time.Now().UnixNano()
	if err := s.saveSiteLocked(ctx, site.RepoID, v); err != nil {
		return false, err
	}
	s.snapshot.sites[site.RepoID] = v
	return true, nil
}

// SavePendingReport stores the newest result for a repository until Gitea
// confirms it. A single result per repository is enough because Gitea accepts
// a result only when its page timestamp still matches; a newer result replaces
// an older one and will be reported with the current deployment state.
func (s *Store) SavePendingReport(ctx context.Context, result gitea.DeploymentResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if result.RepoID <= 0 {
		return errors.New("invalid report repository")
	}
	if err := s.records.Put(ctx, reportKey(result.RepoID), result); err != nil {
		return err
	}
	if s.snapshot.pendingReports == nil {
		s.snapshot.pendingReports = make(map[int64]gitea.DeploymentResult)
	}
	s.snapshot.pendingReports[result.RepoID] = result
	return nil
}

func (s *Store) PendingReports() []gitea.DeploymentResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	results := make([]gitea.DeploymentResult, 0, len(s.snapshot.pendingReports))
	for _, result := range s.snapshot.pendingReports {
		results = append(results, result)
	}
	return results
}

func (s *Store) PendingReport(repoID int64) (gitea.DeploymentResult, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result, ok := s.snapshot.pendingReports[repoID]
	return result, ok
}

func (s *Store) MarkConfirmedReport(ctx context.Context, result gitea.DeploymentResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := result.RepoID
	local, ok := s.snapshot.sites[key]
	if !ok || local.Site.PagesUpdatedUnixNano != result.AppliedPagesUpdatedUnixNano {
		return nil
	}
	local.ConfirmedResult = &result
	if err := s.saveSiteLocked(ctx, result.RepoID, local); err != nil {
		return err
	}
	s.snapshot.sites[key] = local
	return nil
}

func (s *Store) ClearPendingReport(ctx context.Context, repoID int64, expected gitea.DeploymentResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.snapshot.pendingReports[repoID]; !ok || current != expected {
		return nil
	}
	if err := s.records.Delete(ctx, reportKey(repoID)); err != nil {
		return err
	}
	delete(s.snapshot.pendingReports, repoID)
	return nil
}

func (s *Store) TXTState(key string) (TXTState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.snapshot.txtChecks[key]
	return state, ok
}

func (s *Store) SetTXTState(ctx context.Context, key string, state TXTState) error {
	if key == "" {
		return errors.New("empty TXT state key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record := struct {
		Key   string   `json:"key"`
		State TXTState `json:"state"`
	}{key, state}
	if err := s.records.Put(ctx, txtKey(key), record); err != nil {
		return err
	}
	if s.snapshot.txtChecks == nil {
		s.snapshot.txtChecks = make(map[string]TXTState)
	}
	s.snapshot.txtChecks[key] = state
	return nil
}

func (s *Store) SetSyncCursor(ctx context.Context, cursor int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.syncStateLocked()
	state.Cursor, state.Initialized = cursor, true
	if err := s.saveSyncState(ctx, state); err != nil {
		return err
	}
	s.snapshot.sync.Cursor, s.snapshot.sync.Initialized = cursor, true
	return nil
}

func (s *Store) SyncState() SyncState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot.sync
}
