// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	legoacme "github.com/go-acme/lego/v5/acme"
)

func TestACMERateLimitRetrySurvivesRestart(t *testing.T) {
	root := t.TempDir()
	store, err := openStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := newACMEManager(store, "http://127.0.0.1:1/directory", "pages@example.test", "")
	if err != nil {
		t.Fatal(err)
	}
	key := acmeRetryKey("dns01", "example.test")
	limited := &legoacme.RateLimitedError{ProblemDetails: &legoacme.ProblemDetails{Detail: "limited"}, RetryAfter: 2 * time.Hour}
	if err := manager.recordFailure(key, acmeRetryState{}, limited); err != limited {
		t.Fatalf("record failure = %v", err)
	}
	store.close()

	store, err = openStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	manager, err = newACMEManager(store, "http://127.0.0.1:1/directory", "pages@example.test", "")
	if err != nil {
		t.Fatal(err)
	}
	var state acmeRetryState
	if ok, err := store.kv.get(context.Background(), key, &state); err != nil || !ok || time.Until(time.Unix(0, state.NotBeforeUnixNano)) < 2*time.Hour-time.Minute {
		t.Fatalf("persisted CA retry = %#v, found=%v, err=%v", state, ok, err)
	}
	provider := &fakeDNS{}
	if _, err := manager.issueDNS01(context.Background(), []string{"example.test"}, provider); err == nil || !strings.Contains(err.Error(), "scheduled") {
		t.Fatalf("issuance bypassed persisted retry deadline: %v", err)
	}
	if len(provider.records) != 0 {
		t.Fatal("DNS provider ran during ACME backoff")
	}
}
