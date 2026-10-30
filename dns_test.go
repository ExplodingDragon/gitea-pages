// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package page

import (
	"context"
	"net"
	"strings"
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
	cleaned bool
}

func (d *fakeDNS) Present(_ context.Context, n, v string) error {
	if d.records == nil {
		d.records = map[string]string{}
	}
	d.records[n] = v
	return nil
}

func (d *fakeDNS) Cleanup(_ context.Context, n, v string) error {
	if d.records[n] == v {
		delete(d.records, n)
	}
	d.cleaned = true
	return nil
}

func TestCommandDNSProviderUsesChallengeValues(t *testing.T) {
	p := CommandDNSProvider{PresentCommand: []string{"/bin/sh", "-c", "test '{name}' = '_acme-challenge.example' && test '{value}' = 'value'"}, CleanupCommand: []string{"/bin/sh", "-c", "test '{name}' = '_acme-challenge.example' && test '{value}' = 'value'"}}
	if err := p.Present(context.Background(), "_acme-challenge.example", "value"); err != nil {
		t.Fatal(err)
	}
	if err := p.Cleanup(context.Background(), "_acme-challenge.example", "value"); err != nil {
		t.Fatal(err)
	}
}

func TestNewDNS01ProviderRequiresConfiguredCommands(t *testing.T) {
	if _, err := NewDNS01Provider("command", nil, nil); err == nil {
		t.Fatal("empty command provider was accepted")
	}
	provider, err := NewDNS01Provider("command", []string{"/bin/true"}, []string{"/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Present(context.Background(), "_acme-challenge.example", "value"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDNS01Provider("unknown", []string{"/bin/true"}, []string{"/bin/true"}); err == nil {
		t.Fatal("unknown DNS provider was accepted")
	}
}

func TestLegoDNSProviderUsesChallengeRecord(t *testing.T) {
	d := &fakeDNS{}
	p := dnsChallengeProvider{provider: d}
	if err := p.Present(context.Background(), "example.com", "token", "key-auth"); err != nil {
		t.Fatal(err)
	}
	info := dns01.GetChallengeInfo(context.Background(), "example.com", "key-auth")
	name := strings.TrimSuffix(info.FQDN, ".")
	if d.records[name] != info.Value {
		t.Fatalf("DNS record = %q, want %q", d.records[name], info.Value)
	}
	if err := p.CleanUp(context.Background(), "example.com", "token", "key-auth"); err != nil {
		t.Fatal(err)
	}
	if len(d.records) != 0 {
		t.Fatalf("DNS records remain after lego cleanup: %#v", d.records)
	}
}

func TestVerifyTXTResultUsesConfiguredResolver(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		rcode  int
		value  string
		result TXTVerificationResult
	}{
		{name: "valid", rcode: dns.RcodeSuccess, value: "token", result: TXTValid},
		{name: "different value", rcode: dns.RcodeSuccess, value: "different", result: TXTInvalid},
		{name: "not found", rcode: dns.RcodeNameError, result: TXTInvalid},
		{name: "temporary failure", rcode: dns.RcodeServerFailure, result: TXTTemporaryError},
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
			result, _ := VerifyTXTResult(t.Context(), newDNSResolver(address), "example.test", "token")
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
	store, err := OpenStore(t.TempDir(), defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	t.Cleanup(func() { dns01.SetDefaultClient(dns01.NewClient(nil)) })
	if _, err := NewACMEManager(store, "http://127.0.0.1:1/directory", "pages@example.test", address); err != nil {
		t.Fatal(err)
	}
	if _, err := dns01.DefaultClient().FindZoneByFqdn(t.Context(), "example.test."); err != nil {
		t.Fatalf("configured ACME DNS resolver was not used: %v", err)
	}
}

func TestDNSChallengeCleanupAfterRestart(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	dns := &fakeDNS{}
	adapter := dnsChallengeProvider{provider: dns, store: store}
	if err := adapter.Present(context.Background(), "example.test", "token", "key-auth"); err != nil {
		t.Fatal(err)
	}
	if len(dns.records) != 1 {
		t.Fatalf("challenge not published: %#v", dns.records)
	}
	store.Close()
	store, err = OpenStore(root, defaultEtcdQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := NewACMEManager(store, "http://127.0.0.1:1/directory", "pages@example.test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.cleanupDNS(context.Background(), dns); err != nil {
		t.Fatal(err)
	}
	if len(dns.records) != 0 || !dns.cleaned {
		t.Fatalf("stale DNS challenge survived cleanup: %#v", dns.records)
	}
	remaining := 0
	if err := store.kv.ListPrefix(context.Background(), "/gitea-pages/acme/dns/", func(string, []byte) error {
		remaining++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("%d pending challenges remain in etcd", remaining)
	}
}
