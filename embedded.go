package page

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/server/v3/embed"
)

// EmbeddedKV starts a single etcd member and calls its server API directly.
// No client or peer socket is exposed; the Pages process is the only caller.
type EmbeddedKV struct {
	etcd *embed.Etcd
	stop chan struct{}
	wg   sync.WaitGroup
}

func StartEmbeddedKV(root string, quota int64) (*EmbeddedKV, error) {
	dir := filepath.Join(root, "etcd")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create embedded etcd directory: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
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
		return nil, fmt.Errorf("embedded etcd ready timeout")
	}
	k := &EmbeddedKV{etcd: e, stop: make(chan struct{})}
	k.wg.Add(1)
	go k.maintain()
	return k, nil
}

func (k *EmbeddedKV) Close() {
	if k != nil && k.etcd != nil {
		close(k.stop)
		k.wg.Wait()
		k.etcd.Close()
	}
}

func (k *EmbeddedKV) Errors() <-chan error { return k.etcd.Err() }

func (k *EmbeddedKV) maintain() {
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
					fmt.Fprintf(os.Stderr, "pages embedded etcd defragmentation: %v\n", err)
				}
			}
		}
	}
}

func (k *EmbeddedKV) Get(ctx context.Context, key string, out any) (bool, error) {
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

func (k *EmbeddedKV) Put(ctx context.Context, key string, in any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	_, err = k.etcd.Server.Put(ctx, &etcdserverpb.PutRequest{Key: []byte(key), Value: b})
	return err
}

func (k *EmbeddedKV) Delete(ctx context.Context, key string) error {
	_, err := k.etcd.Server.DeleteRange(ctx, &etcdserverpb.DeleteRangeRequest{Key: []byte(key)})
	return err
}

func (k *EmbeddedKV) ListPrefix(ctx context.Context, prefix string, out func(key string, value []byte) error) error {
	end := append([]byte(nil), []byte(prefix)...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xff {
			end[i]++
			end = end[:i+1]
			break
		}
	}
	resp, err := k.etcd.Server.Range(ctx, &etcdserverpb.RangeRequest{Key: []byte(prefix), RangeEnd: end})
	if err != nil {
		return err
	}
	for _, kv := range resp.Kvs {
		if err := out(string(kv.Key), kv.Value); err != nil {
			return err
		}
	}
	return nil
}
