// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pages

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	pageconfig "gitea.dev/page/internal/config"
	"gitea.dev/page/internal/gitea"
	"gitea.dev/page/internal/storage"
)

// siteDeployment is the complete desired publication for one repository.
type siteDeployment struct {
	repoID     int64
	defaultURL url.URL
	customURL  *url.URL
	content    storage.Content
}

// publicationProblem describes an address failure after its configuration was
// applied. A publish error instead means the previous publication was
// retained.
type publicationProblem string

const (
	publicationProblemNone               publicationProblem = ""
	publicationProblemCertificatePending publicationProblem = "certificate_pending"
	publicationProblemACMEFailed         publicationProblem = "acme_failed"
	publicationProblemEntryProbeFailed   publicationProblem = "entry_probe_failed"
)

// publicationResult records the addresses confirmed by the built-in server.
type publicationResult struct {
	defaultPublished bool
	customPublished  bool
	problem          publicationProblem
}

// activeDeployment is one locally confirmed deployment restored at startup.
type activeDeployment struct {
	siteDeployment
	publicationResult
}

// deploymentSource streams active deployments without another complete copy
// in memory for a large Pages instance.
type deploymentSource interface {
	forEach(func(activeDeployment) error) error
}

// sitePublisher applies website content to visitor-facing routes. Calls for one
// repository are serialized; different repositories may be published in
// parallel. A failed publish restores the previous publication before it
// returns.
type sitePublisher interface {
	restore(context.Context, deploymentSource) error
	publish(context.Context, siteDeployment) (publicationResult, error)
	unpublish(context.Context, int64) error
}

func deploymentForSite(cfg pageconfig.Config, site gitea.Site, content storage.Content, custom bool) (siteDeployment, error) {
	host, base, ok := defaultSiteRoute(site, cfg.RootURL.Hostname())
	if !ok {
		return siteDeployment{}, errors.New("invalid default Pages address")
	}
	defaultURL, err := url.Parse(fmt.Sprintf("%s://%s%s", cfg.RootURL.Scheme, host, base))
	if err != nil {
		return siteDeployment{}, err
	}
	deployment := siteDeployment{repoID: site.RepoID, defaultURL: *defaultURL, content: content}
	if custom && site.CustomDomain != nil {
		customURL, err := url.Parse(fmt.Sprintf("%s://%s/", cfg.RootURL.Scheme, site.CustomDomain.Host))
		if err != nil {
			return siteDeployment{}, err
		}
		deployment.customURL = customURL
	}
	return deployment, nil
}

type storedDeployments struct {
	cfg   pageconfig.Config
	store *storage.Store
}

func (d storedDeployments) forEach(fn func(activeDeployment) error) error {
	syncState := d.store.SyncState()
	if !syncState.Initialized || syncState.RootURL != d.cfg.RootURL.String() {
		return nil
	}
	var callbackErr error
	d.store.RangeSites(func(local storage.SiteState) bool {
		if local.InstalledDigest == "" || !local.DefaultPublished && !local.CustomPublished {
			return true
		}
		content, ok := d.store.InstalledContent(local.Site.RepoID, local.InstalledDigest)
		if !ok {
			return true
		}
		deployment, err := deploymentForSite(d.cfg, local.Site, content, local.Site.CustomDomain != nil)
		if err != nil {
			callbackErr = err
			return false
		}
		callbackErr = fn(activeDeployment{
			siteDeployment: deployment,
			publicationResult: publicationResult{
				defaultPublished: local.DefaultPublished,
				customPublished:  local.CustomPublished,
			},
		})
		return callbackErr == nil
	})
	return callbackErr
}
