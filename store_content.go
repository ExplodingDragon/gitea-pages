// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package page

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

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
	if !isSHA256Digest(digest) {
		return errors.New("invalid digest")
	}
	dir := s.SiteDir(repoID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
	if err := os.Chmod(final, 0o755); err != nil {
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
	if !isSHA256Digest(digest) {
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
	if !isSHA256Digest(digest) {
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

func isSHA256Digest(digest string) bool {
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
	if old.OwnerName != current.OwnerName || old.RepoName != current.RepoName || old.Deleted != current.Deleted || old.Enabled != current.Enabled {
		return true
	}
	if old.CustomDomain == nil || current.CustomDomain == nil {
		return old.CustomDomain != current.CustomDomain
	}
	return *old.CustomDomain != *current.CustomDomain
}
