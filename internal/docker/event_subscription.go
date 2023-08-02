package docker

import (
	"sync"
	"sync/atomic"

	"github.com/microsoft/usvc-apiserver/internal/containers"
)

type handleT uint32

const (
	invalidHandle    handleT = 0
	firstValidHandle handleT = 1
)

var (
	nextHandle = invalidHandle
)

type dockerEventSubscription struct {
	handle       handleT
	sink         chan<- containers.EventMessage
	orchestrator *DockerCliOrchestrator
	lock         *sync.Mutex
}

func newDockerEventSubscription(orchestrator *DockerCliOrchestrator, sink chan<- containers.EventMessage) *dockerEventSubscription {
	return &dockerEventSubscription{
		handle:       handleT(atomic.AddUint32((*uint32)(&nextHandle), 1)),
		sink:         sink,
		orchestrator: orchestrator,
		lock:         &sync.Mutex{},
	}
}

func (es *dockerEventSubscription) Cancel() error {
	es.lock.Lock()
	defer es.lock.Unlock()
	if es.handle != invalidHandle {
		es.orchestrator.eventSubscriptionCancelled(es.handle)
		es.handle = invalidHandle
		close(es.sink)
		es.sink = nil
	}
	return nil
}

func (es *dockerEventSubscription) notify(m containers.EventMessage) {
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

var _ containers.EventSubscription = (*dockerEventSubscription)(nil)
