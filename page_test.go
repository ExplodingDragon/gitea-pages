package page

import (
	"path/filepath"
	"testing"
)

func TestLoadConfigValidatesInstanceStorageAndAddress(t *testing.T) {
	dataRoot := filepath.Join(t.TempDir(), "pages")
	t.Setenv("GITEA_PAGES_GITEA_URL", "https://gitea.example.test")
	t.Setenv("GITEA_PAGES_ROOT_URL", "http://pages.example.test")
	t.Setenv("GITEA_PAGES_TOKEN", "secret")
	t.Setenv("GITEA_PAGES_DATA_ROOT", dataRoot)
	t.Setenv("GITEA_PAGES_HTTP_LISTEN", "127.0.0.1:8080")
	t.Setenv("GITEA_PAGES_ETCD_QUOTA_BYTES", "134217728")
	cfg, err := LoadConfig(ModeStandalone)
	if err != nil || cfg.EtcdQuotaBytes != 128*1024*1024 {
		t.Fatalf("valid config: quota=%d err=%v", cfg.EtcdQuotaBytes, err)
	}
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
