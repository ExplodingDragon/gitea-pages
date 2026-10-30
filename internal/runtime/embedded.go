// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/server/v3/embed"
)

const embeddedKVPageSize = 1000

// embeddedKV calls a single embedded etcd member through its server API.
type embeddedKV struct {
	etcd *embed.Etcd
	stop chan struct{}
	wg   sync.WaitGroup
}

// startEmbeddedKV starts local durable state without client or peer listeners.
func startEmbeddedKV(root string, quota int64) (*embeddedKV, error) {
	dir := filepath.Join(root, "etcd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create embedded etcd directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("protect embedded etcd directory: %w", err)
	}
	cfg := embed.NewConfig()
	cfg.Name = "gitea-pages"
	cfg.Dir = dir
	cfg.Logger = "zap"
	cfg.LogLevel = "error"
	cfg.LogOutputs = []string{"stderr"}
	peer, _ := url.Parse("http://127.0.0.1:0")
	cfg.ListenPeerUrls = nil
	cfg.AdvertisePeerUrls = []url.URL{*peer}
	cfg.ListenClientUrls = nil
	cfg.AdvertiseClientUrls = nil
	cfg.ListenClientHttpUrls = nil
	cfg.InitialCluster = "gitea-pages=http://127.0.0.1:0"
	cfg.AutoCompactionMode = "periodic"
	cfg.AutoCompactionRetention = "1h"
	cfg.BootstrapDefragThresholdMegabytes = 64
	if quota > 0 {
		cfg.QuotaBackendBytes = quota
	}
	e, err := embed.StartEtcd(cfg)
	if err != nil {
		return nil, fmt.Errorf("start embedded etcd: %w", err)
	}
	select {
	case <-e.Server.ReadyNotify():
	case <-time.After(30 * time.Second):
		e.Close()
		return nil, errors.New("embedded etcd ready timeout")
	}
	k := &embeddedKV{etcd: e, stop: make(chan struct{})}
	k.wg.Add(1)
	go k.maintain()
	return k, nil
}

func (k *embeddedKV) close() {
	if k != nil && k.etcd != nil {
		close(k.stop)
		k.wg.Wait()
		k.etcd.Close()
	}
}

func (k *embeddedKV) errors() <-chan error { return k.etcd.Err() }

func (k *embeddedKV) maintain() {
	defer k.wg.Done()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-k.stop:
			return
		case <-ticker.C:
			backend := k.etcd.Server.Backend()
			if backend.Size()-backend.SizeInUse() >= 64*1024*1024 {
				if err := k.etcd.Server.Defragment(); err != nil {
					slog.Error("embedded etcd defragmentation failed", "error", err)
				}
			}
		}
	}
}

func (k *embeddedKV) get(ctx context.Context, key string, out any) (bool, error) {
	resp, err := k.etcd.Server.Range(ctx, &etcdserverpb.RangeRequest{Key: []byte(key)})
	if err != nil {
		return false, err
	}
	if len(resp.Kvs) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(resp.Kvs[0].Value, out); err != nil {
		return false, err
	}
	return true, nil
}

func (k *embeddedKV) put(ctx context.Context, key string, in any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	_, err = k.etcd.Server.Put(ctx, &etcdserverpb.PutRequest{Key: []byte(key), Value: b})
	return err
}

func (k *embeddedKV) delete(ctx context.Context, key string) error {
	_, err := k.etcd.Server.DeleteRange(ctx, &etcdserverpb.DeleteRangeRequest{Key: []byte(key)})
	return err
}

func (k *embeddedKV) listPrefix(ctx context.Context, prefix string, out func(key string, value []byte) error) error {
	start, end := []byte(prefix), prefixRangeEnd(prefix)
	for {
		resp, err := k.etcd.Server.Range(ctx, &etcdserverpb.RangeRequest{
			Key: start, RangeEnd: end, Limit: embeddedKVPageSize,
			SortOrder: etcdserverpb.RangeRequest_ASCEND, SortTarget: etcdserverpb.RangeRequest_KEY,
		})
		if err != nil {
			return err
		}
		for _, kv := range resp.Kvs {
			if err := out(string(kv.Key), kv.Value); err != nil {
				return err
			}
		}
		if !resp.More || len(resp.Kvs) == 0 {
			return nil
		}
		start = append(append([]byte(nil), resp.Kvs[len(resp.Kvs)-1].Key...), 0)
	}
}

func (k *embeddedKV) deletePrefix(ctx context.Context, prefix string) error {
	_, err := k.etcd.Server.DeleteRange(ctx, &etcdserverpb.DeleteRangeRequest{
		Key: []byte(prefix), RangeEnd: prefixRangeEnd(prefix),
	})
	return err
}

func prefixRangeEnd(prefix string) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return []byte{0}
}
