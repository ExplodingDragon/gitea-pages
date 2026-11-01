// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pages

import (
	"context"
	"sync"
)

type wakeSignal chan struct{}

func newWakeSignal() wakeSignal {
	return make(chan struct{}, 1)
}

func (s wakeSignal) notify() {
	select {
	case s <- struct{}{}:
	default:
	}
}

type deploymentState uint8

const (
	deploymentQueued deploymentState = iota + 1
	deploymentRunning
	deploymentRerun
)

// deploymentQueue coalesces repository updates while keeping the ready queue
// bounded. The durable site state remains the source of truth.
type deploymentQueue struct {
	ready chan int64

	mu     sync.Mutex
	states map[int64]deploymentState
}

func newDeploymentQueue(capacity int) *deploymentQueue {
	return &deploymentQueue{
		ready:  make(chan int64, capacity),
		states: make(map[int64]deploymentState),
	}
}

// schedule records a new deployment condition. It returns true when the ready
// queue was full and a durable-state scan must recover the repository later.
func (q *deploymentQueue) schedule(repoID int64) (scanRequired bool) {
	q.mu.Lock()
	switch q.states[repoID] {
	case deploymentQueued, deploymentRerun:
		q.mu.Unlock()
		return false
	case deploymentRunning:
		q.states[repoID] = deploymentRerun
		q.mu.Unlock()
		return false
	default:
		q.states[repoID] = deploymentQueued
	}
	q.mu.Unlock()

	select {
	case q.ready <- repoID:
		return false
	default:
		q.removeQueued(repoID)
		return true
	}
}

// next returns one repository and marks it running before the caller reads its
// current durable state.
func (q *deploymentQueue) next(ctx context.Context) (int64, bool) {
	select {
	case <-ctx.Done():
		return 0, false
	case repoID := <-q.ready:
		q.mu.Lock()
		q.states[repoID] = deploymentRunning
		q.mu.Unlock()
		return repoID, true
	}
}

// finish completes a deployment. An update received while the repository was
// running is queued once. It returns true when a scan is needed to recover a
// retry that could not enter the bounded queue.
func (q *deploymentQueue) finish(repoID int64) (scanRequired bool) {
	q.mu.Lock()
	if q.states[repoID] != deploymentRerun {
		delete(q.states, repoID)
		q.mu.Unlock()
		return false
	}
	q.states[repoID] = deploymentQueued
	q.mu.Unlock()

	select {
	case q.ready <- repoID:
		return false
	default:
		q.removeQueued(repoID)
		return true
	}
}

// enqueueScanned adds a repository found by a durable-state scan. Existing
// queued or running work already covers the scan result and is left unchanged.
func (q *deploymentQueue) enqueueScanned(ctx context.Context, repoID int64) bool {
	q.mu.Lock()
	if _, exists := q.states[repoID]; exists {
		q.mu.Unlock()
		return true
	}
	q.states[repoID] = deploymentQueued
	q.mu.Unlock()

	select {
	case q.ready <- repoID:
		return true
	case <-ctx.Done():
		q.removeQueued(repoID)
		return false
	}
}

func (q *deploymentQueue) removeQueued(repoID int64) {
	q.mu.Lock()
	if q.states[repoID] == deploymentQueued {
		delete(q.states, repoID)
	}
	q.mu.Unlock()
}
