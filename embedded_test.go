// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package page

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedKVPersistsWithoutNetworkListeners(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etcd"), 0o755); err != nil {
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
	if info.Mode().Perm() != 0o700 {
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

func TestEmbeddedKVPagesAndDeletesLargePrefix(t *testing.T) {
	kv, err := StartEmbeddedKV(t.TempDir(), 64*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()
	ctx := context.Background()
	for i := 0; i < embeddedKVPageSize+5; i++ {
		key := fmt.Sprintf("/large/%04d", i)
		if err := kv.Put(ctx, key, i); err != nil {
			t.Fatal(err)
		}
	}
	keys := make([]string, 0, embeddedKVPageSize+5)
	if err := kv.ListPrefix(ctx, "/large/", func(key string, _ []byte) error {
		keys = append(keys, key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(keys) != embeddedKVPageSize+5 || keys[0] != "/large/0000" || keys[len(keys)-1] != "/large/1004" {
		t.Fatalf("listed keys = %d, first=%q, last=%q", len(keys), keys[0], keys[len(keys)-1])
	}
	if err := kv.DeletePrefix(ctx, "/large/"); err != nil {
		t.Fatal(err)
	}
	if err := kv.ListPrefix(ctx, "/large/", func(string, []byte) error {
		t.Fatal("deleted prefix still contains a key")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
