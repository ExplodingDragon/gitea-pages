// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pages.yaml")
	content = strings.ReplaceAll(content, "${DATA_ROOT}", filepath.Join(t.TempDir(), "data"))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `
gitea:
  url: http://gitea.example.test
  token: secret
pages:
  root_url: http://pages.example.test
  data_root: ${DATA_ROOT}
server:
  http_listen: 127.0.0.1:8080
  https:
    mode: disabled
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPSMode != HTTPSDisabled || cfg.SyncInterval != time.Minute || cfg.DeployTimeout != 10*time.Minute {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	if cfg.DeployQueueSize != 32 || cfg.DeployConcurrency != 2 || cfg.EtcdQuotaBytes != 2*1024*1024*1024 {
		t.Fatalf("unexpected capacity defaults: %#v", cfg)
	}
	if cfg.GiteaURL.Scheme != "http" || cfg.DNSRefreshInterval != 15*time.Minute {
		t.Fatalf("unexpected URL or DNS defaults: %#v", cfg)
	}
}

func TestLoadConfigHTTPSModes(t *testing.T) {
	proxy, err := LoadConfig(writeConfig(t, `
gitea: {url: https://gitea.example.test, token: secret}
pages: {root_url: https://pages.example.test, data_root: "${DATA_ROOT}"}
server:
  http_listen: 127.0.0.1:8080
  https: {mode: proxy}
`))
	if err != nil {
		t.Fatal(err)
	}
	if proxy.HTTPSMode != HTTPSProxy || proxy.hasPagesHTTPSConfiguration() {
		t.Fatalf("proxy mode enabled local certificate handling: %#v", proxy)
	}

	managed, err := LoadConfig(writeConfig(t, `
gitea: {url: https://gitea.example.test, token: secret}
pages: {root_url: https://pages.example.test, data_root: "${DATA_ROOT}"}
server:
  http_listen: :80
  https:
    mode: pages
    listen: :443
    acme:
      directory_url: https://acme.example.test/directory
      email: pages@example.test
      dns01:
        resolver: 1.1.1.1:5353
        provider: EXEC
        parameters:
          EXEC_PATH: /bin/true
          EXEC_MODE: RAW
      http01:
        probe_addresses: ["127.0.0.1:80", "[::1]:80"]
domains:
  resolver: 2001:db8::53
  refresh_interval: 20m
`))
	if err != nil {
		t.Fatal(err)
	}
	if managed.ACMEDNSResolver != "1.1.1.1:5353" || managed.DNSResolver != "[2001:db8::53]:53" {
		t.Fatalf("unexpected DNS resolvers: %q, %q", managed.ACMEDNSResolver, managed.DNSResolver)
	}
	if len(managed.HTTP01ProbeAddresses) != 2 || managed.DNSRefreshInterval != 20*time.Minute {
		t.Fatalf("unexpected Pages HTTPS config: %#v", managed)
	}
	if managed.DNSProvider != "exec" || managed.DNSProviderParameters["EXEC_PATH"] != "/bin/true" {
		t.Fatalf("unexpected DNS provider config: %#v", managed.DNSProviderParameters)
	}
}

func TestLoadConfigRejectsUnknownFieldsAndExtraDocuments(t *testing.T) {
	unknown := writeConfig(t, `
gitea: {url: http://gitea.example.test, token: secret}
pages: {root_url: http://pages.example.test, data_root: "${DATA_ROOT}", unknown: true}
server: {http_listen: 127.0.0.1:8080, https: {mode: disabled}}
`)
	if _, err := LoadConfig(unknown); err == nil {
		t.Fatal("unknown YAML field was accepted")
	}
	extra := writeConfig(t, `
gitea: {url: http://gitea.example.test, token: secret}
pages: {root_url: http://pages.example.test, data_root: "${DATA_ROOT}"}
server: {http_listen: 127.0.0.1:8080, https: {mode: disabled}}
---
gitea: {}
`)
	if _, err := LoadConfig(extra); err == nil {
		t.Fatal("multiple YAML documents were accepted")
	}
}

func TestLoadConfigValidatesHTTPSOwnership(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{name: "proxy certificate", config: `
gitea: {url: https://gitea.example.test, token: secret}
pages: {root_url: https://pages.example.test, data_root: "${DATA_ROOT}"}
server:
  http_listen: 127.0.0.1:8080
  https: {mode: proxy, certificate_root: /var/lib/gitea-pages-certs}
`},
		{name: "proxy DNS provider", config: `
gitea: {url: https://gitea.example.test, token: secret}
pages: {root_url: https://pages.example.test, data_root: "${DATA_ROOT}"}
server:
  http_listen: 127.0.0.1:8080
  https:
    mode: proxy
    acme:
      dns01: {provider: exec}
`},
		{name: "proxy HTTP address", config: `
gitea: {url: http://gitea.example.test, token: secret}
pages: {root_url: http://pages.example.test, data_root: "${DATA_ROOT}"}
server: {http_listen: 127.0.0.1:8080, https: {mode: proxy}}
`},
		{name: "Pages listener", config: `
gitea: {url: https://gitea.example.test, token: secret}
pages: {root_url: https://pages.example.test, data_root: "${DATA_ROOT}"}
server: {http_listen: 127.0.0.1:8080, https: {mode: pages}}
`},
		{name: "DNS parameters without provider", config: `
gitea: {url: https://gitea.example.test, token: secret}
pages: {root_url: https://pages.example.test, data_root: "${DATA_ROOT}"}
server:
  http_listen: 127.0.0.1:8080
  https:
    mode: pages
    listen: 127.0.0.1:8443
    acme:
      directory_url: https://acme.example.test/directory
      email: pages@example.test
      dns01:
        parameters:
          EXEC_PATH: /bin/true
`},
		{name: "invalid DNS parameter name", config: `
gitea: {url: https://gitea.example.test, token: secret}
pages: {root_url: https://pages.example.test, data_root: "${DATA_ROOT}"}
server:
  http_listen: 127.0.0.1:8080
  https:
    mode: pages
    listen: 127.0.0.1:8443
    acme:
      directory_url: https://acme.example.test/directory
      email: pages@example.test
      dns01:
        provider: exec
        parameters:
          "INVALID-NAME": /bin/true
`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := LoadConfig(writeConfig(t, test.config)); err == nil {
				t.Fatal("invalid HTTPS ownership was accepted")
			}
		})
	}
}
