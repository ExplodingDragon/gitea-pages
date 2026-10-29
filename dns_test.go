package page

import (
	"context"
	"strings"
	"testing"

	"github.com/go-acme/lego/v5/challenge/dns01"
)

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
	p := CommandDNSProvider{PresentCommand: []string{"/bin/sh", "-c", "test '{name}' = '{name}' && test '{value}' = 'value'"}, CleanupCommand: []string{"/bin/sh", "-c", "test '{name}' = '{name}'"}}
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

func TestFakeDNSProviderLifecycle(t *testing.T) {
	d := &fakeDNS{}
	if err := d.Present(context.Background(), "_acme-challenge.example", "record"); err != nil {
		t.Fatal(err)
	}
	if d.records["_acme-challenge.example"] != "record" {
		t.Fatal("DNS record was not published")
	}
	if err := d.Cleanup(context.Background(), "_acme-challenge.example", "record"); err != nil {
		t.Fatal(err)
	}
	if !d.cleaned || len(d.records) != 0 {
		t.Fatal("DNS record was not cleaned")
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

func TestDNSChallengeCleanupAfterRestart(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
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
	store, err = OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := NewACMEManager(store, "http://127.0.0.1:1/directory", "pages@example.test")
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
