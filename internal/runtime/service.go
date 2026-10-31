// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/go-acme/lego/v5/challenge"
)

// Service synchronizes Gitea Pages state and publishes validated content.
type Service struct {
	syncer       *syncer
	store        *store
	certificates *certificateManager
	closeOnce    sync.Once
}

// NewService creates an embeddable Pages service for a Publisher.
func NewService(cfg Config, publisher Publisher, options ...Option) (*Service, error) {
	if publisher == nil {
		return nil, errors.New("pages publisher is required")
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	settings := serviceOptions{logger: slog.Default(), httpClient: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	for _, option := range options {
		option(&settings)
	}
	if target, ok := publisher.(interface{ setLogger(*slog.Logger) }); ok {
		target.setLogger(settings.logger)
	}
	store, err := openStore(cfg.DataRoot, cfg.EtcdQuotaBytes)
	if err != nil {
		return nil, err
	}
	service := &Service{store: store}
	client := &giteaClient{base: cfg.GiteaURL, token: cfg.Token, http: settings.httpClient}
	service.syncer = newSyncer(cfg, client, store, publisher, settings.logger)
	managed, managesCertificates := publisher.(certificatePublisher)
	if managesCertificates && cfg.HTTPSMode == HTTPSPages {
		certificates, acme, dns01Provider, err := configureCertificates(cfg, store)
		if err != nil {
			store.close()
			return nil, err
		}
		service.certificates = certificates
		service.syncer.certificates = certificates
		service.syncer.acme = acme
		service.syncer.dns01Provider = dns01Provider
		service.syncer.http01Addresses = cfg.HTTP01ProbeAddresses
		service.syncer.challengeResponder = managed
	}
	return service, nil
}

func configureCertificates(cfg Config, store *store) (*certificateManager, *acmeManager, challenge.Provider, error) {
	certificates, err := newCertificateManager(store, cfg.CertRoot)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := certificates.ensureDefaultCertificateHosts(cfg.RootURL.Hostname(), "pages-check."+cfg.RootURL.Hostname()); err != nil {
		return nil, nil, nil, err
	}
	var acme *acmeManager
	if cfg.ACMEDirectoryURL != "" {
		acme, err = newACMEManager(store, cfg.ACMEDirectoryURL, cfg.ACMEEmail, cfg.ACMEDNSResolver)
		if err != nil {
			return nil, nil, nil, err
		}
		acme.setExternalAccountBinding(cfg.ACMEEABKID, cfg.ACMEEABKey)
	}
	var dns01Provider challenge.Provider
	if cfg.DNSProvider != "" {
		dns01Provider, err = newDNS01Provider(cfg.DNSProvider, cfg.DNSProviderParameters)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if !certificates.hasUsable("") && (acme == nil || dns01Provider == nil) {
		return nil, nil, nil, errors.New("HTTPS Pages requires a usable default certificate or configured ACME DNS-01 provider")
	}
	return certificates, acme, dns01Provider, nil
}

func (s *Service) Run(ctx context.Context) error { return s.syncer.run(ctx) }

func (s *Service) TLSConfig() *tls.Config {
	if s.certificates == nil {
		return nil
	}
	return s.certificates.tlsConfig()
}

func (s *Service) Close() {
	s.closeOnce.Do(func() {
		s.store.close()
	})
}
