// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigRejectsUnknownMode(t *testing.T) {
	if _, err := LoadConfig(Mode("unknown")); err == nil {
		t.Fatal("unknown serving mode was accepted")
	}
}

func TestLoadConfigValidatesInstanceStorageAndAddress(t *testing.T) {
	dataRoot := filepath.Join(t.TempDir(), "pages")
	t.Setenv("GITEA_PAGES_GITEA_URL", "http://gitea.example.test")
	t.Setenv("GITEA_PAGES_ROOT_URL", "http://pages.example.test")
	t.Setenv("GITEA_PAGES_TOKEN", "secret")
	t.Setenv("GITEA_PAGES_DATA_ROOT", dataRoot)
	t.Setenv("GITEA_PAGES_HTTP_LISTEN", "127.0.0.1:8080")
	t.Setenv("GITEA_PAGES_ETCD_QUOTA_BYTES", "134217728")
	cfg, err := LoadConfig(ModeStandalone)
	if err != nil || cfg.EtcdQuotaBytes != 128*1024*1024 {
		t.Fatalf("valid config: quota=%d err=%v", cfg.EtcdQuotaBytes, err)
	}
	if cfg.GiteaURL.Scheme != "http" {
		t.Fatalf("Gitea URL scheme = %q, want http", cfg.GiteaURL.Scheme)
	}
	t.Setenv("GITEA_PAGES_HTTP01_PROBE_ADDRESSES", "127.0.0.1:80, [::1]:8080")
	cfg, err = LoadConfig(ModeStandalone)
	if err != nil || len(cfg.HTTP01ProbeAddresses) != 2 || cfg.HTTP01ProbeAddresses[1] != "[::1]:8080" {
		t.Fatalf("HTTP-01 probe addresses = %v, err=%v", cfg.HTTP01ProbeAddresses, err)
	}
	t.Setenv("GITEA_PAGES_HTTP01_PROBE_ADDRESSES", "pages.example.test:80")
	if _, err := LoadConfig(ModeStandalone); err == nil {
		t.Fatal("an HTTP-01 probe host name was accepted instead of an IP:port address")
	}
	t.Setenv("GITEA_PAGES_HTTP01_PROBE_ADDRESSES", "")
	t.Setenv("GITEA_PAGES_ROOT_URL", "http://pages.example.test:8080")
	if _, err := LoadConfig(ModeStandalone); err == nil {
		t.Fatal("a root URL that Gitea cannot register was accepted")
	}
	t.Setenv("GITEA_PAGES_ROOT_URL", "http://pages.example.test")
	t.Setenv("GITEA_PAGES_ETCD_QUOTA_BYTES", "1024")
	if _, err := LoadConfig(ModeStandalone); err == nil {
		t.Fatal("an unusably small embedded etcd quota was accepted")
	}
	t.Setenv("GITEA_PAGES_ETCD_QUOTA_BYTES", "134217728")
	t.Setenv("GITEA_PAGES_CERT_ROOT", filepath.Join(dataRoot, "certificates"))
	if _, err := LoadConfig(ModeStandalone); err == nil {
		t.Fatal("a certificate source inside the mutable data root was accepted")
	}
}

func TestLoadConfigValidatesDNSCommandArguments(t *testing.T) {
	t.Setenv("GITEA_PAGES_GITEA_URL", "https://gitea.example.test")
	t.Setenv("GITEA_PAGES_ROOT_URL", "http://pages.example.test")
	t.Setenv("GITEA_PAGES_TOKEN", "secret")
	t.Setenv("GITEA_PAGES_DATA_ROOT", filepath.Join(t.TempDir(), "pages"))
	t.Setenv("GITEA_PAGES_HTTP_LISTEN", "127.0.0.1:8080")
	t.Setenv("GITEA_PAGES_DNS_PRESENT_COMMAND", `["dns-present","{name}"]`)
	t.Setenv("GITEA_PAGES_DNS_CLEANUP_COMMAND", `["dns-cleanup","{name}","{value}"]`)
	if _, err := LoadConfig(ModeStandalone); err == nil {
		t.Fatal("a DNS command without the TXT value was accepted")
	}
	t.Setenv("GITEA_PAGES_DNS_PRESENT_COMMAND", `["dns-present","{name}","{value}"]`)
	cfg, err := LoadConfig(ModeStandalone)
	if err != nil {
		t.Fatalf("valid DNS commands: %v", err)
	}
	if cfg.DNSProvider != "command" {
		t.Fatalf("DNS provider = %q, want command", cfg.DNSProvider)
	}
}

func TestLoadConfigDNSResolvers(t *testing.T) {
	t.Setenv("GITEA_PAGES_GITEA_URL", "https://gitea.example.test")
	t.Setenv("GITEA_PAGES_ROOT_URL", "http://pages.example.test")
	t.Setenv("GITEA_PAGES_TOKEN", "secret")
	t.Setenv("GITEA_PAGES_DATA_ROOT", filepath.Join(t.TempDir(), "pages"))
	t.Setenv("GITEA_PAGES_HTTP_LISTEN", "127.0.0.1:8080")
	t.Setenv("GITEA_PAGES_DNS_RESOLVER", "2001:db8::53")

	cfg, err := LoadConfig(ModeStandalone)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DNSResolver != "[2001:db8::53]:53" || cfg.ACMEDNSResolver != cfg.DNSResolver {
		t.Fatalf("DNS resolvers = %q, %q", cfg.DNSResolver, cfg.ACMEDNSResolver)
	}

	t.Setenv("GITEA_PAGES_ACME_DNS_RESOLVER", "1.1.1.1:5353")
	cfg, err = LoadConfig(ModeStandalone)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ACMEDNSResolver != "1.1.1.1:5353" {
		t.Fatalf("ACME DNS resolver = %q", cfg.ACMEDNSResolver)
	}
	if cfg.DNSRefreshInterval != 15*time.Minute {
		t.Fatalf("DNS refresh interval = %s", cfg.DNSRefreshInterval)
	}
	t.Setenv("GITEA_PAGES_DNS_REFRESH_INTERVAL", "20m")
	cfg, err = LoadConfig(ModeStandalone)
	if err != nil || cfg.DNSRefreshInterval != 20*time.Minute {
		t.Fatalf("custom DNS refresh interval = %s, err=%v", cfg.DNSRefreshInterval, err)
	}

	for _, value := range []string{"resolver.example.test", "1.1.1.1:0", "1.1.1.1:dns"} {
		t.Setenv("GITEA_PAGES_DNS_RESOLVER", value)
		if _, err := LoadConfig(ModeStandalone); err == nil {
			t.Fatalf("invalid DNS resolver %q was accepted", value)
		}
	}
	t.Setenv("GITEA_PAGES_DNS_RESOLVER", "1.1.1.1")
	t.Setenv("GITEA_PAGES_DNS_REFRESH_INTERVAL", "30s")
	if _, err := LoadConfig(ModeStandalone); err == nil {
		t.Fatal("a DNS refresh interval below one minute was accepted")
	}
	t.Setenv("GITEA_PAGES_DNS_REFRESH_INTERVAL", "25h")
	if _, err := LoadConfig(ModeStandalone); err == nil {
		t.Fatal("a DNS refresh interval longer than the authorization grace period was accepted")
	}
}
