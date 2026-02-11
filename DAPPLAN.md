# DAP Bridge Implementation Plan

## Problem Statement

Refactor the Debug Adapter Protocol (DAP) implementation from a middleware proxy pattern to a bridge pattern. The current architecture acts as a middleware between an IDE DAP client and a debug adapter host. The new architecture will:

- Act solely as a DAP client connecting to a downstream debug adapter host launched by DCP
- Provide a Unix domain socket bridge that the IDE's debug adapter client connects to
- Authenticate IDE connections via token + session ID handshake
- Intercept and handle `runInTerminal` requests locally (not forwarding to IDE)
- Ensure `supportsRunInTerminalRequest = true` is declared during initialization
- Capture stdout/stderr from either DAP `output` events or directly from processes launched via `runInTerminal`

## Architecture Overview

```
┌──────────────────────────────────────────────────────────────────────────┐
│ IDE (VS Code, Visual Studio, etc.)                                       │
│  └─ Debug Adapter Client                                                 │
│      └─ Connects to Unix socket provided by DCP in run session response  │
└──────────────────────────────────┬───────────────────────────────────────┘
                                   │ DAP messages (Unix socket)
                                   │ + Initial handshake (token + session ID)
                                   ▼
┌──────────────────────────────────────────────────────────────────────────┐
│ DCP DAP Bridge (DapBridge in internal/dap/)                              │
│  ├─ Unix socket listener for IDE connections                             │
│  ├─ Handshake validation (token + session ID)                            │
│  ├─ Message forwarding (IDE ↔ Debug Adapter)                             │
│  ├─ Interception layer:                                                  │
│  │    ├─ initialize: ensure supportsRunInTerminalRequest = true          │
│  │    ├─ runInTerminal: handle locally, launch process, capture stdio    │
│  │    └─ output events: capture for logging (unless runInTerminal used)  │
│  └─ Process runner for runInTerminal commands                            │
└──────────────────────────────────┬───────────────────────────────────────┘
                                   │ DAP messages (stdio/TCP)
                                   ▼
┌──────────────────────────────────────────────────────────────────────────┐
│ Debug Adapter (launched by DCP via existing LaunchDebugAdapter)          │
│  └─ Delve, Node.js debugger, etc.                                        │
└──────────────────────────────────────────────────────────────────────────┘
```

## Key Architectural Differences from Previous Implementation

| Aspect | Previous (Middleware) | New (Bridge) |
|--------|----------------------|--------------|
| IDE connection | TCP DAP + gRPC side-channel | Unix socket with handshake |
| Role | Proxy between two DAP endpoints | DAP client to downstream adapter |
| Initiation | IDE connects to DCP endpoint | DCP provides socket path, IDE connects |
| Authentication | gRPC metadata tokens | Handshake message (token + session ID) |
| runInTerminal | Forwarded via gRPC to controller | Handled locally by bridge |
| stdout/stderr | Via gRPC events or adapter output | Direct capture or output events |

---

## Workplan

### Phase 1: Unix Socket Transport
- [x] **1.1** Add `unixTransport` implementation in `internal/dap/transport.go`
  - Implement `ReadMessage()`, `WriteMessage()`, `Close()` for Unix domain socket connections
  - Follow existing `tcpTransport` pattern
- [x] **1.2** Add `UnixSocketListener` type for managing Unix domain socket lifecycle
  - Create socket file with appropriate permissions (owner-only)
  - Accept incoming connections
  - Cleanup socket file on close

### Phase 2: Bridge Handshake Protocol
- [x] **2.1** Define handshake message format in `internal/dap/bridge_handshake.go`
  ```go
  type BridgeHandshakeRequest struct {
      Token     string `json:"token"`      // Authentication token
      SessionID string `json:"session_id"` // Debug session identifier
  }

  type BridgeHandshakeResponse struct {
      Success bool   `json:"success"`
      Error   string `json:"error,omitempty"`
  }
  ```
- [x] **2.2** Implement handshake reader/writer using length-prefixed JSON
- [x] **2.3** Add handshake validation logic (token verification, session lookup)

### Phase 3: DAP Bridge Core
- [x] **3.1** Create `internal/dap/bridge.go` with `DapBridge` struct
  - Unix socket listener for IDE connections
  - Debug adapter transport (via existing `LaunchedAdapter`)
  - Session state tracking
  - Lifecycle management tied to context
- [x] **3.2** Implement `DapBridge.Start(ctx)` that:
  - Creates Unix socket listener
  - Waits for IDE connection
  - Validates handshake
  - Launches debug adapter (via existing infrastructure)
  - Begins message forwarding loop
- [x] **3.3** Implement bidirectional message forwarding
  - IDE → Adapter: read from Unix socket, write to adapter transport
  - Adapter → IDE: read from adapter transport, write to Unix socket
  - Apply interception callbacks before forwarding

### Phase 4: Message Interception
- [x] **4.1** Create `internal/dap/bridge_interceptor.go` with `BridgeInterceptor` type
  ```go
  type BridgeInterceptor struct {
      sessionID         string
      runInTerminalUsed bool
      stdoutWriter      io.Writer  // For logging stdout
      stderrWriter      io.Writer  // For logging stderr
      launchedProcess   *LaunchedProcess
      log               logr.Logger
  }
  ```
  *(Note: Interception logic is currently embedded in `bridge.go`; may be extracted to separate file later)*
- [x] **4.2** Implement `initialize` request interception
  - Ensure `Arguments.SupportsRunInTerminalRequest = true`
  - Forward modified request to adapter
- [x] **4.3** Implement `output` event interception
  - Parse `OutputEvent.Body.Category` ("stdout", "stderr", "console", etc.)
  - If `runInTerminalUsed == false`: write content to log files
  - Always forward event to IDE (don't suppress)
- [x] **4.4** Implement `runInTerminal` request interception
  - Set `runInTerminalUsed = true`
  - Launch process with command/args/cwd/env from request
  - Attach stdout/stderr capture from process
  - Generate `RunInTerminalResponse` with process ID
  - Do NOT forward request to IDE

### Phase 5: Process Runner for runInTerminal
- [x] **5.1** Create `internal/dap/process_runner.go` with `ProcessRunner` type
  - Launch processes using `pkg/process` executor
  - Capture stdout/stderr via pipes
  - Track process lifecycle (PID, start time, exit code)
- [x] **5.2** Implement stdout/stderr streaming to log files
  - Non-blocking reads with goroutines
  - Use existing temp file patterns from `IdeExecutableRunner`
  - Handle process termination gracefully
- [x] **5.3** Implement process termination
  - Stop process when debug session ends
  - Clean up resources

### Phase 6: Session Management
- [x] **6.1** Create `internal/dap/bridge_session.go` with session tracking
  ```go
  type BridgeSession struct {
      ID                string
      Token             string
      SocketPath        string
      AdapterConfig     *DebugAdapterConfig
      State             BridgeSessionState
      StdoutLogFile     string
      StderrLogFile     string
      RunInTerminalUsed bool
      LaunchedProcess   *ProcessRunner
  }
  ```
- [x] **6.2** Implement `BridgeSessionManager` for session lifecycle
  - Register session before IDE connection
  - Validate session on handshake
  - Clean up on termination
- [x] **6.3** Integrate with existing `SessionMap` or replace as appropriate

### Phase 7: IDE Protocol Integration
- [x] **7.1** Define new API version for debug bridge support
- [x] **7.2** Update `ideRunSessionRequestV1` (or create V2) to include:
  ```go
  type ideRunSessionRequestV2 struct {
      // ... existing fields ...
      DebugBridgeSocketPath string `json:"debug_bridge_socket_path,omitempty"`
      DebugSessionToken     string `json:"debug_session_token,omitempty"`
      DebugSessionID        string `json:"debug_session_id,omitempty"`
  }
  ```
- [x] **7.3** Update `IdeExecutableRunner` to:
  - Detect when `DebugAdapterLaunch` is specified
  - Create `DapBridge` instance
  - Generate unique socket path and session token
  - Include bridge info in run session request to IDE
  - Coordinate bridge lifecycle with executable lifecycle

### Phase 8: Simplify/Remove Middleware Components
- [x] **8.1** Evaluate `Proxy` in `dap_proxy.go`
  - **Decision**: Kept with deprecation notice. Only used in integration tests, not production.
  - Added deprecation comments pointing to DapBridge as the replacement.
- [x] **8.2** Evaluate `SessionDriver` in `session_driver.go`
  - **Decision**: Kept with deprecation notice. Only used in integration tests, not production.
  - Added deprecation comments pointing to DapBridge as the replacement.
- [x] **8.3** Evaluate gRPC `ControlClient`/`ControlServer`
  - **Decision**: Kept with deprecation notice. Only used in integration tests, not production.
  - Unix socket bridge replaces gRPC for production use.
- [x] **8.4** Update or remove proto definitions as needed
  - **Decision**: Kept with deprecation comment in proto file.
  - Proto definitions are still needed for integration tests.

### Phase 9: Testing
- [x] **9.1** Unit tests for Unix socket transport
  - Added in `transport_test.go`
- [x] **9.2** Unit tests for handshake protocol
  - Added in `bridge_handshake_test.go`
- [x] **9.3** Unit tests for message interception
  - `initialize` modification - `TestBridge_InitializeInterception`
  - `output` event logging - `TestBridge_OutputEventCapture`
  - `runInTerminal` handling - `TestBridge_RunInTerminalInterception`
- [x] **9.4** Unit tests for process runner
  - Added in `process_runner_test.go`
- [x] **9.5** Integration tests for full bridge flow
  - `TestBridge_SuccessfulHandshake` - IDE connects via Unix socket
  - `TestBridge_FailedHandshake_WrongToken` - Handshake fails
  - `TestBridge_FailedHandshake_WrongSessionID` - Handshake fails
  - `TestBridge_HandshakeTimeout` - Timeout scenarios
  - `TestBridge_MessageForwarding` - DAP messages flow correctly
- [x] **9.6** Test output capture scenarios
  - `TestBridge_OutputEventCapture` - Without `runInTerminal`
  - `TestBridge_OutputEventNotCapturedWhenRunInTerminalUsed` - With `runInTerminal`

### Phase 10: Documentation and Cleanup
- [x] **10.1** Update package-level documentation in `internal/dap/`
  - Created `doc.go` with comprehensive package documentation
  - Describes both bridge (recommended) and legacy proxy (deprecated) architectures
- [x] **10.2** Update IDE execution specification reference
  - IDE-execution.md points to external spec (no local changes needed)
  - Debug bridge fields documented in `ideRunSessionRequestV1`
- [x] **10.3** Remove deprecated code paths
  - **Decision**: Kept with deprecation notices for backward compatibility
  - All deprecated types have clear `Deprecated:` comments
- [x] **10.4** Final verification with `make test` and `make lint`
  - Lint: 0 issues
  - Tests: Pass (some pre-existing flakiness in process timing tests)

---

## Design Notes

### Handshake Protocol

The handshake occurs immediately after the IDE connects to the Unix socket, before any DAP messages:

```
IDE connects to Unix socket
  ↓
IDE sends: {"token": "abc123", "session_id": "sess-456"}
  ↓
Bridge validates token + session_id
  ↓
Bridge responds: {"success": true} or {"success": false, "error": "..."}
  ↓
If success: DAP message flow begins
If failure: Connection closed
```

Messages use length-prefixed JSON (4-byte big-endian length prefix + JSON payload).

### Output Capture Strategy

| Scenario | stdout/stderr Source | Output Events |
|----------|---------------------|---------------|
| No `runInTerminal` | Captured from `output` events | Log + forward to IDE |
| With `runInTerminal` | Captured from process pipes | Ignore for logging, still forward to IDE |

### Socket Path Generation

Socket paths will be generated in the system temp directory with a pattern like:
```
/tmp/dcp-dap-{session-id}.sock
```

Permissions: owner read/write only (0600).

### Session Token Generation

Tokens will be cryptographically random strings (e.g., 32 bytes, base64 encoded) generated per debug session. The same token validation pattern used in the existing IDE protocol can be reused.

---

## File Structure (New/Modified)

```
internal/dap/
├── transport.go          # Add unixTransport, UnixSocketListener
├── bridge.go             # NEW: DapBridge main implementation
├── bridge_handshake.go   # NEW: Handshake protocol types and logic
├── bridge_interceptor.go # NEW: Message interception for bridge
├── bridge_session.go     # NEW: Session state management
├── process_runner.go     # NEW: Process launching for runInTerminal
├── dap_proxy.go          # Evaluate: simplify or keep for reuse
├── session_driver.go     # Evaluate: may be replaced by bridge
├── control_*.go          # Evaluate: may be deprecated
└── *_test.go             # Updated/new tests
```

---

## Migration Notes

The existing `Proxy`, `SessionDriver`, `ControlClient`, and `ControlServer` implementations may be:
1. **Reused** if they fit the new architecture with minimal changes
2. **Simplified** to remove unnecessary complexity
3. **Deprecated** if fully replaced by new bridge components

The decision will be made during implementation based on code inspection.
