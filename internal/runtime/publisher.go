// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
)

// Content is an immutable, validated website directory prepared by Service.
// A Publisher may retain it until the next successful Publish or Unpublish for
// the same repository.
type Content struct {
	digest string
	root   string
}

// Digest returns the SHA-256 digest of the source tar.gz archive.
func (c Content) Digest() string { return c.digest }

// Path returns the immutable extracted directory. Publishers must treat it as
// read-only and stop using it when a later Publish or Unpublish succeeds.
func (c Content) Path() string { return c.root }

// Open opens a path relative to the immutable extracted directory.
func (c Content) Open(name string) (fs.File, error) { return os.DirFS(c.root).Open(name) }

// Deployment is the complete desired publication for one repository.
type Deployment struct {
	RepositoryID int64
	DefaultURL   url.URL
	CustomURL    *url.URL
	Content      Content
}

// PublicationProblem describes an address failure after its configuration was
// applied. A non-nil Publish error instead means the previous publication was
// retained.
type PublicationProblem string

const (
	PublicationProblemNone               PublicationProblem = ""
	PublicationProblemCertificatePending PublicationProblem = "certificate_pending"
	PublicationProblemACMEFailed         PublicationProblem = "acme_failed"
	PublicationProblemEntryProbeFailed   PublicationProblem = "entry_probe_failed"
)

// Publication records the addresses confirmed by a Publisher.
type Publication struct {
	DefaultPublished bool
	CustomPublished  bool
	Problem          PublicationProblem
}

// ActiveDeployment is one locally confirmed deployment restored at startup.
type ActiveDeployment struct {
	Deployment
	Publication
}

// Deployments streams active deployments without requiring another complete
// in-memory copy for a large Pages instance.
type Deployments interface {
	ForEach(func(ActiveDeployment) error) error
}

// Publisher applies website content to visitor-facing routes. Calls for one
// repository are serialized; different repositories may be published in
// parallel. Publish must restore the previous publication before returning an
// error.
type Publisher interface {
	Restore(context.Context, Deployments) error
	Publish(context.Context, Deployment) (Publication, error)
	Unpublish(context.Context, int64) error
}

type certificatePublisher interface {
	Publisher
	challengeResponder
}

func deploymentForSite(cfg Config, site site, content Content, custom bool) (Deployment, error) {
	host, base, ok := defaultSiteRoute(site, cfg.RootURL.Hostname())
	if !ok {
		return Deployment{}, errors.New("invalid default Pages address")
	}
	defaultURL, err := url.Parse(fmt.Sprintf("%s://%s%s", cfg.RootURL.Scheme, host, base))
	if err != nil {
		return Deployment{}, err
	}
	deployment := Deployment{RepositoryID: site.RepoID, DefaultURL: *defaultURL, Content: content}
	if custom && site.CustomDomain != nil {
		customURL, err := url.Parse(fmt.Sprintf("%s://%s/", cfg.RootURL.Scheme, site.CustomDomain.Host))
		if err != nil {
			return Deployment{}, err
		}
		deployment.CustomURL = customURL
	}
	return deployment, nil
}

type storedDeployments struct {
	cfg   Config
	store *store
}

func (d storedDeployments) ForEach(fn func(ActiveDeployment) error) error {
	_, initialized, rootURL, _ := d.store.syncState()
	if !initialized || rootURL != d.cfg.RootURL.String() {
		return nil
	}
	var callbackErr error
	d.store.forEachSite(func(local localSite) bool {
		if local.InstalledDigest == "" || !local.DefaultPublished && !local.CustomPublished {
			return true
		}
		content, ok := d.store.installedContent(local.Site.RepoID, local.InstalledDigest)
		if !ok {
			return true
		}
		deployment, err := deploymentForSite(d.cfg, local.Site, content, local.Site.CustomDomain != nil)
		if err != nil {
			callbackErr = err
			return false
		}
		callbackErr = fn(ActiveDeployment{
			Deployment: deployment,
			Publication: Publication{
				DefaultPublished: local.DefaultPublished,
				CustomPublished:  local.CustomPublished,
			},
		})
		return callbackErr == nil
	})
	return callbackErr
}
