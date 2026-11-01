// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pages

import (
	"context"
	"time"

	"gitea.dev/page/internal/storage"
)

// certificateLoop keeps certificate material usable while synchronization is idle.
func (d *deployer) certificateLoop(ctx context.Context) {
	if d.certificates == nil {
		return
	}
	d.refreshCertificates(ctx)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.refreshCertificates(ctx)
		}
	}
}

func (d *deployer) refreshCertificates(ctx context.Context) {
	if err := d.certificates.EnsureDefault(ctx); err != nil {
		d.logger.Error("maintaining default Pages certificate failed", "error", err)
	}
	if err := d.pruneUnusedCertificates(ctx); err != nil {
		d.logger.Error("cleaning unused Pages certificates failed", "error", err)
	}
	d.enqueueCertificateRenewals()
}

func (d *deployer) pruneUnusedCertificates(ctx context.Context) error {
	if !d.store.SyncState().Initialized {
		return nil
	}
	activeHosts := make(map[string]struct{})
	d.store.RangeSites(func(local storage.SiteState) bool {
		if local.Site.Enabled && !local.Site.Deleted && local.Site.CustomDomain != nil {
			activeHosts[local.Site.CustomDomain.Host] = struct{}{}
		}
		return true
	})
	return d.certificates.Prune(ctx, activeHosts)
}

func (d *deployer) enqueueCertificateRenewals() {
	d.store.RangeSites(func(local storage.SiteState) bool {
		if local.Site.Deleted || !local.Site.Enabled {
			return true
		}
		if local.DefaultPublished && !d.certificates.HasUsable("") {
			d.schedule(local.Site.RepoID)
			return true
		}
		if local.Site.CustomDomain != nil && d.certificates.NeedsRenewal(local.Site.CustomDomain.Host) {
			d.schedule(local.Site.RepoID)
		}
		return true
	})
}
