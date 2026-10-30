// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func (s *syncer) deploySite(ctx context.Context, repoID int64) error {
	current, ok := s.store.getSite(repoID)
	if !ok {
		return nil
	}
	limits, _, configKnown := s.store.syncConfig()
	if !configKnown {
		return errors.New("deployment configuration has not been synchronized")
	}
	// The queue carries only an identity. A newer synchronization may already
	// have replaced the intended target while this item was waiting.
	site := current.Site
	if site.Deleted || !site.Enabled || site.TarGzSHA256 == "" {
		if err := s.publisher.Unpublish(ctx, site.RepoID); err != nil {
			return s.reportDeploymentFailure(ctx, site, instanceErrorRouteApplyFailed, err)
		}
		if err := s.store.removeContent(site.RepoID); err != nil {
			return s.reportDeploymentFailure(ctx, site, instanceErrorContentInstallFailed, err)
		}
		applied, err := s.store.saveDeploymentResult(site, "", false, false)
		if err != nil {
			return s.reportDeploymentFailure(ctx, site, instanceErrorContentInstallFailed, err)
		}
		if !applied {
			return nil
		}
		return s.reportResult(ctx, site, "", true, false, false, "")
	}

	content, prepared := s.store.content(site.RepoID, site.TarGzSHA256)
	if !prepared {
		work, err := s.store.workDir(site.RepoID)
		if err != nil {
			return s.reportDeploymentFailure(ctx, site, instanceErrorContentInstallFailed, err)
		}
		defer func() { _ = os.RemoveAll(work) }()
		archive := filepath.Join(work, "site.tar.gz")
		f, err := os.OpenFile(archive, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return s.reportDeploymentFailure(ctx, site, instanceErrorContentInstallFailed, err)
		}
		err = s.client.downloadContent(ctx, site.RepoID, site.TarGzSHA256, limits.MaxTarGzBytes, f)
		closeErr := f.Close()
		if err != nil {
			return s.reportDeploymentFailure(ctx, site, instanceErrorContentDownloadFailed, err)
		}
		if closeErr != nil {
			return s.reportDeploymentFailure(ctx, site, instanceErrorContentInstallFailed, closeErr)
		}
		candidate := filepath.Join(work, "content")
		af, err := os.Open(archive)
		if err != nil {
			return s.reportDeploymentFailure(ctx, site, instanceErrorContentInvalid, err)
		}
		err = extractTarGz(ctx, af, candidate, limits)
		closeErr = af.Close()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return s.reportDeploymentFailure(ctx, site, instanceErrorContentInstallFailed, ctxErr)
			}
			return s.reportDeploymentFailure(ctx, site, instanceErrorContentInvalid, err)
		}
		if closeErr != nil {
			return s.reportDeploymentFailure(ctx, site, instanceErrorContentInvalid, closeErr)
		}
		latest, ok := s.store.getSite(site.RepoID)
		if !ok || latest.Site.PagesUpdatedUnixNano != site.PagesUpdatedUnixNano || latest.Site.TarGzSHA256 != site.TarGzSHA256 {
			return nil
		}
		content, err = s.store.prepareContent(site.RepoID, site.TarGzSHA256, candidate)
		if err != nil {
			return s.reportDeploymentFailure(ctx, site, instanceErrorContentInstallFailed, err)
		}
	}
	return s.publishSite(ctx, site, content)
}

func (s *syncer) publishSite(ctx context.Context, site site, content Content) error {
	customDomainReady, customErr := s.customDomainAuthorized(ctx, site)
	var errorCode instanceErrorCode
	if customErr != nil {
		errorCode = instanceErrorTXTInvalid
	}
	certChanged := false
	if customDomainReady {
		if publisher, ok := s.publisher.(managedCertificatePublisher); ok && site.CustomDomain != nil {
			if err := publisher.prepareCustomDomain(ctx, site.CustomDomain.Host); err != nil {
				return s.reportDeploymentFailure(ctx, site, instanceErrorRouteApplyFailed, err)
			}
		}
		if renewed, err := s.ensureCustomCertificate(ctx, site); err != nil {
			certChanged = renewed
			customDomainReady = s.certificates == nil || s.cfg.RootURL.Scheme != "https" ||
				site.CustomDomain == nil || s.certificates.hasUsable(site.CustomDomain.Host)
			if customDomainReady {
				errorCode = instanceErrorACMEFailed
			} else {
				errorCode = instanceErrorCertificatePending
			}
			s.logger.Error("preparing Pages certificate failed", "repo_id", site.RepoID, "error", err)
		} else {
			certChanged = renewed
		}
	}
	if certChanged && s.certificates != nil {
		if publisher, ok := s.publisher.(managedCertificatePublisher); ok {
			if err := publisher.certificatesChanged(ctx, s.certificates); err != nil {
				return s.reportDeploymentFailure(ctx, site, instanceErrorRouteApplyFailed, err)
			}
		}
	}
	deployment, err := deploymentForSite(s.cfg, site, content, customDomainReady)
	if err != nil {
		return s.reportDeploymentFailure(ctx, site, instanceErrorRouteApplyFailed, err)
	}
	publication, err := s.publisher.Publish(ctx, deployment)
	if err != nil {
		_ = s.store.discardContent(site.RepoID, site.TarGzSHA256)
		return s.reportDeploymentFailure(ctx, site, instanceErrorRouteApplyFailed, err)
	}
	if publication.Problem != "" && errorCode == "" {
		switch publication.Problem {
		case PublicationProblemCertificatePending:
			errorCode = instanceErrorCertificatePending
		case PublicationProblemACMEFailed:
			errorCode = instanceErrorACMEFailed
		default:
			errorCode = instanceErrorEntryProbeFailed
		}
	}
	applied, err := s.store.saveDeploymentResult(site, site.TarGzSHA256, publication.DefaultPublished, publication.CustomPublished)
	if err != nil {
		return s.reportDeploymentFailure(ctx, site, instanceErrorContentInstallFailed, err)
	}
	if !applied {
		return nil
	}
	if err := s.store.cleanupOldContent(site.RepoID, site.TarGzSHA256); err != nil {
		s.logger.Error("cleaning old Pages content failed", "repo_id", site.RepoID, "error", err)
	}
	return s.reportResult(ctx, site, site.TarGzSHA256, true, publication.DefaultPublished, publication.CustomPublished, errorCode)
}

// pendingSiteIDs finds targets that need deployment or a result retry. The
// target itself is already persisted, so the queue remains bounded and a
// process restart can rebuild the work set from the current local state.
func (s *syncer) pendingSiteIDs() []int64 {
	_, initialized, _, rootTime := s.store.syncState()
	if !initialized {
		return nil
	}
	txtRequirementChanged := s.txtRequirementChanged.Swap(false)
	_, requireGiteaTXT, configKnown := s.store.syncConfig()
	if !configKnown {
		return nil
	}
	pending := make(map[int64]struct{})
	for _, result := range s.store.pendingReports() {
		pending[result.RepoID] = struct{}{}
	}
	now := time.Now()
	repoIDs := make([]int64, 0)
	s.store.forEachSite(func(local localSite) bool {
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

func (s *syncer) enqueueSite(repoID int64) {
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

func (s *syncer) finishDeployment(repoID int64) {
	s.inflightMu.Lock()
	delete(s.inflight, repoID)
	_, redeploy := s.redeploy[repoID]
	delete(s.redeploy, repoID)
	s.inflightMu.Unlock()
	if redeploy {
		s.enqueueSite(repoID)
	}
}

func (s *syncer) enqueueSiteBlocking(ctx context.Context, repoID int64) bool {
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

func (s *syncer) requestPendingScan() {
	select {
	case s.scanRequested <- struct{}{}:
	default:
	}
}

func (s *syncer) scanPendingSites(ctx context.Context) {
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

func (s *syncer) reportDeploymentFailure(ctx context.Context, site site, code instanceErrorCode, cause error) error {
	local, _ := s.store.getSite(site.RepoID)
	applied, err := s.store.saveDeploymentResult(site, local.InstalledDigest, local.DefaultPublished, local.CustomPublished)
	if err == nil && !applied {
		return nil
	}
	var saveErr error
	if err != nil {
		saveErr = fmt.Errorf("save deployment failure: %w", err)
	}
	reportErr := s.reportResult(context.WithoutCancel(ctx), site, local.InstalledDigest, false, false, false, code)
	return errors.Join(fmt.Errorf("%s: %w", code, cause), saveErr, reportErr)
}

func (s *syncer) reportResult(ctx context.Context, site site, digest string, configApplied, defaultPublished, customPublished bool, errorCode instanceErrorCode) error {
	_, _, _, rootTime := s.store.syncState()
	_, requireGiteaTXT, configKnown := s.store.syncConfig()
	if !configKnown {
		return errors.New("result configuration has not been synchronized")
	}
	result := deploymentResult{
		RepoID: site.RepoID, AppliedPagesUpdatedUnixNano: site.PagesUpdatedUnixNano, AppliedRootUpdatedUnixNano: rootTime,
		AppliedRequireGiteaTXTRecord: requireGiteaTXT, ConfigApplied: configApplied, ActualTarGzSHA256: digest,
		DefaultPublished: defaultPublished, CustomPublished: customPublished, ErrorCode: errorCode,
	}
	reportAfterStoreError := func(storeErr error) error {
		return errors.Join(storeErr, s.client.reportResult(context.WithoutCancel(ctx), result))
	}
	if old, ok := s.store.pendingReport(site.RepoID); ok {
		err := s.client.reportResult(ctx, old)
		var apiErr *apiError
		if err != nil && (!errors.As(err, &apiErr) || apiErr.Code != apiErrorStateChanged) {
			return err
		}
		if clearErr := s.store.clearPendingReport(site.RepoID, old); clearErr != nil {
			return reportAfterStoreError(clearErr)
		}
		if err != nil {
			return err
		}
		if err := s.store.markConfirmedReport(old); err != nil {
			return reportAfterStoreError(err)
		}
		if old == result {
			return nil
		}
	}
	if err := s.store.savePendingReport(result); err != nil {
		return reportAfterStoreError(err)
	}
	if err := s.client.reportResult(ctx, result); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.Code == apiErrorStateChanged {
			_ = s.store.clearPendingReport(site.RepoID, result)
		}
		return err
	}
	if err := s.store.markConfirmedReport(result); err != nil {
		return err
	}
	return s.store.clearPendingReport(site.RepoID, result)
}

func (s *syncer) startWorkers(ctx context.Context) {
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
						s.logger.Error("deploying Pages site failed", "repo_id", repoID, "error", err)
						var apiErr *apiError
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
