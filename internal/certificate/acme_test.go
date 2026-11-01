// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package certificate

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"gitea.dev/page/internal/storage"

	legoacme "github.com/go-acme/lego/v5/acme"
	"github.com/go-acme/lego/v5/challenge/dns01"
	"github.com/miekg/dns"
)

type fakeDNS struct{ records []string }

func (d *fakeDNS) Present(_ context.Context, domain, _, _ string) error {
	d.records = append(d.records, domain)
	return nil
}

func (*fakeDNS) CleanUp(context.Context, string, string, string) error { return nil }

func TestACMERateLimitRetrySurvivesRestart(t *testing.T) {
	root := t.TempDir()
	directory := "http://127.0.0.1:1/directory"
	store, err := storage.Open(root, testStateQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := newACMEManager(store.State(), directory, "pages@example.test", "")
	if err != nil {
		t.Fatal(err)
	}
	key := acmeRetryKey(directory, "dns01", "example.test")
	limited := &legoacme.RateLimitedError{ProblemDetails: &legoacme.ProblemDetails{Detail: "limited"}, RetryAfter: 2 * time.Hour}
	if err := manager.recordFailure(t.Context(), key, acmeRetryState{}, limited); err != limited {
		t.Fatalf("record failure = %v", err)
	}
	store.Close()

	store, err = storage.Open(root, testStateQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err = newACMEManager(store.State(), directory, "pages@example.test", "")
	if err != nil {
		t.Fatal(err)
	}
	var state acmeRetryState
	if ok, err := store.State().Get(context.Background(), key, &state); err != nil || !ok || time.Until(time.Unix(0, state.NotBeforeUnixNano)) < 2*time.Hour-time.Minute {
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

func TestACMEAccountFollowsDirectory(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(root, testStateQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	firstDirectory := "https://ca-one.example/directory"
	manager, err := newACMEManager(store.State(), firstDirectory, "pages@example.test", "")
	if err != nil {
		t.Fatal(err)
	}
	accountPEM := manager.accountPEM
	if err := store.State().Put(t.Context(), "/gitea-pages/acme/account", acmeAccountState{
		KeyPEM: accountPEM, URI: "https://ca-one.example/account/1", DirectoryURL: firstDirectory,
	}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store, err = storage.Open(root, testStateQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secondDirectory := "https://ca-two.example/directory"
	manager, err = newACMEManager(store.State(), secondDirectory, "pages@example.test", "")
	if err != nil {
		t.Fatal(err)
	}
	if manager.accountPEM != accountPEM || manager.accountURI != "" {
		t.Fatalf("account after directory change: uri=%q key_reused=%v", manager.accountURI, manager.accountPEM == accountPEM)
	}
	var state acmeAccountState
	if ok, err := store.State().Get(t.Context(), "/gitea-pages/acme/account", &state); err != nil || !ok {
		t.Fatalf("load account state: found=%v err=%v", ok, err)
	}
	if state.DirectoryURL != secondDirectory || state.URI != "" || state.KeyPEM != accountPEM {
		t.Fatalf("stored account after directory change: %#v", state)
	}
	if acmeRetryKey(firstDirectory, "dns01", "example.test") == acmeRetryKey(secondDirectory, "dns01", "example.test") {
		t.Fatal("ACME retry state was shared between directories")
	}
}

func TestACMEManagerUsesConfiguredDNSResolver(t *testing.T) {
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &dns.Server{PacketConn: packetConn, Handler: dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(request)
		for _, question := range request.Question {
			if question.Qtype == dns.TypeSOA {
				response.Answer = append(response.Answer, &dns.SOA{
					Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 60},
					Ns:  "ns." + question.Name, Mbox: "hostmaster." + question.Name,
					Serial: 1, Refresh: 60, Retry: 60, Expire: 300, Minttl: 60,
				})
			}
		}
		_ = writer.WriteMsg(response)
	})}
	started := make(chan struct{})
	server.NotifyStartedFunc = func() { close(started) }
	errCh := make(chan error, 1)
	go func() { errCh <- server.ActivateAndServe() }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("DNS server did not start")
	}
	t.Cleanup(func() {
		if err := server.Shutdown(); err != nil {
			t.Errorf("shut down DNS server: %v", err)
		}
		if err := <-errCh; err != nil {
			t.Errorf("serve DNS: %v", err)
		}
	})

	store, err := storage.Open(t.TempDir(), testStateQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	defaultClient := dns01.DefaultClient()
	t.Cleanup(func() { dns01.SetDefaultClient(defaultClient) })
	if _, err := newACMEManager(store.State(), "http://127.0.0.1:1/directory", "pages@example.test", packetConn.LocalAddr().String()); err != nil {
		t.Fatal(err)
	}
	if _, err := dns01.DefaultClient().FindZoneByFqdn(t.Context(), "example.test."); err != nil {
		t.Fatalf("configured ACME DNS resolver was not used: %v", err)
	}
}
