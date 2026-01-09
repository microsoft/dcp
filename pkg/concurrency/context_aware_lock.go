/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package concurrency

import "context"

// ContextAwareLock is a type of lock that can be locked only if the context passed to the Lock() operation is not done.
// It also exposes a TryLock() operation that allows the caller to attempt to acquire the lock without blocking.
type ContextAwareLock struct {
	ch chan struct{}
}

func NewContextAwareLock() *ContextAwareLock {
	return &ContextAwareLock{
		ch: make(chan struct{}, 1),
	}
}

func (sc *ContextAwareLock) Lock(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case sc.ch <- struct{}{}:
	}

	// guard against possible race condition where the context expires and mutex locks at the same time
	if ctx.Err() != nil {
		sc.Unlock()
		return ctx.Err()
	}

	return nil
}

func (sc *ContextAwareLock) TryLock() bool {
	select {
	case sc.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

func (sc *ContextAwareLock) Unlock() {
	// Non-blocking for caller
	select {
	case <-sc.ch:
	default:
	}
}
