// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package certificate

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitea.dev/page/internal/storage"
)

const testStateQuotaBytes = int64(64 * 1024 * 1024)

func testCertificate(t *testing.T, host string) record {
	return testCertificateForHosts(t, host, []string{host})
}

func testCertificateForHosts(t *testing.T, host string, hosts []string) record {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: hosts, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return record{Host: host, CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))}
}

func TestNewManagerUsesOperatorDefaultCertificate(t *testing.T) {
	store, err := storage.Open(t.TempDir(), testStateQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := t.TempDir()
	record := testCertificateForHosts(t, "pages.example", []string{"pages.example", "*.pages.example"})
	dir := filepath.Join(root, "default")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fullchain.pem"), []byte(record.CertificatePEM), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "privkey.pem"), []byte(record.PrivateKeyPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := New(store.State(), Options{RootHost: "pages.example", CertificateRoot: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !manager.HasUsable("") || manager.NeedsRenewal("") || manager.TLSConfig() == nil {
		t.Fatal("operator default certificate was not activated")
	}
	if err := manager.Prune(t.Context(), map[string]struct{}{}); err != nil {
		t.Fatal(err)
	}
}

func TestCertificateManagerPersistsAndSelectsSNI(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(root, testStateQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	m, err := newManager(store.State(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.save(context.Background(), testCertificate(t, "custom.example")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.get("CUSTOM.EXAMPLE."); err != nil {
		t.Fatal(err)
	}
	if !m.HasUsable("CUSTOM.EXAMPLE.") {
		t.Fatal("normalized custom host certificate is not usable")
	}
	if m.needsRenewalWithin("CUSTOM.EXAMPLE.", time.Minute) {
		t.Fatal("normalized custom host certificate unexpectedly needs renewal")
	}
	store.Close()
	store, err = storage.Open(root, testStateQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m, err = newManager(store.State(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.get("custom.example"); err != nil {
		t.Fatalf("custom certificate was not restored after restart: %v", err)
	}
}

func TestCertificateManagerRejectsCertificateForAnotherHost(t *testing.T) {
	store, err := storage.Open(t.TempDir(), testStateQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m, err := newManager(store.State(), "")
	if err != nil {
		t.Fatal(err)
	}
	record := testCertificate(t, "other.example")
	record.Host = "custom.example"
	if err := m.save(context.Background(), record); err == nil {
		t.Fatal("certificate for another host was accepted")
	}
}

func TestCertificateManagerDoesNotUseDefaultForCustomRenewal(t *testing.T) {
	store, err := storage.Open(t.TempDir(), testStateQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m, err := newManager(store.State(), "")
	if err != nil {
		t.Fatal(err)
	}
	defaultCertificate := testCertificate(t, "pages.example")
	defaultCertificate.Host = ""
	if err := m.save(context.Background(), defaultCertificate); err != nil {
		t.Fatal(err)
	}
	if !m.needsRenewalWithin("custom.example", time.Hour) {
		t.Fatal("custom host unexpectedly reused the default certificate")
	}
}

func TestCertificateManagerLoadsOperatorHostCertificate(t *testing.T) {
	store, err := storage.Open(t.TempDir(), testStateQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := t.TempDir()
	host := "custom.example"
	record := testCertificate(t, host)
	sum := sha256.Sum256([]byte(host))
	dir := filepath.Join(root, "hosts", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host"), []byte(host+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fullchain.pem"), []byte(record.CertificatePEM), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "privkey.pem"), []byte(record.PrivateKeyPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := newManager(store.State(), root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.get(host); err != nil {
		t.Fatal(err)
	}
}

func TestCertificateManagerRejectsPartialDefaultCertificateFiles(t *testing.T) {
	store, err := storage.Open(t.TempDir(), testStateQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "default"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "default", "fullchain.pem"), []byte("certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newManager(store.State(), root); err == nil {
		t.Fatal("partial default certificate files were accepted")
	}
}

func TestPruneUnusedACMECertificates(t *testing.T) {
	store, err := storage.Open(t.TempDir(), testStateQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m, err := newManager(store.State(), "")
	if err != nil {
		t.Fatal(err)
	}
	acme := testCertificate(t, "old.example")
	acme.Source = "acme"
	if err := m.save(context.Background(), acme); err != nil {
		t.Fatal(err)
	}
	operator := testCertificate(t, "operator.example")
	operator.Source = "operator"
	if err := m.save(context.Background(), operator); err != nil {
		t.Fatal(err)
	}
	if err := m.Prune(t.Context(), map[string]struct{}{}); err != nil {
		t.Fatal(err)
	}
	if m.HasUsable("old.example") || !m.HasUsable("operator.example") {
		t.Fatal("unused ACME certificate remains or operator certificate was removed")
	}
}

func TestCertificateManagerRemovesStaleACMEDefault(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(root, testStateQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	m, err := newManager(store.State(), "")
	if err != nil {
		t.Fatal(err)
	}
	record := testCertificate(t, "old.pages.example")
	record.Host = ""
	record.Source = "acme"
	if err := m.save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := m.ensureDefaultCertificateHosts("new.pages.example", "pages-check.new.pages.example"); err != nil {
		t.Fatalf("check stale ACME default: %v", err)
	}
	if m.HasUsable("") {
		t.Fatal("stale ACME default certificate remains usable")
	}
	store.Close()
	store, err = storage.Open(root, testStateQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m, err = newManager(store.State(), "")
	if err != nil {
		t.Fatal(err)
	}
	if m.HasUsable("") {
		t.Fatal("stale ACME default certificate was restored")
	}
}

func TestCertificateManagerRejectsStaleOperatorDefault(t *testing.T) {
	store, err := storage.Open(t.TempDir(), testStateQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m, err := newManager(store.State(), "")
	if err != nil {
		t.Fatal(err)
	}
	record := testCertificate(t, "old.pages.example")
	record.Host = ""
	record.Source = "operator"
	if err := m.save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := m.ensureDefaultCertificateHosts("new.pages.example"); err == nil {
		t.Fatal("stale operator default certificate was accepted")
	}
}

func TestCertificateManagerPrunesRemovedOperatorHost(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(t.TempDir(), testStateQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	host := "custom.example"
	record := testCertificate(t, host)
	if err := os.MkdirAll(filepath.Join(root, "hosts"), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(host))
	dir := filepath.Join(root, "hosts", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host"), []byte(host), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fullchain.pem"), []byte(record.CertificatePEM), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "privkey.pem"), []byte(record.PrivateKeyPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := newManager(store.State(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !m.HasUsable(host) {
		t.Fatal("operator host certificate was not loaded")
	}
	if err := os.RemoveAll(filepath.Join(root, "hosts")); err != nil {
		t.Fatal(err)
	}
	m, err = newManager(store.State(), root)
	if err != nil {
		t.Fatal(err)
	}
	if m.HasUsable(host) {
		t.Fatal("removed operator host certificate remains usable")
	}
}
