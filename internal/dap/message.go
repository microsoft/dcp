// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dap

import (
	"sync"

	"github.com/google/go-dap"
)

// Direction indicates the flow direction of a DAP message through the proxy.
type Direction int

const (
	// Upstream indicates a message flowing from IDE to debug adapter.
	Upstream Direction = iota
	// Downstream indicates a message flowing from debug adapter to IDE.
	Downstream
)

// String returns a human-readable representation of the direction.
func (d Direction) String() string {
	switch d {
	case Upstream:
		return "upstream"
	case Downstream:
		return "downstream"
	default:
		return "unknown"
	}
}

// pendingRequest tracks a request that is awaiting a response.
type pendingRequest struct {
	// originalSeq is the sequence number from the IDE (0 if virtual request).
	originalSeq int

	// virtual indicates if this is a proxy-injected request.
	// If true, the response should be sent to responseChan.
	// If false, the response should be forwarded to the IDE.
	virtual bool

	// responseChan receives the response for virtual requests.
	// Only set when virtual is true.
	responseChan chan dap.Message

	// request is the original request message (for debugging/logging).
	request dap.Message
}

// pendingRequestMap is a thread-safe map of pending requests keyed by virtual sequence number.
type pendingRequestMap struct {
	mu       sync.Mutex
	requests map[int]*pendingRequest
}

// newPendingRequestMap creates a new empty pending request map.
func newPendingRequestMap() *pendingRequestMap {
	return &pendingRequestMap{
		requests: make(map[int]*pendingRequest),
	}
}

// Add adds a pending request to the map.
func (m *pendingRequestMap) Add(virtualSeq int, req *pendingRequest) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[virtualSeq] = req
}

// Get retrieves and removes a pending request from the map.
// Returns nil if no request exists for the given virtual sequence number.
func (m *pendingRequestMap) Get(virtualSeq int) *pendingRequest {
	m.mu.Lock()
	defer m.mu.Unlock()

	req, ok := m.requests[virtualSeq]
	if !ok {
		return nil
	}

	delete(m.requests, virtualSeq)
	return req
}

// Len returns the number of pending requests.
func (m *pendingRequestMap) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

// DrainWithError closes all response channels and clears the map.
// This is used during shutdown to unblock any waiting virtual request callers.
func (m *pendingRequestMap) DrainWithError() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, req := range m.requests {
		if req.virtual && req.responseChan != nil {
			close(req.responseChan)
		}
	}

	m.requests = make(map[int]*pendingRequest)
}

// sequenceCounter provides thread-safe sequence number generation.
type sequenceCounter struct {
	mu  sync.Mutex
	seq int
}

// newSequenceCounter creates a new sequence counter starting at 0.
func newSequenceCounter() *sequenceCounter {
	return &sequenceCounter{seq: 0}
}

// Next returns the next sequence number.
func (c *sequenceCounter) Next() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	return c.seq
}

// Current returns the current sequence number without incrementing.
func (c *sequenceCounter) Current() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seq
}
