/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"fmt"
	"sync"

	"github.com/google/go-dap"
)

// syntheticEventGenerator generates synthetic events based on a request and its response.
// It returns a slice of events to be sent to the upstream IDE client.
// The function is called after a successful response is received for a virtual request.
type syntheticEventGenerator func(request dap.Message, response dap.Message, cache *breakpointCache) []dap.Message

// breakpointInfo stores information about a breakpoint for delta computation.
type breakpointInfo struct {
	id       int
	verified bool
	message  string
	source   *dap.Source
	line     int
}

// breakpointCache tracks the current state of breakpoints for delta computation.
// This is used to determine which breakpoints were added, removed, or changed
// when processing breakpoint-related virtual requests.
type breakpointCache struct {
	mu sync.RWMutex

	// sourceBreakpoints maps source path -> (line -> breakpoint info)
	sourceBreakpoints map[string]map[int]breakpointInfo

	// functionBreakpoints maps function name -> breakpoint info
	functionBreakpoints map[string]breakpointInfo

	// exceptionBreakpoints stores exception breakpoints by filter ID
	exceptionBreakpoints map[string]breakpointInfo
}

// newBreakpointCache creates a new breakpoint cache.
func newBreakpointCache() *breakpointCache {
	return &breakpointCache{
		sourceBreakpoints:    make(map[string]map[int]breakpointInfo),
		functionBreakpoints:  make(map[string]breakpointInfo),
		exceptionBreakpoints: make(map[string]breakpointInfo),
	}
}

// updateSourceBreakpoints updates the cache with new breakpoints for a source.
// It returns:
// - newBps: breakpoints that were added
// - removedBps: breakpoints that were removed
// - changedBps: breakpoints that were modified
func (c *breakpointCache) updateSourceBreakpoints(path string, newBreakpoints []dap.Breakpoint) (
	newBps []breakpointInfo, removedBps []breakpointInfo, changedBps []breakpointInfo) {

	c.mu.Lock()
	defer c.mu.Unlock()

	// Get current state
	current := c.sourceBreakpoints[path]
	if current == nil {
		current = make(map[int]breakpointInfo)
	}

	// Build new state and track changes
	newState := make(map[int]breakpointInfo)
	for _, bp := range newBreakpoints {
		info := breakpointInfo{
			id:       bp.Id,
			verified: bp.Verified,
			message:  bp.Message,
			source:   bp.Source,
			line:     bp.Line,
		}
		newState[bp.Line] = info

		// Check if this is new or changed
		if existing, ok := current[bp.Line]; ok {
			// Check if changed
			if existing.verified != bp.Verified || existing.message != bp.Message {
				changedBps = append(changedBps, info)
			}
			delete(current, bp.Line) // Mark as processed
		} else {
			newBps = append(newBps, info)
		}
	}

	// Remaining items in current are removed breakpoints
	for _, info := range current {
		removedBps = append(removedBps, info)
	}

	// Update cache
	c.sourceBreakpoints[path] = newState

	return newBps, removedBps, changedBps
}

// updateFunctionBreakpoints updates the cache with new function breakpoints.
// It returns the same delta information as updateSourceBreakpoints.
func (c *breakpointCache) updateFunctionBreakpoints(names []string, newBreakpoints []dap.Breakpoint) (
	newBps []breakpointInfo, removedBps []breakpointInfo, changedBps []breakpointInfo) {

	c.mu.Lock()
	defer c.mu.Unlock()

	// Get current state
	current := make(map[string]breakpointInfo)
	for k, v := range c.functionBreakpoints {
		current[k] = v
	}

	// Build new state and track changes
	newState := make(map[string]breakpointInfo)
	for i, bp := range newBreakpoints {
		if i >= len(names) {
			break
		}
		name := names[i]
		info := breakpointInfo{
			id:       bp.Id,
			verified: bp.Verified,
			message:  bp.Message,
			line:     bp.Line,
		}
		newState[name] = info

		// Check if this is new or changed
		if existing, ok := current[name]; ok {
			if existing.verified != bp.Verified || existing.message != bp.Message {
				changedBps = append(changedBps, info)
			}
			delete(current, name)
		} else {
			newBps = append(newBps, info)
		}
	}

	// Remaining items are removed
	for _, info := range current {
		removedBps = append(removedBps, info)
	}

	// Update cache
	c.functionBreakpoints = newState

	return newBps, removedBps, changedBps
}

// stateChangingCommands defines which DAP commands change debuggee state
// and require synthetic event generation for virtual requests.
var stateChangingCommands = map[string]syntheticEventGenerator{
	"continue":        generateContinuedEvents,
	"next":            generateContinuedEvents,
	"stepIn":          generateContinuedEvents,
	"stepOut":         generateContinuedEvents,
	"stepBack":        generateContinuedEvents,
	"reverseContinue": generateContinuedEvents,
	"pause":           generatePauseEvents,
	"disconnect":      generateTerminatedEvents,
	"terminate":       generateTerminatedEvents,
	"setBreakpoints":  generateBreakpointEvents,
	// setFunctionBreakpoints and setExceptionBreakpoints are handled separately
	// because they need access to the cache differently
}

// getEventGenerator returns the synthetic event generator for a command, if any.
func getEventGenerator(command string) syntheticEventGenerator {
	return stateChangingCommands[command]
}

// generateContinuedEvents generates a ContinuedEvent for execution-resuming commands.
func generateContinuedEvents(request dap.Message, response dap.Message, _ *breakpointCache) []dap.Message {
	// Verify the response was successful
	if !isSuccessfulResponse(response) {
		return nil
	}

	// Extract thread ID from the request
	var threadID int
	switch req := request.(type) {
	case *dap.ContinueRequest:
		threadID = req.Arguments.ThreadId
	case *dap.NextRequest:
		threadID = req.Arguments.ThreadId
	case *dap.StepInRequest:
		threadID = req.Arguments.ThreadId
	case *dap.StepOutRequest:
		threadID = req.Arguments.ThreadId
	case *dap.StepBackRequest:
		threadID = req.Arguments.ThreadId
	case *dap.ReverseContinueRequest:
		threadID = req.Arguments.ThreadId
	default:
		return nil
	}

	// Create ContinuedEvent
	continuedEvent := &dap.ContinuedEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{
				Type: "event",
			},
			Event: "continued",
		},
		Body: dap.ContinuedEventBody{
			ThreadId:            threadID,
			AllThreadsContinued: true, // Conservative default
		},
	}

	// Check if response has allThreadsContinued info
	if resp, ok := response.(*dap.ContinueResponse); ok {
		continuedEvent.Body.AllThreadsContinued = resp.Body.AllThreadsContinued
	}

	return []dap.Message{continuedEvent}
}

// generatePauseEvents generates a StoppedEvent for the pause command.
func generatePauseEvents(request dap.Message, response dap.Message, _ *breakpointCache) []dap.Message {
	// Verify the response was successful
	if !isSuccessfulResponse(response) {
		return nil
	}

	// Extract thread ID from the request
	pauseReq, ok := request.(*dap.PauseRequest)
	if !ok {
		return nil
	}

	// Create StoppedEvent with reason "pause"
	stoppedEvent := &dap.StoppedEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{
				Type: "event",
			},
			Event: "stopped",
		},
		Body: dap.StoppedEventBody{
			Reason:   "pause",
			ThreadId: pauseReq.Arguments.ThreadId,
		},
	}

	return []dap.Message{stoppedEvent}
}

// generateTerminatedEvents generates a TerminatedEvent for disconnect/terminate commands.
func generateTerminatedEvents(_ dap.Message, response dap.Message, _ *breakpointCache) []dap.Message {
	// Verify the response was successful
	if !isSuccessfulResponse(response) {
		return nil
	}

	// Create TerminatedEvent
	terminatedEvent := &dap.TerminatedEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{
				Type: "event",
			},
			Event: "terminated",
		},
	}

	return []dap.Message{terminatedEvent}
}

// generateBreakpointEvents generates BreakpointEvents for setBreakpoints command.
// This compares the response with the cached state to determine which breakpoints
// were added, removed, or changed.
func generateBreakpointEvents(request dap.Message, response dap.Message, cache *breakpointCache) []dap.Message {
	// Verify the response was successful
	if !isSuccessfulResponse(response) {
		return nil
	}

	bpReq, ok := request.(*dap.SetBreakpointsRequest)
	if !ok {
		return nil
	}

	bpResp, ok := response.(*dap.SetBreakpointsResponse)
	if !ok {
		return nil
	}

	if cache == nil {
		return nil
	}

	// Get the source path
	sourcePath := ""
	if bpReq.Arguments.Source.Path != "" {
		sourcePath = bpReq.Arguments.Source.Path
	} else if bpReq.Arguments.Source.Name != "" {
		sourcePath = bpReq.Arguments.Source.Name
	}

	if sourcePath == "" {
		return nil
	}

	// Update cache and get deltas
	newBps, removedBps, changedBps := cache.updateSourceBreakpoints(sourcePath, bpResp.Body.Breakpoints)

	// Generate events
	var events []dap.Message

	// Emit "new" events for added breakpoints
	for _, bp := range newBps {
		events = append(events, createBreakpointEvent("new", bp))
	}

	// Emit "removed" events for removed breakpoints
	for _, bp := range removedBps {
		events = append(events, createBreakpointEvent("removed", bp))
	}

	// Emit "changed" events for modified breakpoints
	for _, bp := range changedBps {
		events = append(events, createBreakpointEvent("changed", bp))
	}

	return events
}

// generateFunctionBreakpointEvents generates BreakpointEvents for setFunctionBreakpoints.
func generateFunctionBreakpointEvents(request dap.Message, response dap.Message, cache *breakpointCache) []dap.Message {
	// Verify the response was successful
	if !isSuccessfulResponse(response) {
		return nil
	}

	fnReq, ok := request.(*dap.SetFunctionBreakpointsRequest)
	if !ok {
		return nil
	}

	fnResp, ok := response.(*dap.SetFunctionBreakpointsResponse)
	if !ok {
		return nil
	}

	if cache == nil {
		return nil
	}

	// Extract function names from request
	names := make([]string, len(fnReq.Arguments.Breakpoints))
	for i, bp := range fnReq.Arguments.Breakpoints {
		names[i] = bp.Name
	}

	// Update cache and get deltas
	newBps, removedBps, changedBps := cache.updateFunctionBreakpoints(names, fnResp.Body.Breakpoints)

	// Generate events
	var events []dap.Message

	for _, bp := range newBps {
		events = append(events, createBreakpointEvent("new", bp))
	}
	for _, bp := range removedBps {
		events = append(events, createBreakpointEvent("removed", bp))
	}
	for _, bp := range changedBps {
		events = append(events, createBreakpointEvent("changed", bp))
	}

	return events
}

// generateExceptionBreakpointEvents generates BreakpointEvents for setExceptionBreakpoints.
// This only generates events if the response includes breakpoints (optional per DAP spec).
func generateExceptionBreakpointEvents(_ dap.Message, response dap.Message, _ *breakpointCache) []dap.Message {
	// Verify the response was successful
	if !isSuccessfulResponse(response) {
		return nil
	}

	excResp, ok := response.(*dap.SetExceptionBreakpointsResponse)
	if !ok {
		return nil
	}

	// Only generate events if the response includes breakpoints
	if len(excResp.Body.Breakpoints) == 0 {
		return nil
	}

	// Generate "new" events for each breakpoint in the response
	var events []dap.Message
	for _, bp := range excResp.Body.Breakpoints {
		info := breakpointInfo{
			id:       bp.Id,
			verified: bp.Verified,
			message:  bp.Message,
		}
		events = append(events, createBreakpointEvent("new", info))
	}

	return events
}

// createBreakpointEvent creates a BreakpointEvent with the given reason and breakpoint info.
func createBreakpointEvent(reason string, bp breakpointInfo) *dap.BreakpointEvent {
	bpData := dap.Breakpoint{
		Id:       bp.id,
		Verified: bp.verified,
		Message:  bp.message,
		Line:     bp.line,
		Source:   bp.source,
	}

	return &dap.BreakpointEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{
				Type: "event",
			},
			Event: "breakpoint",
		},
		Body: dap.BreakpointEventBody{
			Reason:     reason,
			Breakpoint: bpData,
		},
	}
}

// isSuccessfulResponse checks if a DAP response indicates success.
func isSuccessfulResponse(response dap.Message) bool {
	switch resp := response.(type) {
	case *dap.Response:
		return resp.Success
	case dap.ResponseMessage:
		return resp.GetResponse().Success
	default:
		return false
	}
}

// isStateChangingCommand returns true if the command changes debuggee state
// and should generate synthetic events for virtual requests.
func isStateChangingCommand(command string) bool {
	_, ok := stateChangingCommands[command]
	if ok {
		return true
	}
	// Additional commands handled separately
	return command == "setFunctionBreakpoints" || command == "setExceptionBreakpoints"
}

// getSyntheticEvents generates synthetic events for a virtual request/response pair.
func getSyntheticEvents(request dap.Message, response dap.Message, cache *breakpointCache) []dap.Message {
	var command string
	switch req := request.(type) {
	case *dap.Request:
		command = req.Command
	case dap.RequestMessage:
		command = req.GetRequest().Command
	default:
		return nil
	}

	// Handle special cases first
	switch command {
	case "setFunctionBreakpoints":
		return generateFunctionBreakpointEvents(request, response, cache)
	case "setExceptionBreakpoints":
		return generateExceptionBreakpointEvents(request, response, cache)
	}

	// Use the registered generator
	if generator := getEventGenerator(command); generator != nil {
		return generator(request, response, cache)
	}

	return nil
}

// debugEventType returns a string describing the event type for logging.
func debugEventType(event dap.Message) string {
	switch e := event.(type) {
	case *dap.ContinuedEvent:
		return fmt.Sprintf("continued(threadId=%d)", e.Body.ThreadId)
	case *dap.StoppedEvent:
		return fmt.Sprintf("stopped(reason=%s, threadId=%d)", e.Body.Reason, e.Body.ThreadId)
	case *dap.TerminatedEvent:
		return "terminated"
	case *dap.BreakpointEvent:
		return fmt.Sprintf("breakpoint(reason=%s, id=%d)", e.Body.Reason, e.Body.Breakpoint.Id)
	case dap.EventMessage:
		return e.GetEvent().Event
	default:
		return fmt.Sprintf("%T", event)
	}
}
