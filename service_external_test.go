// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package page_test

import (
	"context"
	"net/url"
	"testing"

	page "gitea.dev/page"
)

type externalPublisher struct{}

func (externalPublisher) Restore(_ context.Context, deployments page.Deployments) error {
	return deployments.ForEach(func(page.ActiveDeployment) error { return nil })
}

func (externalPublisher) Publish(_ context.Context, deployment page.Deployment) (page.Publication, error) {
	root, err := deployment.Content.Open(".")
	if err == nil {
		err = root.Close()
	}
	return page.Publication{DefaultPublished: err == nil}, err
}

func (externalPublisher) Unpublish(context.Context, int64) error { return nil }

func TestServiceAcceptsExternalPublisher(t *testing.T) {
	giteaURL, err := url.Parse("http://gitea.example.test")
	if err != nil {
		t.Fatal(err)
	}
	rootURL, err := url.Parse("http://pages.example.test")
	if err != nil {
		t.Fatal(err)
	}
	service, err := page.NewService(page.Config{
		GiteaURL: giteaURL,
		RootURL:  rootURL,
		Token:    "instance-token",
		DataRoot: t.TempDir(),
	}, externalPublisher{})
	if err != nil {
		t.Fatal(err)
	}
	service.Close()
}
