// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pages

import (
	"context"
	"testing"
	"time"
)

func TestDeploymentQueueCoalescesQueuedUpdates(t *testing.T) {
	queue := newDeploymentQueue(1)
	if queue.schedule(1) || queue.schedule(1) {
		t.Fatal("queued repository unexpectedly requested a scan")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if repoID, ok := queue.next(ctx); !ok || repoID != 1 {
		t.Fatalf("next() = %d, %v", repoID, ok)
	}
	if queue.finish(1) {
		t.Fatal("completed repository unexpectedly requested a scan")
	}
	select {
	case repoID := <-queue.ready:
		t.Fatalf("queued update produced another deployment for repository %d", repoID)
	default:
	}
}

func TestDeploymentQueueRerunsOnceAfterRunningUpdate(t *testing.T) {
	queue := newDeploymentQueue(1)
	queue.schedule(1)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if repoID, ok := queue.next(ctx); !ok || repoID != 1 {
		t.Fatalf("next() = %d, %v", repoID, ok)
	}
	if queue.schedule(1) || queue.schedule(1) {
		t.Fatal("running repository unexpectedly requested a scan")
	}
	if queue.finish(1) {
		t.Fatal("updated repository unexpectedly requested a scan")
	}
	if repoID, ok := queue.next(ctx); !ok || repoID != 1 {
		t.Fatalf("rerun next() = %d, %v", repoID, ok)
	}
	if queue.finish(1) {
		t.Fatal("rerun completion unexpectedly requested a scan")
	}
	select {
	case repoID := <-queue.ready:
		t.Fatalf("multiple updates produced a third deployment for repository %d", repoID)
	default:
	}
}

func TestDeploymentQueueFullRequestsScan(t *testing.T) {
	queue := newDeploymentQueue(1)
	if queue.schedule(1) {
		t.Fatal("first repository requested a scan")
	}
	if !queue.schedule(2) {
		t.Fatal("full queue did not request a scan")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if repoID, ok := queue.next(ctx); !ok || repoID != 1 {
		t.Fatalf("next() = %d, %v", repoID, ok)
	}
	if !queue.enqueueScanned(ctx, 2) {
		t.Fatal("scanned repository was not queued")
	}
	if queue.finish(1) {
		t.Fatal("completed repository unexpectedly requested a scan")
	}
	if repoID, ok := queue.next(ctx); !ok || repoID != 2 {
		t.Fatalf("scanned next() = %d, %v", repoID, ok)
	}
	queue.schedule(2)
	if !queue.enqueueScanned(ctx, 2) {
		t.Fatal("running repository was not covered by the scan")
	}
	if queue.finish(2) {
		t.Fatal("updated repository unexpectedly requested a scan")
	}
	if repoID, ok := queue.next(ctx); !ok || repoID != 2 {
		t.Fatalf("rerun next() = %d, %v", repoID, ok)
	}
	queue.finish(2)
}

func TestWakeSignalCoalescesNotifications(t *testing.T) {
	signal := newWakeSignal()
	signal.notify()
	signal.notify()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("notification was not delivered")
	}
	select {
	case <-signal:
		t.Fatal("notifications were not coalesced")
	default:
	}
}
