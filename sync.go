package page

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
)

type Syncer struct {
	cfg                Config
	client             *Client
	store              *Store
	server             *Server
	queue              chan Site
	workers            sync.WaitGroup
	applier            interface{ Apply(context.Context) error }
	txtEnabled         atomic.Bool
	txtPolicyTime      atomic.Int64
	txtPolicyChanged   atomic.Bool
	certificates       *CertificateManager
	acme               *ACMEManager
	http01Addresses    []string
	challengeResponder ChallengeResponder
	txtLookup          func(context.Context, string, string) (TXTVerificationResult, error)
	inflightMu         sync.Mutex
	inflight           map[int64]struct{}
	refresh            chan struct{}
	applyMu            sync.Mutex
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

func (s *Syncer) apply(ctx context.Context) error {
	if s.applier == nil {
		return nil
	}
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	return s.applier.Apply(ctx)
}

func (s *Syncer) SetCertificateServices(certificates *CertificateManager, acme *ACMEManager, probeAddresses []string, responder ChallengeResponder) {
	s.certificates = certificates
	s.acme = acme
	s.http01Addresses = probeAddresses
	s.challengeResponder = responder
}

func (s *Syncer) updateTXTPolicy(response SitesResponse) error {
	changed, err := s.store.SetTXTPolicy(response.TXTVerificationEnabled, response.TXTPolicyUpdatedUnixNano)
	if err != nil {
		return err
	}
	s.txtEnabled.Store(response.TXTVerificationEnabled)
	s.txtPolicyTime.Store(response.TXTPolicyUpdatedUnixNano)
	if changed {
		s.txtPolicyChanged.Store(true)
	}
	return nil
}

func NewSyncer(cfg Config, client *Client, store *Store, server *Server, applier interface{ Apply(context.Context) error }) *Syncer {
	if cfg.DeployQueueSize < 1 {
		cfg.DeployQueueSize = 1
	}
	if cfg.DeployConcurrency < 1 {
		cfg.DeployConcurrency = 1
	}
	if cfg.DeployTimeout <= 0 {
		cfg.DeployTimeout = 10 * time.Minute
	}
	s := &Syncer{cfg: cfg, client: client, store: store, server: server, queue: make(chan Site, cfg.DeployQueueSize), inflight: make(map[int64]struct{}), refresh: make(chan struct{}, 1), txtLookup: VerifyTXTResult}
	if enabled, updated, known := store.TXTPolicy(); known {
		s.txtEnabled.Store(enabled)
		s.txtPolicyTime.Store(updated)
	} else {
		s.txtEnabled.Store(true)
	}
	s.applier = applier
	return s
}

func (s *Syncer) Run(ctx context.Context) error {
	if err := s.apply(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "pages restore entry: %v\n", err)
	}
	workerCtx, stopWorkers := context.WithCancel(ctx)
	s.StartWorkers(workerCtx)
	go s.certificateLoop(workerCtx)
	go s.txtLoop(workerCtx)
	defer func() {
		stopWorkers()
		s.WaitWorkers()
	}()
	t := time.NewTimer(s.cfg.SyncInterval + time.Duration(rand.Int63n(int64(s.cfg.SyncInterval/4+1))))
	defer t.Stop()
	registered := false
	for {
		if !registered {
			reg, err := s.client.RegisterInstance(ctx, s.cfg.RootURL)
			if err != nil {
				fmt.Fprintf(os.Stderr, "pages registration: %v\n", err)
			} else if err := s.store.SetRegistration(reg.RootURL, reg.RootUpdatedUnixNano); err != nil {
				fmt.Fprintf(os.Stderr, "pages registration state: %v\n", err)
			} else {
				registered = true
				s.rebuildRoutes()
				if err := s.apply(ctx); err != nil {
					fmt.Fprintf(os.Stderr, "pages registered entry: %v\n", err)
				}
			}
		}
		if registered {
			if err := s.syncWithRefresh(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "pages sync: %v\n", err)
			}
			s.enqueuePending()
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-s.store.kv.Errors():
			if err == nil {
				return fmt.Errorf("embedded etcd stopped")
			}
			return fmt.Errorf("embedded etcd stopped: %w", err)
		case <-t.C:
			t.Reset(s.cfg.SyncInterval + time.Duration(rand.Int63n(int64(s.cfg.SyncInterval/4+1))))
		case <-s.refresh:
			if registered {
				if err := s.syncWithRefresh(ctx); err != nil {
					fmt.Fprintf(os.Stderr, "pages state refresh: %v\n", err)
				}
				s.enqueuePending()
			}
		}
	}
}

func (s *Syncer) txtLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.txtEnabled.Load() {
				continue
			}
			s.store.ForEachSite(func(local LocalSite) bool {
				if local.Site.Enabled && !local.Site.Deleted && len(local.Site.CustomDomains) > 0 {
					s.enqueue(local.Site)
				}
				return true
			})
		}
	}
}

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
		fmt.Fprintf(os.Stderr, "pages default certificate: %v\n", err)
	}
	if err := s.pruneUnusedCertificates(); err != nil {
		fmt.Fprintf(os.Stderr, "pages certificate cleanup: %v\n", err)
	}
	s.enqueueCertificateRenewals()
}

func (s *Syncer) pruneUnusedCertificates() error {
	_, initialized, _, _ := s.store.State()
	if !initialized {
		return nil
	}
	allowed := make(map[string]struct{})
	s.store.ForEachSite(func(local LocalSite) bool {
		if local.Site.Deleted || !local.Site.Enabled {
			return true
		}
		for _, domain := range local.Site.CustomDomains {
			if domain.State == "active" {
				allowed[domain.Host] = struct{}{}
			}
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
			s.enqueue(local.Site)
			return true
		}
		for _, domain := range local.Site.CustomDomains {
			if domain.State == "active" && s.certificates.NeedsRenewal(domain.Host, 30*24*time.Hour) {
				s.enqueue(local.Site)
				break
			}
		}
		return true
	})
}

func (s *Syncer) ensureDefaultCertificate(ctx context.Context) error {
	if s.certificates == nil || s.cfg.RootURL.Scheme != "https" {
		return nil
	}
	if !s.certificates.NeedsRenewal("", 30*24*time.Hour) {
		if !s.certificates.HasUsable("") {
			return fmt.Errorf("default certificate has expired")
		}
		return nil
	}
	if s.acme == nil {
		return fmt.Errorf("default certificate needs renewal but DNS-01 is not configured")
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
		if err := s.apply(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Syncer) syncWithRefresh(ctx context.Context) error {
	for attempt := 0; attempt < 2; attempt++ {
		err := s.syncOnce(ctx)
		if err == nil {
			return nil
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Code != "state_changed" {
			return err
		}
	}
	return fmt.Errorf("state changed repeatedly while deploying")
}

func (s *Syncer) syncOnce(ctx context.Context) error {
	cursor, initialized, rootURL, rootTime := s.store.State()
	if !initialized {
		return s.full(ctx)
	}
	resp, err := s.client.ListSites(ctx, false, cursor, 0, 0, 0)
	if err != nil {
		return err
	}
	if resp.RootURL != rootURL || resp.RootUpdatedUnixNano != rootTime {
		return s.full(ctx)
	}
	if err := s.updateTXTPolicy(resp); err != nil {
		return err
	}
	until := resp.Until
	next := resp.NextSince
	if err := s.saveSites(resp.Sites, true); err != nil {
		return err
	}
	at, ar := int64(0), int64(0)
	for resp.NextCursor != nil || len(resp.Sites) == 100 {
		if until == 0 {
			until = resp.Until
		}
		if len(resp.Sites) > 0 {
			last := resp.Sites[len(resp.Sites)-1]
			at, ar = last.PagesUpdatedUnixNano, last.RepoID
		}
		if resp.NextCursor != nil {
			at, ar = resp.NextCursor.PagesUpdatedUnixNano, resp.NextCursor.RepoID
		}
		resp, err = s.client.ListSites(ctx, false, cursor, until, at, ar)
		if err != nil {
			return err
		}
		if resp.RootURL != rootURL || resp.RootUpdatedUnixNano != rootTime {
			return s.full(ctx)
		}
		if err := s.updateTXTPolicy(resp); err != nil {
			return err
		}
		if err := s.saveSites(resp.Sites, true); err != nil {
			return err
		}
		if resp.NextSince > next {
			next = resp.NextSince
		}
	}
	if next == 0 {
		next = until
	}
	return s.store.SetCursor(next)
}

func (s *Syncer) full(ctx context.Context) error {
	var until, at, ar int64
	var rootURL string
	var rootTime int64
	var policyEnabled bool
	var policyTime int64
	sites := make(map[int64]Site)
	for {
		resp, err := s.client.ListSites(ctx, true, 0, until, at, ar)
		if err != nil {
			return err
		}
		if rootURL == "" {
			rootURL, rootTime = resp.RootURL, resp.RootUpdatedUnixNano
		} else if resp.RootURL != rootURL || resp.RootUpdatedUnixNano != rootTime {
			return &APIError{Code: "state_changed", Message: "pages registration changed during full listing"}
		}
		policyEnabled, policyTime = resp.TXTVerificationEnabled, resp.TXTPolicyUpdatedUnixNano
		if until == 0 {
			until = resp.Until
		}
		for _, site := range resp.Sites {
			sites[site.RepoID] = site
		}
		if resp.NextCursor == nil && len(resp.Sites) < 100 {
			if resp.NextSince > until {
				until = resp.NextSince
			}
			break
		}
		if resp.NextCursor != nil {
			at, ar = resp.NextCursor.PagesUpdatedUnixNano, resp.NextCursor.RepoID
		} else if len(resp.Sites) > 0 {
			last := resp.Sites[len(resp.Sites)-1]
			at, ar = last.PagesUpdatedUnixNano, last.RepoID
		}
	}
	// The full listing has a fixed upper bound. One incremental pass closes
	// the gap created while the listing was being paged.
	if until > 0 {
		initialUntil := until
		catchUntil, at, ar := int64(0), int64(0), int64(0)
		for {
			updates, err := s.client.ListSites(ctx, false, initialUntil, catchUntil, at, ar)
			if err != nil {
				return err
			}
			if updates.RootURL != rootURL || updates.RootUpdatedUnixNano != rootTime {
				return &APIError{Code: "state_changed", Message: "pages registration changed during full catch-up"}
			}
			policyEnabled, policyTime = updates.TXTVerificationEnabled, updates.TXTPolicyUpdatedUnixNano
			for _, site := range updates.Sites {
				sites[site.RepoID] = site
			}
			if updates.NextSince > until {
				until = updates.NextSince
			}
			if updates.NextCursor == nil {
				break
			}
			catchUntil = updates.Until
			at, ar = updates.NextCursor.PagesUpdatedUnixNano, updates.NextCursor.RepoID
		}
	}
	if rootURL == "" {
		return fmt.Errorf("full sites response did not establish a snapshot")
	}
	listing := make([]Site, 0, len(sites))
	for _, site := range sites {
		listing = append(listing, site)
	}
	if err := s.store.ReplaceSites(rootURL, rootTime, until, listing, policyEnabled, policyTime); err != nil {
		return err
	}
	if err := s.store.CleanupUntrackedContent(); err != nil {
		fmt.Fprintf(os.Stderr, "pages untracked content cleanup: %v\n", err)
	}
	s.txtEnabled.Store(policyEnabled)
	s.txtPolicyTime.Store(policyTime)
	for _, report := range s.store.PendingReports() {
		if _, ok := sites[report.RepoID]; ok {
			continue
		}
		if err := s.store.ClearPendingReport(report.RepoID, report); err != nil {
			fmt.Fprintf(os.Stderr, "pages site %d report cleanup: %v\n", report.RepoID, err)
		}
	}
	s.rebuildRoutes()
	return nil
}

// saveSites records the response before a deployment worker handles it. Full
// snapshots intentionally leave route rebuilding and queueing to enqueuePending
// after the snapshot is complete, while incremental responses can publish each
// site as soon as its current facts are durable.
func (s *Syncer) saveSites(sites []Site, enqueue bool) error {
	for _, site := range sites {
		if err := s.store.Upsert(site); err != nil {
			return err
		}
	}
	if enqueue && len(sites) > 0 {
		s.rebuildRoutes()
		for _, site := range sites {
			s.enqueue(site)
		}
	}
	return nil
}

func (s *Syncer) deploy(ctx context.Context, target Site) error {
	current, ok := s.store.Get(target.RepoID)
	if !ok {
		return nil
	}
	// The queue carries only an identity. A newer synchronization may already
	// have replaced the intended target while this item was waiting.
	site := current.Site
	if site.Deleted || !site.Enabled || site.TarGzSHA256 == "" {
		if err := s.store.RemoveContent(site.RepoID); err != nil {
			return s.reportDeploymentFailure(ctx, site, "content_install_failed", err)
		}
		applied, err := s.store.SetResultForSite(site, "", false, false, "")
		if err != nil || !applied {
			return err
		}
		s.updateSiteRoute(site.RepoID)
		if err := s.apply(ctx); err != nil {
			return s.reportDeploymentFailure(ctx, site, "route_apply_failed", err)
		}
		if err := s.probePlatform(ctx); err != nil {
			return s.reportDeploymentFailure(ctx, site, "entry_probe_failed", err)
		}
		return s.reportConfig(ctx, site, "", true, false, false, "")
	}

	if current.InstalledDigest != site.TarGzSHA256 || !s.store.InstalledMatches(site.RepoID, site.TarGzSHA256) {
		work, err := s.store.WorkDir(site.RepoID)
		if err != nil {
			return s.reportDeploymentFailure(ctx, site, "content_install_failed", err)
		}
		defer func() { _ = os.RemoveAll(work) }()
		archive := work + "/site.tar.gz"
		f, err := os.OpenFile(archive, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return s.reportDeploymentFailure(ctx, site, "content_install_failed", err)
		}
		err = s.client.DownloadContent(ctx, site.RepoID, site.TarGzSHA256, f)
		closeErr := f.Close()
		if err != nil {
			return s.reportDeploymentFailure(ctx, site, "content_download_failed", err)
		}
		if closeErr != nil {
			return s.reportDeploymentFailure(ctx, site, "content_install_failed", closeErr)
		}
		candidate := work + "/content"
		af, err := os.Open(archive)
		if err != nil {
			return s.reportDeploymentFailure(ctx, site, "content_invalid", err)
		}
		err = ExtractTarGz(ctx, af, candidate, site.RepoID, site.TarGzSHA256)
		closeErr = af.Close()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return s.reportDeploymentFailure(ctx, site, "content_install_failed", ctxErr)
			}
			return s.reportDeploymentFailure(ctx, site, "content_invalid", err)
		}
		if closeErr != nil {
			return s.reportDeploymentFailure(ctx, site, "content_invalid", closeErr)
		}
		latest, ok := s.store.Get(site.RepoID)
		if !ok || latest.Site.PagesUpdatedUnixNano != site.PagesUpdatedUnixNano || latest.Site.TarGzSHA256 != site.TarGzSHA256 {
			return nil
		}
		if err := s.store.Install(site.RepoID, site.TarGzSHA256, candidate); err != nil {
			return s.reportDeploymentFailure(ctx, site, "content_install_failed", err)
		}
	}
	return s.publishCurrent(ctx, site)
}

func (s *Syncer) publishCurrent(ctx context.Context, site Site) error {
	customReady, customErr := s.customReady(ctx, site)
	code := ""
	if customErr != nil {
		code = "txt_invalid"
	}
	certChanged := false
	if customReady {
		needsCertificate := false
		if s.certificates != nil && s.cfg.RootURL.Scheme == "https" {
			for _, domain := range site.CustomDomains {
				if domain.State == "active" && s.certificates.NeedsRenewal(domain.Host, 30*24*time.Hour) {
					needsCertificate = true
					break
				}
			}
		}
		if _, isNginx := s.applier.(*NginxPublisher); isNginx && needsCertificate {
			if err := s.apply(ctx); err != nil {
				return s.reportDeploymentFailure(ctx, site, "route_apply_failed", err)
			}
		}
		if renewed, err := s.ensureCertificates(ctx, site); err != nil {
			certChanged = renewed
			customReady = s.allCustomCertificatesUsable(site)
			if customReady {
				code = "acme_failed"
			} else {
				code = "certificate_pending"
			}
			fmt.Fprintf(os.Stderr, "pages certificate for repo %d: %v\n", site.RepoID, err)
		} else {
			certChanged = renewed
		}
	}
	// Candidate flags open only the currently intended routes. Publication is
	// confirmed after the local listener returns the exact probe body.
	previous, _ := s.store.Get(site.RepoID)
	applied, err := s.store.SetResultForSite(site, site.TarGzSHA256, true, customReady, code)
	if err != nil || !applied {
		return err
	}
	s.updateSiteRoute(site.RepoID)
	if p, ok := s.applier.(*NginxPublisher); ok && s.certificates != nil && certChanged {
		if err := s.certificates.ExportNginx(p.cfg.NginxRoot); err != nil {
			return s.failCandidatePublication(ctx, site, "route_apply_failed", err)
		}
	}
	if p, isNginx := s.applier.(*NginxPublisher); isNginx {
		if !previous.DefaultPublished || previous.CustomPublished != customReady || certChanged || p.confirmedConfigDigest() == "" {
			if err := s.apply(ctx); err != nil {
				return s.failCandidatePublication(ctx, site, "route_apply_failed", err)
			}
		}
	} else if err := s.apply(ctx); err != nil {
		return s.failCandidatePublication(ctx, site, "route_apply_failed", err)
	}
	if err := s.probePlatform(ctx); err != nil {
		return s.failCandidatePublication(ctx, site, "entry_probe_failed", err)
	}
	base := "/"
	if site.RepoName != ".pages" {
		base = "/" + site.RepoName + "/"
	}
	defaultReady := false
	if host := ownerHost(site.OwnerName, s.cfg.RootURL.Hostname()); host != "" {
		if err := s.probeEntry(ctx, host, base, site.RepoID, site.TarGzSHA256); err == nil {
			defaultReady = true
		} else {
			code = "entry_probe_failed"
			fmt.Fprintf(os.Stderr, "pages default entry for repo %d: %v\n", site.RepoID, err)
		}
	} else {
		code = "entry_probe_failed"
	}
	if customReady {
		for _, domain := range site.CustomDomains {
			if domain.State != "active" {
				continue
			}
			if err := s.probeEntry(ctx, domain.Host, "/", site.RepoID, site.TarGzSHA256); err != nil {
				customReady = false
				if code == "" {
					code = "entry_probe_failed"
				}
				fmt.Fprintf(os.Stderr, "pages custom entry for repo %d: %v\n", site.RepoID, err)
				break
			}
		}
	}
	applied, err = s.store.SetResultForSite(site, site.TarGzSHA256, defaultReady, customReady, code)
	if err != nil || !applied {
		return err
	}
	if !defaultReady || len(site.CustomDomains) > 0 && !customReady {
		s.updateSiteRoute(site.RepoID)
		if err := s.apply(ctx); err != nil {
			return s.failCandidatePublication(ctx, site, "route_apply_failed", err)
		}
	}
	return s.reportConfig(ctx, site, site.TarGzSHA256, true, defaultReady, customReady, code)
}

// enqueuePending retries targets that could not enter the bounded queue, and
// resumes unfinished deployment after a process restart. The target itself is
// already persisted in the local state, so no separate work table is needed.
func (s *Syncer) enqueuePending() {
	_, initialized, _, rootTime := s.store.State()
	if !initialized {
		return
	}
	policyChanged := s.txtPolicyChanged.Swap(false)
	pending := make(map[int64]struct{})
	for _, result := range s.store.PendingReports() {
		pending[result.RepoID] = struct{}{}
	}
	now := time.Now()
	s.store.ForEachSite(func(local LocalSite) bool {
		site := local.Site
		_, needsReport := pending[site.RepoID]
		policyTime := int64(0)
		if !site.Deleted && len(site.CustomDomains) > 0 {
			policyTime = s.txtPolicyTime.Load()
		}
		confirmed := local.Confirmed
		needsCurrentResult := confirmed == nil || confirmed.AppliedPagesUpdatedUnixNano != site.PagesUpdatedUnixNano || confirmed.AppliedRootUpdatedUnixNano != rootTime || confirmed.AppliedTXTPolicyUpdatedUnixNano != policyTime
		if confirmed != nil && (confirmed.DefaultPublished != local.DefaultPublished || confirmed.CustomPublished != local.CustomPublished || confirmed.ActualTarGzSHA256 != local.InstalledDigest) {
			needsCurrentResult = true
		}
		needsContent := site.Enabled && site.TarGzSHA256 != "" && local.InstalledDigest != site.TarGzSHA256
		needsRetry := site.Enabled && site.TarGzSHA256 != "" && (!local.DefaultPublished || len(site.CustomDomains) > 0 && !local.CustomPublished) && now.Sub(time.Unix(0, local.LastAttemptUnixNano)) >= 5*time.Minute
		needsCleanup := (site.Deleted || !site.Enabled || site.TarGzSHA256 == "") && (local.InstalledDigest != "" || local.DefaultPublished || local.CustomPublished)
		if needsReport || needsCurrentResult || needsContent || needsRetry || needsCleanup || policyChanged && len(site.CustomDomains) > 0 {
			s.enqueue(site)
		}
		return true
	})
}

func (s *Syncer) enqueue(site Site) {
	s.inflightMu.Lock()
	if _, ok := s.inflight[site.RepoID]; ok {
		s.inflightMu.Unlock()
		return
	}
	s.inflight[site.RepoID] = struct{}{}
	s.inflightMu.Unlock()
	select {
	case s.queue <- site:
	default:
		s.inflightMu.Lock()
		delete(s.inflight, site.RepoID)
		s.inflightMu.Unlock()
	}
}

func (s *Syncer) clearInflight(repoID int64) {
	s.inflightMu.Lock()
	delete(s.inflight, repoID)
	s.inflightMu.Unlock()
}

func (s *Syncer) customReady(ctx context.Context, site Site) (bool, error) {
	if len(site.CustomDomains) == 0 {
		return false, nil
	}
	for _, d := range site.CustomDomains {
		if d.State != "active" {
			return false, fmt.Errorf("domain %s is not active", d.Host)
		}
		if !s.txtEnabled.Load() {
			continue
		}
		policyTime := s.txtPolicyTime.Load()
		temporarilyAuthorized := d.TXTResult == "temporary_error" && d.TXTLastSuccessUnixNano >= policyTime && d.TXTLastSuccessUnixNano != 0
		if d.TXTResult != "valid" && !temporarilyAuthorized {
			return false, fmt.Errorf("domain %s is not authorized by Gitea", d.Host)
		}
		key := fmt.Sprintf("%d/%s/%s/%d", d.ID, d.Host, d.TXTToken, policyTime)
		local, present := s.store.GetTXTState(key)
		now := time.Now()
		if present && now.Sub(time.Unix(0, local.LastCheckUnixNano)) < 15*time.Minute {
			if local.Valid && now.Sub(time.Unix(0, local.LastSuccessUnixNano)) <= 24*time.Hour {
				continue
			}
			return false, fmt.Errorf("domain %s has no current local TXT authorization", d.Host)
		}
		result, err := s.txtLookup(ctx, d.Host, d.TXTToken)
		local.LastCheckUnixNano = now.UnixNano()
		switch result {
		case TXTValid:
			local.LastSuccessUnixNano = now.UnixNano()
			local.Valid = true
		case TXTInvalid:
			local.LastSuccessUnixNano = 0
			local.Valid = false
		case TXTTemporaryError:
			local.Valid = local.Valid && local.LastSuccessUnixNano != 0 && now.Sub(time.Unix(0, local.LastSuccessUnixNano)) <= 24*time.Hour
		default:
			return false, fmt.Errorf("unknown TXT verification result")
		}
		if saveErr := s.store.SetTXTState(key, local); saveErr != nil {
			return false, saveErr
		}
		if !local.Valid {
			if err != nil {
				return false, err
			}
			return false, fmt.Errorf("domain %s has no current local TXT authorization", d.Host)
		}
	}
	return true, nil
}

func (s *Syncer) ensureCertificates(ctx context.Context, site Site) (bool, error) {
	if s.certificates == nil || s.cfg.RootURL.Scheme != "https" {
		return false, nil
	}
	renewed := false
	for _, d := range site.CustomDomains {
		if d.State != "active" {
			continue
		}
		if s.certificates.HasUsable(d.Host) && !s.certificates.NeedsRenewal(d.Host, 30*24*time.Hour) {
			continue
		}
		if s.acme == nil {
			return renewed, fmt.Errorf("no certificate for %s", d.Host)
		}
		if len(s.http01Addresses) == 0 {
			return renewed, fmt.Errorf("HTTP-01 probe addresses are not configured")
		}
		if s.challengeResponder == nil {
			return renewed, fmt.Errorf("HTTP-01 challenge responder is not configured")
		}
		r, err := s.acme.IssueHTTP01(ctx, d.Host, s.challengeResponder, s.http01Addresses)
		if err != nil {
			return renewed, err
		}
		if err := s.certificates.Save(ctx, r); err != nil {
			return renewed, err
		}
		renewed = true
	}
	return renewed, nil
}

func (s *Syncer) allCustomCertificatesUsable(site Site) bool {
	if s.certificates == nil || s.cfg.RootURL.Scheme != "https" {
		return true
	}
	for _, domain := range site.CustomDomains {
		if domain.State == "active" && !s.certificates.HasUsable(domain.Host) {
			return false
		}
	}
	return true
}

func (s *Syncer) reportDeploymentFailure(ctx context.Context, site Site, code string, cause error) error {
	local, _ := s.store.Get(site.RepoID)
	applied, err := s.store.SetResultForSite(site, local.InstalledDigest, local.DefaultPublished, local.CustomPublished, code)
	if err != nil {
		return errors.Join(fmt.Errorf("%s: %w", code, cause), fmt.Errorf("save deployment failure: %w", err))
	}
	if !applied {
		return nil
	}
	if reportErr := s.reportConfig(ctx, site, local.InstalledDigest, false, false, false, code); reportErr != nil {
		return errors.Join(fmt.Errorf("%s: %w", code, cause), reportErr)
	}
	return fmt.Errorf("%s: %w", code, cause)
}

func (s *Syncer) failCandidatePublication(ctx context.Context, site Site, code string, cause error) error {
	applied, err := s.store.SetResultForSite(site, site.TarGzSHA256, false, false, code)
	if err != nil {
		return errors.Join(fmt.Errorf("%s: %w", code, cause), fmt.Errorf("close candidate publication: %w", err))
	}
	if !applied {
		return nil
	}
	s.updateSiteRoute(site.RepoID)
	if err := s.apply(ctx); err != nil {
		cause = errors.Join(cause, fmt.Errorf("apply closed publication: %w", err))
	}
	if reportErr := s.reportConfig(ctx, site, site.TarGzSHA256, false, false, false, code); reportErr != nil {
		return errors.Join(fmt.Errorf("%s: %w", code, cause), reportErr)
	}
	return fmt.Errorf("%s: %w", code, cause)
}

func (s *Syncer) reportConfig(ctx context.Context, site Site, digest string, config, def, custom bool, code string) error {
	_, _, _, rootTime := s.store.State()
	policy := int64(0)
	if !site.Deleted && len(site.CustomDomains) > 0 {
		policy = s.txtPolicyTime.Load()
	}
	result := Result{RepoID: site.RepoID, AppliedPagesUpdatedUnixNano: site.PagesUpdatedUnixNano, AppliedRootUpdatedUnixNano: rootTime, AppliedTXTPolicyUpdatedUnixNano: policy, ConfigApplied: config, ActualTarGzSHA256: digest, DefaultPublished: def, CustomPublished: custom, ErrorCode: code}
	if old, ok := s.store.PendingReport(site.RepoID); ok {
		err := s.client.ReportResult(ctx, old)
		var apiErr *APIError
		if err != nil && (!errors.As(err, &apiErr) || apiErr.Code != "state_changed") {
			return err
		}
		if clearErr := s.store.ClearPendingReport(site.RepoID, old); clearErr != nil {
			return clearErr
		}
		if err != nil {
			return err
		}
		if err := s.store.MarkConfirmedReport(old); err != nil {
			return err
		}
		if reflect.DeepEqual(old, result) {
			return nil
		}
	}
	if err := s.store.SavePendingReport(result); err != nil {
		return err
	}
	if err := s.client.ReportResult(ctx, result); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == "state_changed" {
			_ = s.store.ClearPendingReport(site.RepoID, result)
		}
		return err
	}
	if err := s.store.MarkConfirmedReport(result); err != nil {
		return err
	}
	return s.store.ClearPendingReport(site.RepoID, result)
}

func (s *Syncer) StartWorkers(ctx context.Context) {
	for i := 0; i < s.cfg.DeployConcurrency; i++ {
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case site := <-s.queue:
					deployCtx, cancel := context.WithTimeout(ctx, s.cfg.DeployTimeout)
					err := s.deploy(deployCtx, site)
					cancel()
					s.clearInflight(site.RepoID)
					if err != nil {
						fmt.Fprintf(os.Stderr, "pages repo %d: %v\n", site.RepoID, err)
						var apiErr *APIError
						if errors.As(err, &apiErr) && apiErr.Code == "state_changed" {
							select {
							case s.refresh <- struct{}{}:
							default:
							}
						}
					}
				}
			}
		}()
	}
}

func (s *Syncer) WaitWorkers() { s.workers.Wait() }
