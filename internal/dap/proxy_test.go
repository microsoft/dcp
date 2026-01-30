// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dap

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/go-dap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockTransport is a mock Transport implementation for testing.
type mockTransport struct {
	readChan  chan dap.Message
	writeChan chan dap.Message
	closed    bool
	mu        sync.Mutex
}

func newMockTransport() *mockTransport {
	return &mockTransport{
		readChan:  make(chan dap.Message, 100),
		writeChan: make(chan dap.Message, 100),
	}
}

func (t *mockTransport) ReadMessage() (dap.Message, error) {
	msg, ok := <-t.readChan
	if !ok {
		return nil, ErrProxyClosed
	}
	return msg, nil
}

func (t *mockTransport) WriteMessage(msg dap.Message) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return ErrProxyClosed
	}
	t.mu.Unlock()

	t.writeChan <- msg
	return nil
}

func (t *mockTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.closed {
		t.closed = true
		close(t.readChan)
	}
	return nil
}

// Inject simulates receiving a message from the remote end.
func (t *mockTransport) Inject(msg dap.Message) {
	t.readChan <- msg
}

// Receive gets the next message written to this transport.
func (t *mockTransport) Receive(timeout time.Duration) (dap.Message, bool) {
	select {
	case msg := <-t.writeChan:
		return msg, true
	case <-time.After(timeout):
		return nil, false
	}
}

func TestProxy_ForwardRequestAndResponse(t *testing.T) {
	t.Parallel()

	upstream := newMockTransport()   // IDE side
	downstream := newMockTransport() // Adapter side

	proxy := NewProxy(upstream, downstream, ProxyConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start proxy in background
	var proxyErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		proxyErr = proxy.Start(ctx)
	}()

	// Give proxy time to start
	time.Sleep(50 * time.Millisecond)

	// IDE sends a request
	request := &dap.ContinueRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{Seq: 5, Type: "request"},
			Command:         "continue",
		},
		Arguments: dap.ContinueArguments{ThreadId: 1},
	}
	upstream.Inject(request)

	// Adapter should receive the request (with remapped seq)
	adapterMsg, received := downstream.Receive(time.Second)
	require.True(t, received, "adapter should receive request")
	adapterReq, ok := adapterMsg.(*dap.ContinueRequest)
	require.True(t, ok)
	assert.Equal(t, "continue", adapterReq.Command)
	remappedSeq := adapterReq.Seq

	// Adapter sends response
	response := &dap.ContinueResponse{
		Response: dap.Response{
			ProtocolMessage: dap.ProtocolMessage{Seq: 1, Type: "response"},
			Command:         "continue",
			RequestSeq:      remappedSeq,
			Success:         true,
		},
		Body: dap.ContinueResponseBody{AllThreadsContinued: true},
	}
	downstream.Inject(response)

	// IDE should receive response with original seq
	ideMsg, received := upstream.Receive(time.Second)
	require.True(t, received, "IDE should receive response")
	ideResp, ok := ideMsg.(*dap.ContinueResponse)
	require.True(t, ok)
	assert.Equal(t, 5, ideResp.RequestSeq, "response should have original request seq")
	assert.True(t, ideResp.Success)

	// Shutdown
	cancel()
	wg.Wait()

	assert.Error(t, proxyErr) // Context cancelled is an error
}

func TestProxy_ForwardEvent(t *testing.T) {
	t.Parallel()

	upstream := newMockTransport()
	downstream := newMockTransport()

	proxy := NewProxy(upstream, downstream, ProxyConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = proxy.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	// Adapter sends an event
	event := &dap.StoppedEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{Seq: 10, Type: "event"},
			Event:           "stopped",
		},
		Body: dap.StoppedEventBody{
			Reason:   "breakpoint",
			ThreadId: 1,
		},
	}
	downstream.Inject(event)

	// IDE should receive the event
	ideMsg, received := upstream.Receive(time.Second)
	require.True(t, received, "IDE should receive event")
	ideEvent, ok := ideMsg.(*dap.StoppedEvent)
	require.True(t, ok)
	assert.Equal(t, "stopped", ideEvent.Event.Event)
	assert.Equal(t, "breakpoint", ideEvent.Body.Reason)

	cancel()
	wg.Wait()
}

func TestProxy_InitializeRequestSetsSupportsRunInTerminal(t *testing.T) {
	t.Parallel()

	upstream := newMockTransport()
	downstream := newMockTransport()

	proxy := NewProxy(upstream, downstream, ProxyConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = proxy.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	// IDE sends initialize request without terminal support
	request := &dap.InitializeRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{Seq: 1, Type: "request"},
			Command:         "initialize",
		},
		Arguments: dap.InitializeRequestArguments{
			AdapterID:                    "test",
			SupportsRunInTerminalRequest: false,
		},
	}
	upstream.Inject(request)

	// Adapter should receive request with terminal support enabled
	adapterMsg, received := downstream.Receive(time.Second)
	require.True(t, received, "adapter should receive request")
	initReq, ok := adapterMsg.(*dap.InitializeRequest)
	require.True(t, ok)
	assert.True(t, initReq.Arguments.SupportsRunInTerminalRequest,
		"supportsRunInTerminalRequest should be forced to true")

	cancel()
	wg.Wait()
}

func TestProxy_InterceptRunInTerminal(t *testing.T) {
	t.Parallel()

	upstream := newMockTransport()
	downstream := newMockTransport()

	terminalCalled := false
	var terminalArgs dap.RunInTerminalRequestArguments

	proxy := NewProxy(upstream, downstream, ProxyConfig{
		TerminalHandler: func(req *dap.RunInTerminalRequest) *dap.RunInTerminalResponse {
			terminalCalled = true
			terminalArgs = req.Arguments
			return &dap.RunInTerminalResponse{
				Response: dap.Response{
					ProtocolMessage: dap.ProtocolMessage{Type: "response"},
					Command:         "runInTerminal",
					RequestSeq:      req.Seq,
					Success:         true,
				},
				Body: dap.RunInTerminalResponseBody{
					ProcessId: 12345,
				},
			}
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = proxy.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	// Adapter sends runInTerminal request
	runInTerminal := &dap.RunInTerminalRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{Seq: 1, Type: "request"},
			Command:         "runInTerminal",
		},
		Arguments: dap.RunInTerminalRequestArguments{
			Kind:  "integrated",
			Title: "Debug",
			Cwd:   "/home/user",
			Args:  []string{"python", "app.py"},
		},
	}
	downstream.Inject(runInTerminal)

	// Response should go back to adapter
	adapterMsg, received := downstream.Receive(time.Second)
	require.True(t, received, "adapter should receive response")
	resp, ok := adapterMsg.(*dap.RunInTerminalResponse)
	require.True(t, ok)
	assert.True(t, resp.Success)
	assert.Equal(t, 12345, resp.Body.ProcessId)

	// Terminal handler should have been called
	assert.True(t, terminalCalled)
	assert.Equal(t, "integrated", terminalArgs.Kind)
	assert.Equal(t, []string{"python", "app.py"}, terminalArgs.Args)

	// Request should NOT be forwarded to IDE
	_, received = upstream.Receive(100 * time.Millisecond)
	assert.False(t, received, "runInTerminal should not be forwarded to IDE")

	cancel()
	wg.Wait()
}

func TestProxy_SendVirtualRequest(t *testing.T) {
	t.Parallel()

	upstream := newMockTransport()
	downstream := newMockTransport()

	proxy := NewProxy(upstream, downstream, ProxyConfig{
		RequestTimeout: 5 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = proxy.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	// Send virtual request in background
	type virtualResult struct {
		resp dap.Message
		err  error
	}
	resultChan := make(chan virtualResult, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := proxy.SendRequest(ctx, &dap.ThreadsRequest{
			Request: dap.Request{
				ProtocolMessage: dap.ProtocolMessage{Seq: 0, Type: "request"},
				Command:         "threads",
			},
		})
		resultChan <- virtualResult{resp: resp, err: err}
	}()

	// Adapter should receive the request
	adapterMsg, received := downstream.Receive(time.Second)
	require.True(t, received, "adapter should receive virtual request")
	threadsReq, ok := adapterMsg.(*dap.ThreadsRequest)
	require.True(t, ok)
	virtualSeq := threadsReq.Seq

	// Send response from adapter
	downstream.Inject(&dap.ThreadsResponse{
		Response: dap.Response{
			ProtocolMessage: dap.ProtocolMessage{Seq: 1, Type: "response"},
			Command:         "threads",
			RequestSeq:      virtualSeq,
			Success:         true,
		},
		Body: dap.ThreadsResponseBody{
			Threads: []dap.Thread{{Id: 1, Name: "main"}},
		},
	})

	// Wait for virtual request to complete
	var result virtualResult
	select {
	case result = <-resultChan:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for virtual request result")
	}

	require.NoError(t, result.err)
	require.NotNil(t, result.resp)
	threadsResp, ok := result.resp.(*dap.ThreadsResponse)
	require.True(t, ok)
	assert.Len(t, threadsResp.Body.Threads, 1)

	// Response should NOT be forwarded to IDE
	_, received = upstream.Receive(100 * time.Millisecond)
	assert.False(t, received, "virtual response should not be forwarded to IDE")

	cancel()
	wg.Wait()
}

func TestProxy_EventDeduplication(t *testing.T) {
	t.Parallel()

	upstream := newMockTransport()
	downstream := newMockTransport()

	proxy := NewProxy(upstream, downstream, ProxyConfig{
		DeduplicationWindow: 500 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = proxy.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	// Emit virtual event
	event := &dap.ContinuedEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{Seq: 0, Type: "event"},
			Event:           "continued",
		},
		Body: dap.ContinuedEventBody{
			ThreadId:            1,
			AllThreadsContinued: true,
		},
	}
	emitErr := proxy.EmitEvent(event)
	require.NoError(t, emitErr)

	// IDE should receive the virtual event
	ideMsg, received := upstream.Receive(time.Second)
	require.True(t, received, "IDE should receive virtual event")
	_, ok := ideMsg.(*dap.ContinuedEvent)
	require.True(t, ok)

	// Now adapter sends same event
	downstream.Inject(&dap.ContinuedEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{Seq: 5, Type: "event"},
			Event:           "continued",
		},
		Body: dap.ContinuedEventBody{
			ThreadId:            1,
			AllThreadsContinued: true,
		},
	})

	// Duplicate should be suppressed
	_, received = upstream.Receive(100 * time.Millisecond)
	assert.False(t, received, "duplicate event should be suppressed")

	cancel()
	wg.Wait()
}

func TestProxy_MessageHandler(t *testing.T) {
	t.Parallel()

	upstream := newMockTransport()
	downstream := newMockTransport()

	handlerCalledChan := make(chan struct{}, 1)
	proxy := NewProxy(upstream, downstream, ProxyConfig{
		Handler: func(msg dap.Message, direction Direction) (dap.Message, bool) {
			if _, ok := msg.(*dap.ContinueRequest); ok && direction == Upstream {
				select {
				case handlerCalledChan <- struct{}{}:
				default:
				}
				// Suppress the message
				return nil, false
			}
			return msg, true
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = proxy.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	// IDE sends a continue request (should be suppressed)
	upstream.Inject(&dap.ContinueRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{Seq: 1, Type: "request"},
			Command:         "continue",
		},
	})

	// Wait for handler to be called
	select {
	case <-handlerCalledChan:
		// Handler was called
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for handler to be called")
	}

	// Message should not reach adapter
	_, received := downstream.Receive(100 * time.Millisecond)
	assert.False(t, received, "suppressed message should not reach adapter")

	cancel()
	wg.Wait()
}

func TestProxy_GracefulShutdown(t *testing.T) {
	t.Parallel()

	upstream := newMockTransport()
	downstream := newMockTransport()

	proxy := NewProxy(upstream, downstream, ProxyConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var proxyErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		proxyErr = proxy.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	// Stop the proxy gracefully
	proxy.Stop()

	wg.Wait()

	// Should have an error (context cancelled)
	assert.Error(t, proxyErr)
}
