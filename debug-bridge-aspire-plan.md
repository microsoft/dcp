# Implement 2026-02-01 Debug Bridge Protocol in dotnet/aspire

## TL;DR

DCP now supports a "debug bridge" mode (protocol version `2026-02-01`) where it launches debug adapters and proxies DAP messages through a Unix domain socket. Instead of VS Code launching its own debug adapter process, it connects to DCP's bridge socket, tells DCP which adapter to launch (via a length-prefixed JSON handshake), and then communicates DAP messages through that same socket. This requires changes to the IDE execution spec, the VS Code extension's session endpoint, debug adapter descriptor factory, and protocol capabilities.

Currently, `protocols_supported` tops out at `"2025-10-01"`. No `2026-02-01` or `debug_bridge` references exist anywhere in the aspire repo.

---

## Architecture

```
┌──────────────────────────────────────────────────────────────────────────┐
│ IDE (VS Code)                                                            │
│  └─ Debug Adapter Client                                                 │
│      └─ Connects to Unix socket provided by DCP in run session response  │
└──────────────────────────────────┬───────────────────────────────────────┘
                                   │ DAP messages (Unix socket)
                                   │ + Initial handshake (token + session ID + run ID + adapter config)
                                   ▼
┌──────────────────────────────────────────────────────────────────────────┐
│ DCP DAP Bridge (BridgeManager + DapBridge)                               │
│  ├─ SecureSocketListener for IDE connections                             │
│  ├─ Handshake validation (session ID + token)                            │
│  ├─ Sequence number remapping (IDE ↔ Adapter seq isolation)              │
│  ├─ RawMessage forwarding (transparent proxy for unknown DAP messages)   │
│  ├─ Interception layer:                                                  │
│  │    ├─ initialize: ensure supportsRunInTerminalRequest = true          │
│  │    ├─ runInTerminal: handle locally, launch process, capture stdio    │
│  │    └─ output events: capture for logging (unless runInTerminal used)  │
│  ├─ Inline runInTerminal handling (exec.Command via process.Executor)    │
│  └─ Output routing (BridgeConnectionHandler → OutputHandler + writers)   │
└──────────────────────────────────┬───────────────────────────────────────┘
                                   │ DAP messages (stdio/TCP)
                                   ▼
┌──────────────────────────────────────────────────────────────────────────┐
│ Debug Adapter (launched by DCP)                                          │
│  └─ coreclr, debugpy, etc.                                               │
└──────────────────────────────────────────────────────────────────────────┘
```

### How It Differs from the Current Flow

| Aspect | Current (no bridge) | New (bridge mode, 2026-02-01+) |
|--------|---------------------|-------------------------------|
| Who launches the debug adapter | VS Code (via `vscode.debug.startDebugging`) | DCP (via bridge, using config from IDE) |
| DAP transport | VS Code manages directly | Unix socket through DCP bridge |
| `runInTerminal` handling | VS Code handles | DCP handles locally (IDE never sees it) |
| stdout/stderr capture | Adapter tracker sends `serviceLogs` | DCP captures from process pipes or output events |
| IDE role | Full debug orchestrator | DAP client connected through bridge socket |

---

## Step-by-Step Implementation

### Step 1: Update the IDE Execution Spec

**File:** `docs/specs/IDE-execution.md`

Add the `2026-02-01` protocol version under **Protocol Versioning → Well-known protocol versions**:

> **`2026-02-01`**
> Changes:
> - Adds debug bridge support. When this version (or later) is negotiated, the `PUT /run_session` payload may include `debug_bridge_socket_path` and `debug_session_id` fields.

Add the two new fields to the **Create Session Request** payload documentation:

| Property | Description | Type |
|----------|-------------|------|
| `debug_bridge_socket_path` | Unix domain socket path that the IDE should connect to for DAP bridging. Present only when API version ≥ `2026-02-01`. | `string` (optional) |
| `debug_session_id` | A unique session identifier the IDE must include in the debug bridge handshake. | `string` (optional) |

Add a new section **"Debug Bridge Protocol"** describing the full protocol (see [Appendix A](#appendix-a-debug-bridge-protocol-specification) below for the complete spec text).

---

### Step 2: Update Protocol Capabilities

**File:** `extension/src/capabilities.ts` (~line 55)

Add `"2026-02-01"` to the `protocols_supported` array:

```ts
export function getRunSessionInfo(): RunSessionInfo {
    return {
        protocols_supported: ["2024-03-03", "2024-04-23", "2025-10-01", "2026-02-01"],
        supported_launch_configurations: getSupportedCapabilities()
    };
}
```

---

### Step 3: Update TypeScript Types

**File:** `extension/src/dcp/types.ts`

Add the new fields to the run session payload type, and add new types for the handshake:

```ts
// Add to existing RunSessionPayload (or equivalent) interface:
debug_bridge_socket_path?: string;
debug_session_id?: string;

// New types for the bridge protocol:
export interface DebugAdapterConfig {
    args: string[];
    mode?: "stdio" | "tcp-callback" | "tcp-connect";
    env?: Array<{ name: string; value: string }>;
    connectionTimeoutSeconds?: number;
}

export interface DebugBridgeHandshakeRequest {
    token: string;
    session_id: string;
    run_id?: string;
    debug_adapter_config: DebugAdapterConfig;
}

export interface DebugBridgeHandshakeResponse {
    success: boolean;
    error?: string;
}
```

---

### Step 4: Create a Debug Bridge Client Module

**New file:** `extension/src/debugger/debugBridgeClient.ts`

Implement the IDE side of the bridge connection:

```ts
export async function connectToDebugBridge(
    socketPath: string,
    token: string,
    sessionId: string,
    runId: string,
    adapterConfig: DebugAdapterConfig
): Promise<net.Socket>
```

This function should:

1. Connect to the Unix domain socket at `socketPath` using `net.connect({ path: socketPath })`
2. Send the handshake request as **length-prefixed JSON**:
   - Write a 4-byte big-endian `uint32` containing the JSON payload length
   - Write the UTF-8 encoded JSON bytes of `DebugBridgeHandshakeRequest` (including `run_id` for output routing)
3. Read the handshake response:
   - Read 4 bytes → big-endian `uint32` length
   - Read that many bytes → parse as `DebugBridgeHandshakeResponse`
4. If `success === true`, return the connected socket
5. If `success === false`, throw an error with the `error` message

**Important constraints:**
- Max handshake message size: **64 KB** (65536 bytes)
- Handshake timeout: **30 seconds** (DCP closes the connection if the handshake isn't received in time)

---

### Step 5: Map Launch Configuration Types to Debug Adapter Configs

The `debug_adapter_config` in the handshake tells DCP what debug adapter binary to launch. The IDE must determine this from the launch configuration type.

The mapping information already exists in `extension/src/debugger/debuggerExtensions.ts` and the language-specific files:

| Launch Config Type | Debug Adapter | Source Extension |
|-------------------|---------------|-----------------|
| `project` | `coreclr` | `ms-dotnettools.csharp` |
| `python` | `debugpy` | `ms-python.python` |

Add a method to `ResourceDebuggerExtension` (or a standalone utility) that returns a `DebugAdapterConfig`:

```ts
export interface ResourceDebuggerExtension {
    // ... existing fields ...
    getDebugAdapterConfig?(launchConfig: LaunchConfiguration): DebugAdapterConfig;
}
```

For each resource type:
- **`project` / `coreclr`**: Resolve the path to the C# debug adapter executable from the `ms-dotnettools.csharp` extension. Set `mode: "stdio"`. The `args` array should be the command line to launch the adapter (e.g., `["/path/to/Microsoft.CodeAnalysis.LanguageServer", "--debug"]` or whatever the coreclr adapter binary is).
- **`python` / `debugpy`**: Resolve the path to the debugpy adapter. Set `mode: "stdio"` or `"tcp-connect"` as appropriate. For `"tcp-connect"`, use `{{port}}` as a placeholder in `args` — DCP will replace it with an actual port number.

This is the **key integration point** — the extension needs to locate the actual debug adapter binary that would normally be launched by VS Code's built-in debug infrastructure and package it as an `args` array for the handshake.

---

### Step 6: Update `PUT /run_session` Handler

**File:** `extension/src/dcp/AspireDcpServer.ts` (~lines 84-120)

Modify the `PUT /run_session` handler:

```
Parse request body
  ↓
Extract debug_bridge_socket_path and debug_session_id
  ↓
┌─ If BOTH fields are present (bridge mode):
│   1. Resolve DebugAdapterConfig for the launch configuration type (Step 5)
│   2. Call connectToDebugBridge() with socket path, bearer token, session ID, adapter config
│   3. Get back the connected net.Socket
│   4. Create a DebugBridgeAdapter wrapping the socket (Step 7)
│   5. Start a VS Code debug session using this adapter
│   6. Respond 201 Created + Location header
│
└─ If fields are ABSENT (legacy mode):
    Follow existing flow unchanged
```

---

### Step 7: Create a Bridge Debug Adapter

**New file:** `extension/src/debugger/debugBridgeAdapter.ts`

Create a custom `vscode.DebugAdapter` that proxies DAP messages to/from the connected Unix socket:

```ts
export class DebugBridgeAdapter implements vscode.DebugAdapter {
    private sendMessage: vscode.EventEmitter<vscode.DebugProtocolMessage>;
    onDidSendMessage: vscode.Event<vscode.DebugProtocolMessage>;

    constructor(private socket: net.Socket) { ... }

    // Called by VS Code when it wants to send a DAP message to the adapter
    handleMessage(message: vscode.DebugProtocolMessage): void {
        // Write as DAP-framed message (Content-Length header + JSON) to the socket
    }

    // Read DAP-framed messages from the socket and emit via onDidSendMessage

    dispose(): void {
        // Close the socket
    }
}
```

**Why not `DebugAdapterNamedPipeServer`?** The handshake must complete before DAP messages flow. `DebugAdapterNamedPipeServer` would try to send DAP messages immediately on connect, bypassing the handshake. The inline adapter approach gives full control over the connection lifecycle.

Then update `AspireDebugAdapterDescriptorFactory` to return a `DebugAdapterInlineImplementation` wrapping this adapter for bridge sessions:

```ts
return new vscode.DebugAdapterInlineImplementation(new DebugBridgeAdapter(connectedSocket));
```

---

### Step 8: Update Debug Session Lifecycle

**File:** `extension/src/debugger/AspireDebugSession.ts`

For bridge-mode sessions:
- The `launch` request handler should **not** spawn `aspire run --start-debug-session` (DCP already manages the process)
- Track whether this is a bridge session (e.g., via a flag or session metadata)
- On `disconnect`/`terminate`, close the bridge socket connection
- Teardown should notify DCP via the existing WebSocket notification path (`sessionTerminated`)

---

### Step 9: Update Adapter Tracker for Bridge Sessions

**File:** `extension/src/debugger/adapterTracker.ts`

For bridge sessions:
- DCP captures stdout/stderr directly from the debug adapter's output events and from `runInTerminal` process pipes — the tracker should **not** send duplicate `serviceLogs` notifications for output that DCP is already capturing
- The tracker should still send `processRestarted` and `sessionTerminated` notifications
- Consider skipping tracker registration entirely for bridge sessions, or adding a bridge-mode flag that suppresses log forwarding

---

### Step 10: Update C# Models (if needed)

**Files in:** `src/Aspire.Hosting/Dcp/Model/`

If the app host or dashboard reads the run session payload structure, update any C# deserialization models to include the new optional fields for forward compatibility. Check:
- `RunSessionInfo.cs`
- Any request/response models that mirror the run session payload

This may not be strictly necessary if the C# side doesn't interact with these fields — DCP adds them server-side. But it's good practice for model completeness.

---

## Error Reporting

### Current State (Implemented in DCP)

The DCP bridge now sends meaningful DAP error information to the IDE when errors occur after the handshake. The implementation uses `OutputEvent` (category: `"stderr"`) followed by `TerminatedEvent` to communicate errors through the standard DAP protocol.

### Error Scenarios and Behavior

| Scenario | What IDE Sees |
|----------|---------------|
| Handshake failure (bad token, invalid session, missing config) | Handshake error JSON response — handled cleanly |
| Handshake read failure (malformed data, timeout) | Raw connection drop — no DAP-level error possible (pre-handshake) |
| Debug adapter fails to launch (bad command, missing binary) | `OutputEvent` (stderr) with error text + `TerminatedEvent` |
| Adapter connection timeout (TCP modes) | `OutputEvent` (stderr) with error text + `TerminatedEvent` |
| Adapter crashes before sending `TerminatedEvent` | Synthesized `TerminatedEvent` (with optional `OutputEvent` if transport error) |
| Transport read/write failure mid-session | `OutputEvent` (stderr) + synthesized `TerminatedEvent` |

### DCP Implementation Details

#### 1. DAP message helpers in `internal/dap/message.go`

Unexported helper functions synthesize DAP messages for error reporting:

```go
// newOutputEvent creates an OutputEvent for sending error/info text to the IDE.
func newOutputEvent(seq int, category, output string) *dap.OutputEvent

// newTerminatedEvent creates a TerminatedEvent to signal session end.
func newTerminatedEvent(seq int) *dap.TerminatedEvent
```

Note: `NewErrorResponse` was considered but not implemented — `OutputEvent` + `TerminatedEvent` is sufficient for all error scenarios.

#### 2. Error delivery via `sendErrorToIDE()` in `bridge.go`

When errors occur after the IDE transport is established, `sendErrorToIDE()` sends an `OutputEvent` with `category: "stderr"` followed by a `TerminatedEvent`. Sequence numbers for bridge-originated messages use `b.ideSeqCounter` (an atomic counter separate from the IDE's own sequence numbers):

```go
func (b *DapBridge) sendErrorToIDE(message string) {
    outputEvent := newOutputEvent(int(b.ideSeqCounter.Add(1)), "stderr", message+"\n")
    b.ideTransport.WriteMessage(outputEvent)
    b.sendTerminatedToIDE()
}
```

#### 3. Adapter launch failure

When `launchAdapterWithConfig` fails, `sendErrorToIDE()` is called before returning the error:

```go
launchErr := b.launchAdapterWithConfig(ctx, adapterConfig)
if launchErr != nil {
    b.sendErrorToIDE(fmt.Sprintf("Failed to launch debug adapter: %v", launchErr))
    return fmt.Errorf("failed to launch debug adapter: %w", launchErr)
}
```

#### 4. Unexpected adapter exit

When `<-b.adapter.Done()` fires and the adapter did NOT send a `TerminatedEvent` (tracked via `terminatedEventSeen` flag), the bridge synthesizes one. If the exit was due to a transport error (as opposed to clean EOF/cancellation), an `OutputEvent` with the error text is sent first.

#### 5. Transport failures

When read/write errors occur in the message loop, the bridge attempts to send an `OutputEvent` describing the failure before closing the connection.

### Required Changes — IDE/Aspire Side

#### 1. Handle handshake failures in `debugBridgeClient.ts`

When `connectToDebugBridge()` receives `{"success": false, "error": "..."}`, throw an error that includes the error message. The VS Code extension should surface this to the user via:
- A `vscode.window.showErrorMessage()` call with the error text
- A `sessionMessage` notification (level: `error`) sent to DCP via the WebSocket notification stream
- Clean termination of the debug session

#### 2. Handle DAP error events in `DebugBridgeAdapter`

The `DebugBridgeAdapter` (Step 7 in the main plan) should watch for `OutputEvent` messages with `category: "stderr"` that arrive before the first `InitializeResponse`. These indicate adapter launch errors from DCP. The adapter should:
- Forward them to VS Code (which will display them in the Debug Console)
- If followed by a `TerminatedEvent`, terminate the session cleanly

#### 3. Handle unexpected connection drops

If the Unix socket closes unexpectedly (without a `TerminatedEvent` or `DisconnectResponse`), the `DebugBridgeAdapter` should:
- Fire a `TerminatedEvent` to VS Code so the debug session ends cleanly
- Optionally display an error message indicating the debug bridge connection was lost

---

## Key Decisions

| Decision | Rationale |
|----------|-----------|
| **Inline adapter over named pipe descriptor** | The handshake must complete before DAP messages flow, so we need a `DebugAdapterInlineImplementation` wrapping a custom adapter that manages the socket lifecycle |
| **Token reuse** | The same bearer token used for HTTP authentication (`DEBUG_SESSION_TOKEN`) is reused as the bridge handshake token — no new credential needed |
| **IDE decides adapter** | DCP does NOT tell the IDE which adapter to use; the IDE determines this from the launch configuration type and sends the adapter binary path + args back in the handshake's `debug_adapter_config` |
| **Backward compatible** | When `debug_bridge_socket_path` is absent from the run session request, the existing non-bridge flow is used unchanged |
| **DAP-level error reporting** | DCP sends `OutputEvent` (category: stderr) + `TerminatedEvent` to the IDE when errors occur after handshake, so the IDE can display meaningful errors instead of a silent connection drop |
| **Single `BridgeManager`** | Session management, socket listening, and bridge lifecycle are combined into one `BridgeManager` type rather than separate `BridgeSessionManager` and `BridgeSocketManager` — simpler lifecycle management with a single mutex |
| **Sequence number remapping** | Bridge-assigned seq numbers prevent collisions between IDE-originated and bridge-originated (e.g., `runInTerminal` response) messages; a `seqMap` restores original seq values on responses |
| **`RawMessage` fallback** | Unknown/proprietary DAP messages that the `go-dap` library can't decode are wrapped in `RawMessage` and forwarded transparently, enabling support for custom debug adapter extensions |
| **`SecureSocketListener`** | Uses the project's `networking.SecureSocketListener` instead of a plain Unix domain socket for enhanced security |
| **Environment filtering on adapter launch** | Adapter processes inherit the DCP environment but with `DEBUG_SESSION*` and `DCP_*` variables removed, preventing credential leakage to debug adapters |

---

## Verification

1. **Unit tests**: Test `connectToDebugBridge()` with a mock Unix socket server that validates the length-prefixed JSON format, token, and session ID
2. **Integration test**: Start a DCP instance with debug bridge enabled, verify the extension:
   - Reports `"2026-02-01"` in `protocols_supported`
   - Connects to the bridge socket when `debug_bridge_socket_path` is in the run request
   - Sends a valid handshake with correct adapter config
   - Successfully forwards DAP messages through the bridge
3. **Error scenario tests**:
   - Handshake failure (bad token) → extension shows meaningful error, session terminates cleanly
   - Adapter launch failure (bad binary path) → extension receives `OutputEvent` with error text and `TerminatedEvent`, session terminates cleanly
   - Unexpected connection drop → extension fires synthetic `TerminatedEvent` to VS Code, session ends without hang
4. **Regression**: Ensure the existing (non-bridge) flow still works when DCP negotiates an older API version
5. **Manual test**: Debug a .NET Aspire app with the updated extension and verify breakpoints, stepping, variable inspection all work through the bridge

---

## DCP Implementation Details

These sections document key aspects of the DCP-side implementation that the IDE extension should be aware of.

### Sequence Number Remapping

The bridge remaps DAP sequence numbers to prevent collisions between IDE-originated messages and bridge-originated messages (such as `RunInTerminalResponse` or synthesized error events). This is implemented in `bridge.go` with three components:

- **`adapterSeqCounter`** (atomic `int64`): Generates monotonically increasing `seq` numbers for all messages sent to the adapter. When forwarding an IDE message, the bridge replaces `seq` with a bridge-assigned value and records the mapping.
- **`ideSeqCounter`** (atomic `int64`): Generates `seq` numbers for bridge-originated messages sent to the IDE (synthesized `OutputEvent`, `TerminatedEvent`, `RunInTerminalResponse`).
- **`seqMap`** (`syncmap.Map[int, int]`): Maps bridge-assigned seq numbers → original IDE seq numbers. When a response comes back from the adapter, the bridge looks up the `request_seq` in this map and restores the original IDE seq value before forwarding.

This is transparent to the IDE — the IDE sees its own seq numbers on all responses.

### RawMessage and MessageEnvelope

`message.go` contains two key types that enable transparent proxying of all DAP messages, including those unknown to the `go-dap` library:

**`RawMessage`**: Wraps the raw JSON bytes of a DAP message that couldn't be decoded by `go-dap`. This enables the bridge to transparently forward proprietary/custom DAP messages (e.g., custom commands from language-specific debug adapters) without needing to understand their schema.

**`MessageEnvelope`**: A wrapper that provides uniform access to DAP header fields (`seq`, `type`, `request_seq`, `command`, `event`) across both typed `go-dap` messages and `RawMessage` instances. It supports:
- Lazy extraction of header fields at creation time
- Free modification of `Seq`, `RequestSeq`, etc.
- `Finalize()` to apply changes back — zero-cost for typed messages, single JSON field patch for raw messages

The bridge uses `ReadMessageWithFallback` / `WriteMessageWithFallback` instead of the standard `go-dap` reader/writer. These functions attempt standard decoding first, falling back to `RawMessage` for unrecognized message types.

### Output Routing

When a bridge connection is established, `BridgeManager` invokes a `BridgeConnectionHandler` callback to resolve output routing:

```go
type BridgeConnectionHandler func(sessionID string, runID string) (OutputHandler, io.Writer, io.Writer)
```

This returns:
- An `OutputHandler` interface (`HandleOutput(category, output string)`) for routing DAP `OutputEvent` messages
- `io.Writer` instances for stdout and stderr (used as sinks for `runInTerminal` process pipes)

The `run_id` field in the handshake request is what connects the bridge session to the correct executable's output files. In `internal/exerunners/`, the `bridgeOutputHandler` implementation routes:
- `"stdout"` and `"console"` category events → stdout writer
- `"stderr"` category events → stderr writer
- Other categories → silently discarded

Output routing only captures via `OutputHandler` when `runInTerminal` was NOT used (tracked by `runInTerminalUsed` flag). When `runInTerminal` launches a process, DCP captures stdout/stderr directly from the process pipes, avoiding double-capture.

### BridgeManager Lifecycle

`BridgeManager` is the single orchestrator for all bridge sessions. It combines session registration, socket management, and bridge lifecycle:

1. **Creation**: `NewBridgeManager(BridgeManagerConfig{Logger, ConnectionHandler})` — requires a `BridgeConnectionHandler` callback
2. **Start**: `Start(ctx)` creates a `SecureSocketListener`, signals readiness via `Ready()` channel, then enters an accept loop
3. **Session registration**: `RegisterSession(sessionID, token)` creates a `BridgeSession` in `BridgeSessionStateCreated` state. Session ID is typically `string(exe.UID)`.
4. **Connection handling**: Each accepted connection goes through handshake, validation, `markSessionConnected()`, then `runBridge()`. If anything fails between marking connected and running, `markSessionDisconnected()` rolls back to allow retry.
5. **Bridge construction**: Creates a `DapBridge` via `NewDapBridge(BridgeConfig{...})` where `BridgeConfig` includes `SessionID`, `AdapterConfig`, `Executor`, `Logger`, `OutputHandler`, `StdoutWriter`, `StderrWriter`
6. **Termination**: Session moves to `BridgeSessionStateTerminated` (success) or `BridgeSessionStateError` (failure) when the bridge's `RunWithConnection` returns

### DapBridge Lifecycle

The `DapBridge` handles a single debug session's message forwarding:

1. `RunWithConnection(ctx, ideConn)` creates an IDE transport and calls `launchAdapterWithConfig`
2. On adapter launch failure: `sendErrorToIDE()` → return error
3. On success: enters `runMessageLoop(ctx)`
4. Message loop starts two goroutines (`forwardIDEToAdapter`, `forwardAdapterToIDE`) and watches for adapter process exit via `<-b.adapter.Done()`
5. On adapter exit without `TerminatedEvent`: synthesizes one (optionally preceded by an error `OutputEvent`)
6. Cleanup: closes both transports, waits for goroutines, collects errors

### Adapter Launch Environment Filtering

When launching a debug adapter process, `buildFilteredEnv()` in `adapter_launcher.go`:
1. Inherits the DCP process's full environment
2. Removes variables with `DEBUG_SESSION` or `DCP_` prefixes (case-insensitive on Windows)
3. Applies any environment variables specified in the `DebugAdapterConfig.Env` array on top

Additionally, all adapter modes capture the adapter's stderr via a pipe and log it for diagnostic purposes.

---

## Appendix A: Debug Bridge Protocol Specification

### Overview

When API version `2026-02-01` or later is negotiated, DCP may include debug bridge fields in the `PUT /run_session` request. When present, the IDE should connect to the provided Unix domain socket and use DCP as a DAP bridge instead of launching its own debug adapter.

### Connection Flow

1. IDE receives `PUT /run_session` with `debug_bridge_socket_path` and `debug_session_id`
2. IDE responds `201 Created` with `Location` header (as normal)
3. IDE connects to the Unix domain socket at `debug_bridge_socket_path`
4. IDE sends a handshake request (length-prefixed JSON)
5. DCP validates and responds with a handshake response
6. On success, standard DAP messages flow over the same socket connection
7. DCP launches the debug adapter specified in the handshake and bridges messages bidirectionally

### Handshake Wire Format

All handshake messages use **length-prefixed JSON**:
```
[4 bytes: big-endian uint32 payload length][JSON payload bytes]
```

Maximum message size: **65536 bytes** (64 KB).

### Handshake Request (IDE → DCP)

```json
{
    "token": "<same bearer token used for HTTP auth (DEBUG_SESSION_TOKEN)>",
    "session_id": "<debug_session_id from the run session request>",
    "run_id": "<optional: correlates with executable's output writers for log routing>",
    "debug_adapter_config": {
        "args": ["/path/to/debug-adapter", "--arg1", "value1"],
        "mode": "stdio",
        "env": [
            { "name": "VAR_NAME", "value": "var_value" }
        ],
        "connectionTimeoutSeconds": 10
    }
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `token` | `string` | Yes | The same bearer token used for HTTP authentication |
| `session_id` | `string` | Yes | The `debug_session_id` from the run session request |
| `run_id` | `string` | No | Correlates the bridge session with the executable's output writers for log routing |
| `debug_adapter_config` | `object` | Yes | Configuration for launching the debug adapter |
| `debug_adapter_config.args` | `string[]` | Yes | Command + arguments to launch the adapter. First element is the executable path. |
| `debug_adapter_config.mode` | `string` | No | `"stdio"` (default), `"tcp-callback"`, or `"tcp-connect"` |
| `debug_adapter_config.env` | `array` | No | Environment variables as `[{"name":"N","value":"V"}]` (uses `apiv1.EnvVar` type on DCP side) |
| `debug_adapter_config.connectionTimeoutSeconds` | `number` | No | Timeout for TCP connections (default: 10 seconds) |

### Debug Adapter Modes

| Mode | Description |
|------|-------------|
| `stdio` (default) | DCP launches the adapter and communicates via stdin/stdout |
| `tcp-callback` | DCP starts a TCP listener, substitutes `{{port}}` in `args` with the listener port, then launches the adapter. The adapter connects back to DCP on that port. |
| `tcp-connect` | DCP allocates a port, replaces `{{port}}` placeholder in `args`, launches the adapter (which listens on that port), then DCP connects to it. |

### Handshake Response (DCP → IDE)

Success:
```json
{
    "success": true
}
```

Failure:
```json
{
    "success": false,
    "error": "error description"
}
```

### Handshake Validation

DCP validates the handshake in this order:
1. Session ID exists → otherwise `"bridge session not found"` (`ErrBridgeSessionNotFound`)
2. Token matches the registered session token → otherwise `"invalid session token"` (`ErrBridgeSessionInvalidToken`)
3. `debug_adapter_config` is present → otherwise `"debug adapter configuration is required"`
4. Session not already connected → otherwise `"session already connected"` (`ErrBridgeSessionAlreadyConnected`) (only one IDE connection per session allowed)

If connection fails after marking the session as connected (between step 4 and running the bridge), the connected state is rolled back via `markSessionDisconnected()` so the session can be retried.

### Timeouts

| Timeout | Duration | Description |
|---------|----------|-------------|
| Handshake | 30 seconds | DCP closes the connection if the handshake request isn't received within this time |
| Adapter connection (TCP modes) | 10 seconds (configurable) | Time to establish TCP connection to/from adapter |

### DAP Message Flow After Handshake

After a successful handshake, standard DAP messages flow over the Unix socket using the standard DAP wire format (`Content-Length: N\r\n\r\n{JSON}`).

DCP intercepts the following messages:
- **`initialize` request** (IDE → Adapter): DCP forces `supportsRunInTerminalRequest = true` in the arguments before forwarding
- **`runInTerminal` reverse request** (Adapter → IDE): DCP handles this locally by launching the process. The IDE will **never** receive `runInTerminal` requests.
- **`output` events** (Adapter → IDE): DCP captures these for logging purposes, then forwards to the IDE

All other DAP messages are forwarded transparently in both directions.

### Output Capture

| Scenario | stdout/stderr source | Output events |
|----------|---------------------|---------------|
| No `runInTerminal` | Captured from DAP `output` events | Logged by DCP + forwarded to IDE |
| With `runInTerminal` | Captured from process pipes by DCP | Forwarded to IDE (not logged from events) |

---

## Appendix B: Relevant DCP Source Files

These files in the `microsoft/dcp` repo implement the DCP side of the bridge protocol, for reference:

### `internal/dap/` — Core Bridge Package

| File | Purpose |
|------|---------|
| `internal/dap/doc.go` | Package-level documentation |
| `internal/dap/bridge.go` | Core `DapBridge` — bidirectional message forwarding with interception, sequence number remapping, inline `runInTerminal` handling (`handleRunInTerminalRequest`), and error reporting via `sendErrorToIDE()` |
| `internal/dap/bridge_handshake.go` | Length-prefixed JSON handshake protocol: `HandshakeRequest`/`HandshakeResponse` types, `HandshakeReader`/`HandshakeWriter`, `performClientHandshake()` convenience function, `maxHandshakeMessageSize` (64 KB) constant |
| `internal/dap/bridge_manager.go` | `BridgeManager` — combined session management, `SecureSocketListener` socket lifecycle, handshake processing, and bridge lifecycle. Contains `BridgeSession` with states (`Created`, `Connected`, `Terminated`, `Error`), session registration/rollback, and `BridgeConnectionHandler` callback type |
| `internal/dap/adapter_types.go` | `DebugAdapterConfig` struct (args, mode, env as `[]apiv1.EnvVar`, connectionTimeoutSeconds) and `DebugAdapterMode` constants (`stdio`, `tcp-callback`, `tcp-connect`) |
| `internal/dap/adapter_launcher.go` | `LaunchDebugAdapter()` — starts adapter processes in all 3 modes, environment filtering (`buildFilteredEnv()` removes `DEBUG_SESSION*`/`DCP_*` variables), adapter stderr capture via pipe, `LaunchedAdapter` struct with transport + process handle + done channel |
| `internal/dap/transport.go` | `Transport` interface with a single `connTransport` backing implementation shared by three factory functions: `NewTCPTransportWithContext`, `NewStdioTransportWithContext`, `NewUnixTransportWithContext`. Uses `dcpio.NewContextReader` for cancellation-aware reads |
| `internal/dap/message.go` | `RawMessage` (transparent forwarding of unknown/proprietary DAP messages), `MessageEnvelope` (uniform header access with lazy seq patching), `ReadMessageWithFallback`/`WriteMessageWithFallback`, unexported helpers `newOutputEvent`/`newTerminatedEvent` |

### `internal/exerunners/` — Integration Points

| File | Purpose |
|------|---------|
| `internal/exerunners/ide_executable_runner.go` | Integration point — creates `BridgeManager` when `SupportsDebugBridge()`, registers bridge sessions using `exe.UID` as session ID, includes `debug_bridge_socket_path` and `debug_session_id` in run session requests |
| `internal/exerunners/ide_requests_responses.go` | Protocol types, API version definitions (`version20260201 = "2026-02-01"`), `ideRunSessionRequestV1` with bridge fields (`DebugBridgeSocketPath`, `DebugSessionID`) |
| `internal/exerunners/ide_connection_info.go` | Version negotiation, `SupportsDebugBridge()` helper (checks `>= version20260201`) |
| `internal/exerunners/bridge_output_handler.go` | `bridgeOutputHandler` implementing `dap.OutputHandler` — routes DAP output events by category (`"stdout"`/`"console"` → stdout writer, `"stderr"` → stderr writer) |
