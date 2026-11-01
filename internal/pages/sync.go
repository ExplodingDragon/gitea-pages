// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	pageconfig "gitea.dev/page/internal/config"
	"gitea.dev/page/internal/gitea"
	"gitea.dev/page/internal/storage"
)

// syncer registers the instance and saves complete Gitea site facts before
// asking the deployer to apply affected repositories.
type syncer struct {
	cfg      pageconfig.Config
	client   *gitea.Client
	store    *storage.Store
	deployer *deployer
	logger   *slog.Logger
}

func newSyncer(cfg pageconfig.Config, client *gitea.Client, store *storage.Store, deployer *deployer, logger *slog.Logger) *syncer {
	cfg.ApplyDefaults()
	return &syncer{cfg: cfg, client: client, store: store, deployer: deployer, logger: logger}
}

func (s *syncer) run(ctx context.Context) error {
	nextSyncDelay := func() time.Duration {
		return s.cfg.SyncInterval + time.Duration(rand.Int64N(int64(s.cfg.SyncInterval/4+1)))
	}
	timer := time.NewTimer(nextSyncDelay())
	defer timer.Stop()
	registered := false
	recoveryQueued := false
	for {
		if !registered {
			registration, err := s.client.RegisterInstance(ctx, s.cfg.RootURL)
			if err != nil {
				s.logger.Error("Pages registration failed", "error", err)
			} else if err := s.store.SetRegistration(ctx, registration.RootURL, registration.RootUpdatedUnixNano); err != nil {
				s.logger.Error("saving Pages registration failed", "error", err)
			} else {
				registered = true
			}
		}
		if registered {
			if err := s.syncWithStateRetry(ctx); err != nil {
				s.logger.Error("Pages synchronization failed", "error", err)
			} else if !recoveryQueued {
				s.deployer.requestScan()
				recoveryQueued = true
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-s.store.Errors():
			if err == nil {
				return errors.New("embedded etcd stopped")
			}
			return fmt.Errorf("embedded etcd stopped: %w", err)
		case <-timer.C:
			timer.Reset(nextSyncDelay())
		case <-s.deployer.refreshRequested:
			// The next iteration refreshes state through the same registration and
			// synchronization path as the regular timer.
		}
	}
}
