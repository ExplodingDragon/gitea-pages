// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

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
)

func testCertificate(t *testing.T, host string) certificateRecord {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return certificateRecord{Host: host, CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))}
}

func TestCertificateManagerPersistsAndSelectsSNI(t *testing.T) {
	root := t.TempDir()
	store, err := openStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	m, err := newCertificateManager(store, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.save(context.Background(), testCertificate(t, "custom.example")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.get("CUSTOM.EXAMPLE."); err != nil {
		t.Fatal(err)
	}
	if !m.hasUsable("CUSTOM.EXAMPLE.") {
		t.Fatal("normalized custom host certificate is not usable")
	}
	if m.needsRenewal("CUSTOM.EXAMPLE.", time.Minute) {
		t.Fatal("normalized custom host certificate unexpectedly needs renewal")
	}
	store.close()
	store, err = openStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	m, err = newCertificateManager(store, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.get("custom.example"); err != nil {
		t.Fatalf("custom certificate was not restored after restart: %v", err)
	}
}

func TestCertificateManagerRejectsCertificateForAnotherHost(t *testing.T) {
	store, err := openStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	m, err := newCertificateManager(store, "")
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
	store, err := openStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	m, err := newCertificateManager(store, "")
	if err != nil {
		t.Fatal(err)
	}
	defaultCertificate := testCertificate(t, "pages.example")
	defaultCertificate.Host = ""
	if err := m.save(context.Background(), defaultCertificate); err != nil {
		t.Fatal(err)
	}
	if !m.needsRenewal("custom.example", time.Hour) {
		t.Fatal("custom host unexpectedly reused the default certificate")
	}
}

func TestCertificateManagerLoadsOperatorHostCertificate(t *testing.T) {
	store, err := openStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
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
	m, err := newCertificateManager(store, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.get(host); err != nil {
		t.Fatal(err)
	}
}

func TestCertificateManagerRejectsPartialDefaultCertificateFiles(t *testing.T) {
	store, err := openStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "default"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "default", "fullchain.pem"), []byte("certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newCertificateManager(store, root); err == nil {
		t.Fatal("partial default certificate files were accepted")
	}
}

func TestPruneUnusedACMECertificates(t *testing.T) {
	store, err := openStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	m, err := newCertificateManager(store, "")
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
	changed, err := m.pruneUnusedCustom(map[string]struct{}{})
	if err != nil || !changed {
		t.Fatalf("prune unused certificate: changed=%t err=%v", changed, err)
	}
	if m.hasUsable("old.example") || !m.hasUsable("operator.example") {
		t.Fatal("unused ACME certificate remains or operator certificate was removed")
	}
}

func TestCertificateManagerRemovesStaleACMEDefault(t *testing.T) {
	root := t.TempDir()
	store, err := openStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	m, err := newCertificateManager(store, "")
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
	if m.hasUsable("") {
		t.Fatal("stale ACME default certificate remains usable")
	}
	store.close()
	store, err = openStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	m, err = newCertificateManager(store, "")
	if err != nil {
		t.Fatal(err)
	}
	if m.hasUsable("") {
		t.Fatal("stale ACME default certificate was restored")
	}
}

func TestCertificateManagerRejectsStaleOperatorDefault(t *testing.T) {
	store, err := openStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	m, err := newCertificateManager(store, "")
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
	store, err := openStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
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
	m, err := newCertificateManager(store, root)
	if err != nil {
		t.Fatal(err)
	}
	if !m.hasUsable(host) {
		t.Fatal("operator host certificate was not loaded")
	}
	if err := os.RemoveAll(filepath.Join(root, "hosts")); err != nil {
		t.Fatal(err)
	}
	m, err = newCertificateManager(store, root)
	if err != nil {
		t.Fatal(err)
	}
	if m.hasUsable(host) {
		t.Fatal("removed operator host certificate remains usable")
	}
}
