// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"
)

type fileConfig struct {
	Gitea struct {
		URL   string `yaml:"url"`
		Token string `yaml:"token"`
	} `yaml:"gitea"`
	Pages struct {
		RootURL    string `yaml:"root_url"`
		DataRoot   string `yaml:"data_root"`
		CustomRoot string `yaml:"custom_root"`
	} `yaml:"pages"`
	Server struct {
		HTTPListen string `yaml:"http_listen"`
		HTTPS      struct {
			Mode            string `yaml:"mode"`
			Listen          string `yaml:"listen"`
			CertificateRoot string `yaml:"certificate_root"`
			ACME            struct {
				DirectoryURL string `yaml:"directory_url"`
				Email        string `yaml:"email"`
				EAB          struct {
					KID     string `yaml:"kid"`
					HMACKey string `yaml:"hmac_key"`
				} `yaml:"eab"`
				DNS01 struct {
					Provider   string            `yaml:"provider"`
					Parameters map[string]string `yaml:"parameters"`
					Resolver   string            `yaml:"resolver"`
				} `yaml:"dns01"`
				HTTP01 struct {
					ProbeAddresses []string `yaml:"probe_addresses"`
				} `yaml:"http01"`
			} `yaml:"acme"`
		} `yaml:"https"`
	} `yaml:"server"`
	Sync struct {
		Interval      string `yaml:"interval"`
		DeployTimeout string `yaml:"deploy_timeout"`
		QueueSize     *int   `yaml:"queue_size"`
		Concurrency   *int   `yaml:"concurrency"`
	} `yaml:"sync"`
	Storage struct {
		EtcdQuotaBytes *int64 `yaml:"etcd_quota_bytes"`
	} `yaml:"storage"`
	Domains struct {
		Resolver        string `yaml:"resolver"`
		RefreshInterval string `yaml:"refresh_interval"`
	} `yaml:"domains"`
}

// LoadConfig reads one strict YAML document and returns its validated runtime
// configuration.
func LoadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open Pages config: %w", err)
	}
	defer func() { _ = f.Close() }()
	decoder := yaml.NewDecoder(f)
	decoder.KnownFields(true)
	var raw fileConfig
	if err := decoder.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("decode Pages config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, errors.New("pages config must contain one YAML document")
		}
		return Config{}, fmt.Errorf("decode Pages config: %w", err)
	}
	return raw.runtimeConfig()
}

func (raw fileConfig) runtimeConfig() (Config, error) {
	var cfg Config
	var err error
	cfg.GiteaURL, err = parseRootURL("gitea.url", raw.Gitea.URL)
	if err != nil {
		return cfg, err
	}
	cfg.RootURL, err = parseRootURL("pages.root_url", raw.Pages.RootURL)
	if err != nil {
		return cfg, err
	}
	cfg.Token = strings.TrimSpace(raw.Gitea.Token)
	if cfg.Token == "" {
		return cfg, errors.New("gitea.token is required")
	}
	cfg.DataRoot = strings.TrimSpace(raw.Pages.DataRoot)
	cfg.CustomRoot = strings.TrimSpace(raw.Pages.CustomRoot)
	cfg.HTTPListen = strings.TrimSpace(raw.Server.HTTPListen)
	cfg.HTTPSMode = HTTPSMode(strings.ToLower(strings.TrimSpace(raw.Server.HTTPS.Mode)))
	cfg.HTTPSListen = strings.TrimSpace(raw.Server.HTTPS.Listen)
	cfg.CertRoot = strings.TrimSpace(raw.Server.HTTPS.CertificateRoot)

	cfg.SyncInterval, err = configDuration("sync.interval", raw.Sync.Interval, defaultSyncInterval)
	if err != nil {
		return cfg, err
	}
	cfg.DeployTimeout, err = configDuration("sync.deploy_timeout", raw.Sync.DeployTimeout, defaultDeployTimeout)
	if err != nil {
		return cfg, err
	}
	cfg.DeployQueueSize = defaultDeployQueueSize
	if raw.Sync.QueueSize != nil {
		cfg.DeployQueueSize = *raw.Sync.QueueSize
	}
	cfg.DeployConcurrency = defaultDeployConcurrency
	if raw.Sync.Concurrency != nil {
		cfg.DeployConcurrency = *raw.Sync.Concurrency
	}
	cfg.EtcdQuotaBytes = defaultEtcdQuotaBytes
	if raw.Storage.EtcdQuotaBytes != nil {
		cfg.EtcdQuotaBytes = *raw.Storage.EtcdQuotaBytes
	}
	cfg.DNSResolver, err = configDNSResolver("domains.resolver", raw.Domains.Resolver)
	if err != nil {
		return cfg, err
	}
	cfg.DNSRefreshInterval, err = configDuration("domains.refresh_interval", raw.Domains.RefreshInterval, defaultDNSRefreshInterval)
	if err != nil {
		return cfg, err
	}

	acme := raw.Server.HTTPS.ACME
	cfg.ACMEDirectoryURL = strings.TrimSpace(acme.DirectoryURL)
	cfg.ACMEEmail = strings.TrimSpace(acme.Email)
	cfg.ACMEEABKID = strings.TrimSpace(acme.EAB.KID)
	if key := strings.TrimSpace(acme.EAB.HMACKey); key != "" {
		cfg.ACMEEABKey, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(key, "="))
		if err != nil || len(cfg.ACMEEABKey) < 16 {
			return cfg, errors.New("server.https.acme.eab.hmac_key must be a base64url HMAC key")
		}
	}
	cfg.ACMEDNSResolver, err = configDNSResolver("server.https.acme.dns01.resolver", acme.DNS01.Resolver)
	if err != nil {
		return cfg, err
	}
	if cfg.ACMEDNSResolver == "" && cfg.HTTPSMode == HTTPSPages && cfg.ACMEDirectoryURL != "" {
		cfg.ACMEDNSResolver = cfg.DNSResolver
	}
	cfg.DNSProvider = strings.ToLower(strings.TrimSpace(acme.DNS01.Provider))
	cfg.DNSProviderParameters = maps.Clone(acme.DNS01.Parameters)
	for i, value := range acme.HTTP01.ProbeAddresses {
		name := fmt.Sprintf("server.https.acme.http01.probe_addresses[%d]", i)
		address, parseErr := netip.ParseAddrPort(strings.TrimSpace(value))
		if parseErr != nil {
			return cfg, fmt.Errorf("%s must be an IP:port address", name)
		}
		if address.Port() == 0 {
			return cfg, fmt.Errorf("%s must include a nonzero TCP port", name)
		}
		cfg.HTTP01ProbeAddresses = append(cfg.HTTP01ProbeAddresses, address.String())
	}
	if err := cfg.validateBuiltInServer(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func configDuration(name, value string, fallback time.Duration) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return d, nil
}

func configDNSResolver(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if address, err := netip.ParseAddr(value); err == nil {
		return net.JoinHostPort(address.String(), "53"), nil
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", fmt.Errorf("%s must be an IP address with an optional numeric port", name)
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return "", fmt.Errorf("%s must use an IP address", name)
	}
	parsedPort, err := strconv.ParseUint(port, 10, 16)
	if err != nil || parsedPort == 0 {
		return "", fmt.Errorf("%s must use a port between 1 and 65535", name)
	}
	return net.JoinHostPort(address.String(), strconv.FormatUint(parsedPort, 10)), nil
}
