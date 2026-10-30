// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package page

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Syncer) deploySite(ctx context.Context, repoID int64) error {
	current, ok := s.store.GetSite(repoID)
	if !ok {
		return nil
	}
	limits, _, configKnown := s.store.SyncConfig()
	if !configKnown {
		return errors.New("deployment configuration has not been synchronized")
	}
	// The queue carries only an identity. A newer synchronization may already
	// have replaced the intended target while this item was waiting.
	site := current.Site
	if site.Deleted || !site.Enabled || site.TarGzSHA256 == "" {
		if err := s.store.RemoveContent(site.RepoID); err != nil {
			return s.reportDeploymentFailure(ctx, site, InstanceErrorContentInstallFailed, err)
		}
		applied, err := s.store.SaveDeploymentResult(site, "", false, false)
		if err != nil || !applied {
			return err
		}
		s.updateSiteRoute(site.RepoID)
		if err := s.applyRoutes(ctx); err != nil {
			return s.reportDeploymentFailure(ctx, site, InstanceErrorRouteApplyFailed, err)
		}
		if err := s.probePlatform(ctx); err != nil {
			return s.reportDeploymentFailure(ctx, site, InstanceErrorEntryProbeFailed, err)
		}
		return s.reportResult(ctx, site, "", true, false, false, "")
	}

	if current.InstalledDigest != site.TarGzSHA256 || !s.store.InstalledMatches(site.RepoID, site.TarGzSHA256) {
		work, err := s.store.WorkDir(site.RepoID)
		if err != nil {
			return s.reportDeploymentFailure(ctx, site, InstanceErrorContentInstallFailed, err)
		}
		defer func() { _ = os.RemoveAll(work) }()
		archive := filepath.Join(work, "site.tar.gz")
		f, err := os.OpenFile(archive, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return s.reportDeploymentFailure(ctx, site, InstanceErrorContentInstallFailed, err)
		}
		err = s.client.DownloadContent(ctx, site.RepoID, site.TarGzSHA256, limits.MaxTarGzBytes, f)
		closeErr := f.Close()
		if err != nil {
			return s.reportDeploymentFailure(ctx, site, InstanceErrorContentDownloadFailed, err)
		}
		if closeErr != nil {
			return s.reportDeploymentFailure(ctx, site, InstanceErrorContentInstallFailed, closeErr)
		}
		candidate := filepath.Join(work, "content")
		af, err := os.Open(archive)
		if err != nil {
			return s.reportDeploymentFailure(ctx, site, InstanceErrorContentInvalid, err)
		}
		err = ExtractTarGz(ctx, af, candidate, site.RepoID, site.TarGzSHA256, limits)
		closeErr = af.Close()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return s.reportDeploymentFailure(ctx, site, InstanceErrorContentInstallFailed, ctxErr)
			}
			return s.reportDeploymentFailure(ctx, site, InstanceErrorContentInvalid, err)
		}
		if closeErr != nil {
			return s.reportDeploymentFailure(ctx, site, InstanceErrorContentInvalid, closeErr)
		}
		latest, ok := s.store.GetSite(site.RepoID)
		if !ok || latest.Site.PagesUpdatedUnixNano != site.PagesUpdatedUnixNano || latest.Site.TarGzSHA256 != site.TarGzSHA256 {
			return nil
		}
		if err := s.store.Install(site.RepoID, site.TarGzSHA256, candidate); err != nil {
			return s.reportDeploymentFailure(ctx, site, InstanceErrorContentInstallFailed, err)
		}
	}
	return s.publishSite(ctx, site)
}

func (s *Syncer) publishSite(ctx context.Context, site Site) error {
	customDomainPublished, customErr := s.customDomainAuthorized(ctx, site)
	var errorCode InstanceErrorCode
	if customErr != nil {
		errorCode = InstanceErrorTXTInvalid
	}
	nginxPublisher, isNginx := s.applier.(*NginxPublisher)
	certChanged := false
	if customDomainPublished {
		needsCertificate := s.certificates != nil && s.cfg.RootURL.Scheme == "https" && site.CustomDomain != nil &&
			s.certificates.NeedsRenewal(site.CustomDomain.Host, certificateRenewalWindow)
		if isNginx && needsCertificate {
			if err := s.applyRoutes(ctx); err != nil {
				return s.reportDeploymentFailure(ctx, site, InstanceErrorRouteApplyFailed, err)
			}
		}
		if renewed, err := s.ensureCustomCertificate(ctx, site); err != nil {
			certChanged = renewed
			customDomainPublished = s.certificates == nil || s.cfg.RootURL.Scheme != "https" ||
				site.CustomDomain == nil || s.certificates.HasUsable(site.CustomDomain.Host)
			if customDomainPublished {
				errorCode = InstanceErrorACMEFailed
			} else {
				errorCode = InstanceErrorCertificatePending
			}
			slog.Error("preparing Pages certificate failed", "repo_id", site.RepoID, "error", err)
		} else {
			certChanged = renewed
		}
	}
	// Candidate flags open only the currently intended routes. Publication is
	// confirmed after the local listener returns the exact probe body.
	previous, _ := s.store.GetSite(site.RepoID)
	current, err := s.store.SaveDeploymentResult(site, site.TarGzSHA256, true, customDomainPublished)
	if err != nil || !current {
		return err
	}
	s.updateSiteRoute(site.RepoID)
	if isNginx && s.certificates != nil && certChanged {
		if err := s.certificates.ExportNginx(nginxPublisher.cfg.NginxRoot); err != nil {
			return s.failCandidatePublication(ctx, site, InstanceErrorRouteApplyFailed, err)
		}
	}
	if isNginx {
		confirmedDigest, _, _ := strings.Cut(nginxPublisher.confirmedReadyState(), "\n")
		if !previous.DefaultPublished || previous.CustomPublished != customDomainPublished || certChanged || strings.TrimSpace(confirmedDigest) == "" {
			if err := s.applyRoutes(ctx); err != nil {
				return s.failCandidatePublication(ctx, site, InstanceErrorRouteApplyFailed, err)
			}
		}
	} else if err := s.applyRoutes(ctx); err != nil {
		return s.failCandidatePublication(ctx, site, InstanceErrorRouteApplyFailed, err)
	}
	if err := s.probePlatform(ctx); err != nil {
		return s.failCandidatePublication(ctx, site, InstanceErrorEntryProbeFailed, err)
	}
	defaultPublished := false
	if host, base, ok := defaultSiteRoute(site, s.cfg.RootURL.Hostname()); ok {
		if err := s.probeEntry(ctx, host, base, site.RepoID, site.TarGzSHA256); err == nil {
			defaultPublished = true
		} else {
			errorCode = InstanceErrorEntryProbeFailed
			slog.Error("probing default Pages entry failed", "repo_id", site.RepoID, "error", err)
		}
	} else {
		errorCode = InstanceErrorEntryProbeFailed
	}
	if customDomainPublished && site.CustomDomain != nil {
		if err := s.probeEntry(ctx, site.CustomDomain.Host, "/", site.RepoID, site.TarGzSHA256); err != nil {
			customDomainPublished = false
			if errorCode == "" {
				errorCode = InstanceErrorEntryProbeFailed
			}
			slog.Error("probing custom Pages entry failed", "repo_id", site.RepoID, "host", site.CustomDomain.Host, "error", err)
		}
	}
	current, err = s.store.SaveDeploymentResult(site, site.TarGzSHA256, defaultPublished, customDomainPublished)
	if err != nil || !current {
		return err
	}
	if !defaultPublished || site.CustomDomain != nil && !customDomainPublished {
		s.updateSiteRoute(site.RepoID)
		if err := s.applyRoutes(ctx); err != nil {
			return s.failCandidatePublication(ctx, site, InstanceErrorRouteApplyFailed, err)
		}
	}
	return s.reportResult(ctx, site, site.TarGzSHA256, true, defaultPublished, customDomainPublished, errorCode)
}

// pendingSiteIDs finds targets that need deployment or a result retry. The
// target itself is already persisted, so the queue remains bounded and a
// process restart can rebuild the work set from the current local state.
func (s *Syncer) pendingSiteIDs() []int64 {
	_, initialized, _, rootTime := s.store.SyncState()
	if !initialized {
		return nil
	}
	txtRequirementChanged := s.txtRequirementChanged.Swap(false)
	_, requireGiteaTXT, configKnown := s.store.SyncConfig()
	if !configKnown {
		return nil
	}
	pending := make(map[int64]struct{})
	for _, result := range s.store.PendingReports() {
		pending[result.RepoID] = struct{}{}
	}
	now := time.Now()
	repoIDs := make([]int64, 0)
	s.store.ForEachSite(func(local LocalSite) bool {
		site := local.Site
		_, needsReport := pending[site.RepoID]
		confirmed := local.ConfirmedResult
		needsCurrentResult := confirmed == nil || confirmed.AppliedPagesUpdatedUnixNano != site.PagesUpdatedUnixNano || confirmed.AppliedRootUpdatedUnixNano != rootTime ||
			site.CustomDomain != nil && confirmed.AppliedRequireGiteaTXTRecord != requireGiteaTXT
		if confirmed != nil && (confirmed.DefaultPublished != local.DefaultPublished || confirmed.CustomPublished != local.CustomPublished || confirmed.ActualTarGzSHA256 != local.InstalledDigest) {
			needsCurrentResult = true
		}
		needsContent := site.Enabled && site.TarGzSHA256 != "" && local.InstalledDigest != site.TarGzSHA256
		needsRetry := site.Enabled && site.TarGzSHA256 != "" && (!local.DefaultPublished || site.CustomDomain != nil && !local.CustomPublished) && now.Sub(time.Unix(0, local.LastAttemptUnixNano)) >= deploymentRetryInterval
		needsCleanup := (site.Deleted || !site.Enabled || site.TarGzSHA256 == "") && (local.InstalledDigest != "" || local.DefaultPublished || local.CustomPublished)
		if needsReport || needsCurrentResult || needsContent || needsRetry || needsCleanup || txtRequirementChanged && site.CustomDomain != nil {
			repoIDs = append(repoIDs, site.RepoID)
		}
		return true
	})
	return repoIDs
}

func (s *Syncer) enqueueSite(repoID int64) {
	s.inflightMu.Lock()
	if _, ok := s.inflight[repoID]; ok {
		s.redeploy[repoID] = struct{}{}
		s.inflightMu.Unlock()
		return
	}
	s.inflight[repoID] = struct{}{}
	s.inflightMu.Unlock()
	select {
	case s.queue <- repoID:
	default:
		s.inflightMu.Lock()
		delete(s.inflight, repoID)
		s.inflightMu.Unlock()
		s.requestPendingScan()
	}
}

func (s *Syncer) finishDeployment(repoID int64) {
	s.inflightMu.Lock()
	delete(s.inflight, repoID)
	_, redeploy := s.redeploy[repoID]
	delete(s.redeploy, repoID)
	s.inflightMu.Unlock()
	if redeploy {
		s.enqueueSite(repoID)
	}
}

func (s *Syncer) enqueueSiteBlocking(ctx context.Context, repoID int64) bool {
	s.inflightMu.Lock()
	if _, ok := s.inflight[repoID]; ok {
		s.inflightMu.Unlock()
		return true
	}
	s.inflight[repoID] = struct{}{}
	s.inflightMu.Unlock()
	select {
	case s.queue <- repoID:
		return true
	case <-ctx.Done():
		s.inflightMu.Lock()
		delete(s.inflight, repoID)
		s.inflightMu.Unlock()
		return false
	}
}

func (s *Syncer) requestPendingScan() {
	select {
	case s.scanRequested <- struct{}{}:
	default:
	}
}

func (s *Syncer) scanPendingSites(ctx context.Context) {
	defer s.workers.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.scanRequested:
			for _, repoID := range s.pendingSiteIDs() {
				if !s.enqueueSiteBlocking(ctx, repoID) {
					return
				}
			}
		}
	}
}

func (s *Syncer) reportDeploymentFailure(ctx context.Context, site Site, code InstanceErrorCode, cause error) error {
	local, _ := s.store.GetSite(site.RepoID)
	applied, err := s.store.SaveDeploymentResult(site, local.InstalledDigest, local.DefaultPublished, local.CustomPublished)
	if err != nil {
		return errors.Join(fmt.Errorf("%s: %w", code, cause), fmt.Errorf("save deployment failure: %w", err))
	}
	if !applied {
		return nil
	}
	if reportErr := s.reportResult(ctx, site, local.InstalledDigest, false, false, false, code); reportErr != nil {
		return errors.Join(fmt.Errorf("%s: %w", code, cause), reportErr)
	}
	return fmt.Errorf("%s: %w", code, cause)
}

func (s *Syncer) failCandidatePublication(ctx context.Context, site Site, code InstanceErrorCode, cause error) error {
	applied, err := s.store.SaveDeploymentResult(site, site.TarGzSHA256, false, false)
	if err != nil {
		return errors.Join(fmt.Errorf("%s: %w", code, cause), fmt.Errorf("close candidate publication: %w", err))
	}
	if !applied {
		return nil
	}
	s.updateSiteRoute(site.RepoID)
	if err := s.applyRoutes(ctx); err != nil {
		cause = errors.Join(cause, fmt.Errorf("apply closed publication: %w", err))
	}
	if reportErr := s.reportResult(ctx, site, site.TarGzSHA256, false, false, false, code); reportErr != nil {
		return errors.Join(fmt.Errorf("%s: %w", code, cause), reportErr)
	}
	return fmt.Errorf("%s: %w", code, cause)
}

func (s *Syncer) reportResult(ctx context.Context, site Site, digest string, configApplied, defaultPublished, customPublished bool, errorCode InstanceErrorCode) error {
	_, _, _, rootTime := s.store.SyncState()
	_, requireGiteaTXT, configKnown := s.store.SyncConfig()
	if !configKnown {
		return errors.New("result configuration has not been synchronized")
	}
	result := DeploymentResult{
		RepoID: site.RepoID, AppliedPagesUpdatedUnixNano: site.PagesUpdatedUnixNano, AppliedRootUpdatedUnixNano: rootTime,
		AppliedRequireGiteaTXTRecord: requireGiteaTXT, ConfigApplied: configApplied, ActualTarGzSHA256: digest,
		DefaultPublished: defaultPublished, CustomPublished: customPublished, ErrorCode: errorCode,
	}
	if old, ok := s.store.PendingReport(site.RepoID); ok {
		err := s.client.ReportResult(ctx, old)
		var apiErr *APIError
		if err != nil && (!errors.As(err, &apiErr) || apiErr.Code != apiErrorStateChanged) {
			return err
		}
		if clearErr := s.store.ClearPendingReport(site.RepoID, old); clearErr != nil {
			return clearErr
		}
		if err != nil {
			return err
		}
		if err := s.store.MarkConfirmedReport(old); err != nil {
			return err
		}
		if old == result {
			return nil
		}
	}
	if err := s.store.SavePendingReport(result); err != nil {
		return err
	}
	if err := s.client.ReportResult(ctx, result); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == apiErrorStateChanged {
			_ = s.store.ClearPendingReport(site.RepoID, result)
		}
		return err
	}
	if err := s.store.MarkConfirmedReport(result); err != nil {
		return err
	}
	return s.store.ClearPendingReport(site.RepoID, result)
}

func (s *Syncer) startWorkers(ctx context.Context) {
	s.workers.Add(1)
	go s.scanPendingSites(ctx)
	for i := 0; i < s.cfg.DeployConcurrency; i++ {
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case repoID := <-s.queue:
					deployCtx, cancel := context.WithTimeout(ctx, s.cfg.DeployTimeout)
					err := s.deploySite(deployCtx, repoID)
					cancel()
					s.finishDeployment(repoID)
					if err != nil {
						slog.Error("deploying Pages site failed", "repo_id", repoID, "error", err)
						var apiErr *APIError
						if errors.As(err, &apiErr) && apiErr.Code == apiErrorStateChanged {
							select {
							case s.syncRequested <- struct{}{}:
							default:
							}
						}
					}
				}
			}
		}()
	}
}
