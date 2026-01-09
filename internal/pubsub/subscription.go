/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package pubsub

import (
	"sync"
	"sync/atomic"
)

type HandleT uint32

const (
	InvalidHandle HandleT = 0
)

var (
	nextHandle = InvalidHandle
)

type Subscription[NotificationT any] struct {
	Handle HandleT
	sink   chan<- NotificationT
	owner  *SubscriptionSet[NotificationT]
	lock   *sync.Mutex
}

func NewSubscription[NotificationT any](owner *SubscriptionSet[NotificationT], sink chan<- NotificationT) *Subscription[NotificationT] {
	return &Subscription[NotificationT]{
		Handle: HandleT(atomic.AddUint32((*uint32)(&nextHandle), 1)),
		sink:   sink,
		owner:  owner,
		lock:   &sync.Mutex{},
	}
}

func (es *Subscription[NotificationT]) Cancel() {
	es.lock.Lock()

	handle := es.Handle
	if handle != InvalidHandle {
		// Make sure onSubscriptionCancelled is called after the subscription lock is released.
		defer es.owner.onSubscriptionCancelled(handle)
	}
	defer es.lock.Unlock()

	if handle != InvalidHandle {
		es.Handle = InvalidHandle
		close(es.sink)
		es.sink = nil
	}
}

func (es *Subscription[NotificationT]) Notify(n NotificationT) {
	es.lock.Lock()
	defer es.lock.Unlock()

	if es.sink == nil {
		return
	}

	// The user of a subscription should make sure that the notification can always be delivered without excessive blocking.
	// The call to Notify() will no-op if the subscription has been canceled.
	es.sink <- n
}

func (es *Subscription[NotificationT]) Cancelled() bool {
	es.lock.Lock()
	defer es.lock.Unlock()
	return es.Handle == InvalidHandle
}
