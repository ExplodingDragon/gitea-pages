// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultSyncInterval       = time.Minute
	defaultDeployTimeout      = 10 * time.Minute
	defaultDeployQueueSize    = 32
	defaultDeployConcurrency  = 2
	defaultDNSRefreshInterval = 15 * time.Minute
)

// HTTPSMode identifies which component owns the public HTTPS entry.
type HTTPSMode string

const (
	HTTPSDisabled HTTPSMode = "disabled"
	HTTPSPages    HTTPSMode = "pages"
	HTTPSProxy    HTTPSMode = "proxy"
)

// Config contains the synchronization, storage, HTTP, DNS, and certificate
// settings used by one Pages process. Programs embedding Service with a custom
// Publisher do not use the built-in listener and certificate fields.
type Config struct {
	GiteaURL              *url.URL
	Token                 string
	RootURL               *url.URL
	DataRoot              string
	CustomRoot            string
	SyncInterval          time.Duration
	DeployTimeout         time.Duration
	DeployQueueSize       int
	DeployConcurrency     int
	EtcdQuotaBytes        int64
	DNSResolver           string
	DNSRefreshInterval    time.Duration
	HTTPListen            string
	HTTPSMode             HTTPSMode
	HTTPSListen           string
	CertRoot              string
	ACMEDirectoryURL      string
	ACMEEmail             string
	ACMEEABKID            string
	ACMEEABKey            []byte
	ACMEDNSResolver       string
	HTTP01ProbeAddresses  []string
	DNSProvider           string
	DNSProviderParameters map[string]string
}

func (cfg *Config) applyDefaults() {
	if cfg.SyncInterval == 0 {
		cfg.SyncInterval = defaultSyncInterval
	}
	if cfg.DeployTimeout == 0 {
		cfg.DeployTimeout = defaultDeployTimeout
	}
	if cfg.DeployQueueSize == 0 {
		cfg.DeployQueueSize = defaultDeployQueueSize
	}
	if cfg.DeployConcurrency == 0 {
		cfg.DeployConcurrency = defaultDeployConcurrency
	}
	if cfg.EtcdQuotaBytes == 0 {
		cfg.EtcdQuotaBytes = defaultEtcdQuotaBytes
	}
	if cfg.DNSRefreshInterval == 0 {
		cfg.DNSRefreshInterval = defaultDNSRefreshInterval
	}
}

// Validate checks the configuration shared by the synchronization service and
// every Publisher implementation.
func (cfg Config) Validate() error {
	cfg.applyDefaults()
	for _, configuredURL := range []struct {
		name  string
		value *url.URL
	}{{"Gitea URL", cfg.GiteaURL}, {"Pages root URL", cfg.RootURL}} {
		name, value := configuredURL.name, configuredURL.value
		if value == nil || value.Host == "" || value.User != nil || value.Path != "" || value.RawQuery != "" || value.Fragment != "" || value.Scheme != "http" && value.Scheme != "https" {
			return fmt.Errorf("%s must be an HTTP or HTTPS root URL", name)
		}
	}
	if cfg.RootURL.Port() != "" {
		return errors.New("pages root URL must use the standard HTTP or HTTPS port")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return errors.New("pages instance token is required")
	}
	if cfg.DataRoot == "" || !filepath.IsAbs(cfg.DataRoot) {
		return errors.New("pages data root must be an absolute path")
	}
	if cfg.SyncInterval < time.Minute {
		return errors.New("pages synchronization interval must be at least one minute")
	}
	if cfg.DeployTimeout <= 0 || cfg.DeployQueueSize < 1 || cfg.DeployConcurrency < 1 {
		return errors.New("pages deployment timeout, queue size, and concurrency must be positive")
	}
	if cfg.EtcdQuotaBytes < 64*1024*1024 {
		return errors.New("embedded etcd quota must be at least 67108864 bytes")
	}
	if cfg.DNSRefreshInterval < time.Minute || cfg.DNSRefreshInterval > 24*time.Hour {
		return errors.New("pages DNS refresh interval must be between one minute and 24 hours")
	}
	return nil
}

func (cfg Config) validateBuiltInServer() error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.HTTPListen) == "" {
		return errors.New("server.http_listen is required")
	}
	switch cfg.HTTPSMode {
	case HTTPSDisabled:
		if cfg.RootURL.Scheme != "http" {
			return errors.New("server.https.mode disabled requires an http pages.root_url")
		}
	case HTTPSProxy:
		if cfg.RootURL.Scheme != "https" {
			return errors.New("server.https.mode proxy requires an https pages.root_url")
		}
	case HTTPSPages:
		if cfg.RootURL.Scheme != "https" {
			return errors.New("server.https.mode pages requires an https pages.root_url")
		}
		if strings.TrimSpace(cfg.HTTPSListen) == "" {
			return errors.New("server.https.listen is required when Pages provides HTTPS")
		}
	default:
		return fmt.Errorf("unsupported server.https.mode %q", cfg.HTTPSMode)
	}
	if cfg.HTTPSMode != HTTPSPages && cfg.hasPagesHTTPSConfiguration() {
		return errors.New("server.https certificate and ACME settings require mode pages")
	}
	if err := cfg.validateACME(); err != nil {
		return err
	}
	if cfg.CertRoot != "" {
		if !filepath.IsAbs(cfg.CertRoot) {
			return errors.New("server.https.certificate_root must be an absolute path")
		}
		if pathsOverlap(cfg.CertRoot, cfg.DataRoot) {
			return errors.New("server.https.certificate_root and pages.data_root must be separate directories")
		}
	}
	if cfg.CustomRoot != "" {
		if !filepath.IsAbs(cfg.CustomRoot) {
			return errors.New("pages.custom_root must be an absolute path")
		}
		entry, err := os.Lstat(cfg.CustomRoot)
		if err != nil {
			return fmt.Errorf("pages.custom_root: %w", err)
		}
		if entry.Mode()&os.ModeSymlink != 0 || !entry.IsDir() {
			return errors.New("pages.custom_root must be a directory, not a symbolic link")
		}
		for name, path := range map[string]string{"pages.data_root": cfg.DataRoot, "server.https.certificate_root": cfg.CertRoot} {
			if path != "" && pathsOverlap(cfg.CustomRoot, path) {
				return fmt.Errorf("pages.custom_root overlaps %s", name)
			}
		}
	}
	return nil
}

func (cfg Config) hasPagesHTTPSConfiguration() bool {
	return cfg.HTTPSListen != "" || cfg.CertRoot != "" || cfg.hasACMEConfiguration()
}

func (cfg Config) hasACMEConfiguration() bool {
	return cfg.ACMEDirectoryURL != "" || cfg.ACMEEmail != "" || cfg.ACMEEABKID != "" || len(cfg.ACMEEABKey) != 0 || cfg.ACMEDNSResolver != "" || len(cfg.HTTP01ProbeAddresses) != 0 || cfg.DNSProvider != "" || len(cfg.DNSProviderParameters) != 0
}

func (cfg Config) validateACME() error {
	if cfg.ACMEDirectoryURL == "" {
		if cfg.hasACMEConfiguration() {
			return errors.New("server.https.acme.directory_url is required for ACME settings")
		}
		return nil
	}
	if cfg.ACMEEmail == "" {
		return errors.New("server.https.acme.email is required when ACME is enabled")
	}
	if (cfg.ACMEEABKID == "") != (len(cfg.ACMEEABKey) == 0) {
		return errors.New("server.https.acme.eab.kid and hmac_key must be configured together")
	}
	if strings.TrimSpace(cfg.DNSProvider) == "" && len(cfg.DNSProviderParameters) != 0 {
		return errors.New("server.https.acme.dns01.provider is required when parameters are configured")
	}
	for name, value := range cfg.DNSProviderParameters {
		if !validEnvironmentName(name) {
			return fmt.Errorf("server.https.acme.dns01.parameters contains invalid name %q", name)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("server.https.acme.dns01.parameters[%q] contains a null byte", name)
		}
	}
	return nil
}

func validEnvironmentName(name string) bool {
	if name == "" || name[0] != '_' && (name[0] < 'A' || name[0] > 'Z') && (name[0] < 'a' || name[0] > 'z') {
		return false
	}
	for i := 1; i < len(name); i++ {
		if c := name[i]; c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func parseRootURL(name, raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%s must be an absolute URL without path, query, fragment, or user info", name)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%s must use http or https", name)
	}
	if strings.ContainsAny(u.Hostname(), " /\\\t\r\n") || u.Hostname() == "" {
		return nil, fmt.Errorf("%s must contain a valid host name", name)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u, nil
}

func pathsOverlap(a, b string) bool {
	return pathWithin(a, b) || pathWithin(b, a)
}
