// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

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

// localSite combines a Gitea site fact with this instance's active result.
type localSite struct {
	Site                site              `json:"site"`
	InstalledDigest     string            `json:"installed_digest"`
	DefaultPublished    bool              `json:"default_published"`
	CustomPublished     bool              `json:"custom_published"`
	LastAttemptUnixNano int64             `json:"last_attempt_unix_nano,omitempty"`
	ConfirmedResult     *deploymentResult `json:"confirmed_result,omitempty"`
}

// txtState records the most recent local ownership check for a domain.
type txtState struct {
	LastSuccessUnixNano int64 `json:"last_success_unix_nano"`
	LastCheckUnixNano   int64 `json:"last_check_unix_nano"`
}

type localState struct {
	cursor              int64
	initialized         bool
	rootURL             string
	rootUpdatedUnixNano int64
	archiveLimits       archiveLimits
	requireGiteaTXT     bool
	sites               map[int64]localSite
	pendingReports      map[int64]deploymentResult
	txtChecks           map[string]txtState
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
	ArchiveLimits       archiveLimits `json:"archive_limits"`
	RequireGiteaTXT     bool          `json:"require_gitea_txt_record"`
}

// store keeps local synchronization facts in embedded etcd and site files on disk.
type store struct {
	root  string
	kv    *embeddedKV
	lock  *os.File
	mu    sync.RWMutex
	state localState
}

// openStore locks and restores one Pages data root.
func openStore(root string, quotaBytes int64) (*store, error) {
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
	s := &store{
		root: root,
		lock: lock,
		state: localState{
			sites:          make(map[int64]localSite),
			pendingReports: make(map[int64]deploymentResult),
			txtChecks:      make(map[string]txtState),
		},
	}
	opened := false
	defer func() {
		if !opened {
			s.close()
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
	kv, err := startEmbeddedKV(root, quotaBytes)
	if err != nil {
		return nil, err
	}
	s.kv = kv
	var meta storeMeta
	ok, err := kv.get(context.Background(), storeMetaKey, &meta)
	if err != nil {
		return nil, err
	}
	if ok {
		s.state.cursor, s.state.initialized, s.state.rootURL, s.state.rootUpdatedUnixNano = meta.Cursor, meta.Initialized, meta.RootURL, meta.RootUpdatedUnixNano
		s.state.archiveLimits, s.state.requireGiteaTXT = meta.ArchiveLimits, meta.RequireGiteaTXT
	}
	var fullSyncMarkerPresent bool
	fullSyncMarkerPresent, err = kv.get(context.Background(), fullSyncProgressKey, new(bool))
	if err != nil {
		return nil, err
	}
	if fullSyncMarkerPresent {
		s.state.cursor = 0
		s.state.initialized = false
	}
	if !fullSyncMarkerPresent {
		if err := kv.listPrefix(context.Background(), storedSitesPrefix, func(key string, value []byte) error {
			var site localSite
			if err := json.Unmarshal(value, &site); err != nil {
				return fmt.Errorf("read site %s: %w", key, err)
			}
			repoID, err := strconv.ParseInt(strings.TrimPrefix(key, storedSitesPrefix), 10, 64)
			if err != nil || repoID <= 0 || site.Site.RepoID != repoID {
				return fmt.Errorf("read site %s: invalid repository ID", key)
			}
			s.state.sites[repoID] = site
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if err := kv.listPrefix(context.Background(), storedReportsPrefix, func(key string, value []byte) error {
		var result deploymentResult
		if err := json.Unmarshal(value, &result); err != nil {
			return fmt.Errorf("read pending report %s: %w", key, err)
		}
		repoID, err := strconv.ParseInt(strings.TrimPrefix(key, storedReportsPrefix), 10, 64)
		if err != nil || repoID <= 0 || result.RepoID != repoID {
			return fmt.Errorf("read pending report %s: invalid repository ID", key)
		}
		s.state.pendingReports[repoID] = result
		return nil
	}); err != nil {
		return nil, err
	}
	if err := kv.listPrefix(context.Background(), storedTXTStatePrefix, func(key string, value []byte) error {
		var record struct {
			Key   string   `json:"key"`
			State txtState `json:"state"`
		}
		if err := json.Unmarshal(value, &record); err != nil {
			return fmt.Errorf("read TXT state %s: %w", key, err)
		}
		if record.Key != "" {
			s.state.txtChecks[record.Key] = record.State
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for repoID, local := range s.state.sites {
		changed := false
		if local.InstalledDigest != "" && !s.installedMatches(local.Site.RepoID, local.InstalledDigest) {
			local.InstalledDigest = ""
			local.DefaultPublished = false
			local.CustomPublished = false
			changed = true
		}
		if local.DefaultPublished || local.CustomPublished {
			confirmed := local.ConfirmedResult
			if confirmed == nil || confirmed.RepoID != local.Site.RepoID ||
				confirmed.AppliedPagesUpdatedUnixNano != local.Site.PagesUpdatedUnixNano ||
				confirmed.AppliedRootUpdatedUnixNano != s.state.rootUpdatedUnixNano ||
				!confirmed.ConfigApplied || confirmed.ActualTarGzSHA256 != local.InstalledDigest ||
				confirmed.DefaultPublished != local.DefaultPublished || confirmed.CustomPublished != local.CustomPublished ||
				local.Site.CustomDomain != nil && (!archiveLimitsKnown(s.state.archiveLimits) || confirmed.AppliedRequireGiteaTXTRecord != s.state.requireGiteaTXT) {
				local.DefaultPublished = false
				local.CustomPublished = false
				changed = true
			}
		}
		if changed {
			s.state.sites[repoID] = local
			if err := s.saveSiteLocked(local.Site.RepoID, local); err != nil {
				return nil, err
			}
		}
		_ = s.cleanupOldContent(local.Site.RepoID, local.InstalledDigest)
	}
	if !fullSyncMarkerPresent {
		_ = s.cleanupUntrackedContent()
	}
	opened = true
	return s, nil
}

func (s *store) close() {
	if s.kv != nil {
		s.kv.close()
		s.kv = nil
	}
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		_ = s.lock.Close()
		s.lock = nil
	}
}

func (s *store) metaLocked() storeMeta {
	return storeMeta{
		Cursor:              s.state.cursor,
		Initialized:         s.state.initialized,
		RootURL:             s.state.rootURL,
		RootUpdatedUnixNano: s.state.rootUpdatedUnixNano,
		ArchiveLimits:       s.state.archiveLimits,
		RequireGiteaTXT:     s.state.requireGiteaTXT,
	}
}

func (s *store) saveMeta(meta storeMeta) error {
	return s.kv.put(context.Background(), storeMetaKey, meta)
}

func (s *store) setSyncConfig(limits archiveLimits, requireGiteaTXT bool) (bool, error) {
	if _, err := limits.maxTarBytes(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	known := archiveLimitsKnown(s.state.archiveLimits)
	txtChanged := !known || s.state.requireGiteaTXT != requireGiteaTXT
	if known && s.state.archiveLimits == limits && !txtChanged {
		return false, nil
	}
	if txtChanged {
		if err := s.kv.deletePrefix(context.Background(), storedTXTStatePrefix); err != nil {
			return false, err
		}
	}
	meta := s.metaLocked()
	meta.ArchiveLimits, meta.RequireGiteaTXT = limits, requireGiteaTXT
	if err := s.saveMeta(meta); err != nil {
		return false, err
	}
	s.state.archiveLimits, s.state.requireGiteaTXT = limits, requireGiteaTXT
	if txtChanged {
		s.state.txtChecks = make(map[string]txtState)
	}
	return txtChanged, nil
}

func (s *store) syncConfig() (limits archiveLimits, requireGiteaTXT, known bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.archiveLimits, s.state.requireGiteaTXT, archiveLimitsKnown(s.state.archiveLimits)
}

func archiveLimitsKnown(limits archiveLimits) bool {
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

func (s *store) saveSiteLocked(repoID int64, site localSite) error {
	return s.kv.put(context.Background(), siteKey(repoID), site)
}

// replaceSites commits a completed full listing as one in-memory snapshot. The
// existing deployment facts are carried over for repository IDs that remain
// in the listing; a changed route starts deployment again. Keeping the old
// map until all pages have been fetched lets request handlers continue serving
// the last complete snapshot when a listing or persistence operation fails.
func (s *store) replaceSites(rootURL string, rootTime, cursor int64, sites []site, limits archiveLimits, requireGiteaTXT bool) error {
	if _, err := limits.maxTarBytes(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	txtChanged := !archiveLimitsKnown(s.state.archiveLimits) || s.state.requireGiteaTXT != requireGiteaTXT
	if txtChanged {
		if err := s.kv.deletePrefix(context.Background(), storedTXTStatePrefix); err != nil {
			return err
		}
	}

	previous := s.state.sites
	next := make(map[int64]localSite, len(sites))
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
	s.state.sites = next
	s.state.cursor, s.state.initialized = cursor, true
	s.state.rootURL, s.state.rootUpdatedUnixNano = rootURL, rootTime
	s.state.archiveLimits, s.state.requireGiteaTXT = limits, requireGiteaTXT
	if txtChanged {
		s.state.txtChecks = make(map[string]txtState)
	}
	return nil
}

func (s *store) persistSiteSetLocked(next map[int64]localSite, meta storeMeta) error {
	if err := s.kv.put(context.Background(), fullSyncProgressKey, true); err != nil {
		return err
	}
	if err := s.kv.deletePrefix(context.Background(), storedSitesPrefix); err != nil {
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
	return s.kv.delete(context.Background(), fullSyncProgressKey)
}

func (s *store) setRegistration(rootURL string, rootTime int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta := s.metaLocked()
	meta.RootURL, meta.RootUpdatedUnixNano = rootURL, rootTime
	if s.state.rootURL != "" && (s.state.rootURL != rootURL || s.state.rootUpdatedUnixNano != rootTime) {
		next := maps.Clone(s.state.sites)
		for key, local := range next {
			local.DefaultPublished = false
			local.CustomPublished = false
			next[key] = local
		}
		if err := s.persistSiteSetLocked(next, meta); err != nil {
			return err
		}
		s.state.sites = next
	} else if err := s.saveMeta(meta); err != nil {
		return err
	}
	s.state.rootURL, s.state.rootUpdatedUnixNano = rootURL, rootTime
	return nil
}

func (s *store) upsertSite(site site) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.state.sites[site.RepoID]
	if old.Site.RepoID != 0 && routeStateChanged(old.Site, site) {
		old.DefaultPublished = false
		old.CustomPublished = false
	}
	old.Site = site
	if err := s.saveSiteLocked(site.RepoID, old); err != nil {
		return err
	}
	s.state.sites[site.RepoID] = old
	return nil
}

func (s *store) getSite(repoID int64) (localSite, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.state.sites[repoID]
	return v, ok
}

// forEachSite visits the current local site facts without creating another
// copy of the complete site map. Returning false stops the scan early.
func (s *store) forEachSite(fn func(localSite) bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, site := range s.state.sites {
		if !fn(site) {
			return
		}
	}
}

// saveDeploymentResult records the result only when it still describes the
// current synchronized site. A stale worker therefore cannot reopen an older
// route after a newer site update has arrived.
func (s *store) saveDeploymentResult(site site, digest string, defaultPublished, customPublished bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.state.sites[site.RepoID]
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
	s.state.sites[site.RepoID] = v
	return true, nil
}

// savePendingReport stores the newest result for a repository until Gitea
// confirms it. A single result per repository is enough because Gitea accepts
// a result only when its page timestamp still matches; a newer result replaces
// an older one and will be reported with the current deployment state.
func (s *store) savePendingReport(result deploymentResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if result.RepoID <= 0 {
		return errors.New("invalid report repository")
	}
	if err := s.kv.put(context.Background(), reportKey(result.RepoID), result); err != nil {
		return err
	}
	if s.state.pendingReports == nil {
		s.state.pendingReports = make(map[int64]deploymentResult)
	}
	s.state.pendingReports[result.RepoID] = result
	return nil
}

func (s *store) pendingReports() []deploymentResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	results := make([]deploymentResult, 0, len(s.state.pendingReports))
	for _, result := range s.state.pendingReports {
		results = append(results, result)
	}
	return results
}

func (s *store) pendingReport(repoID int64) (deploymentResult, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result, ok := s.state.pendingReports[repoID]
	return result, ok
}

func (s *store) markConfirmedReport(result deploymentResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := result.RepoID
	local, ok := s.state.sites[key]
	if !ok || local.Site.PagesUpdatedUnixNano != result.AppliedPagesUpdatedUnixNano {
		return nil
	}
	local.ConfirmedResult = &result
	if err := s.saveSiteLocked(result.RepoID, local); err != nil {
		return err
	}
	s.state.sites[key] = local
	return nil
}

func (s *store) clearPendingReport(repoID int64, expected deploymentResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.state.pendingReports[repoID]; !ok || current != expected {
		return nil
	}
	if err := s.kv.delete(context.Background(), reportKey(repoID)); err != nil {
		return err
	}
	delete(s.state.pendingReports, repoID)
	return nil
}

func (s *store) getTXTState(key string) (txtState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.state.txtChecks[key]
	return state, ok
}

func (s *store) setTXTState(key string, state txtState) error {
	if key == "" {
		return errors.New("empty TXT state key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record := struct {
		Key   string   `json:"key"`
		State txtState `json:"state"`
	}{key, state}
	if err := s.kv.put(context.Background(), txtKey(key), record); err != nil {
		return err
	}
	if s.state.txtChecks == nil {
		s.state.txtChecks = make(map[string]txtState)
	}
	s.state.txtChecks[key] = state
	return nil
}

func (s *store) setCursor(cursor int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta := s.metaLocked()
	meta.Cursor, meta.Initialized = cursor, true
	if err := s.saveMeta(meta); err != nil {
		return err
	}
	s.state.cursor, s.state.initialized = cursor, true
	return nil
}

func (s *store) syncState() (cursor int64, initialized bool, rootURL string, rootTime int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.cursor, s.state.initialized, s.state.rootURL, s.state.rootUpdatedUnixNano
}
