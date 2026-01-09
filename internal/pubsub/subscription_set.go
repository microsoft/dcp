/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package pubsub

import (
	"context"
	"sync"

	"github.com/microsoft/dcp/pkg/maps"
)

type NotifierFunc[NotificationT any] func(ctx context.Context, ss *SubscriptionSet[NotificationT])

// The subscription set helps manage a set of subscriptions that share the same source of notifications.
type SubscriptionSet[NotificationT any] struct {
	// The function that will be called (in a separate goroutine a.k.a. notifier goroutine)
	// when the first subscription is added. The purpose of the function is to monitor the source of notifications
	// and call Notify() on the subscription set when a new notification is available.
	// The passed-in context will be canceled when the last subscription is cancelled.
	notifierFunc NotifierFunc[NotificationT]

	// The set of subscriptions.
	subscriptions map[HandleT]*Subscription[NotificationT]

	// The cancel function for the context that is passed to the notifierFunc.
	notifierCtxCancel context.CancelFunc

	// The parent context that is used to create the notifier context.
	// Typically this is either context.Background() or a context that is canceled
	// when the owner of the subscription set is disposed.
	notifierParentCtx context.Context

	// The mutex that makes the subscription set goroutine-safe.
	mutex *sync.Mutex
}

func NewSubscriptionSet[NotificationT any](notifierFunc NotifierFunc[NotificationT], parentCtx context.Context) *SubscriptionSet[NotificationT] {
	ss := SubscriptionSet[NotificationT]{
		notifierFunc:      notifierFunc,
		subscriptions:     make(map[HandleT]*Subscription[NotificationT]),
		notifierCtxCancel: nil,
		notifierParentCtx: parentCtx,
		mutex:             &sync.Mutex{},
	}

	if ss.notifierParentCtx == nil {
		ss.notifierParentCtx = context.Background()
	}

	return &ss
}

func (ss *SubscriptionSet[NotificationT]) Subscribe(sink chan<- NotificationT) *Subscription[NotificationT] {
	sub := NewSubscription(ss, sink)

	ss.mutex.Lock()
	defer ss.mutex.Unlock()

	ss.subscriptions[sub.Handle] = sub

	if len(ss.subscriptions) == 1 && ss.notifierFunc != nil {
		notifierCtx, cancelFunc := context.WithCancel(ss.notifierParentCtx)
		ss.notifierCtxCancel = cancelFunc
		go ss.notifierFunc(notifierCtx, ss)
	}

	return sub
}

func (ss *SubscriptionSet[NotificationT]) Notify(n NotificationT) {
	ss.mutex.Lock()
	currentSubs := maps.Values(ss.subscriptions)
	ss.mutex.Unlock()

	for _, sub := range currentSubs {
		sub.Notify(n)
	}
}

func (ss *SubscriptionSet[NotificationT]) CancelAll() {
	ss.mutex.Lock()
	if ss.notifierFunc != nil && ss.notifierCtxCancel != nil {
		ss.notifierCtxCancel()
		ss.notifierCtxCancel = nil
	}
	currentSubs := maps.Values(ss.subscriptions)
	clear(ss.subscriptions)
	ss.mutex.Unlock()

	for _, sub := range currentSubs {
		sub.Cancel()
	}
}

func (ss *SubscriptionSet[NotificationT]) onSubscriptionCancelled(handle HandleT) {
	ss.mutex.Lock()
	defer ss.mutex.Unlock()

	before := len(ss.subscriptions)
	if len(ss.subscriptions) >= 0 {
		delete(ss.subscriptions, handle) // This is a no-op if the handle does not exist.
	}

	if before == 1 && len(ss.subscriptions) == 0 && ss.notifierFunc != nil && ss.notifierCtxCancel != nil {
		// We removed the last subscription; cancel the notifier goroutine
		ss.notifierCtxCancel()
		ss.notifierCtxCancel = nil
	}
}
