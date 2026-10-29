package page

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// probeEntry checks the actual local HTTP or TLS listener while retaining the
// public Host and SNI. A successful config reload alone cannot prove that the
// intended repository is being served at its public address.
func (s *Syncer) probeEntry(ctx context.Context, host, base string, repoID int64, digest string) error {
	address, err := s.probeAddress()
	if err != nil {
		return err
	}
	path := base + ".gitea-pages-probe"
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("invalid probe path")
	}
	return probeHTTP(ctx, s.cfg.RootURL.Scheme, address, host, path, http.StatusOK, strconv.FormatInt(repoID, 10)+" "+digest+"\n")
}

func (s *Syncer) probePlatform(ctx context.Context) error {
	// The platform probe deliberately uses HTTP even for HTTPS instances. It
	// confirms the entry configuration independently of certificate issuance.
	var address string
	if s.cfg.Mode == ModeNginx {
		address = s.cfg.NginxHTTPProbeAddr
	} else {
		var err error
		address, err = localListenAddress(s.cfg.HTTPListen)
		if err != nil {
			return err
		}
	}
	return probeHTTP(ctx, "http", address, s.cfg.RootURL.Hostname(), "/@gitea-pages/ready", http.StatusNoContent, "")
}

func (s *Syncer) probeAddress() (string, error) {
	if s.cfg.Mode == ModeNginx {
		if s.cfg.RootURL.Scheme == "https" {
			return s.cfg.NginxHTTPSProbeAddr, nil
		}
		return s.cfg.NginxHTTPProbeAddr, nil
	}
	if s.cfg.RootURL.Scheme == "https" {
		return localListenAddress(s.cfg.HTTPSListen)
	}
	return localListenAddress(s.cfg.HTTPListen)
}

func localListenAddress(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return "", fmt.Errorf("invalid local listen address %q", listen)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return net.JoinHostPort(host, port), nil
}

func probeHTTP(ctx context.Context, scheme, address, host, path string, status int, body string) error {
	if address == "" || host == "" {
		return fmt.Errorf("entry probe address or Host is empty")
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
		},
	}
	if scheme == "https" {
		// The certificate is checked explicitly against the requested Host after
		// the local handshake; this also works with operator private CAs.
		transport.TLSClientConfig = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: true} //nolint:gosec
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	u := &url.URL{Scheme: scheme, Host: host, Path: path}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("entry probe %s: %w", host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if scheme == "https" {
		if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
			return fmt.Errorf("entry probe %s: missing TLS certificate", host)
		}
		leaf := resp.TLS.PeerCertificates[0]
		if err := leaf.VerifyHostname(host); err != nil {
			return fmt.Errorf("entry probe %s: %w", host, err)
		}
		if now := time.Now(); now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
			return fmt.Errorf("entry probe %s: expired TLS certificate", host)
		}
	}
	actual, err := io.ReadAll(io.LimitReader(resp.Body, int64(len(body)+1)))
	if err != nil {
		return err
	}
	if resp.StatusCode != status || string(actual) != body {
		return fmt.Errorf("entry probe %s: status %d or content mismatch", host, resp.StatusCode)
	}
	return nil
}
