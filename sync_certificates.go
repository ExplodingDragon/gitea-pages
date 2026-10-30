// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package page

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// certificateLoop keeps the instance certificate usable while synchronization
// is idle. Certificate material belongs to Pages, so this work is deliberately
// local and does not add another Gitea task or state transition.
func (s *Syncer) certificateLoop(ctx context.Context) {
	if s.certificates == nil || s.cfg.RootURL.Scheme != "https" {
		return
	}
	s.refreshCertificates(ctx)
	interval := time.Hour
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refreshCertificates(ctx)
		}
	}
}

func (s *Syncer) refreshCertificates(ctx context.Context) {
	if err := s.ensureDefaultCertificate(ctx); err != nil {
		slog.Error("maintaining default Pages certificate failed", "error", err)
	}
	if err := s.pruneUnusedCertificates(); err != nil {
		slog.Error("cleaning unused Pages certificates failed", "error", err)
	}
	s.enqueueCertificateRenewals()
}

func (s *Syncer) pruneUnusedCertificates() error {
	_, initialized, _, _ := s.store.SyncState()
	if !initialized {
		return nil
	}
	allowed := make(map[string]struct{})
	s.store.ForEachSite(func(local LocalSite) bool {
		if local.Site.Deleted || !local.Site.Enabled {
			return true
		}
		if local.Site.CustomDomain != nil {
			allowed[local.Site.CustomDomain.Host] = struct{}{}
		}
		return true
	})
	changed, err := s.certificates.PruneUnusedCustom(allowed)
	if err != nil || !changed {
		return err
	}
	if p, ok := s.applier.(*NginxPublisher); ok {
		return s.certificates.ExportNginx(p.cfg.NginxRoot)
	}
	return nil
}

func (s *Syncer) enqueueCertificateRenewals() {
	if s.certificates == nil {
		return
	}
	s.store.ForEachSite(func(local LocalSite) bool {
		if local.Site.Deleted || !local.Site.Enabled {
			return true
		}
		if s.cfg.RootURL.Scheme == "https" && local.DefaultPublished && !s.certificates.HasUsable("") {
			s.enqueueSite(local.Site.RepoID)
			return true
		}
		if local.Site.CustomDomain != nil && s.certificates.NeedsRenewal(local.Site.CustomDomain.Host, certificateRenewalWindow) {
			s.enqueueSite(local.Site.RepoID)
		}
		return true
	})
}

func (s *Syncer) ensureDefaultCertificate(ctx context.Context) error {
	if s.certificates == nil || s.cfg.RootURL.Scheme != "https" {
		return nil
	}
	if !s.certificates.NeedsRenewal("", certificateRenewalWindow) {
		if !s.certificates.HasUsable("") {
			return errors.New("default certificate has expired")
		}
		return nil
	}
	if s.acme == nil {
		return errors.New("default certificate needs renewal but DNS-01 is not configured")
	}
	provider, err := NewDNS01Provider(s.cfg.DNSProvider, s.cfg.DNSPresentCommand, s.cfg.DNSCleanupCommand)
	if err != nil {
		return err
	}
	record, err := s.acme.IssueDefaultDNS01(ctx, s.cfg.RootURL.Hostname(), provider)
	if err != nil {
		return err
	}
	if err := s.certificates.Save(ctx, record); err != nil {
		return err
	}
	if p, ok := s.applier.(*NginxPublisher); ok {
		if err := s.certificates.ExportNginx(p.cfg.NginxRoot); err != nil {
			return err
		}
		if err := s.applyRoutes(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Syncer) customDomainAuthorized(ctx context.Context, site Site) (bool, error) {
	if site.CustomDomain == nil {
		return false, nil
	}
	domain := site.CustomDomain
	if !s.requireGiteaTXT.Load() {
		return true, nil
	}
	now := time.Now()
	lastGiteaSuccess := time.Unix(0, domain.TXTLastSuccessUnixNano)
	withinGiteaGrace := domain.TXTLastSuccessUnixNano != 0 && !lastGiteaSuccess.After(now) && now.Sub(lastGiteaSuccess) < txtAuthorizationGracePeriod
	if domain.TXTResult != DomainTXTValid && !withinGiteaGrace {
		return false, fmt.Errorf("domain %s is not authorized by Gitea", domain.Host)
	}
	key := fmt.Sprintf("%d/%s/%s", domain.ID, domain.Host, domain.TXTToken)
	local, present := s.store.GetTXTState(key)
	lastCheck := time.Unix(0, local.LastCheckUnixNano)
	lastLocalSuccess := time.Unix(0, local.LastSuccessUnixNano)
	localValid := local.LastSuccessUnixNano != 0 && !lastLocalSuccess.After(now) && now.Sub(lastLocalSuccess) < txtAuthorizationGracePeriod
	if present && !lastCheck.After(now) && now.Sub(lastCheck) < s.cfg.DNSRefreshInterval {
		if localValid {
			return true, nil
		}
		return false, fmt.Errorf("domain %s has no current local TXT authorization", domain.Host)
	}
	result, err := s.txtLookup(ctx, domain.Host, domain.TXTToken)
	local.LastCheckUnixNano = now.UnixNano()
	switch result {
	case TXTValid:
		local.LastSuccessUnixNano = now.UnixNano()
		localValid = true
	case TXTInvalid, TXTTemporaryError:
		// A failed refresh keeps a recent successful authorization until its
		// fixed grace period expires.
	default:
		return false, errors.New("unknown TXT verification result")
	}
	if saveErr := s.store.SetTXTState(key, local); saveErr != nil {
		return false, saveErr
	}
	if !localValid {
		if err != nil {
			return false, err
		}
		return false, fmt.Errorf("domain %s has no current local TXT authorization", domain.Host)
	}
	return true, nil
}

func (s *Syncer) ensureCustomCertificate(ctx context.Context, site Site) (bool, error) {
	if s.certificates == nil || s.cfg.RootURL.Scheme != "https" {
		return false, nil
	}
	domain := site.CustomDomain
	if domain == nil || s.certificates.HasUsable(domain.Host) && !s.certificates.NeedsRenewal(domain.Host, certificateRenewalWindow) {
		return false, nil
	}
	if s.acme == nil {
		return false, fmt.Errorf("no certificate for %s", domain.Host)
	}
	if len(s.http01Addresses) == 0 {
		return false, errors.New("HTTP-01 probe addresses are not configured")
	}
	if s.challengeResponder == nil {
		return false, errors.New("HTTP-01 challenge responder is not configured")
	}
	record, err := s.acme.IssueHTTP01(ctx, domain.Host, s.challengeResponder, s.http01Addresses)
	if err != nil {
		return false, err
	}
	if err := s.certificates.Save(ctx, record); err != nil {
		return false, err
	}
	return true, nil
}
