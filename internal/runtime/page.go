// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Mode selects the built-in server or external Nginx publisher.
type Mode string

const (
	ModeStandalone Mode = "standalone"
	ModeNginx      Mode = "nginx"

	defaultSyncInterval       = time.Minute
	defaultDeployTimeout      = 10 * time.Minute
	defaultDeployQueueSize    = 32
	defaultDeployConcurrency  = 2
	defaultDNSRefreshInterval = 15 * time.Minute
)

// Config contains the settings shared by both Pages processes. Values come
// from the environment so a service manager can keep deployment-specific
// addresses and credentials out of the binary.
type Config struct {
	Mode                 Mode
	GiteaURL             *url.URL
	Token                string
	RootURL              *url.URL
	DataRoot             string
	SyncInterval         time.Duration
	DeployTimeout        time.Duration
	DeployQueueSize      int
	DeployConcurrency    int
	EtcdQuotaBytes       int64
	HTTPListen           string
	HTTPSListen          string
	CertRoot             string
	ACMEDirectoryURL     string
	ACMEEmail            string
	ACMEEABKID           string
	ACMEEABKey           []byte
	DNSProvider          string
	DNSResolver          string
	ACMEDNSResolver      string
	DNSRefreshInterval   time.Duration
	HTTP01ProbeAddresses []string
	DNSPresentCommand    []string
	DNSCleanupCommand    []string
	NginxRoot            string
	NginxHTTP01Root      string
	NginxHTTPProbeAddr   string
	NginxHTTPSProbeAddr  string
	NginxTestCommand     []string
	NginxReloadCommand   []string
	NginxCommandTimeout  time.Duration
	CustomRoot           string
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

// Validate checks the configuration required by Service. Serving adapters may
// require additional fields, such as Nginx output paths or listen addresses.
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

// LoadConfig reads and validates the environment for one serving mode.
func LoadConfig(mode Mode) (Config, error) {
	var cfg Config
	cfg.Mode = mode
	var err error
	cfg.GiteaURL, err = requiredURL("GITEA_PAGES_GITEA_URL")
	if err != nil {
		return cfg, err
	}
	cfg.RootURL, err = requiredURL("GITEA_PAGES_ROOT_URL")
	if err != nil {
		return cfg, err
	}
	if cfg.RootURL.Port() != "" {
		return cfg, errors.New("GITEA_PAGES_ROOT_URL must use the standard HTTP or HTTPS port")
	}
	cfg.Token = strings.TrimSpace(os.Getenv("GITEA_PAGES_TOKEN"))
	if cfg.Token == "" {
		return cfg, errors.New("GITEA_PAGES_TOKEN is required")
	}
	cfg.DataRoot = strings.TrimSpace(os.Getenv("GITEA_PAGES_DATA_ROOT"))
	if cfg.DataRoot == "" || !filepath.IsAbs(cfg.DataRoot) {
		return cfg, errors.New("GITEA_PAGES_DATA_ROOT must be an absolute path")
	}
	cfg.SyncInterval, err = envDuration("GITEA_PAGES_SYNC_INTERVAL", defaultSyncInterval)
	if err != nil {
		return cfg, err
	}
	if cfg.SyncInterval < time.Minute {
		return cfg, errors.New("GITEA_PAGES_SYNC_INTERVAL must be at least 1m")
	}
	cfg.DeployTimeout, err = envDuration("GITEA_PAGES_DEPLOY_TIMEOUT", defaultDeployTimeout)
	if err != nil {
		return cfg, err
	}
	cfg.DeployQueueSize, err = envInt("GITEA_PAGES_DEPLOY_QUEUE_SIZE", defaultDeployQueueSize)
	if err != nil {
		return cfg, err
	}
	if cfg.DeployQueueSize < 1 {
		return cfg, errors.New("GITEA_PAGES_DEPLOY_QUEUE_SIZE must be positive")
	}
	cfg.DeployConcurrency, err = envInt("GITEA_PAGES_DEPLOY_CONCURRENCY", defaultDeployConcurrency)
	if err != nil {
		return cfg, err
	}
	if cfg.DeployConcurrency < 1 {
		return cfg, errors.New("GITEA_PAGES_DEPLOY_CONCURRENCY must be positive")
	}
	cfg.EtcdQuotaBytes = defaultEtcdQuotaBytes
	if raw := strings.TrimSpace(os.Getenv("GITEA_PAGES_ETCD_QUOTA_BYTES")); raw != "" {
		cfg.EtcdQuotaBytes, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || cfg.EtcdQuotaBytes < 64*1024*1024 {
			return cfg, errors.New("GITEA_PAGES_ETCD_QUOTA_BYTES must be at least 67108864")
		}
	}
	cfg.CustomRoot = strings.TrimSpace(os.Getenv("GITEA_PAGES_CUSTOM_ROOT"))
	if cfg.CustomRoot != "" {
		if !filepath.IsAbs(cfg.CustomRoot) {
			return cfg, errors.New("GITEA_PAGES_CUSTOM_ROOT must be an absolute path")
		}
		entry, lstatErr := os.Lstat(cfg.CustomRoot)
		if lstatErr != nil {
			return cfg, fmt.Errorf("GITEA_PAGES_CUSTOM_ROOT: %w", lstatErr)
		}
		if entry.Mode()&os.ModeSymlink != 0 {
			return cfg, errors.New("GITEA_PAGES_CUSTOM_ROOT must not be a symbolic link")
		}
		info, statErr := os.Stat(cfg.CustomRoot)
		if statErr != nil {
			return cfg, fmt.Errorf("GITEA_PAGES_CUSTOM_ROOT: %w", statErr)
		}
		if !info.IsDir() {
			return cfg, errors.New("GITEA_PAGES_CUSTOM_ROOT must be a directory")
		}
	}
	if mode == ModeStandalone {
		cfg.HTTPListen = strings.TrimSpace(os.Getenv("GITEA_PAGES_HTTP_LISTEN"))
		if cfg.HTTPListen == "" {
			return cfg, errors.New("GITEA_PAGES_HTTP_LISTEN is required for standalone mode")
		}
		if cfg.RootURL.Scheme == "https" {
			cfg.HTTPSListen = strings.TrimSpace(os.Getenv("GITEA_PAGES_HTTPS_LISTEN"))
			if cfg.HTTPSListen == "" {
				return cfg, errors.New("GITEA_PAGES_HTTPS_LISTEN is required for an https standalone instance")
			}
		}
	} else {
		cfg.NginxRoot = strings.TrimSpace(os.Getenv("GITEA_PAGES_NGINX_ROOT"))
		if cfg.NginxRoot == "" || !filepath.IsAbs(cfg.NginxRoot) {
			return cfg, errors.New("GITEA_PAGES_NGINX_ROOT must be an absolute path")
		}
		cfg.NginxHTTP01Root = strings.TrimSpace(os.Getenv("GITEA_PAGES_NGINX_HTTP01_ROOT"))
		if cfg.RootURL.Scheme == "https" && (cfg.NginxHTTP01Root == "" || !filepath.IsAbs(cfg.NginxHTTP01Root)) {
			return cfg, errors.New("GITEA_PAGES_NGINX_HTTP01_ROOT must be an absolute path for HTTPS")
		}
		if cfg.NginxHTTP01Root != "" && !filepath.IsAbs(cfg.NginxHTTP01Root) {
			return cfg, errors.New("GITEA_PAGES_NGINX_HTTP01_ROOT must be an absolute path")
		}
		cfg.NginxHTTPProbeAddr, err = requiredProbeAddress("GITEA_PAGES_NGINX_HTTP_PROBE_ADDR")
		if err != nil {
			return cfg, err
		}
		if cfg.RootURL.Scheme == "https" {
			cfg.NginxHTTPSProbeAddr, err = requiredProbeAddress("GITEA_PAGES_NGINX_HTTPS_PROBE_ADDR")
			if err != nil {
				return cfg, err
			}
		}
		cfg.NginxTestCommand, err = envCommand("GITEA_PAGES_NGINX_TEST_COMMAND", true)
		if err != nil {
			return cfg, err
		}
		cfg.NginxReloadCommand, err = envCommand("GITEA_PAGES_NGINX_RELOAD_COMMAND", false)
		if err != nil {
			return cfg, err
		}
		cfg.NginxCommandTimeout, err = envDuration("GITEA_PAGES_NGINX_COMMAND_TIMEOUT", 30*time.Second)
		if err != nil {
			return cfg, err
		}
	}
	cfg.CertRoot = strings.TrimSpace(os.Getenv("GITEA_PAGES_CERT_ROOT"))
	if cfg.CertRoot != "" && !filepath.IsAbs(cfg.CertRoot) {
		return cfg, errors.New("GITEA_PAGES_CERT_ROOT must be an absolute path")
	}
	if cfg.CertRoot != "" && pathsOverlap(cfg.CertRoot, cfg.DataRoot) {
		return cfg, errors.New("GITEA_PAGES_CERT_ROOT and GITEA_PAGES_DATA_ROOT must be separate directories")
	}
	if mode == ModeNginx {
		if pathsOverlap(cfg.NginxRoot, cfg.DataRoot) {
			return cfg, errors.New("GITEA_PAGES_NGINX_ROOT and GITEA_PAGES_DATA_ROOT must be separate directories")
		}
		if cfg.CertRoot != "" && pathsOverlap(cfg.CertRoot, cfg.NginxRoot) {
			return cfg, errors.New("GITEA_PAGES_CERT_ROOT and GITEA_PAGES_NGINX_ROOT must be separate directories")
		}
		if cfg.NginxHTTP01Root != "" && (pathsOverlap(cfg.NginxHTTP01Root, cfg.DataRoot) || pathsOverlap(cfg.NginxHTTP01Root, cfg.NginxRoot) || cfg.CertRoot != "" && pathsOverlap(cfg.NginxHTTP01Root, cfg.CertRoot)) {
			return cfg, errors.New("GITEA_PAGES_NGINX_HTTP01_ROOT must be separate from other Pages directories")
		}
	}
	if cfg.CustomRoot != "" {
		for _, dir := range []struct {
			name string
			path string
		}{
			{"GITEA_PAGES_DATA_ROOT", cfg.DataRoot},
			{"GITEA_PAGES_NGINX_ROOT", cfg.NginxRoot},
			{"GITEA_PAGES_NGINX_HTTP01_ROOT", cfg.NginxHTTP01Root},
			{"GITEA_PAGES_CERT_ROOT", cfg.CertRoot},
		} {
			if dir.path != "" && pathsOverlap(cfg.CustomRoot, dir.path) {
				return cfg, fmt.Errorf("GITEA_PAGES_CUSTOM_ROOT overlaps %s", dir.name)
			}
		}
	}
	cfg.ACMEDirectoryURL = strings.TrimSpace(os.Getenv("GITEA_PAGES_ACME_DIRECTORY_URL"))
	cfg.ACMEEmail = strings.TrimSpace(os.Getenv("GITEA_PAGES_ACME_EMAIL"))
	if cfg.ACMEDirectoryURL != "" && cfg.ACMEEmail == "" {
		return cfg, errors.New("GITEA_PAGES_ACME_EMAIL is required when ACME is enabled")
	}
	cfg.ACMEEABKID = strings.TrimSpace(os.Getenv("GITEA_PAGES_ACME_EAB_KID"))
	key := strings.TrimSpace(os.Getenv("GITEA_PAGES_ACME_EAB_HMAC_KEY"))
	if (cfg.ACMEEABKID == "") != (key == "") {
		return cfg, errors.New("GITEA_PAGES_ACME_EAB_KID and GITEA_PAGES_ACME_EAB_HMAC_KEY must be set together")
	}
	if key != "" {
		cfg.ACMEEABKey, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(key, "="))
		if err != nil || len(cfg.ACMEEABKey) < 16 {
			return cfg, errors.New("GITEA_PAGES_ACME_EAB_HMAC_KEY must be a base64url HMAC key")
		}
	}
	if raw := strings.TrimSpace(os.Getenv("GITEA_PAGES_HTTP01_PROBE_ADDRESSES")); raw != "" {
		for _, v := range strings.Split(raw, ",") {
			v = strings.TrimSpace(v)
			if v == "" {
				return cfg, errors.New("GITEA_PAGES_HTTP01_PROBE_ADDRESSES contains an empty address")
			}
			address, err := parseProbeAddress("GITEA_PAGES_HTTP01_PROBE_ADDRESSES", v)
			if err != nil {
				return cfg, err
			}
			cfg.HTTP01ProbeAddresses = append(cfg.HTTP01ProbeAddresses, address)
		}
	}
	cfg.DNSResolver, err = optionalDNSResolver("GITEA_PAGES_DNS_RESOLVER")
	if err != nil {
		return cfg, err
	}
	cfg.ACMEDNSResolver, err = optionalDNSResolver("GITEA_PAGES_ACME_DNS_RESOLVER")
	if err != nil {
		return cfg, err
	}
	if cfg.ACMEDNSResolver == "" {
		cfg.ACMEDNSResolver = cfg.DNSResolver
	}
	cfg.DNSRefreshInterval, err = envDuration("GITEA_PAGES_DNS_REFRESH_INTERVAL", defaultDNSRefreshInterval)
	if err != nil {
		return cfg, err
	}
	if cfg.DNSRefreshInterval < time.Minute || cfg.DNSRefreshInterval > 24*time.Hour {
		return cfg, errors.New("GITEA_PAGES_DNS_REFRESH_INTERVAL must be between 1m and 24h")
	}
	presentRaw, cleanupRaw := strings.TrimSpace(os.Getenv("GITEA_PAGES_DNS_PRESENT_COMMAND")), strings.TrimSpace(os.Getenv("GITEA_PAGES_DNS_CLEANUP_COMMAND"))
	cfg.DNSProvider = strings.TrimSpace(os.Getenv("GITEA_PAGES_DNS_PROVIDER"))
	if cfg.DNSProvider == "" && presentRaw != "" {
		cfg.DNSProvider = "command"
	}
	if (presentRaw == "") != (cleanupRaw == "") {
		return cfg, errors.New("DNS present and cleanup commands must be configured together")
	}
	if presentRaw != "" {
		cfg.DNSPresentCommand, err = envCommand("GITEA_PAGES_DNS_PRESENT_COMMAND", false)
		if err != nil {
			return cfg, err
		}
		cfg.DNSCleanupCommand, err = envCommand("GITEA_PAGES_DNS_CLEANUP_COMMAND", false)
		if err != nil {
			return cfg, err
		}
		for _, configured := range []struct {
			name string
			args []string
		}{
			{"GITEA_PAGES_DNS_PRESENT_COMMAND", cfg.DNSPresentCommand},
			{"GITEA_PAGES_DNS_CLEANUP_COMMAND", cfg.DNSCleanupCommand},
		} {
			nameCount, valueCount, candidateCount := 0, 0, 0
			for _, arg := range configured.args {
				nameCount += strings.Count(arg, "{name}")
				valueCount += strings.Count(arg, "{value}")
				candidateCount += strings.Count(arg, "{candidate}")
			}
			if nameCount != 1 || valueCount != 1 || candidateCount != 0 {
				return cfg, fmt.Errorf("%s must contain {name} and {value} exactly once", configured.name)
			}
		}
	}
	return cfg, nil
}

func optionalDNSResolver(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
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

func requiredProbeAddress(name string) (string, error) {
	return parseProbeAddress(name, strings.TrimSpace(os.Getenv(name)))
}

func parseProbeAddress(name, value string) (string, error) {
	address, err := netip.ParseAddrPort(value)
	if err != nil {
		return "", fmt.Errorf("%s must be an IP:port address", name)
	}
	if address.Port() == 0 {
		return "", fmt.Errorf("%s must include a nonzero TCP port", name)
	}
	return address.String(), nil
}

func envCommand(name string, requireCandidatePlaceholder bool) ([]string, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	var cmd []string
	if err := json.Unmarshal([]byte(raw), &cmd); err != nil || len(cmd) == 0 {
		return nil, fmt.Errorf("%s must be a non-empty JSON string array", name)
	}
	count := 0
	for _, arg := range cmd {
		count += strings.Count(arg, "{candidate}")
	}
	if (requireCandidatePlaceholder && count != 1) || (!requireCandidatePlaceholder && count > 1) {
		return nil, fmt.Errorf("%s has invalid {candidate} placeholder count", name)
	}
	return cmd, nil
}

func requiredURL(name string) (*url.URL, error) {
	raw := strings.TrimSpace(os.Getenv(name))
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

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return d, nil
}

func envInt(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return n, nil
}
