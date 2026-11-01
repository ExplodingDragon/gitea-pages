// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pages

import (
	"net"
	"testing"
	"time"

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
