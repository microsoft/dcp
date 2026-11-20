/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package concurrency

import (
	"context"
	"sync"
	"sync/atomic"
)

type leaseEntry struct {
	lock *ContextAwareLock
	refs int
}

// Lease represents an exclusive per-key lease acquired from a LeaseMap.
type Lease[Key comparable] struct {
	released atomic.Bool
	owner    *LeaseMap[Key]
	key      Key
	entry    *leaseEntry
}

// Release unlocks the leased key and releases its reference in the owning LeaseMap.
// Calling Release on a nil Lease or calling it more than once is a no-op.
func (l *Lease[Key]) Release() {
	if l == nil {
		return
	}
	if l.released.CompareAndSwap(false, true) {
		owner := l.owner
		entry := l.entry
		l.owner = nil
		l.entry = nil
		if owner != nil && entry != nil {
			owner.releaseLease(l.key, entry)
		}
	}
}

// LeaseMap coordinates mutually exclusive per-key leases across goroutines and
// evicts per-key state automatically once all holders and waiters finish.
// Construct instances via NewLeaseMap.
type LeaseMap[Key comparable] struct {
	lock    sync.Mutex
	entries map[Key]*leaseEntry
}

// NewLeaseMap creates a ready-to-use LeaseMap.
func NewLeaseMap[Key comparable]() *LeaseMap[Key] {
	return &LeaseMap[Key]{
		entries: make(map[Key]*leaseEntry),
	}
}

// Acquire waits until it holds the exclusive lease for key or ctx is done.
// On success, the caller must call Release on the returned Lease when finished.
func (lm *LeaseMap[Key]) Acquire(ctx context.Context, key Key) (*Lease[Key], error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	entry := lm.reserveEntry(key)
	if lockErr := entry.lock.Lock(ctx); lockErr != nil {
		lm.releaseRef(key, entry)
		return nil, lockErr
	}

	return &Lease[Key]{
		owner: lm,
		key:   key,
		entry: entry,
	}, nil
}

// TryAcquire attempts to acquire the exclusive lease for key without blocking.
// It returns the acquired Lease and true on success, or nil and false if the key is currently held.
func (lm *LeaseMap[Key]) TryAcquire(key Key) (*Lease[Key], bool) {
	entry := lm.reserveEntry(key)
	if !entry.lock.TryLock() {
		lm.releaseRef(key, entry)
		return nil, false
	}

	return &Lease[Key]{
		owner: lm,
		key:   key,
		entry: entry,
	}, true
}

func (lm *LeaseMap[Key]) reserveEntry(key Key) *leaseEntry {
	lm.lock.Lock()
	defer lm.lock.Unlock()

	entry, exists := lm.entries[key]
	if !exists {
		entry = &leaseEntry{
			lock: NewContextAwareLock(),
		}
		lm.entries[key] = entry
	}
	entry.refs++
	return entry
}

func (lm *LeaseMap[Key]) releaseRef(key Key, entry *leaseEntry) {
	lm.lock.Lock()
	defer lm.lock.Unlock()

	entry.refs--
	if entry.refs == 0 {
		delete(lm.entries, key)
	}
}

func (lm *LeaseMap[Key]) releaseLease(key Key, entry *leaseEntry) {
	lm.lock.Lock()
	defer lm.lock.Unlock()

	entry.lock.Unlock()
	entry.refs--
	if entry.refs == 0 {
		delete(lm.entries, key)
	}
}
