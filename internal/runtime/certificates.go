// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
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
)

// certificateRecord is the durable certificate material managed by Pages.
type certificateRecord struct {
	Host           string    `json:"host"`
	CertificatePEM string    `json:"certificate_pem"`
	PrivateKeyPEM  string    `json:"private_key_pem"`
	Source         string    `json:"source"`
	NotAfter       time.Time `json:"not_after"`
}

// certificateManager persists certificates and builds the active TLS view.
type certificateManager struct {
	store   *store
	mu      sync.RWMutex
	records map[string]certificateRecord
	parsed  map[string]*tls.Certificate
}

func (m *certificateManager) pruneUnusedCustom(allowed map[string]struct{}) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := false
	for host, record := range m.records {
		if host == "" || record.Source != "acme" {
			continue
		}
		if _, keep := allowed[host]; keep {
			continue
		}
		if err := m.store.kv.delete(context.Background(), certKey(host)); err != nil {
			return changed, err
		}
		delete(m.records, host)
		delete(m.parsed, host)
		changed = true
	}
	return changed, nil
}

// newCertificateManager restores stored and operator-provided certificates.
func newCertificateManager(store *store, root string) (*certificateManager, error) {
	m := &certificateManager{store: store, records: map[string]certificateRecord{}, parsed: map[string]*tls.Certificate{}}
	var defaultRecord certificateRecord
	defaultExists, err := store.kv.get(context.Background(), certKey(""), &defaultRecord)
	if err != nil {
		return nil, err
	}
	if defaultExists {
		if err := validateStoredCertificate(defaultRecord); err != nil {
			// Operator certificates are a projection of the configured
			// certificate directory. A stale record must not prevent Pages
			// from starting when that file has since been replaced or removed.
			if defaultRecord.Source == "operator" {
				if deleteErr := store.kv.delete(context.Background(), certKey("")); deleteErr != nil {
					return nil, deleteErr
				}
			} else {
				return nil, err
			}
		} else {
			m.records[""] = defaultRecord
		}
	}
	if err := store.kv.listPrefix(context.Background(), "/gitea-pages/cert/host/", func(_ string, value []byte) error {
		var r certificateRecord
		if err := json.Unmarshal(value, &r); err != nil {
			return err
		}
		if err := validateStoredCertificate(r); err != nil {
			if r.Source == "operator" {
				if deleteErr := store.kv.delete(context.Background(), certKey(r.Host)); deleteErr != nil {
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
	}); err != nil {
		return nil, err
	}
	if root != "" {
		certPath := filepath.Join(root, "default", "fullchain.pem")
		keyPath := filepath.Join(root, "default", "privkey.pem")
		certInfo, certErr := os.Lstat(certPath)
		keyInfo, keyErr := os.Lstat(keyPath)
		certMissing := os.IsNotExist(certErr)
		keyMissing := os.IsNotExist(keyErr)
		if certErr != nil && !certMissing {
			return nil, certErr
		}
		if keyErr != nil && !keyMissing {
			return nil, keyErr
		}
		if certMissing != keyMissing {
			return nil, fmt.Errorf("default certificate requires both %s and %s", certPath, keyPath)
		}
		if !certMissing && !keyMissing {
			if !certInfo.Mode().IsRegular() || !keyInfo.Mode().IsRegular() {
				return nil, errors.New("default certificate files must be regular files")
			}
			r, err := readCertificateFiles(certPath, keyPath, "")
			if err != nil {
				return nil, err
			}
			m.records[""] = r
			if err := store.kv.put(context.Background(), certKey(""), r); err != nil {
				return nil, err
			}
		} else {
			if record, ok := m.records[""]; ok && record.Source == "operator" {
				if err := store.kv.delete(context.Background(), certKey("")); err != nil {
					return nil, err
				}
				delete(m.records, "")
			}
		}
		if err := m.loadOperatorHostCertificates(root); err != nil {
			return nil, err
		}
	} else {
		for host, record := range m.records {
			if record.Source != "operator" {
				continue
			}
			if err := store.kv.delete(context.Background(), certKey(host)); err != nil {
				return nil, err
			}
			delete(m.records, host)
		}
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

func (m *certificateManager) loadOperatorHostCertificates(root string) error {
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
		if err := m.store.kv.put(context.Background(), certKey(host), r); err != nil {
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
		if err := m.store.kv.delete(context.Background(), certKey(host)); err != nil {
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

func (m *certificateManager) save(ctx context.Context, record certificateRecord) error {
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
	if err := m.store.kv.put(ctx, certKey(record.Host), record); err != nil {
		return err
	}
	m.mu.Lock()
	m.records[record.Host] = record
	m.parsed[record.Host] = &parsed
	m.mu.Unlock()
	return nil
}

func (m *certificateManager) get(host string) (*tls.Certificate, error) {
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

func (m *certificateManager) hasUsable(host string) bool {
	host = normalizeHost(host)
	m.mu.RLock()
	r, ok := m.records[host]
	m.mu.RUnlock()
	return ok && !r.NotAfter.IsZero() && time.Now().Before(r.NotAfter)
}

// ensureDefaultCertificateHosts verifies the hosts needed by the default
// Pages endpoint. An ACME certificate is replaceable state, so a certificate
// issued for an earlier root address is removed and the certificate loop can
// issue a matching one. Operator material is authoritative and therefore
// produces an explicit configuration error until its files cover both hosts.
func (m *certificateManager) ensureDefaultCertificateHosts(hosts ...string) error {
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
			if err := m.store.kv.delete(context.Background(), certKey("")); err != nil {
				return fmt.Errorf("remove stale default certificate: %w", err)
			}
			delete(m.records, "")
			delete(m.parsed, "")
			return nil
		}
	}
	return nil
}

func (m *certificateManager) needsRenewal(host string, within time.Duration) bool {
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

func (m *certificateManager) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return m.get(hello.ServerName)
		},
	}
}

func validateCertificate(r certificateRecord) error {
	return validateCertificateRecord(r, false)
}

func validateCertificateRecord(r certificateRecord, allowExpired bool) error {
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

func validateStoredCertificate(r certificateRecord) error {
	return validateCertificateRecord(r, r.Source == "acme")
}

func certificateLeaf(r certificateRecord) (*x509.Certificate, error) {
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

func readCertificateFiles(certFile, keyFile, host string) (certificateRecord, error) {
	for _, name := range []string{certFile, keyFile} {
		info, err := os.Lstat(name)
		if err != nil {
			return certificateRecord{}, err
		}
		if !info.Mode().IsRegular() {
			return certificateRecord{}, fmt.Errorf("certificate file %s is not a regular file", name)
		}
	}
	cert, err := os.ReadFile(certFile)
	if err != nil {
		return certificateRecord{}, err
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		return certificateRecord{}, err
	}
	r := certificateRecord{Host: host, CertificatePEM: string(cert), PrivateKeyPEM: string(key), Source: "operator"}
	if err := validateCertificate(r); err != nil {
		return certificateRecord{}, err
	}
	block, _ := pem.Decode(cert)
	if block != nil {
		if c, e := x509.ParseCertificate(block.Bytes); e == nil {
			r.NotAfter = c.NotAfter
		}
	}
	return r, nil
}
