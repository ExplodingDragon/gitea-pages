// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type challengeMap struct{ values map[string]string }

func (m *challengeMap) setChallenge(host, token, keyAuth string) {
	if m.values == nil {
		m.values = map[string]string{}
	}
	m.values[host+"/"+token] = keyAuth
}

func (m *challengeMap) clearChallenge(host, token string) { delete(m.values, host+"/"+token) }

func TestHTTP01PrecheckPinsAddressAndHost(t *testing.T) {
	const token, key = "token-1", "token-1.key"
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		if r.URL.Path != "/.well-known/acme-challenge/"+token {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(key))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := http01Precheck(t.Context(), "custom.example", token, key, []string{addr}); err != nil {
		t.Fatal(err)
	}
	if gotHost != "custom.example" {
		t.Fatalf("Host=%q", gotHost)
	}
}

func TestLegoHTTPProviderPublishesAndCleansChallenge(t *testing.T) {
	const host = "custom.example"
	const token = "lego-token"
	const keyAuth = "lego-key-auth"
	var challenges challengeMap
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value := challenges.values[host+"/"+token]
		if r.Host != host || r.URL.Path != "/.well-known/acme-challenge/"+token || value != keyAuth {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(value))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	p := httpChallengeProvider{server: &challenges, host: host, addresses: []string{addr}}
	if err := p.Present(t.Context(), host, token, keyAuth); err != nil {
		t.Fatal(err)
	}
	if err := p.CleanUp(t.Context(), host, token, keyAuth); err != nil {
		t.Fatal(err)
	}
	if len(challenges.values) != 0 {
		t.Fatalf("challenge remains after cleanup: %#v", challenges.values)
	}
}
