// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pages

import (
	"context"
	"errors"

	"gitea.dev/page/internal/artifact"
	"gitea.dev/page/internal/gitea"
)

func (s *syncer) syncWithStateRetry(ctx context.Context) error {
	for attempt := 0; attempt < 2; attempt++ {
		err := s.syncOnce(ctx)
		if err == nil {
			return nil
		}
		var apiErr *gitea.APIError
		if !errors.As(err, &apiErr) || apiErr.Code != gitea.APIErrorStateChanged {
			return err
		}
	}
	return errors.New("state changed repeatedly during synchronization")
}

func (s *syncer) syncOnce(ctx context.Context) error {
	syncState := s.store.SyncState()
	if !syncState.Initialized {
		return s.syncAllSites(ctx)
	}
	resp, err := s.client.ListSites(ctx, false, syncState.Cursor, 0, 0, 0)
	if err != nil {
		return err
	}
	if resp.RootURL != syncState.RootURL || resp.RootUpdatedUnixNano != syncState.RootUpdatedUnixNano {
		return s.syncAllSites(ctx)
	}
	firstConfig := resp
	txtRequirementChanged, err := s.store.SetSyncConfig(ctx, resp.ArchiveLimits, resp.RequireGiteaTXTRecord)
	if err != nil {
		return err
	}
	if txtRequirementChanged {
		s.deployer.requestScan()
	}
	until := resp.Until
	if err := s.storeSiteUpdates(ctx, resp.Sites); err != nil {
		return err
	}
	at, ar := int64(0), int64(0)
	for resp.NextCursor != nil {
		if until == 0 {
			until = resp.Until
		}
		at, ar = resp.NextCursor.PagesUpdatedUnixNano, resp.NextCursor.RepoID
		resp, err = s.client.ListSites(ctx, false, syncState.Cursor, until, at, ar)
		if err != nil {
			return err
		}
		if resp.RootURL != syncState.RootURL || resp.RootUpdatedUnixNano != syncState.RootUpdatedUnixNano {
			return s.syncAllSites(ctx)
		}
		if resp.ArchiveLimits != firstConfig.ArchiveLimits || resp.RequireGiteaTXTRecord != firstConfig.RequireGiteaTXTRecord {
			return &gitea.APIError{Code: gitea.APIErrorStateChanged}
		}
		if err := s.storeSiteUpdates(ctx, resp.Sites); err != nil {
			return err
		}
	}
	return s.store.SetSyncCursor(ctx, until)
}

func (s *syncer) syncAllSites(ctx context.Context) error {
	var until, at, ar int64
	var rootURL string
	var rootTime int64
	var limits artifact.Limits
	var requireGiteaTXT bool
	configKnown := false
	sites := make(map[int64]gitea.Site)
	for {
		resp, err := s.client.ListSites(ctx, true, 0, until, at, ar)
		if err != nil {
			return err
		}
		if rootURL == "" {
			rootURL, rootTime = resp.RootURL, resp.RootUpdatedUnixNano
		} else if resp.RootURL != rootURL || resp.RootUpdatedUnixNano != rootTime {
			return &gitea.APIError{Code: gitea.APIErrorStateChanged}
		}
		if err := resp.ArchiveLimits.Validate(); err != nil {
			return err
		}
		if !configKnown {
			limits, requireGiteaTXT, configKnown = resp.ArchiveLimits, resp.RequireGiteaTXTRecord, true
		} else if resp.ArchiveLimits != limits || resp.RequireGiteaTXTRecord != requireGiteaTXT {
			return &gitea.APIError{Code: gitea.APIErrorStateChanged}
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
				return &gitea.APIError{Code: gitea.APIErrorStateChanged}
			}
			if updates.ArchiveLimits != limits || updates.RequireGiteaTXTRecord != requireGiteaTXT {
				return &gitea.APIError{Code: gitea.APIErrorStateChanged}
			}
			for _, site := range updates.Sites {
				sites[site.RepoID] = site
			}
			if catchUntil == 0 {
				catchUntil = updates.Until
			} else if updates.Until != catchUntil {
				return &gitea.APIError{Code: gitea.APIErrorStateChanged}
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
	if err := s.store.ReplaceSites(ctx, rootURL, rootTime, until, sites, limits, requireGiteaTXT); err != nil {
		return err
	}
	if err := s.store.RemoveUntrackedContent(); err != nil {
		s.logger.Error("cleaning untracked Pages content failed", "error", err)
	}
	for _, report := range s.store.PendingReports() {
		if _, ok := sites[report.RepoID]; ok {
			continue
		}
		if err := s.store.ClearPendingReport(ctx, report.RepoID, report); err != nil {
			s.logger.Error("cleaning stale Pages report failed", "repo_id", report.RepoID, "error", err)
		}
	}
	s.deployer.requestScan()
	return nil
}

// storeSiteUpdates records incremental facts before deployment workers handle
// them, then makes the durable changes visible to the active serving mode.
func (s *syncer) storeSiteUpdates(ctx context.Context, sites []gitea.Site) error {
	updated := make(map[int64]struct{}, len(sites))
	for _, site := range sites {
		if err := s.store.UpsertSite(ctx, site); err != nil {
			return err
		}
		updated[site.RepoID] = struct{}{}
	}
	for repoID := range updated {
		s.deployer.schedule(repoID)
	}
	return nil
}
