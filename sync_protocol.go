// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package page

import (
	"context"
	"errors"
	"log/slog"
)

func (s *Syncer) syncWithStateRetry(ctx context.Context) error {
	for attempt := 0; attempt < 2; attempt++ {
		err := s.syncOnce(ctx)
		if err == nil {
			return nil
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Code != apiErrorStateChanged {
			return err
		}
	}
	return errors.New("state changed repeatedly during synchronization")
}

func (s *Syncer) syncOnce(ctx context.Context) error {
	cursor, initialized, rootURL, rootTime := s.store.SyncState()
	if !initialized {
		return s.syncAllSites(ctx)
	}
	resp, err := s.client.ListSites(ctx, false, cursor, 0, 0, 0)
	if err != nil {
		return err
	}
	if resp.RootURL != rootURL || resp.RootUpdatedUnixNano != rootTime {
		return s.syncAllSites(ctx)
	}
	firstConfig := resp
	txtRequirementChanged, err := s.store.SetSyncConfig(resp.ArchiveLimits, resp.RequireGiteaTXTRecord)
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
		resp, err = s.client.ListSites(ctx, false, cursor, until, at, ar)
		if err != nil {
			return err
		}
		if resp.RootURL != rootURL || resp.RootUpdatedUnixNano != rootTime {
			return s.syncAllSites(ctx)
		}
		if resp.ArchiveLimits != firstConfig.ArchiveLimits || resp.RequireGiteaTXTRecord != firstConfig.RequireGiteaTXTRecord {
			return &APIError{Code: apiErrorStateChanged}
		}
		if err := s.storeSiteUpdates(resp.Sites); err != nil {
			return err
		}
	}
	return s.store.SetCursor(until)
}

func (s *Syncer) syncAllSites(ctx context.Context) error {
	var until, at, ar int64
	var rootURL string
	var rootTime int64
	var limits ArchiveLimits
	var requireGiteaTXT bool
	configKnown := false
	sites := make(map[int64]Site)
	for {
		resp, err := s.client.ListSites(ctx, true, 0, until, at, ar)
		if err != nil {
			return err
		}
		if rootURL == "" {
			rootURL, rootTime = resp.RootURL, resp.RootUpdatedUnixNano
		} else if resp.RootURL != rootURL || resp.RootUpdatedUnixNano != rootTime {
			return &APIError{Code: apiErrorStateChanged}
		}
		if _, err := resp.ArchiveLimits.maxTarBytes(); err != nil {
			return err
		}
		if !configKnown {
			limits, requireGiteaTXT, configKnown = resp.ArchiveLimits, resp.RequireGiteaTXTRecord, true
		} else if resp.ArchiveLimits != limits || resp.RequireGiteaTXTRecord != requireGiteaTXT {
			return &APIError{Code: apiErrorStateChanged}
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
			updates, err := s.client.ListSites(ctx, false, initialUntil, catchUntil, at, ar)
			if err != nil {
				return err
			}
			if updates.RootURL != rootURL || updates.RootUpdatedUnixNano != rootTime {
				return &APIError{Code: apiErrorStateChanged}
			}
			if updates.ArchiveLimits != limits || updates.RequireGiteaTXTRecord != requireGiteaTXT {
				return &APIError{Code: apiErrorStateChanged}
			}
			for _, site := range updates.Sites {
				sites[site.RepoID] = site
			}
			if catchUntil == 0 {
				catchUntil = updates.Until
			} else if updates.Until != catchUntil {
				return &APIError{Code: apiErrorStateChanged}
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
	listing := make([]Site, 0, len(sites))
	for _, site := range sites {
		listing = append(listing, site)
	}
	if err := s.store.ReplaceSites(rootURL, rootTime, until, listing, limits, requireGiteaTXT); err != nil {
		return err
	}
	if err := s.store.CleanupUntrackedContent(); err != nil {
		slog.Error("cleaning untracked Pages content failed", "error", err)
	}
	s.requireGiteaTXT.Store(requireGiteaTXT)
	for _, report := range s.store.PendingReports() {
		if _, ok := sites[report.RepoID]; ok {
			continue
		}
		if err := s.store.ClearPendingReport(report.RepoID, report); err != nil {
			slog.Error("cleaning obsolete Pages report failed", "repo_id", report.RepoID, "error", err)
		}
	}
	s.rebuildRoutes()
	s.requestPendingScan()
	return nil
}

// storeSiteUpdates records incremental facts before deployment workers handle
// them, then makes the durable changes visible to the active serving mode.
func (s *Syncer) storeSiteUpdates(sites []Site) error {
	updated := make(map[int64]struct{}, len(sites))
	for _, site := range sites {
		if err := s.store.UpsertSite(site); err != nil {
			return err
		}
		updated[site.RepoID] = struct{}{}
	}
	for repoID := range updated {
		s.updateSiteRoute(repoID)
		s.enqueueSite(repoID)
	}
	return nil
}
