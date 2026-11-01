// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pages

import (
	"log/slog"
	"testing"

	"gitea.dev/page/internal/artifact"
	pageconfig "gitea.dev/page/internal/config"
	"gitea.dev/page/internal/gitea"
	"gitea.dev/page/internal/storage"
)

const testEtcdQuotaBytes = int64(64 * 1024 * 1024)

func testArchiveLimits() artifact.Limits {
	return artifact.Limits{
		MaxTarGzBytes: 100 * 1024 * 1024,
		MaxSiteBytes:  512 * 1024 * 1024,
		MaxFileBytes:  100 * 1024 * 1024,
		MaxEntries:    10_000,
	}
}

func setTestSites(t *testing.T, store *storage.Store, rootURL string, cursor int64, sites []gitea.Site) {
	t.Helper()
	listing := make(map[int64]gitea.Site, len(sites))
	for _, site := range sites {
		listing[site.RepoID] = site
	}
	if err := store.ReplaceSites(t.Context(), rootURL, 1, cursor, listing, testArchiveLimits(), true); err != nil {
		t.Fatal(err)
	}
}

func newTestGiteaClient(cfg pageconfig.Config) *gitea.Client {
	return gitea.NewClient(cfg.GiteaURL, cfg.Token)
}

func newTestSyncer(cfg pageconfig.Config, client *gitea.Client, store *storage.Store, publisher sitePublisher) *syncer {
	logger := slog.Default()
	deployer := newDeployer(cfg, client, store, publisher, nil, logger)
	return newSyncer(cfg, client, store, deployer, logger)
}
