/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

/*
Package dap provides Debug Adapter Protocol (DAP) infrastructure for debugging
executables managed by DCP.

# Architecture Overview

The package uses a bridge architecture to connect an IDE's debug adapter
client to a debug adapter launched by DCP. Communication occurs over Unix
domain sockets with a length-prefixed JSON handshake protocol.

# Key Components

  - DapBridge: Main bridge implementation that manages the connection lifecycle
  - BridgeManager: Manages active debug sessions, a shared Unix socket, and bridge lifecycle
  - DebugAdapterConfig: Configuration for launching debug adapters

# Connection Flow

 1. DCP registers a debug session with BridgeManager
 2. BridgeManager listens on a shared Unix socket
 3. Socket path and authentication token are sent to the IDE
 4. IDE connects to the socket and performs handshake
 5. BridgeManager launches the debug adapter via DapBridge
 6. Bridge forwards DAP messages bidirectionally with interception

The bridge intercepts:
  - initialize requests: Forces supportsRunInTerminalRequest=true
  - runInTerminal requests: Handles locally instead of forwarding to IDE
  - output events: Captures stdout/stderr when runInTerminal is not used

# Usage

For debug session implementations, use DapBridge:

	// Create and start the bridge manager
	manager := dap.NewBridgeManager(dap.BridgeManagerConfig{
		Logger: log,
	})

	// Register a session and start the manager
	session, _ := manager.RegisterSession(sessionID, token)
	err := manager.Start(ctx)

# Handshake Protocol

The IDE must perform a handshake immediately after connecting to the Unix socket.
The handshake uses length-prefixed JSON messages (4-byte big-endian length prefix):

	Request:  {"token": "...", "session_id": "..."}
	Response: {"success": true} or {"success": false, "error": "..."}

# Output Capture

Output is captured differently based on whether runInTerminal is used:
  - Without runInTerminal: Bridge captures from DAP output events
  - With runInTerminal: Bridge captures from process stdout/stderr pipes
*/
package dap
