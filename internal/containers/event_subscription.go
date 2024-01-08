package containers

import (
	"sync"
	"sync/atomic"
)

type handleT uint32

const (
	invalidHandle    handleT = 0
	firstValidHandle handleT = 1
)

var (
	nextHandle = invalidHandle
)

type EventSubscription struct {
	handle  handleT
	sink    chan<- EventMessage
	watcher *EventWatcher
	lock    *sync.Mutex
}

func NewEventSubscription(watcher *EventWatcher, sink chan<- EventMessage) *EventSubscription {
	return &EventSubscription{
		handle:  handleT(atomic.AddUint32((*uint32)(&nextHandle), 1)),
		sink:    sink,
		watcher: watcher,
		lock:    &sync.Mutex{},
	}
}

func (es *EventSubscription) Cancel() error {
	es.lock.Lock()
	defer es.lock.Unlock()
	if es.handle != invalidHandle {
		es.watcher.eventSubscriptionCancelled(es.handle)
		es.handle = invalidHandle
		close(es.sink)
		es.sink = nil
	}
	return nil
}

func (es *EventSubscription) notify(m EventMessage) {
	es.lock.Lock()
	defer es.lock.Unlock()

	if es.sink == nil {
		return
	}

	// Try to deliver the message, but do not ever block (best-effort).
	// The use of a subscription should make sure that the event can always be delivered without blocking.
	// The call to notify() will also no-op if the subscription has been canceled.
	select {
	case es.sink <- m:
	default:
	}
}
