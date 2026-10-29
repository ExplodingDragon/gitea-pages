package page

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

func testCertificate(t *testing.T, host string) CertificateRecord {
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
	return CertificateRecord{Host: host, CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))}
}

func TestCertificateManagerPersistsAndSelectsSNI(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewCertificateManager(store, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Save(context.Background(), testCertificate(t, "custom.example")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get("custom.example"); err != nil {
		t.Fatal(err)
	}
	nginxRoot := t.TempDir()
	if err := m.ExportNginx(nginxRoot); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(nginxRoot, "certs", "custom.example", "privkey.pem")
	keyInfo, err := os.Stat(keyPath)
	if err != nil || keyInfo.Mode().Perm() != 0600 {
		t.Fatalf("Nginx private key mode: info=%v err=%v", keyInfo, err)
	}
	store.Close()
	store, err = OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m, err = NewCertificateManager(store, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get("custom.example"); err != nil {
		t.Fatalf("custom certificate was not restored after restart: %v", err)
	}
}

func TestCertificateManagerRejectsCertificateForAnotherHost(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m, err := NewCertificateManager(store, "")
	if err != nil {
		t.Fatal(err)
	}
	record := testCertificate(t, "other.example")
	record.Host = "custom.example"
	if err := m.Save(context.Background(), record); err == nil {
		t.Fatal("certificate for another host was accepted")
	}
}

func TestCertificateManagerDoesNotUseDefaultForCustomRenewal(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m, err := NewCertificateManager(store, "")
	if err != nil {
		t.Fatal(err)
	}
	defaultCertificate := testCertificate(t, "pages.example")
	defaultCertificate.Host = ""
	if err := m.Save(context.Background(), defaultCertificate); err != nil {
		t.Fatal(err)
	}
	if !m.NeedsRenewal("custom.example", time.Hour) {
		t.Fatal("custom host unexpectedly reused the default certificate")
	}
}

func TestCertificateManagerLoadsOperatorHostCertificate(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := t.TempDir()
	host := "custom.example"
	record := testCertificate(t, host)
	sum := sha256.Sum256([]byte(host))
	dir := filepath.Join(root, "hosts", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host"), []byte(host+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fullchain.pem"), []byte(record.CertificatePEM), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "privkey.pem"), []byte(record.PrivateKeyPEM), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := NewCertificateManager(store, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(host); err != nil {
		t.Fatal(err)
	}
}

func TestCertificateManagerRejectsPartialDefaultCertificateFiles(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "default"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "default", "fullchain.pem"), []byte("certificate"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCertificateManager(store, root); err == nil {
		t.Fatal("partial default certificate files were accepted")
	}
}

func TestPruneUnusedACMECertificates(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m, err := NewCertificateManager(store, "")
	if err != nil {
		t.Fatal(err)
	}
	acme := testCertificate(t, "old.example")
	acme.Source = "acme"
	if err := m.Save(context.Background(), acme); err != nil {
		t.Fatal(err)
	}
	operator := testCertificate(t, "operator.example")
	operator.Source = "operator"
	if err := m.Save(context.Background(), operator); err != nil {
		t.Fatal(err)
	}
	nginxRoot := t.TempDir()
	if err := m.ExportNginx(nginxRoot); err != nil {
		t.Fatal(err)
	}
	changed, err := m.PruneUnusedCustom(map[string]struct{}{})
	if err != nil || !changed {
		t.Fatalf("prune unused certificate: changed=%t err=%v", changed, err)
	}
	if m.HasUsable("old.example") || !m.HasUsable("operator.example") {
		t.Fatal("unused ACME certificate remains or operator certificate was removed")
	}
	if err := m.ExportNginx(nginxRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(nginxRoot, "certs", "old.example")); !os.IsNotExist(err) {
		t.Fatalf("obsolete Nginx certificate directory remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nginxRoot, "certs", "operator.example", "privkey.pem")); err != nil {
		t.Fatalf("operator certificate output was removed: %v", err)
	}
}

func TestCertificateManagerRemovesStaleACMEDefault(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewCertificateManager(store, "")
	if err != nil {
		t.Fatal(err)
	}
	record := testCertificate(t, "old.pages.example")
	record.Host = ""
	record.Source = "acme"
	if err := m.Save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureDefaultCertificateHosts("new.pages.example", "pages-check.new.pages.example"); err != nil {
		t.Fatalf("check stale ACME default: %v", err)
	}
	if m.HasUsable("") {
		t.Fatal("stale ACME default certificate remains usable")
	}
	store.Close()
	store, err = OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m, err = NewCertificateManager(store, "")
	if err != nil {
		t.Fatal(err)
	}
	if m.HasUsable("") {
		t.Fatal("stale ACME default certificate was restored")
	}
}

func TestCertificateManagerRejectsStaleOperatorDefault(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m, err := NewCertificateManager(store, "")
	if err != nil {
		t.Fatal(err)
	}
	record := testCertificate(t, "old.pages.example")
	record.Host = ""
	record.Source = "operator"
	if err := m.Save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureDefaultCertificateHosts("new.pages.example"); err == nil {
		t.Fatal("stale operator default certificate was accepted")
	}
}

func TestCertificateManagerPrunesRemovedOperatorHost(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	host := "custom.example"
	record := testCertificate(t, host)
	if err := os.MkdirAll(filepath.Join(root, "hosts"), 0755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(host))
	dir := filepath.Join(root, "hosts", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host"), []byte(host), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fullchain.pem"), []byte(record.CertificatePEM), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "privkey.pem"), []byte(record.PrivateKeyPEM), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := NewCertificateManager(store, root)
	if err != nil {
		t.Fatal(err)
	}
	if !m.HasUsable(host) {
		t.Fatal("operator host certificate was not loaded")
	}
	if err := os.RemoveAll(filepath.Join(root, "hosts")); err != nil {
		t.Fatal(err)
	}
	m, err = NewCertificateManager(store, root)
	if err != nil {
		t.Fatal(err)
	}
	if m.HasUsable(host) {
		t.Fatal("removed operator host certificate remains usable")
	}
}
