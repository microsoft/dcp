/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/go-dap"
)

const (
	// DefaultDeduplicationWindow is the default time window for event deduplication.
	// Events received within this window after a virtual event will be suppressed.
	DefaultDeduplicationWindow = 200 * time.Millisecond
)

// eventSignature uniquely identifies an event for deduplication purposes.
type eventSignature struct {
	// eventType is the type of the event (e.g., "continued", "stopped").
	eventType string

	// key contains identifying information specific to the event type.
	// For example, for a continued event, this might be the thread ID.
	key string
}

// eventDeduplicator tracks recently emitted virtual events and suppresses
// matching events from the debug adapter within a configurable time window.
type eventDeduplicator struct {
	mu         sync.Mutex
	events     map[eventSignature]time.Time
	window     time.Duration
	timeSource func() time.Time // For testing
}

// newEventDeduplicator creates a new event deduplicator with the specified window.
func newEventDeduplicator(window time.Duration) *eventDeduplicator {
	return &eventDeduplicator{
		events:     make(map[eventSignature]time.Time),
		window:     window,
		timeSource: time.Now,
	}
}

// RecordVirtualEvent records that a virtual event was emitted.
// Matching events from the adapter within the deduplication window will be suppressed.
func (d *eventDeduplicator) RecordVirtualEvent(event dap.Message) {
	sig := d.getEventSignature(event)
	if sig == nil {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	d.events[*sig] = d.timeSource()
	d.cleanup()
}

// ShouldSuppress returns true if the event should be suppressed because
// a matching virtual event was recently emitted.
func (d *eventDeduplicator) ShouldSuppress(event dap.Message) bool {
	sig := d.getEventSignature(event)
	if sig == nil {
		return false
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	recorded, ok := d.events[*sig]
	if !ok {
		return false
	}

	// Check if the event is within the deduplication window
	if d.timeSource().Sub(recorded) <= d.window {
		// Remove the entry since we're suppressing the matching event
		delete(d.events, *sig)
		return true
	}

	// Event is outside the window; don't suppress
	delete(d.events, *sig)
	return false
}

// cleanup removes expired entries from the event map.
// Must be called with mu held.
func (d *eventDeduplicator) cleanup() {
	now := d.timeSource()
	for sig, recorded := range d.events {
		if now.Sub(recorded) > d.window {
			delete(d.events, sig)
		}
	}
}

// getEventSignature extracts a signature from a DAP event message.
// Returns nil for non-event messages or events that shouldn't be deduplicated.
func (d *eventDeduplicator) getEventSignature(msg dap.Message) *eventSignature {
	switch event := msg.(type) {
	case *dap.ContinuedEvent:
		return &eventSignature{
			eventType: "continued",
			key:       fmt.Sprintf("thread:%d", event.Body.ThreadId),
		}

	case *dap.StoppedEvent:
		return &eventSignature{
			eventType: "stopped",
			key:       fmt.Sprintf("thread:%d:reason:%s", event.Body.ThreadId, event.Body.Reason),
		}

	case *dap.ThreadEvent:
		return &eventSignature{
			eventType: "thread",
			key:       fmt.Sprintf("thread:%d:reason:%s", event.Body.ThreadId, event.Body.Reason),
		}

	case *dap.OutputEvent:
		// Don't deduplicate output events - each output is unique
		return nil

	case *dap.BreakpointEvent:
		return &eventSignature{
			eventType: "breakpoint",
			key:       fmt.Sprintf("id:%d:reason:%s", event.Body.Breakpoint.Id, event.Body.Reason),
		}

	case *dap.ModuleEvent:
		return &eventSignature{
			eventType: "module",
			key:       fmt.Sprintf("id:%v:reason:%s", event.Body.Module.Id, event.Body.Reason),
		}

	case *dap.LoadedSourceEvent:
		return &eventSignature{
			eventType: "loadedSource",
			key:       fmt.Sprintf("path:%s:reason:%s", event.Body.Source.Path, event.Body.Reason),
		}

	case *dap.ProcessEvent:
		return &eventSignature{
			eventType: "process",
			key:       fmt.Sprintf("name:%s", event.Body.Name),
		}

	case *dap.CapabilitiesEvent:
		// Don't deduplicate capabilities - they should be rare and always forwarded
		return nil

	case *dap.ProgressStartEvent:
		return &eventSignature{
			eventType: "progressStart",
			key:       fmt.Sprintf("id:%s", event.Body.ProgressId),
		}

	case *dap.ProgressUpdateEvent:
		return &eventSignature{
			eventType: "progressUpdate",
			key:       fmt.Sprintf("id:%s", event.Body.ProgressId),
		}

	case *dap.ProgressEndEvent:
		return &eventSignature{
			eventType: "progressEnd",
			key:       fmt.Sprintf("id:%s", event.Body.ProgressId),
		}

	case *dap.InvalidatedEvent:
		// Always forward invalidated events
		return nil

	case *dap.MemoryEvent:
		return &eventSignature{
			eventType: "memory",
			key:       fmt.Sprintf("ref:%s:offset:%d", event.Body.MemoryReference, event.Body.Offset),
		}

	default:
		// For unknown events, don't deduplicate
		return nil
	}
}
