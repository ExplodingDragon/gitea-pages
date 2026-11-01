// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gitea.dev/page/internal/artifact"
	"gitea.dev/page/internal/gitea"
)

// Content identifies an immutable, validated website directory.
type Content struct {
	Digest string
	Root   string
}

func (s *Store) siteDir(repoID int64) string {
	return filepath.Join(s.root, "sites", strconv.FormatInt(repoID, 10))
}

// RemoveInstalledContent removes the installed files but keeps the repository record.
// Gitea keeps repository tombstones so a later repository with the same name
// cannot inherit the old local deployment state.
func (s *Store) RemoveInstalledContent(repoID int64) error {
	dir := s.siteDir(repoID)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "content-") {
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

func (s *Store) InstallContent(repoID int64, digest, candidate string) (Content, error) {
	if !artifact.ValidDigest(digest) {
		return Content{}, errors.New("invalid digest")
	}
	dir := s.siteDir(repoID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Content{}, err
	}
	final := filepath.Join(dir, "content-"+digest)
	if info, err := os.Lstat(final); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return Content{}, errors.New("installed content path is not a directory")
		}
		if err := os.RemoveAll(candidate); err != nil {
			return Content{}, err
		}
		return Content{Digest: digest, Root: final}, nil
	} else if !os.IsNotExist(err) {
		return Content{}, err
	}
	if err := os.Rename(candidate, final); err != nil {
		return Content{}, err
	}
	if err := os.Chmod(final, 0o755); err != nil {
		return Content{}, err
	}
	return Content{Digest: digest, Root: final}, nil
}

func (s *Store) InstalledContent(repoID int64, digest string) (Content, bool) {
	if !s.hasInstalledContent(repoID, digest) {
		return Content{}, false
	}
	return Content{Digest: digest, Root: filepath.Join(s.siteDir(repoID), "content-"+digest)}, true
}

// RemoveInactiveContent removes installed content unless it is the confirmed
// content currently recorded for the repository.
func (s *Store) RemoveInactiveContent(repoID int64, digest string) error {
	local, ok := s.Site(repoID)
	if ok && local.InstalledDigest == digest {
		return nil
	}
	if !artifact.ValidDigest(digest) {
		return errors.New("invalid digest")
	}
	return os.RemoveAll(filepath.Join(s.siteDir(repoID), "content-"+digest))
}

func (s *Store) RemoveSupersededContent(repoID int64, digest string) error {
	if !artifact.ValidDigest(digest) {
		return nil
	}
	dir := s.siteDir(repoID)
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

// RemoveUntrackedContent retries removal of stale content and site directories
// whose repository record is no longer in the completed Gitea listing. It runs
// at startup and after a successful full sync, so a transient filesystem error
// can recover without adding another persistent cleanup queue.
func (s *Store) RemoveUntrackedContent() error {
	s.mu.RLock()
	tracked := make(map[int64]struct{}, len(s.snapshot.sites))
	trackedSites := make([]SiteState, 0, len(s.snapshot.sites))
	for _, site := range s.snapshot.sites {
		tracked[site.Site.RepoID] = struct{}{}
		trackedSites = append(trackedSites, site)
	}
	s.mu.RUnlock()
	var cleanupErr error
	for _, site := range trackedSites {
		if err := s.RemoveSupersededContent(site.Site.RepoID, site.InstalledDigest); err != nil && cleanupErr == nil {
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

func (s *Store) hasInstalledContent(repoID int64, digest string) bool {
	if !artifact.ValidDigest(digest) {
		return false
	}
	info, err := os.Lstat(filepath.Join(s.siteDir(repoID), "content-"+digest))
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func publicationRouteChanged(old, current gitea.Site) bool {
	if old.OwnerName != current.OwnerName || old.RepoName != current.RepoName || old.Deleted != current.Deleted || old.Enabled != current.Enabled {
		return true
	}
	if old.CustomDomain == nil || current.CustomDomain == nil {
		return old.CustomDomain != current.CustomDomain
	}
	return *old.CustomDomain != *current.CustomDomain
}
