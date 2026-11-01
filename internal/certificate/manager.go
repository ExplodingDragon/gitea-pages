// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package certificate

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-acme/lego/v5/challenge"
)

const renewalWindow = 30 * 24 * time.Hour

// record is the durable certificate material managed by Pages.
type record struct {
	Host           string    `json:"host"`
	CertificatePEM string    `json:"certificate_pem"`
	PrivateKeyPEM  string    `json:"private_key_pem"`
	Source         string    `json:"source"`
	NotAfter       time.Time `json:"not_after"`
}

// StateStore persists certificate and ACME records without exposing the
// embedded state implementation to this package.
type StateStore interface {
	Get(context.Context, string, any) (bool, error)
	Put(context.Context, string, any) error
	Delete(context.Context, string) error
	Range(context.Context, string, func(string, []byte) error) error
}

// ChallengeResponder exposes HTTP-01 tokens through the active HTTP server.
type ChallengeResponder interface {
	SetChallenge(host, token, keyAuthorization string)
	ClearChallenge(host, token string)
}

// Options contains the certificate settings used by one Pages instance.
type Options struct {
	RootHost              string
	CertificateRoot       string
	DirectoryURL          string
	Email                 string
	EABKeyID              string
	EABHMACKey            []byte
	DNSResolver           string
	DNSProvider           string
	DNSProviderParameters map[string]string
	HTTP01Addresses       []string
}

// Manager persists certificates and builds the active TLS view.
type Manager struct {
	state           StateStore
	rootHost        string
	acme            *acmeManager
	dnsProvider     challenge.Provider
	responder       ChallengeResponder
	http01Addresses []string
	mu              sync.RWMutex
	records         map[string]record
	parsed          map[string]*tls.Certificate
}

// New restores certificate state and prepares the configured ACME clients.
func New(state StateStore, options Options, responder ChallengeResponder) (*Manager, error) {
	if state == nil {
		return nil, errors.New("certificate state store is required")
	}
	rootHost := normalizeHost(options.RootHost)
	if rootHost == "" {
		return nil, errors.New("default Pages host is empty")
	}
	manager, err := newManager(state, options.CertificateRoot)
	if err != nil {
		return nil, err
	}
	manager.rootHost = rootHost
	manager.responder = responder
	manager.http01Addresses = append([]string(nil), options.HTTP01Addresses...)
	if err := manager.ensureDefaultCertificateHosts(rootHost, "pages-check."+rootHost); err != nil {
		return nil, err
	}
	if options.DirectoryURL != "" {
		manager.acme, err = newACMEManager(state, options.DirectoryURL, options.Email, options.DNSResolver)
		if err != nil {
			return nil, err
		}
		if options.EABKeyID != "" && len(options.EABHMACKey) > 0 {
			manager.acme.eabKID = strings.TrimSpace(options.EABKeyID)
			manager.acme.eabKey = base64.RawURLEncoding.EncodeToString(options.EABHMACKey)
		}
	}
	if options.DNSProvider != "" {
		manager.dnsProvider, err = newDNS01Provider(options.DNSProvider, options.DNSProviderParameters)
		if err != nil {
			return nil, err
		}
	}
	if !manager.HasUsable("") && (manager.acme == nil || manager.dnsProvider == nil) {
		return nil, errors.New("HTTPS Pages requires a usable default certificate or configured ACME DNS-01 provider")
	}
	return manager, nil
}

// EnsureDefault keeps the root and wildcard certificate usable.
func (m *Manager) EnsureDefault(ctx context.Context) error {
	if !m.NeedsRenewal("") {
		if !m.HasUsable("") {
			return errors.New("default certificate has expired")
		}
		return nil
	}
	if m.acme == nil || m.dnsProvider == nil {
		return errors.New("default certificate needs renewal but DNS-01 is not configured")
	}
	record, err := m.acme.issueDefaultDNS01(ctx, m.rootHost, m.dnsProvider)
	if err != nil {
		return err
	}
	return m.save(ctx, record)
}

// EnsureCustom keeps one custom host certificate usable.
func (m *Manager) EnsureCustom(ctx context.Context, host string) error {
	host = normalizeHost(host)
	if m.HasUsable(host) && !m.NeedsRenewal(host) {
		return nil
	}
	if m.acme == nil {
		return fmt.Errorf("no certificate for %s", host)
	}
	if len(m.http01Addresses) == 0 {
		return errors.New("HTTP-01 probe addresses are not configured")
	}
	if m.responder == nil {
		return errors.New("HTTP-01 challenge responder is not configured")
	}
	record, err := m.acme.issueHTTP01(ctx, host, m.responder, m.http01Addresses)
	if err != nil {
		return err
	}
	return m.save(ctx, record)
}

// HasUsable reports whether the exact host has a currently valid certificate.
func (m *Manager) HasUsable(host string) bool {
	host = normalizeHost(host)
	m.mu.RLock()
	record, ok := m.records[host]
	m.mu.RUnlock()
	return ok && !record.NotAfter.IsZero() && time.Now().Before(record.NotAfter)
}

// NeedsRenewal reports whether the exact host lacks a certificate or its ACME
// certificate is within the maintenance window.
func (m *Manager) NeedsRenewal(host string) bool { return m.needsRenewalWithin(host, renewalWindow) }

// Prune removes ACME certificates for custom hosts that are no longer active.
func (m *Manager) Prune(ctx context.Context, activeHosts map[string]struct{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for host, record := range m.records {
		if host == "" || record.Source != "acme" {
			continue
		}
		if _, keep := activeHosts[host]; keep {
			continue
		}
		if err := m.state.Delete(ctx, certKey(host)); err != nil {
			return err
		}
		delete(m.records, host)
		delete(m.parsed, host)
	}
	return nil
}

// TLSConfig returns the live SNI certificate view.
func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return m.get(hello.ServerName)
		},
	}
}

// newManager restores stored and operator-provided certificates.
func newManager(state StateStore, root string) (*Manager, error) {
	m := &Manager{state: state, records: map[string]record{}, parsed: map[string]*tls.Certificate{}}
	if err := m.restoreStoredCertificates(); err != nil {
		return nil, err
	}
	if err := m.loadOperatorCertificates(root); err != nil {
		return nil, err
	}
	for host, record := range m.records {
		certificate, err := tls.X509KeyPair([]byte(record.CertificatePEM), []byte(record.PrivateKeyPEM))
		if err != nil {
			return nil, err
		}
		m.parsed[host] = &certificate
	}
	return m, nil
}

func (m *Manager) restoreStoredCertificates() error {
	ctx := context.Background()
	var defaultRecord record
	defaultExists, err := m.state.Get(ctx, certKey(""), &defaultRecord)
	if err != nil {
		return err
	}
	if defaultExists {
		if err := validateStoredCertificate(defaultRecord); err != nil {
			// Operator certificates are a projection of the configured
			// certificate directory. A stale record must not prevent Pages
			// from starting when that file has since been replaced or removed.
			if defaultRecord.Source == "operator" {
				if deleteErr := m.state.Delete(ctx, certKey("")); deleteErr != nil {
					return deleteErr
				}
			} else {
				return err
			}
		} else {
			m.records[""] = defaultRecord
		}
	}
	return m.state.Range(ctx, "/gitea-pages/cert/host/", func(_ string, value []byte) error {
		var r record
		if err := json.Unmarshal(value, &r); err != nil {
			return err
		}
		if err := validateStoredCertificate(r); err != nil {
			if r.Source == "operator" {
				if deleteErr := m.state.Delete(ctx, certKey(r.Host)); deleteErr != nil {
					return deleteErr
				}
				return nil
			}
			return err
		}
		r.Host = normalizeHost(r.Host)
		if r.Host == "" {
			return errors.New("stored custom certificate has an empty host")
		}
		m.records[r.Host] = r
		return nil
	})
}

func (m *Manager) loadOperatorCertificates(root string) error {
	ctx := context.Background()
	if root != "" {
		certPath := filepath.Join(root, "default", "fullchain.pem")
		keyPath := filepath.Join(root, "default", "privkey.pem")
		certInfo, certErr := os.Lstat(certPath)
		keyInfo, keyErr := os.Lstat(keyPath)
		certMissing := os.IsNotExist(certErr)
		keyMissing := os.IsNotExist(keyErr)
		if certErr != nil && !certMissing {
			return certErr
		}
		if keyErr != nil && !keyMissing {
			return keyErr
		}
		if certMissing != keyMissing {
			return fmt.Errorf("default certificate requires both %s and %s", certPath, keyPath)
		}
		if !certMissing && !keyMissing {
			if !certInfo.Mode().IsRegular() || !keyInfo.Mode().IsRegular() {
				return errors.New("default certificate files must be regular files")
			}
			r, err := readCertificateFiles(certPath, keyPath, "")
			if err != nil {
				return err
			}
			m.records[""] = r
			if err := m.state.Put(ctx, certKey(""), r); err != nil {
				return err
			}
		} else {
			if record, ok := m.records[""]; ok && record.Source == "operator" {
				if err := m.state.Delete(ctx, certKey("")); err != nil {
					return err
				}
				delete(m.records, "")
			}
		}
		if err := m.loadOperatorHostCertificates(root); err != nil {
			return err
		}
	} else {
		for host, record := range m.records {
			if record.Source != "operator" {
				continue
			}
			if err := m.state.Delete(ctx, certKey(host)); err != nil {
				return err
			}
			delete(m.records, host)
		}
	}
	return nil
}

func (m *Manager) loadOperatorHostCertificates(root string) error {
	dir := filepath.Join(root, "hosts")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		entries = nil
	} else if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			return fmt.Errorf("certificate host entry %s is not a directory", entry.Name())
		}
		if len(entry.Name()) != 64 {
			return fmt.Errorf("certificate host entry %s has an invalid name", entry.Name())
		}
		hostPath := filepath.Join(dir, entry.Name(), "host")
		info, err := os.Lstat(hostPath)
		if err != nil {
			return fmt.Errorf("read certificate host %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("certificate host %s is not a regular file", entry.Name())
		}
		hostBytes, err := os.ReadFile(hostPath)
		if err != nil {
			return err
		}
		host := strings.ToLower(strings.TrimSpace(string(hostBytes)))
		if host == "" {
			return fmt.Errorf("certificate host %s is empty", entry.Name())
		}
		seen[host] = struct{}{}
		sum := sha256.Sum256([]byte(host))
		if hex.EncodeToString(sum[:]) != entry.Name() {
			return fmt.Errorf("certificate host %s does not match its directory", host)
		}
		r, err := readCertificateFiles(filepath.Join(dir, entry.Name(), "fullchain.pem"), filepath.Join(dir, entry.Name(), "privkey.pem"), host)
		if err != nil {
			return fmt.Errorf("load certificate for %s: %w", host, err)
		}
		m.records[host] = r
		if err := m.state.Put(context.Background(), certKey(host), r); err != nil {
			return err
		}
	}
	for host, record := range m.records {
		if host == "" || record.Source != "operator" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		if err := m.state.Delete(context.Background(), certKey(host)); err != nil {
			return err
		}
		delete(m.records, host)
	}
	return nil
}

func certKey(host string) string {
	if host == "" {
		return "/gitea-pages/cert/default"
	}
	return "/gitea-pages/cert/host/" + host
}

func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

func (m *Manager) save(ctx context.Context, record record) error {
	if err := validateCertificate(record); err != nil {
		return err
	}
	block, _ := pem.Decode([]byte(record.CertificatePEM))
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	record.NotAfter = leaf.NotAfter
	record.Host = normalizeHost(record.Host)
	parsed, err := tls.X509KeyPair([]byte(record.CertificatePEM), []byte(record.PrivateKeyPEM))
	if err != nil {
		return err
	}
	if err := m.state.Put(ctx, certKey(record.Host), record); err != nil {
		return err
	}
	m.mu.Lock()
	m.records[record.Host] = record
	m.parsed[record.Host] = &parsed
	m.mu.Unlock()
	return nil
}

func (m *Manager) get(host string) (*tls.Certificate, error) {
	host = normalizeHost(host)
	m.mu.RLock()
	r, ok := m.records[host]
	c := m.parsed[host]
	if !ok {
		r, ok = m.records[""]
		c = m.parsed[""]
	}
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no certificate for %s", host)
	}
	if !r.NotAfter.IsZero() && !time.Now().Before(r.NotAfter) {
		return nil, fmt.Errorf("certificate for %s has expired", host)
	}
	return c, nil
}

// ensureDefaultCertificateHosts verifies the hosts needed by the default
// Pages endpoint. An ACME certificate is replaceable state, so a certificate
// issued for an earlier root address is removed and the certificate loop can
// issue a matching one. Operator material is authoritative and therefore
// produces an explicit configuration error until its files cover both hosts.
func (m *Manager) ensureDefaultCertificateHosts(hosts ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[""]
	if !ok {
		return nil
	}
	leaf, err := certificateLeaf(record)
	if err != nil {
		return fmt.Errorf("validate default certificate: %w", err)
	}
	for _, host := range hosts {
		if err := leaf.VerifyHostname(host); err != nil {
			if record.Source != "acme" {
				return fmt.Errorf("default operator certificate does not cover %s: %w", host, err)
			}
			if err := m.state.Delete(context.Background(), certKey("")); err != nil {
				return fmt.Errorf("remove stale default certificate: %w", err)
			}
			delete(m.records, "")
			delete(m.parsed, "")
			return nil
		}
	}
	return nil
}

func (m *Manager) needsRenewalWithin(host string, within time.Duration) bool {
	host = normalizeHost(host)
	m.mu.RLock()
	r, ok := m.records[host]
	m.mu.RUnlock()
	if !ok || r.NotAfter.IsZero() {
		return true
	}
	// Operator-provided certificates remain under the operator's rotation
	// policy. A near-expiry operator certificate must not trigger an ACME order.
	if r.Source == "operator" {
		return false
	}
	return time.Until(r.NotAfter) <= within
}

func validateCertificate(r record) error {
	return validateCertificateRecord(r, false)
}

func validateCertificateRecord(r record, allowExpired bool) error {
	cert, err := certificateLeaf(r)
	if err != nil {
		return err
	}
	now := time.Now()
	if !allowExpired && (now.Before(cert.NotBefore) || now.After(cert.NotAfter)) {
		return fmt.Errorf("certificate for %s is outside validity period", r.Host)
	}
	// A non-empty Host identifies a custom certificate. Checking the leaf
	// here prevents an otherwise valid certificate for another domain from
	// being persisted and later selected for this host. The default record
	// is checked against the instance root by the caller because that root
	// is not part of the certificate store configuration.
	if r.Host != "" {
		if err := cert.VerifyHostname(r.Host); err != nil {
			return fmt.Errorf("certificate does not cover %s: %w", r.Host, err)
		}
	}
	return nil
}

func validateStoredCertificate(r record) error {
	return validateCertificateRecord(r, r.Source == "acme")
}

func certificateLeaf(r record) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(r.CertificatePEM))
	if block == nil {
		return nil, errors.New("certificate PEM is invalid")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	if _, err := tls.X509KeyPair([]byte(r.CertificatePEM), []byte(r.PrivateKeyPEM)); err != nil {
		return nil, fmt.Errorf("certificate key mismatch: %w", err)
	}
	return cert, nil
}

func readCertificateFiles(certFile, keyFile, host string) (record, error) {
	for _, name := range []string{certFile, keyFile} {
		info, err := os.Lstat(name)
		if err != nil {
			return record{}, err
		}
		if !info.Mode().IsRegular() {
			return record{}, fmt.Errorf("certificate file %s is not a regular file", name)
		}
	}
	cert, err := os.ReadFile(certFile)
	if err != nil {
		return record{}, err
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		return record{}, err
	}
	r := record{Host: host, CertificatePEM: string(cert), PrivateKeyPEM: string(key), Source: "operator"}
	if err := validateCertificate(r); err != nil {
		return record{}, err
	}
	block, _ := pem.Decode(cert)
	if block != nil {
		if c, e := x509.ParseCertificate(block.Bytes); e == nil {
			r.NotAfter = c.NotAfter
		}
	}
	return r, nil
}
