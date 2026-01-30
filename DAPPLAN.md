# DAP Proxy Implementation Plan

## Problem Statement
Create a Debug Adapter Protocol (DAP) proxy that sits between an IDE client (upstream) and a debug adapter server (downstream). The proxy must:
- Forward DAP messages bidirectionally with support for message modification
- Manage virtual sequence numbers for injected requests
- Intercept and handle `runInTerminal` reverse requests
- Provide a synchronous API for injecting virtual requests
- Handle event deduplication for virtual request side effects
- Support both TCP and stdio transports

## Proposed Approach
Fresh implementation in `internal/dap/`, replacing the existing partial implementation. The architecture will use:
- Separate read/write goroutines for each connection direction
- A pending request map indexed by virtual sequence number for response routing
- Channel-based message queues with sync wrappers for virtual request injection
- Handler function pattern for message modification/interception

---

## Workplan

### Phase 1: Core Types and Transport Abstraction
- [x] **1.1** Define transport interface (`DapTransport`) supporting both TCP and stdio
- [x] **1.2** Implement TCP transport (`TcpTransport`)
- [x] **1.3** Implement stdio transport (`StdioTransport`)
- [x] **1.4** Define core message wrapper type (`proxyMessage`) with original seq, virtual seq, and virtual flag
- [x] **1.5** Define pending request tracking structure (`pendingRequest`) with response channel

### Phase 2: Proxy Core Structure
- [x] **2.1** Define `DapProxy` struct with:
  - Upstream/downstream transports
  - Pending request map (keyed by virtual seq)
  - Sequence counters (IDE-facing and adapter-facing)
  - Message handler function
  - Lifecycle context
- [x] **2.2** Define `ProxyConfig` options struct (handler, logger, timeouts)
- [x] **2.3** Implement constructor `NewDapProxy()`

### Phase 3: Message Pumps
- [x] **3.1** Implement upstream reader goroutine (IDE → Proxy)
  - Read messages from IDE
  - Call handler for modification/interception
  - Assign virtual sequence number for requests
  - Track pending requests
  - Queue for downstream forwarding
- [x] **3.2** Implement downstream reader goroutine (Adapter → Proxy)
  - Read messages from debug adapter
  - For responses: map virtual seq back to original, route to IDE or virtual request caller
  - For events: check for deduplication, forward to IDE
  - For reverse requests (like `runInTerminal`): intercept and handle
- [x] **3.3** Implement upstream writer goroutine (Proxy → IDE)
  - Consume from outgoing queue
  - Write to IDE transport
- [x] **3.4** Implement downstream writer goroutine (Proxy → Adapter)
  - Consume from outgoing queue
  - Write to adapter transport

### Phase 4: Virtual Request Injection
- [x] **4.1** Implement async `SendRequestAsync(request, responseChan)` for injecting virtual requests
- [x] **4.2** Implement sync wrapper `SendRequest(ctx, request) (response, error)` that blocks until response
- [x] **4.3** Add virtual event emission capability `EmitEvent(event)` for proxy-generated events

### Phase 5: Initialize Request Handling
- [x] **5.1** Implement default handler that forces `supportsRunInTerminalRequest = true` on `InitializeRequest`
- [x] **5.2** Ensure handler composes with user-provided handlers

### Phase 6: RunInTerminal Interception
- [x] **6.1** Detect `RunInTerminalRequest` from downstream adapter
- [x] **6.2** Implement stub terminal handler (placeholder for future side-channel implementation)
- [x] **6.3** Generate appropriate `RunInTerminalResponse` back to adapter
- [x] **6.4** Do NOT forward request to IDE

### Phase 7: Event Deduplication
- [x] **7.1** Track recently emitted virtual events (type + key fields)
- [x] **7.2** When adapter sends event that matches a recently emitted virtual event, suppress it
- [x] **7.3** Use time-based expiration for dedup window (configurable, ~100-200ms default)

### Phase 8: Shutdown and Error Handling
- [x] **8.1** Implement graceful shutdown on context cancellation
  - Send terminated event to IDE if possible
  - Drain pending requests with errors
  - Close transports
- [x] **8.2** Implement hard stop mechanism (timeout-based or separate context)
- [x] **8.3** Handle connection errors and propagate shutdown
- [x] **8.4** Return error from blocking `Start()` method

### Phase 9: Testing
- [x] **9.1** Unit tests for sequence number mapping
- [x] **9.2** Unit tests for pending request routing
- [x] **9.3** Unit tests for event deduplication
- [x] **9.4** Integration tests with mock DAP client/server
- [x] **9.5** Test graceful shutdown scenarios

### Phase 10: Cleanup
- [x] **10.1** Remove old proxy.go, server.go, client.go files (or refactor to use new implementation)
- [x] **10.2** Update any existing references to old types
- [x] **10.3** Add package-level documentation

---

## Design Notes

### Sequence Number Flow
```
IDE sends request seq=5
  ↓
Proxy injects virtual request → assigned virtual seq=6 to adapter
Proxy forwards IDE request   → assigned virtual seq=7 to adapter (stores: 7 → {original: 5, virtual: false})
  ↓
Adapter responds to seq=7
  ↓
Proxy looks up seq=7 → not virtual, original=5 → forward to IDE as response to seq=5
```

### Pending Request Structure
```go
type pendingRequest struct {
    originalSeq    int                // Seq from IDE (0 if virtual)
    virtual        bool               // True if proxy-injected
    responseChan   chan dap.Message   // For virtual requests only
    request        dap.Message        // Original request for debugging
}
```

### Event Deduplication Strategy
When proxy sends a virtual request that implies an event (e.g., `ContinueRequest` → `ContinuedEvent`):
1. Proxy emits `ContinuedEvent` to IDE immediately (ensures UI updates)
2. Record event signature in dedup cache with timestamp
3. If adapter sends matching `ContinuedEvent` within dedup window, suppress it
4. Clear dedup entry after window expires

### Handler Function Signature
```go
type MessageHandler func(msg dap.Message, direction Direction) (modified dap.Message, forward bool)

type Direction int
const (
    Upstream   Direction = iota  // IDE → Adapter
    Downstream                   // Adapter → IDE
)
```

### Transport Interface
```go
type DapTransport interface {
    ReadMessage() (dap.Message, error)
    WriteMessage(msg dap.Message) error
    Close() error
}
```

---

## File Structure
```
internal/dap/
├── transport.go      # Transport interface and implementations
├── proxy.go          # DapProxy main implementation
├── message.go        # Message wrapper types, pending request tracking
├── handler.go        # Default handlers (initialize, runInTerminal)
├── dedup.go          # Event deduplication logic
├── proxy_test.go     # Unit tests
└── integration_test.go # Integration tests with mock client/server
```

---

## Open Questions (resolved)
1. ~~Build on existing or fresh implementation?~~ → Fresh implementation
2. ~~Terminal handler behavior?~~ → Stub for now, future side-channel feature
3. ~~Transport support?~~ → Both TCP and stdio
4. ~~Sequence management approach?~~ → Single counter with lookup table
5. ~~Virtual request API?~~ → Sync wrapper around async channel
6. ~~Event deduplication?~~ → Content + time-based, first-wins approach
7. ~~Message modification API?~~ → Single handler function
8. ~~Shutdown behavior?~~ → Graceful with timeout to hard stop, return error from Start()
9. ~~Code location?~~ → internal/dap/ replacing existing files
