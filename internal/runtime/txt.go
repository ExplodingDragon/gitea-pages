// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"
)

type txtVerificationResult string

const (
	txtValid          txtVerificationResult = "valid"
	txtInvalid        txtVerificationResult = "invalid"
	txtTemporaryError txtVerificationResult = "temporary_error"
)

// verifyTXTResult distinguishes an absent or wrong record from a temporary DNS
// outage. Callers retain the result for status reporting while applying the
// same grace period to every failure after a successful verification.
func verifyTXTResult(ctx context.Context, resolver *net.Resolver, host, expected string) (txtVerificationResult, error) {
	if host == "" || expected == "" {
		return txtInvalid, errors.New("empty TXT binding")
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	values, err := resolver.LookupTXT(lookupCtx, "_gitea-pages."+strings.TrimSuffix(host, "."))
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return txtInvalid, err
		}
		// Resolver implementations do not all set IsTemporary for SERVFAIL.
		// Any error other than an explicit not-found result is treated as
		// temporary so a transient outage cannot revoke ownership.
		return txtTemporaryError, err
	}
	for _, value := range values {
		if value == expected {
			return txtValid, nil
		}
	}
	return txtInvalid, errors.New("TXT value does not match")
}

func newDNSResolver(address string) *net.Resolver {
	if address == "" {
		return net.DefaultResolver
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		},
	}
}
