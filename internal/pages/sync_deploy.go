// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gitea.dev/page/internal/artifact"
	"gitea.dev/page/internal/gitea"
	"gitea.dev/page/internal/storage"
)

func (d *deployer) deploySite(ctx context.Context, repoID int64) error {
	current, ok := d.store.Site(repoID)
	if !ok {
		return nil
	}
	syncState := d.store.SyncState()
	if !syncState.Initialized {
		return errors.New("deployment configuration has not been synchronized")
	}
	limits, requireGiteaTXT := syncState.ArchiveLimits, syncState.RequireGiteaTXT
	if err := limits.Validate(); err != nil {
		return fmt.Errorf("invalid synchronized deployment limits: %w", err)
	}
	// The queue carries only an identity. A newer synchronization may already
	// have replaced the intended target while this item was waiting.
	site := current.Site
	if site.Deleted || !site.Enabled || site.TarGzSHA256 == "" {
		if err := d.publisher.unpublish(ctx, site.RepoID); err != nil {
			return d.reportDeploymentFailure(ctx, site, requireGiteaTXT, gitea.ErrorRouteApplyFailed, err)
		}
		if err := d.store.RemoveInstalledContent(site.RepoID); err != nil {
			return d.reportDeploymentFailure(ctx, site, requireGiteaTXT, gitea.ErrorContentInstallFailed, err)
		}
		applied, err := d.store.SaveDeploymentResult(ctx, site, "", false, false)
		if err != nil {
			return d.reportDeploymentFailure(ctx, site, requireGiteaTXT, gitea.ErrorContentInstallFailed, err)
		}
		if !applied {
			return nil
		}
		return d.reportResult(ctx, site, requireGiteaTXT, "", true, false, false, "")
	}

	content, installed := d.store.InstalledContent(site.RepoID, site.TarGzSHA256)
	if !installed {
		work, err := d.store.WorkDir(site.RepoID)
		if err != nil {
			return d.reportDeploymentFailure(ctx, site, requireGiteaTXT, gitea.ErrorContentInstallFailed, err)
		}
		defer func() { _ = os.RemoveAll(work) }()
		archive := filepath.Join(work, "site.tar.gz")
		f, err := os.OpenFile(archive, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return d.reportDeploymentFailure(ctx, site, requireGiteaTXT, gitea.ErrorContentInstallFailed, err)
		}
		err = d.client.DownloadContent(ctx, site.RepoID, site.TarGzSHA256, limits.MaxTarGzBytes, f)
		closeErr := f.Close()
		if err != nil {
			return d.reportDeploymentFailure(ctx, site, requireGiteaTXT, gitea.ErrorContentDownloadFailed, err)
		}
		if closeErr != nil {
			return d.reportDeploymentFailure(ctx, site, requireGiteaTXT, gitea.ErrorContentInstallFailed, closeErr)
		}
		candidate := filepath.Join(work, "content")
		af, err := os.Open(archive)
		if err != nil {
			return d.reportDeploymentFailure(ctx, site, requireGiteaTXT, gitea.ErrorContentInvalid, err)
		}
		err = artifact.Extract(ctx, af, candidate, limits)
		closeErr = af.Close()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return d.reportDeploymentFailure(ctx, site, requireGiteaTXT, gitea.ErrorContentInstallFailed, ctxErr)
			}
			return d.reportDeploymentFailure(ctx, site, requireGiteaTXT, gitea.ErrorContentInvalid, err)
		}
		if closeErr != nil {
			return d.reportDeploymentFailure(ctx, site, requireGiteaTXT, gitea.ErrorContentInvalid, closeErr)
		}
		latest, ok := d.store.Site(site.RepoID)
		if !ok || latest.Site.PagesUpdatedUnixNano != site.PagesUpdatedUnixNano || latest.Site.TarGzSHA256 != site.TarGzSHA256 {
			return nil
		}
		content, err = d.store.InstallContent(site.RepoID, site.TarGzSHA256, candidate)
		if err != nil {
			return d.reportDeploymentFailure(ctx, site, requireGiteaTXT, gitea.ErrorContentInstallFailed, err)
		}
	}
	return d.publishSite(ctx, site, content, requireGiteaTXT)
}

func (d *deployer) publishSite(ctx context.Context, site gitea.Site, content storage.Content, requireGiteaTXT bool) error {
	customDomainReady, customErr := d.customDomainAuthorized(ctx, site, requireGiteaTXT)
	var errorCode gitea.ErrorCode
	if customErr != nil {
		errorCode = gitea.ErrorTXTInvalid
	}
	if customDomainReady && d.certificates != nil {
		if err := d.certificates.EnsureCustom(ctx, site.CustomDomain.Host); err != nil {
			customDomainReady = d.certificates.HasUsable(site.CustomDomain.Host)
			if customDomainReady {
				errorCode = gitea.ErrorACMEFailed
			} else {
				errorCode = gitea.ErrorCertificatePending
			}
			d.logger.Error("preparing Pages certificate failed", "repo_id", site.RepoID, "error", err)
		}
	}
	deployment, err := deploymentForSite(d.cfg, site, content, customDomainReady)
	if err != nil {
		return d.reportDeploymentFailure(ctx, site, requireGiteaTXT, gitea.ErrorRouteApplyFailed, err)
	}
	publication, err := d.publisher.publish(ctx, deployment)
	if err != nil {
		_ = d.store.RemoveInactiveContent(site.RepoID, site.TarGzSHA256)
		return d.reportDeploymentFailure(ctx, site, requireGiteaTXT, gitea.ErrorRouteApplyFailed, err)
	}
	if publication.problem != "" && errorCode == "" {
		switch publication.problem {
		case publicationProblemCertificatePending:
			errorCode = gitea.ErrorCertificatePending
		case publicationProblemACMEFailed:
			errorCode = gitea.ErrorACMEFailed
		default:
			errorCode = gitea.ErrorEntryProbeFailed
		}
	}
	applied, err := d.store.SaveDeploymentResult(ctx, site, site.TarGzSHA256, publication.defaultPublished, publication.customPublished)
	if err != nil {
		return d.reportDeploymentFailure(ctx, site, requireGiteaTXT, gitea.ErrorContentInstallFailed, err)
	}
	if !applied {
		return nil
	}
	if err := d.store.RemoveSupersededContent(site.RepoID, site.TarGzSHA256); err != nil {
		d.logger.Error("cleaning superseded Pages content failed", "repo_id", site.RepoID, "error", err)
	}
	return d.reportResult(ctx, site, requireGiteaTXT, site.TarGzSHA256, true, publication.defaultPublished, publication.customPublished, errorCode)
}

func (d *deployer) reportDeploymentFailure(ctx context.Context, site gitea.Site, requireGiteaTXT bool, code gitea.ErrorCode, cause error) error {
	local, _ := d.store.Site(site.RepoID)
	applied, err := d.store.SaveDeploymentResult(ctx, site, local.InstalledDigest, local.DefaultPublished, local.CustomPublished)
	if err == nil && !applied {
		return nil
	}
	var saveErr error
	if err != nil {
		saveErr = fmt.Errorf("save deployment failure: %w", err)
	}
	reportErr := d.reportResult(ctx, site, requireGiteaTXT, local.InstalledDigest, false, false, false, code)
	return errors.Join(fmt.Errorf("%s: %w", code, cause), saveErr, reportErr)
}

func (d *deployer) reportResult(ctx context.Context, site gitea.Site, requireGiteaTXT bool, digest string, configApplied, defaultPublished, customPublished bool, errorCode gitea.ErrorCode) error {
	rootTime := d.store.SyncState().RootUpdatedUnixNano
	result := gitea.DeploymentResult{
		RepoID: site.RepoID, AppliedPagesUpdatedUnixNano: site.PagesUpdatedUnixNano, AppliedRootUpdatedUnixNano: rootTime,
		AppliedRequireGiteaTXTRecord: requireGiteaTXT, ConfigApplied: configApplied, ActualTarGzSHA256: digest,
		DefaultPublished: defaultPublished, CustomPublished: customPublished, ErrorCode: errorCode,
	}
	reportAfterStoreError := func(storeErr error) error {
		return errors.Join(storeErr, d.client.ReportResult(ctx, result))
	}
	if old, ok := d.store.PendingReport(site.RepoID); ok {
		err := d.client.ReportResult(ctx, old)
		var apiErr *gitea.APIError
		if err != nil && (!errors.As(err, &apiErr) || apiErr.Code != gitea.APIErrorStateChanged) {
			return err
		}
		if clearErr := d.store.ClearPendingReport(ctx, site.RepoID, old); clearErr != nil {
			return reportAfterStoreError(clearErr)
		}
		if err != nil {
			return err
		}
		if err := d.store.MarkConfirmedReport(ctx, old); err != nil {
			return reportAfterStoreError(err)
		}
		if old == result {
			return nil
		}
	}
	if err := d.store.SavePendingReport(ctx, result); err != nil {
		return reportAfterStoreError(err)
	}
	if err := d.client.ReportResult(ctx, result); err != nil {
		var apiErr *gitea.APIError
		if errors.As(err, &apiErr) && apiErr.Code == gitea.APIErrorStateChanged {
			_ = d.store.ClearPendingReport(ctx, site.RepoID, result)
		}
		return err
	}
	if err := d.store.MarkConfirmedReport(ctx, result); err != nil {
		return err
	}
	return d.store.ClearPendingReport(ctx, site.RepoID, result)
}
