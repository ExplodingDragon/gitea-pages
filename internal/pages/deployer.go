// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"gitea.dev/page/internal/certificate"
	pageconfig "gitea.dev/page/internal/config"
	"gitea.dev/page/internal/gitea"
	"gitea.dev/page/internal/storage"
)

const deploymentRetryInterval = 5 * time.Minute

// deployer turns durable Gitea site facts into local publications and reports
// the confirmed result. Queue entries contain only repository IDs.
type deployer struct {
	cfg          pageconfig.Config
	client       *gitea.Client
	store        *storage.Store
	publisher    sitePublisher
	certificates *certificate.Manager
	resolver     *net.Resolver
	logger       *slog.Logger

	queue            *deploymentQueue
	scanRequested    wakeSignal
	refreshRequested wakeSignal
	workers          sync.WaitGroup
}

func newDeployer(cfg pageconfig.Config, client *gitea.Client, store *storage.Store, publisher sitePublisher, certificates *certificate.Manager, logger *slog.Logger) *deployer {
	cfg.ApplyDefaults()
	return &deployer{
		cfg:              cfg,
		client:           client,
		store:            store,
		publisher:        publisher,
		certificates:     certificates,
		resolver:         newDNSResolver(cfg.DNSResolver),
		logger:           logger,
		queue:            newDeploymentQueue(cfg.DeployQueueSize),
		scanRequested:    newWakeSignal(),
		refreshRequested: newWakeSignal(),
	}
}

func (d *deployer) start(ctx context.Context) error {
	if err := d.publisher.restore(ctx, storedDeployments{cfg: d.cfg, store: d.store}); err != nil {
		return fmt.Errorf("restore Pages publication: %w", err)
	}
	d.workers.Go(func() { d.scanLoop(ctx) })
	d.workers.Go(func() { d.certificateLoop(ctx) })
	d.workers.Go(func() { d.txtVerificationLoop(ctx) })
	for range d.cfg.DeployConcurrency {
		d.workers.Go(func() { d.worker(ctx) })
	}
	return nil
}

func (d *deployer) wait() {
	d.workers.Wait()
}

func (d *deployer) schedule(repoID int64) {
	if d.queue.schedule(repoID) {
		d.requestScan()
	}
}

func (d *deployer) requestScan() {
	d.scanRequested.notify()
}

func (d *deployer) worker(ctx context.Context) {
	for {
		repoID, ok := d.queue.next(ctx)
		if !ok {
			return
		}
		deployCtx, cancel := context.WithTimeout(ctx, d.cfg.DeployTimeout)
		err := d.deploySite(deployCtx, repoID)
		cancel()
		if d.queue.finish(repoID) {
			d.requestScan()
		}
		if err == nil {
			continue
		}
		d.logger.Error("deploying Pages site failed", "repo_id", repoID, "error", err)
		var apiErr *gitea.APIError
		if errors.As(err, &apiErr) && apiErr.Code == gitea.APIErrorStateChanged {
			d.refreshRequested.notify()
		}
	}
}

func (d *deployer) scanLoop(ctx context.Context) {
	ticker := time.NewTicker(deploymentRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.scanRequested:
		case <-ticker.C:
		}
		if err := d.scanPendingSites(ctx); err != nil && ctx.Err() == nil {
			d.logger.Error("scanning pending Pages deployments failed", "error", err)
		}
	}
}

func (d *deployer) scanPendingSites(ctx context.Context) error {
	syncState := d.store.SyncState()
	if !syncState.Initialized {
		return nil
	}
	if err := syncState.ArchiveLimits.Validate(); err != nil {
		return fmt.Errorf("invalid synchronized deployment limits: %w", err)
	}
	now := time.Now()
	return d.store.ScanSites(ctx, func(local storage.SiteState, needsReport bool) error {
		if !siteNeedsDeployment(local, needsReport, syncState.RootUpdatedUnixNano, syncState.RequireGiteaTXT, now) {
			return nil
		}
		if d.queue.enqueueScanned(ctx, local.Site.RepoID) {
			return nil
		}
		return ctx.Err()
	})
}

func siteNeedsDeployment(local storage.SiteState, needsReport bool, rootTime int64, requireGiteaTXT bool, now time.Time) bool {
	site := local.Site
	if needsReport {
		return true
	}
	confirmed := local.ConfirmedResult
	if confirmed == nil || confirmed.AppliedPagesUpdatedUnixNano != site.PagesUpdatedUnixNano || confirmed.AppliedRootUpdatedUnixNano != rootTime {
		return true
	}
	if site.CustomDomain != nil && confirmed.AppliedRequireGiteaTXTRecord != requireGiteaTXT {
		return true
	}
	if confirmed.DefaultPublished != local.DefaultPublished || confirmed.CustomPublished != local.CustomPublished || confirmed.ActualTarGzSHA256 != local.InstalledDigest {
		return true
	}
	if site.Enabled && site.TarGzSHA256 != "" {
		if local.InstalledDigest != site.TarGzSHA256 {
			return true
		}
		unpublished := !local.DefaultPublished || site.CustomDomain != nil && !local.CustomPublished
		return unpublished && now.Sub(time.Unix(0, local.LastAttemptUnixNano)) >= deploymentRetryInterval
	}
	return local.InstalledDigest != "" || local.DefaultPublished || local.CustomPublished
}

func (d *deployer) txtVerificationLoop(ctx context.Context) {
	ticker := time.NewTicker(d.cfg.DNSRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if syncState := d.store.SyncState(); !syncState.Initialized || !syncState.RequireGiteaTXT {
				continue
			}
			d.store.RangeSites(func(local storage.SiteState) bool {
				if local.Site.Enabled && !local.Site.Deleted && local.Site.CustomDomain != nil {
					d.schedule(local.Site.RepoID)
				}
				return true
			})
		}
	}
}
