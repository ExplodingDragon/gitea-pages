// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pages

import (
	"context"
	"log/slog"

	"gitea.dev/page/internal/certificate"
	pageconfig "gitea.dev/page/internal/config"
	"gitea.dev/page/internal/gitea"
	"gitea.dev/page/internal/storage"
)

// service owns the local state, synchronization, and certificate lifecycle.
type service struct {
	syncer       *syncer
	deployer     *deployer
	store        *storage.Store
	certificates *certificate.Manager
}

func newService(cfg pageconfig.Config, publisher *httpPublisher) (*service, error) {
	store, err := storage.Open(cfg.DataRoot, cfg.EtcdQuotaBytes)
	if err != nil {
		return nil, err
	}
	pagesService := &service{store: store}
	if cfg.HTTPSMode == pageconfig.HTTPSPages {
		pagesService.certificates, err = certificate.New(store.State(), certificate.Options{
			RootHost:              cfg.RootURL.Hostname(),
			CertificateRoot:       cfg.CertRoot,
			DirectoryURL:          cfg.ACMEDirectoryURL,
			Email:                 cfg.ACMEEmail,
			EABKeyID:              cfg.ACMEEABKID,
			EABHMACKey:            cfg.ACMEEABKey,
			DNSResolver:           cfg.ACMEDNSResolver,
			DNSProvider:           cfg.DNSProvider,
			DNSProviderParameters: cfg.DNSProviderParameters,
			HTTP01Addresses:       cfg.HTTP01ProbeAddresses,
		}, publisher)
		if err != nil {
			store.Close()
			return nil, err
		}
	}
	client := gitea.NewClient(cfg.GiteaURL, cfg.Token)
	logger := slog.Default()
	pagesService.deployer = newDeployer(cfg, client, store, publisher, pagesService.certificates, logger)
	pagesService.syncer = newSyncer(cfg, client, store, pagesService.deployer, logger)
	return pagesService, nil
}

func (s *service) run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	if err := s.deployer.start(runCtx); err != nil {
		cancel()
		return err
	}
	defer func() {
		cancel()
		s.deployer.wait()
	}()
	return s.syncer.run(runCtx)
}
