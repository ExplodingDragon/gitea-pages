// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

const (
	certificateRenewalWindow    = 30 * 24 * time.Hour
	txtAuthorizationGracePeriod = 24 * time.Hour
	deploymentRetryInterval     = 5 * time.Minute
)

// syncer pulls current Gitea facts, deploys sites, and reports active results.
type syncer struct {
	cfg                   Config
	client                *giteaClient
	store                 *store
	publisher             Publisher
	logger                *slog.Logger
	queue                 chan int64
	workers               sync.WaitGroup
	requireGiteaTXT       atomic.Bool
	txtRequirementChanged atomic.Bool
	certificates          *certificateManager
	acme                  *acmeManager
	http01Addresses       []string
	challengeResponder    challengeResponder
	txtLookup             func(context.Context, string, string) (txtVerificationResult, error)
	inflightMu            sync.Mutex
	inflight              map[int64]struct{}
	redeploy              map[int64]struct{}
	syncRequested         chan struct{}
	scanRequested         chan struct{}
}

func newSyncer(cfg Config, client *giteaClient, store *store, publisher Publisher, logger *slog.Logger) *syncer {
	cfg.applyDefaults()
	resolver := newDNSResolver(cfg.DNSResolver)
	s := &syncer{
		cfg: cfg, client: client, store: store, publisher: publisher, logger: logger,
		queue: make(chan int64, cfg.DeployQueueSize), inflight: make(map[int64]struct{}), redeploy: make(map[int64]struct{}),
		syncRequested: make(chan struct{}, 1), scanRequested: make(chan struct{}, 1),
		txtLookup: func(ctx context.Context, host, expected string) (txtVerificationResult, error) {
			return verifyTXTResult(ctx, resolver, host, expected)
		},
	}
	if _, requireGiteaTXT, known := store.syncConfig(); known {
		s.requireGiteaTXT.Store(requireGiteaTXT)
	} else {
		s.requireGiteaTXT.Store(true)
	}
	return s
}

func (s *syncer) run(ctx context.Context) error {
	if err := s.publisher.Restore(ctx, storedDeployments{cfg: s.cfg, store: s.store}); err != nil {
		return fmt.Errorf("restore Pages publication: %w", err)
	}
	workerCtx, stopWorkers := context.WithCancel(ctx)
	s.startWorkers(workerCtx)
	go s.certificateLoop(workerCtx)
	go s.txtVerificationLoop(workerCtx)
	defer func() {
		stopWorkers()
		s.workers.Wait()
	}()
	t := time.NewTimer(s.cfg.SyncInterval + time.Duration(rand.Int63n(int64(s.cfg.SyncInterval/4+1))))
	defer t.Stop()
	retryTicker := time.NewTicker(deploymentRetryInterval)
	defer retryTicker.Stop()
	registered := false
	recoveryQueued := false
	for {
		if !registered {
			reg, err := s.client.registerInstance(ctx, s.cfg.RootURL)
			if err != nil {
				s.logger.Error("Pages registration failed", "error", err)
			} else if err := s.store.setRegistration(reg.RootURL, reg.RootUpdatedUnixNano); err != nil {
				s.logger.Error("saving Pages registration failed", "error", err)
			} else {
				registered = true
			}
		}
		if registered {
			if err := s.syncWithStateRetry(ctx); err != nil {
				s.logger.Error("Pages synchronization failed", "error", err)
			} else if !recoveryQueued || s.txtRequirementChanged.Load() {
				s.requestPendingScan()
				recoveryQueued = true
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-s.store.kv.errors():
			if err == nil {
				return errors.New("embedded etcd stopped")
			}
			return fmt.Errorf("embedded etcd stopped: %w", err)
		case <-t.C:
			t.Reset(s.cfg.SyncInterval + time.Duration(rand.Int63n(int64(s.cfg.SyncInterval/4+1))))
		case <-retryTicker.C:
			if registered {
				s.requestPendingScan()
			}
		case <-s.syncRequested:
			// The next loop iteration performs the refresh once through the same
			// registration and recovery path as a scheduled synchronization.
		}
	}
}

func (s *syncer) txtVerificationLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.DNSRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.requireGiteaTXT.Load() {
				continue
			}
			s.store.forEachSite(func(local localSite) bool {
				if local.Site.Enabled && !local.Site.Deleted && local.Site.CustomDomain != nil {
					s.enqueueSite(local.Site.RepoID)
				}
				return true
			})
		}
	}
}
