// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pages

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func probeDeploymentURL(ctx context.Context, scheme, address string, target url.URL, repoID int64, digest string) error {
	base := target.Path
	if base == "" {
		base = "/"
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	path := base + ".gitea-pages-probe"
	if !strings.HasPrefix(path, "/") {
		return errors.New("invalid probe path")
	}
	return probeHTTP(ctx, scheme, address, target.Hostname(), path, http.StatusOK, strconv.FormatInt(repoID, 10)+" "+digest+"\n")
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
		return errors.New("entry probe address or Host is empty")
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
