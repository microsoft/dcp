/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package ide

import (
	"context"
	"sync"

	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

// sessionState is the state machine of a single IDE run session as tracked
// inside the Client. It is separate from any state surfaced to consumers via
// SessionHandler and is used to coordinate the StartSession/notification race.
type sessionState uint32

const (
	sessionStateNotStarted    sessionState = 0
	sessionStateRunning       sessionState = 1
	sessionStateCompleted     sessionState = 2
	sessionStateFailedToStart sessionState = 3
)

// requiredHandlerReadiness is the number of times sessionData.handlerReadyWG
// must be decremented before queued notifications are delivered to the
// session's handler. The two events are:
//  1. StartSession registers the handler on the session (or aborts with an
//     EarlyTermination result, in which case the WG is released without a
//     handler ever being set so the dispatch queue can drain cleanly).
//  2. The caller invokes StartSessionResult.ConfirmHandlerReady, indicating
//     the consumer has finished any bookkeeping that must happen before
//     notifications start arriving.
const requiredHandlerReadiness = 2

// sessionData is the per-session bookkeeping kept inside the Client. It is
// keyed by SessionID and accessed from both the notification-dispatch
// goroutines (writers) and the consumer goroutine driving StartSession/
// StopSession (readers).
type sessionData struct {
	lock *sync.Mutex

	state    sessionState
	pid      process.Pid_t
	exitCode *int32

	// exitCh is closed when a session-terminated notification has been
	// recorded for this session. StopSession waits on this channel.
	exitCh chan struct{}

	// handler is the SessionHandler registered by a successful StartSession
	// call. It is nil if no handler was registered (early termination or before
	// StartSession returns).
	handler SessionHandler

	// handlerReadyWG starts with count==requiredHandlerReadiness and gates
	// the per-session notification dispatch queue. See requiredHandlerReadiness.
	handlerReadyWG *sync.WaitGroup

	// notifyQueue serializes per-session notification delivery. Its workers
	// wait on handlerReadyWG before invoking the handler, so notifications
	// received before the handler is ready are queued and delivered in order.
	notifyQueue *resiliency.WorkQueue
}

func newSessionData(lifetimeCtx context.Context) *sessionData {
	sd := &sessionData{
		lock:           &sync.Mutex{},
		state:          sessionStateNotStarted,
		exitCh:         make(chan struct{}),
		handlerReadyWG: &sync.WaitGroup{},
		// concurrency=1: per-session notifications are processed sequentially.
		notifyQueue: resiliency.NewWorkQueue(lifetimeCtx, 1),
	}
	sd.handlerReadyWG.Add(requiredHandlerReadiness)
	return sd
}

// releaseHandlerReady decrements handlerReadyWG by the given amount, releasing
// queued notification dispatchers.
func (sd *sessionData) releaseHandlerReady(count int) {
	for i := 0; i < count; i++ {
		sd.handlerReadyWG.Done()
	}
}

// dispatch enqueues a notification on the session's serial dispatch queue.
// The work item waits for the handler to be ready, then invokes op with the
// registered handler. If no handler is registered by the time the WG releases
// (early termination), op is not called.
func (sd *sessionData) dispatch(op func(handler SessionHandler)) {
	// Errors only if lifetime context is cancelled, in which case there is no
	// useful action to take.
	_ = sd.notifyQueue.Enqueue(func(_ context.Context) {
		sd.handlerReadyWG.Wait()

		sd.lock.Lock()
		handler := sd.handler
		sd.lock.Unlock()

		if handler == nil {
			return
		}
		op(handler)
	})
}
