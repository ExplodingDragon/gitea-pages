// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package page

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

type routeApplier interface {
	Apply(context.Context) error
}

// Syncer pulls current Gitea facts, deploys sites, and reports active results.
type Syncer struct {
	cfg                   Config
	client                *Client
	store                 *Store
	server                *Server
	queue                 chan int64
	workers               sync.WaitGroup
	applier               routeApplier
	requireGiteaTXT       atomic.Bool
	txtRequirementChanged atomic.Bool
	certificates          *CertificateManager
	acme                  *ACMEManager
	http01Addresses       []string
	challengeResponder    ChallengeResponder
	txtLookup             func(context.Context, string, string) (TXTVerificationResult, error)
	inflightMu            sync.Mutex
	inflight              map[int64]struct{}
	redeploy              map[int64]struct{}
	syncRequested         chan struct{}
	scanRequested         chan struct{}
	routeApplyMu          sync.Mutex
}

func (s *Syncer) rebuildRoutes() {
	if s.server != nil {
		s.server.RebuildRoutes()
	}
}

func (s *Syncer) updateSiteRoute(repoID int64) {
	if s.server != nil {
		s.server.UpdateSite(repoID)
	}
}

func (s *Syncer) applyRoutes(ctx context.Context) error {
	if s.applier == nil {
		return nil
	}
	s.routeApplyMu.Lock()
	defer s.routeApplyMu.Unlock()
	return s.applier.Apply(ctx)
}

func (s *Syncer) SetCertificateServices(certificates *CertificateManager, acme *ACMEManager, probeAddresses []string, responder ChallengeResponder) {
	s.certificates = certificates
	s.acme = acme
	s.http01Addresses = probeAddresses
	s.challengeResponder = responder
}

// NewSyncer creates the bounded synchronization and deployment workers.
func NewSyncer(cfg Config, client *Client, store *Store, server *Server, applier routeApplier) *Syncer {
	if cfg.DeployQueueSize < 1 {
		cfg.DeployQueueSize = 1
	}
	if cfg.DeployConcurrency < 1 {
		cfg.DeployConcurrency = 1
	}
	if cfg.DeployTimeout <= 0 {
		cfg.DeployTimeout = 10 * time.Minute
	}
	if cfg.DNSRefreshInterval <= 0 {
		cfg.DNSRefreshInterval = 15 * time.Minute
	}
	resolver := newDNSResolver(cfg.DNSResolver)
	s := &Syncer{
		cfg: cfg, client: client, store: store, server: server,
		queue: make(chan int64, cfg.DeployQueueSize), inflight: make(map[int64]struct{}), redeploy: make(map[int64]struct{}),
		syncRequested: make(chan struct{}, 1), scanRequested: make(chan struct{}, 1),
		txtLookup: func(ctx context.Context, host, expected string) (TXTVerificationResult, error) {
			return VerifyTXTResult(ctx, resolver, host, expected)
		},
	}
	if _, requireGiteaTXT, known := store.SyncConfig(); known {
		s.requireGiteaTXT.Store(requireGiteaTXT)
	} else {
		s.requireGiteaTXT.Store(true)
	}
	s.applier = applier
	return s
}

func (s *Syncer) Run(ctx context.Context) error {
	if err := s.applyRoutes(ctx); err != nil {
		slog.Error("restoring Pages entry failed", "error", err)
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
			reg, err := s.client.RegisterInstance(ctx, s.cfg.RootURL)
			if err != nil {
				slog.Error("Pages registration failed", "error", err)
			} else if err := s.store.SetRegistration(reg.RootURL, reg.RootUpdatedUnixNano); err != nil {
				slog.Error("saving Pages registration failed", "error", err)
			} else {
				registered = true
				s.rebuildRoutes()
				if err := s.applyRoutes(ctx); err != nil {
					slog.Error("applying registered Pages entry failed", "error", err)
				}
			}
		}
		if registered {
			if err := s.syncWithStateRetry(ctx); err != nil {
				slog.Error("Pages synchronization failed", "error", err)
			} else if !recoveryQueued || s.txtRequirementChanged.Load() {
				s.requestPendingScan()
				recoveryQueued = true
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-s.store.kv.Errors():
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

func (s *Syncer) txtVerificationLoop(ctx context.Context) {
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
			s.store.ForEachSite(func(local LocalSite) bool {
				if local.Site.Enabled && !local.Site.Deleted && local.Site.CustomDomain != nil {
					s.enqueueSite(local.Site.RepoID)
				}
				return true
			})
		}
	}
}
