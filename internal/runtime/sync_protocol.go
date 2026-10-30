// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"errors"
)

func (s *syncer) syncWithStateRetry(ctx context.Context) error {
	for attempt := 0; attempt < 2; attempt++ {
		err := s.syncOnce(ctx)
		if err == nil {
			return nil
		}
		var apiErr *apiError
		if !errors.As(err, &apiErr) || apiErr.Code != apiErrorStateChanged {
			return err
		}
	}
	return errors.New("state changed repeatedly during synchronization")
}

func (s *syncer) syncOnce(ctx context.Context) error {
	cursor, initialized, rootURL, rootTime := s.store.syncState()
	if !initialized {
		return s.syncAllSites(ctx)
	}
	resp, err := s.client.listSites(ctx, false, cursor, 0, 0, 0)
	if err != nil {
		return err
	}
	if resp.RootURL != rootURL || resp.RootUpdatedUnixNano != rootTime {
		return s.syncAllSites(ctx)
	}
	firstConfig := resp
	txtRequirementChanged, err := s.store.setSyncConfig(resp.ArchiveLimits, resp.RequireGiteaTXTRecord)
	if err != nil {
		return err
	}
	s.requireGiteaTXT.Store(resp.RequireGiteaTXTRecord)
	if txtRequirementChanged {
		s.txtRequirementChanged.Store(true)
	}
	until := resp.Until
	if err := s.storeSiteUpdates(resp.Sites); err != nil {
		return err
	}
	at, ar := int64(0), int64(0)
	for resp.NextCursor != nil {
		if until == 0 {
			until = resp.Until
		}
		at, ar = resp.NextCursor.PagesUpdatedUnixNano, resp.NextCursor.RepoID
		resp, err = s.client.listSites(ctx, false, cursor, until, at, ar)
		if err != nil {
			return err
		}
		if resp.RootURL != rootURL || resp.RootUpdatedUnixNano != rootTime {
			return s.syncAllSites(ctx)
		}
		if resp.ArchiveLimits != firstConfig.ArchiveLimits || resp.RequireGiteaTXTRecord != firstConfig.RequireGiteaTXTRecord {
			return &apiError{Code: apiErrorStateChanged}
		}
		if err := s.storeSiteUpdates(resp.Sites); err != nil {
			return err
		}
	}
	return s.store.setCursor(until)
}

func (s *syncer) syncAllSites(ctx context.Context) error {
	var until, at, ar int64
	var rootURL string
	var rootTime int64
	var limits archiveLimits
	var requireGiteaTXT bool
	configKnown := false
	sites := make(map[int64]site)
	for {
		resp, err := s.client.listSites(ctx, true, 0, until, at, ar)
		if err != nil {
			return err
		}
		if rootURL == "" {
			rootURL, rootTime = resp.RootURL, resp.RootUpdatedUnixNano
		} else if resp.RootURL != rootURL || resp.RootUpdatedUnixNano != rootTime {
			return &apiError{Code: apiErrorStateChanged}
		}
		if _, err := resp.ArchiveLimits.maxTarBytes(); err != nil {
			return err
		}
		if !configKnown {
			limits, requireGiteaTXT, configKnown = resp.ArchiveLimits, resp.RequireGiteaTXTRecord, true
		} else if resp.ArchiveLimits != limits || resp.RequireGiteaTXTRecord != requireGiteaTXT {
			return &apiError{Code: apiErrorStateChanged}
		}
		if until == 0 {
			until = resp.Until
		}
		for _, site := range resp.Sites {
			sites[site.RepoID] = site
		}
		if resp.NextCursor == nil {
			break
		}
		at, ar = resp.NextCursor.PagesUpdatedUnixNano, resp.NextCursor.RepoID
	}
	// The full listing has a fixed upper bound. One incremental pass closes
	// the gap created while the listing was being paged.
	if until > 0 {
		initialUntil := until
		catchUntil, at, ar := int64(0), int64(0), int64(0)
		for {
			updates, err := s.client.listSites(ctx, false, initialUntil, catchUntil, at, ar)
			if err != nil {
				return err
			}
			if updates.RootURL != rootURL || updates.RootUpdatedUnixNano != rootTime {
				return &apiError{Code: apiErrorStateChanged}
			}
			if updates.ArchiveLimits != limits || updates.RequireGiteaTXTRecord != requireGiteaTXT {
				return &apiError{Code: apiErrorStateChanged}
			}
			for _, site := range updates.Sites {
				sites[site.RepoID] = site
			}
			if catchUntil == 0 {
				catchUntil = updates.Until
			} else if updates.Until != catchUntil {
				return &apiError{Code: apiErrorStateChanged}
			}
			if updates.NextCursor == nil {
				until = updates.Until
				break
			}
			at, ar = updates.NextCursor.PagesUpdatedUnixNano, updates.NextCursor.RepoID
		}
	}
	if rootURL == "" {
		return errors.New("full sites response did not establish a snapshot")
	}
	listing := make([]site, 0, len(sites))
	for _, site := range sites {
		listing = append(listing, site)
	}
	if err := s.store.replaceSites(rootURL, rootTime, until, listing, limits, requireGiteaTXT); err != nil {
		return err
	}
	if err := s.store.cleanupUntrackedContent(); err != nil {
		s.logger.Error("cleaning untracked Pages content failed", "error", err)
	}
	s.requireGiteaTXT.Store(requireGiteaTXT)
	for _, report := range s.store.pendingReports() {
		if _, ok := sites[report.RepoID]; ok {
			continue
		}
		if err := s.store.clearPendingReport(report.RepoID, report); err != nil {
			s.logger.Error("cleaning obsolete Pages report failed", "repo_id", report.RepoID, "error", err)
		}
	}
	s.requestPendingScan()
	return nil
}

// storeSiteUpdates records incremental facts before deployment workers handle
// them, then makes the durable changes visible to the active serving mode.
func (s *syncer) storeSiteUpdates(sites []site) error {
	updated := make(map[int64]struct{}, len(sites))
	for _, site := range sites {
		if err := s.store.upsertSite(site); err != nil {
			return err
		}
		updated[site.RepoID] = struct{}{}
	}
	for repoID := range updated {
		s.enqueueSite(repoID)
	}
	return nil
}
