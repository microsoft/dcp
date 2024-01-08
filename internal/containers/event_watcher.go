package containers

import (
	"context"
	"fmt"
	"sync"

	"github.com/go-logr/logr"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

type EventWatcher struct {
	orchestrator      ContainerOrchestrator
	watchFunc         func(ctx context.Context)
	subscriptions     *syncmap.Map[handleT, *EventSubscription]
	cancelFunc        context.CancelFunc
	subscriptionCount uint32
	mutex             *sync.Mutex
	log               logr.Logger
}

func NewEventWatcher(
	orchestrator ContainerOrchestrator,
	watchFunc func(ctx context.Context),
	log logr.Logger,
) *EventWatcher {
	return &EventWatcher{
		orchestrator:      orchestrator,
		watchFunc:         watchFunc,
		subscriptions:     &syncmap.Map[handleT, *EventSubscription]{},
		subscriptionCount: 0,
		cancelFunc:        nil,
		mutex:             &sync.Mutex{},
		log:               log,
	}
}

func (w *EventWatcher) Watch(sink chan<- EventMessage) (*EventSubscription, error) {
	sub := NewEventSubscription(w, sink)

	w.mutex.Lock()
	defer w.mutex.Unlock()

	w.subscriptions.Store(sub.handle, sub)
	w.subscriptionCount += 1

	if w.subscriptionCount == 1 {
		watcherCtx, cancelFunc := context.WithCancel(context.Background())
		w.cancelFunc = cancelFunc
		go w.watchFunc(watcherCtx)
	}

	return sub, nil
}

func (w *EventWatcher) Notify(evt EventMessage) {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	w.subscriptions.Range(func(_ handleT, sub *EventSubscription) bool {
		sub.notify(evt)
		return true
	})
}

func (w *EventWatcher) eventSubscriptionCancelled(handle handleT) {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	w.subscriptions.Delete(handle)

	if w.subscriptionCount == 0 {
		w.log.Error(fmt.Errorf("eventSubscriptionCancelled called when there are no subscriptions"), "")
		return
	}

	w.subscriptionCount -= 1
	if w.subscriptionCount == 0 {
		w.cancelFunc()
		w.cancelFunc = nil
	}
}
