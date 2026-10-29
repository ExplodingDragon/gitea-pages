package page

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedKVPersistsWithoutNetworkListeners(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etcd"), 0755); err != nil {
		t.Fatal(err)
	}
	kv, err := StartEmbeddedKV(root, 64*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(kv.etcd.Clients) != 0 || len(kv.etcd.Peers) != 0 {
		t.Fatalf("embedded etcd opened network listeners: clients=%d peers=%d", len(kv.etcd.Clients), len(kv.etcd.Peers))
	}
	info, err := os.Stat(filepath.Join(root, "etcd"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("embedded etcd directory mode = %o, want 700", info.Mode().Perm())
	}
	if err := kv.Put(context.Background(), "/test", map[string]string{"value": "ok"}); err != nil {
		t.Fatal(err)
	}
	kv.Close()
	kv, err = StartEmbeddedKV(root, 64*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()
	var got map[string]string
	ok, err := kv.Get(context.Background(), "/test", &got)
	if err != nil || !ok || got["value"] != "ok" {
		t.Fatalf("persisted value = %#v, ok=%v, err=%v", got, ok, err)
	}
}
