// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/go-acme/lego/v5/challenge/dns01"
	"github.com/miekg/dns"
)

func startTestDNSServer(t *testing.T, handler dns.Handler) string {
	t.Helper()
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &dns.Server{PacketConn: packetConn, Handler: handler}
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
	return packetConn.LocalAddr().String()
}

type fakeDNS struct {
	records map[string]string
}

func (d *fakeDNS) Present(ctx context.Context, domain, _ string, keyAuth string) error {
	if d.records == nil {
		d.records = map[string]string{}
	}
	d.records[domain] = dns01.GetChallengeInfo(ctx, domain, keyAuth).Value
	return nil
}

func (d *fakeDNS) CleanUp(_ context.Context, domain, _, _ string) error {
	delete(d.records, domain)
	return nil
}

func TestVerifyTXTResultUsesConfiguredResolver(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		rcode  int
		value  string
		result txtVerificationResult
	}{
		{name: "valid", rcode: dns.RcodeSuccess, value: "token", result: txtValid},
		{name: "different value", rcode: dns.RcodeSuccess, value: "different", result: txtInvalid},
		{name: "not found", rcode: dns.RcodeNameError, result: txtInvalid},
		{name: "temporary failure", rcode: dns.RcodeServerFailure, result: txtTemporaryError},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			address := startTestDNSServer(t, dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
				response := new(dns.Msg)
				response.SetReply(request)
				response.Rcode = testCase.rcode
				if testCase.value != "" {
					for _, question := range request.Question {
						if question.Qtype == dns.TypeTXT {
							response.Answer = append(response.Answer, &dns.TXT{
								Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60},
								Txt: []string{testCase.value},
							})
						}
					}
				}
				_ = writer.WriteMsg(response)
			}))
			result, _ := verifyTXTResult(t.Context(), newDNSResolver(address), "example.test", "token")
			if result != testCase.result {
				t.Fatalf("TXT result = %q, want %q", result, testCase.result)
			}
		})
	}
}

func TestACMEManagerUsesConfiguredDNSResolver(t *testing.T) {
	address := startTestDNSServer(t, dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
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
	}))
	store, err := openStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	t.Cleanup(func() { dns01.SetDefaultClient(dns01.NewClient(nil)) })
	if _, err := newACMEManager(store, "http://127.0.0.1:1/directory", "pages@example.test", address); err != nil {
		t.Fatal(err)
	}
	if _, err := dns01.DefaultClient().FindZoneByFqdn(t.Context(), "example.test."); err != nil {
		t.Fatalf("configured ACME DNS resolver was not used: %v", err)
	}
}
