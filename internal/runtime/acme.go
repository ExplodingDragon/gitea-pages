// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	legoacme "github.com/go-acme/lego/v5/acme"
	"github.com/go-acme/lego/v5/certcrypto"
	"github.com/go-acme/lego/v5/certificate"
	"github.com/go-acme/lego/v5/challenge"
	"github.com/go-acme/lego/v5/challenge/dns01"
	"github.com/go-acme/lego/v5/lego"
	"github.com/go-acme/lego/v5/registration"
)

// dns01Provider manages the DNS records requested by lego.
type dns01Provider interface {
	Present(context.Context, string, string) error
	Cleanup(context.Context, string, string) error
}

// challengeResponder exposes HTTP-01 tokens through the active serving mode.
type challengeResponder interface {
	setChallenge(string, string, string)
	clearChallenge(string, string)
}

// commandDNSProvider delegates DNS record changes to operator-provided commands.
type commandDNSProvider struct{ PresentCommand, CleanupCommand []string }

// newDNS01Provider keeps provider credentials outside the Pages state. The
// service manager supplies them through the command environment, so adding a
// deployment-specific DNS integration does not change certificate handling.
func newDNS01Provider(name string, present, cleanup []string) (dns01Provider, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "command":
		if len(present) == 0 || len(cleanup) == 0 {
			return nil, errors.New("DNS command provider requires present and cleanup commands")
		}
		return commandDNSProvider{PresentCommand: present, CleanupCommand: cleanup}, nil
	default:
		return nil, fmt.Errorf("unsupported DNS-01 provider %q", name)
	}
}

func (p commandDNSProvider) Present(ctx context.Context, name, value string) error {
	return runDNSCommand(ctx, p.PresentCommand, name, value)
}

func (p commandDNSProvider) Cleanup(ctx context.Context, name, value string) error {
	return runDNSCommand(ctx, p.CleanupCommand, name, value)
}

func runDNSCommand(ctx context.Context, command []string, name, value string) error {
	if len(command) == 0 {
		return errors.New("DNS provider command is not configured")
	}
	args := make([]string, len(command))
	for i, v := range command {
		args[i] = strings.ReplaceAll(strings.ReplaceAll(v, "{name}", name), "{value}", value)
	}
	return exec.CommandContext(ctx, args[0], args[1:]...).Run()
}

// acmeManager owns the persisted lego account and certificate requests.
type acmeManager struct {
	store                  *store
	directory, email       string
	accountKey             *ecdsa.PrivateKey
	accountURI, accountPEM string
	eabKID, eabKey         string
	mu                     sync.Mutex
}

func (m *acmeManager) setExternalAccountBinding(kid string, key []byte) {
	if kid != "" && len(key) > 0 {
		m.eabKID = strings.TrimSpace(kid)
		m.eabKey = base64.RawURLEncoding.EncodeToString(key)
	}
}

type acmeAccountState struct {
	KeyPEM string `json:"key_pem"`
	URI    string `json:"uri"`
}

type acmeRetryState struct {
	Failures          int   `json:"failures"`
	NotBeforeUnixNano int64 `json:"not_before_unix_nano"`
}

func acmeRetryKey(method, host string) string {
	sum := sha256.Sum256([]byte(method + ":" + strings.ToLower(host)))
	return "/gitea-pages/acme/retry/" + hex.EncodeToString(sum[:])
}

type acmeUser struct {
	email string
	key   crypto.Signer
	reg   *legoacme.ExtendedAccount
}

func (u *acmeUser) GetEmail() string                           { return u.email }
func (u *acmeUser) GetPrivateKey() crypto.Signer               { return u.key }
func (u *acmeUser) GetRegistration() *legoacme.ExtendedAccount { return u.reg }

// newACMEManager restores or creates the lego account stored in embedded etcd.
func newACMEManager(store *store, directory, email, dnsResolver string) (*acmeManager, error) {
	dnsOptions := dns01.NewOptions()
	if dnsResolver != "" {
		dnsOptions.RecursiveNameservers = []string{dnsResolver}
	}
	dns01.SetDefaultClient(dns01.NewClient(dnsOptions))
	m := &acmeManager{store: store, directory: strings.TrimSpace(directory), email: strings.TrimSpace(email)}
	if m.directory == "" {
		return nil, errors.New("ACME directory is required")
	}
	var state acmeAccountState
	ok, err := store.kv.get(context.Background(), "/gitea-pages/acme/account", &state)
	if err != nil {
		return nil, err
	}
	if ok {
		m.accountPEM = state.KeyPEM
		block, _ := pem.Decode([]byte(state.KeyPEM))
		if block == nil {
			return nil, errors.New("ACME account key is invalid")
		}
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse ACME account key: %w", err)
		}
		m.accountKey, m.accountURI = key, state.URI
		return m, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	b, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	m.accountKey = key
	m.accountPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: b}))
	if err := store.kv.put(context.Background(), "/gitea-pages/acme/account", acmeAccountState{KeyPEM: m.accountPEM}); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *acmeManager) client() (*lego.Client, error) {
	user := &acmeUser{email: m.email, key: m.accountKey}
	if m.accountURI != "" {
		user.reg = &legoacme.ExtendedAccount{Location: m.accountURI}
	}
	config := lego.NewConfig(user)
	config.CADirURL = m.directory
	config.UserAgent = "gitea-pages"
	return lego.NewClient(config)
}

func (m *acmeManager) ensureAccount(ctx context.Context, c *lego.Client) error {
	if m.accountURI != "" {
		return nil
	}
	var account *legoacme.ExtendedAccount
	var err error
	if m.eabKID != "" && m.eabKey != "" {
		account, err = c.Registration.RegisterWithExternalAccountBinding(ctx, registration.RegisterEABOptions{
			TermsOfServiceAgreed: true,
			Kid:                  m.eabKID, HmacEncoded: m.eabKey,
		})
	} else {
		account, err = c.Registration.Register(ctx, registration.RegisterOptions{TermsOfServiceAgreed: true})
	}
	if err != nil {
		return err
	}
	if account == nil || account.Location == "" {
		return errors.New("ACME server returned no account location")
	}
	m.accountURI = account.Location
	if err := m.store.kv.put(ctx, "/gitea-pages/acme/account", acmeAccountState{KeyPEM: m.accountPEM, URI: m.accountURI}); err != nil {
		return fmt.Errorf("save ACME account: %w", err)
	}
	return nil
}

type httpChallengeProvider struct {
	server    challengeResponder
	host      string
	addresses []string
}

func (p httpChallengeProvider) Present(ctx context.Context, domain, token, keyAuth string) error {
	if domain != p.host {
		return fmt.Errorf("ACME HTTP-01 requested unexpected domain %q", domain)
	}
	p.server.setChallenge(domain, token, keyAuth)
	if err := http01Precheck(ctx, domain, token, keyAuth, p.addresses); err != nil {
		p.server.clearChallenge(domain, token)
		return fmt.Errorf("HTTP-01 challenge precheck: %w", err)
	}
	return nil
}

func (p httpChallengeProvider) CleanUp(_ context.Context, domain, token, _ string) error {
	p.server.clearChallenge(domain, token)
	return nil
}

type dnsChallengeProvider struct {
	provider dns01Provider
	store    *store
}

type pendingDNSChallenge struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func dnsChallengeKey(name, value string) string {
	sum := sha256.Sum256([]byte(name + "\x00" + value))
	return "/gitea-pages/acme/dns/" + hex.EncodeToString(sum[:])
}

func (p dnsChallengeProvider) Present(ctx context.Context, domain, _, keyAuth string) error {
	info := dns01.GetChallengeInfo(ctx, domain, keyAuth)
	name := strings.TrimSuffix(info.FQDN, ".")
	if p.store != nil {
		if err := p.store.kv.put(ctx, dnsChallengeKey(name, info.Value), pendingDNSChallenge{Name: name, Value: info.Value}); err != nil {
			return err
		}
	}
	return p.provider.Present(ctx, name, info.Value)
}

func (p dnsChallengeProvider) CleanUp(ctx context.Context, domain, _, keyAuth string) error {
	info := dns01.GetChallengeInfo(ctx, domain, keyAuth)
	name := strings.TrimSuffix(info.FQDN, ".")
	if err := p.provider.Cleanup(ctx, name, info.Value); err != nil {
		return err
	}
	if p.store != nil {
		return p.store.kv.delete(ctx, dnsChallengeKey(name, info.Value))
	}
	return nil
}

func (m *acmeManager) cleanupDNS(ctx context.Context, provider dns01Provider) error {
	var pending []pendingDNSChallenge
	if err := m.store.kv.listPrefix(ctx, "/gitea-pages/acme/dns/", func(_ string, value []byte) error {
		var item pendingDNSChallenge
		if err := json.Unmarshal(value, &item); err != nil {
			return err
		}
		pending = append(pending, item)
		return nil
	}); err != nil {
		return err
	}
	for _, item := range pending {
		if err := provider.Cleanup(ctx, item.Name, item.Value); err != nil {
			return err
		}
		if err := m.store.kv.delete(ctx, dnsChallengeKey(item.Name, item.Value)); err != nil {
			return err
		}
	}
	return nil
}

func randomChallengeToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (m *acmeManager) issueHTTP01(ctx context.Context, host string, server challengeResponder, addresses []string) (certificateRecord, error) {
	if server == nil {
		return certificateRecord{}, errors.New("HTTP-01 challenge responder is not configured")
	}
	token, err := randomChallengeToken()
	if err != nil {
		return certificateRecord{}, err
	}
	keyAuth, err := randomChallengeToken()
	if err != nil {
		return certificateRecord{}, err
	}
	server.setChallenge(host, token, keyAuth)
	err = http01Precheck(ctx, host, token, keyAuth, addresses)
	server.clearChallenge(host, token)
	if err != nil {
		return certificateRecord{}, fmt.Errorf("HTTP-01 precheck: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	return m.issue(ctx, acmeRetryKey("http01", host), []string{host}, func(c *lego.Client) error {
		return c.Challenge.SetHTTP01Provider(httpChallengeProvider{server: server, host: host, addresses: addresses})
	})
}

func (m *acmeManager) issueDNS01(ctx context.Context, names []string, provider dns01Provider) (certificateRecord, error) {
	if provider == nil {
		return certificateRecord{}, errors.New("DNS-01 provider is not configured")
	}
	if len(names) == 0 {
		return certificateRecord{}, errors.New("DNS-01 names are empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.issue(ctx, acmeRetryKey("dns01", names[0]), names, func(c *lego.Client) error {
		if err := m.cleanupDNS(ctx, provider); err != nil {
			return err
		}
		return c.Challenge.SetDNS01Provider(dnsChallengeProvider{provider: provider, store: m.store})
	})
}

// issue serializes local certificate work and keeps the retry deadline when a
// CA rejects an order. The provider itself remains a thin adapter to lego.
func (m *acmeManager) issue(ctx context.Context, retryKey string, names []string, setProvider func(*lego.Client) error) (certificateRecord, error) {
	var retry acmeRetryState
	_, err := m.store.kv.get(ctx, retryKey, &retry)
	if err != nil {
		return certificateRecord{}, err
	}
	if now := time.Now(); now.Before(time.Unix(0, retry.NotBeforeUnixNano)) {
		return certificateRecord{}, fmt.Errorf("ACME retry for %s is scheduled after %s", names[0], time.Unix(0, retry.NotBeforeUnixNano).Format(time.RFC3339))
	}
	c, err := m.client()
	if err != nil {
		return certificateRecord{}, err
	}
	if err := m.ensureAccount(ctx, c); err != nil {
		return certificateRecord{}, m.recordFailure(retryKey, retry, err)
	}
	if err := setProvider(c); err != nil {
		return certificateRecord{}, err
	}
	record, err := m.obtain(ctx, c, names)
	if err != nil {
		return certificateRecord{}, m.recordFailure(retryKey, retry, err)
	}
	if err := m.store.kv.delete(ctx, retryKey); err != nil {
		return certificateRecord{}, fmt.Errorf("clear ACME retry after certificate issuance: %w", err)
	}
	return record, nil
}

func (m *acmeManager) recordFailure(key string, state acmeRetryState, issueErr error) error {
	state.Failures++
	delay := 15 * time.Minute
	for i := 1; i < state.Failures && delay < 24*time.Hour; i++ {
		delay *= 2
	}
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	var rateLimit *legoacme.RateLimitedError
	if errors.As(issueErr, &rateLimit) && rateLimit.RetryAfter > delay {
		delay = rateLimit.RetryAfter
	}
	state.NotBeforeUnixNano = time.Now().Add(delay).UnixNano()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.store.kv.put(ctx, key, state); err != nil {
		return fmt.Errorf("%w; persist ACME retry: %v", issueErr, err)
	}
	return issueErr
}

func (m *acmeManager) issueDefaultDNS01(ctx context.Context, rootHost string, provider dns01Provider) (certificateRecord, error) {
	rootHost = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(rootHost), "."))
	if rootHost == "" {
		return certificateRecord{}, errors.New("default Pages host is empty")
	}
	r, err := m.issueDNS01(ctx, []string{rootHost, "*." + rootHost}, provider)
	if err == nil {
		r.Host = ""
	}
	return r, err
}

func (m *acmeManager) obtain(ctx context.Context, c *lego.Client, names []string) (certificateRecord, error) {
	resource, err := c.Certificate.Obtain(ctx, certificate.ObtainRequest{Domains: names, Bundle: true, KeyType: certcrypto.EC256})
	if err != nil {
		return certificateRecord{}, err
	}
	record, err := certificateRecordFromResource(resource, names[0])
	if err != nil {
		return certificateRecord{}, err
	}
	block, _ := pem.Decode([]byte(record.CertificatePEM))
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return certificateRecord{}, err
	}
	for _, name := range names {
		probeName := name
		if strings.HasPrefix(name, "*.") {
			probeName = "pages-check." + strings.TrimPrefix(name, "*.")
		}
		if err := leaf.VerifyHostname(probeName); err != nil {
			return certificateRecord{}, fmt.Errorf("ACME certificate does not cover %s: %w", name, err)
		}
	}
	return record, nil
}

func certificateRecordFromResource(resource *certificate.Resource, host string) (certificateRecord, error) {
	if resource == nil || len(resource.Certificate) == 0 || len(resource.PrivateKey) == 0 {
		return certificateRecord{}, errors.New("ACME server returned an empty certificate")
	}
	record := certificateRecord{Host: host, CertificatePEM: string(resource.Certificate), PrivateKeyPEM: string(resource.PrivateKey), Source: "acme"}
	block, _ := pem.Decode(resource.Certificate)
	if block == nil {
		return certificateRecord{}, errors.New("ACME server returned invalid certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return certificateRecord{}, fmt.Errorf("parse ACME certificate: %w", err)
	}
	record.NotAfter = cert.NotAfter
	return record, nil
}

var _ challenge.Provider = httpChallengeProvider{}
var _ challenge.Provider = dnsChallengeProvider{}
