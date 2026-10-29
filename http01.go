package page

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// HTTP01Precheck validates every resolved public address before an ACME order
// is created. The request is pinned to addr while preserving the real Host.
func HTTP01Precheck(ctx context.Context, host, token, keyAuthorization string, addresses []string) error {
	if host == "" || !normalizeChallengeToken(token) || keyAuthorization == "" || len(addresses) == 0 {
		return fmt.Errorf("incomplete HTTP-01 precheck")
	}
	path := "/.well-known/acme-challenge/" + token
	for _, addr := range addresses {
		transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
		}}
		client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
		if err != nil {
			return err
		}
		req.Host = host
		resp, err := client.Do(req)
		if err != nil {
			transport.CloseIdleConnections()
			return fmt.Errorf("HTTP-01 address %s: %w", addr, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
		closeErr := resp.Body.Close()
		transport.CloseIdleConnections()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if resp.StatusCode != http.StatusOK || string(body) != keyAuthorization {
			return fmt.Errorf("HTTP-01 address %s returned %d or incorrect body", addr, resp.StatusCode)
		}
	}
	return nil
}

func normalizeChallengeToken(token string) bool {
	if token == "" || len(token) > 256 {
		return false
	}
	for _, c := range token {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_~.", c) {
			continue
		}
		return false
	}
	return true
}
