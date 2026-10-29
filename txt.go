package page

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

type TXTVerificationResult string

const (
	TXTValid          TXTVerificationResult = "valid"
	TXTInvalid        TXTVerificationResult = "invalid"
	TXTTemporaryError TXTVerificationResult = "temporary_error"
)

// VerifyTXTResult distinguishes an explicit loss of the binding from a DNS
// outage. Callers use the distinction to keep a recently valid binding during
// a short resolver failure while immediately closing it after NXDOMAIN,
// NODATA, or a value mismatch.
func VerifyTXTResult(ctx context.Context, host, expected string) (TXTVerificationResult, error) {
	if host == "" || expected == "" {
		return TXTInvalid, fmt.Errorf("empty TXT binding")
	}
	values, err := net.DefaultResolver.LookupTXT(ctx, "_gitea-pages."+strings.TrimSuffix(host, "."))
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return TXTInvalid, err
		}
		// Resolver implementations do not all set IsTemporary for SERVFAIL.
		// Any error other than an explicit not-found result is treated as
		// temporary so a transient outage cannot revoke ownership.
		return TXTTemporaryError, err
	}
	for _, value := range values {
		if value == expected {
			return TXTValid, nil
		}
	}
	return TXTInvalid, fmt.Errorf("TXT value does not match")
}
