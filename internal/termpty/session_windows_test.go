//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"

	"github.com/microsoft/dcp/internal/hmp1"
)

// TestTerminalSessionEndToEndWindows is the slice's headline integration test:
// it spawns a real cmd.exe process under ConPTY, listens for an HMP v1 client
// on a UDS, connects from the test as a fake terminal-host client, and asserts
// the full Hello -> StateSync -> Output -> Exit frame sequence is observed
// with sensible content.
func TestTerminalSessionEndToEndWindows(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end ConPTY test in short mode")
	}

	log := testr.New(t)

	udsPath := filepath.Join(t.TempDir(), "term.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Test plays the role of the terminal host: listen on the UDS first,
	// then start the session (which dials in), then accept the dialed
	// connection.
	lis, err := net.Listen("unix", udsPath)
	if err != nil {
		t.Fatalf("listen UDS: %v", err)
	}
	defer lis.Close()

	tp, err := StartProcess(ctx, CommandSpec{
		Cmd:  []string{`C:\Windows\System32\cmd.exe`, "/c", "echo hello-world-from-conpty && exit 0"},
		Cols: 100,
		Rows: 30,
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	session, err := StartSession(ctx, SessionConfig{
		UDSPath: udsPath,
		Cols:    100,
		Rows:    30,
	}, tp, log)
	if err != nil {
		_ = tp.PTY.Close()
		t.Fatalf("StartSession: %v", err)
	}
	defer session.Close()

	conn, err := lis.Accept()
	if err != nil {
		t.Fatalf("accept UDS: %v", err)
	}
	defer conn.Close()

	// Read frames until we see Exit (or hit a timeout). Collect all Output
	// payloads so we can assert on their concatenation.
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))

	var (
		sawHello     bool
		sawStateSync bool
		sawExit      bool
		exitCode     int32
		outputBuf    bytes.Buffer
	)

readLoop:
	for {
		frameType, payload, readErr := readClientFrame(conn)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break readLoop
			}
			t.Fatalf("read frame: %v", readErr)
		}
		switch frameType {
		case hmp1.FrameHello:
			sawHello = true
			var p hmp1.HelloPayload
			unmarshalErr := json.Unmarshal(payload, &p)
			if unmarshalErr != nil {
				t.Fatalf("Hello payload was not valid JSON (%v): %q", unmarshalErr, string(payload))
			}
			if p.Version != 1 {
				t.Errorf("Hello.Version = %d, want 1", p.Version)
			}
			if p.Width != 100 || p.Height != 30 {
				t.Errorf("Hello dimensions = %dx%d, want 100x30", p.Width, p.Height)
			}
		case hmp1.FrameStateSync:
			sawStateSync = true
		case hmp1.FrameOutput:
			outputBuf.Write(payload)
		case hmp1.FrameExit:
			sawExit = true
			if len(payload) != 4 {
				t.Errorf("Exit payload length = %d, want 4", len(payload))
			} else {
				exitCode = int32(binary.LittleEndian.Uint32(payload))
			}
			break readLoop
		default:
			t.Logf("ignoring unexpected frame type 0x%02x len=%d", frameType, len(payload))
		}
	}

	if !sawHello {
		t.Errorf("did not receive Hello frame")
	}
	if !sawStateSync {
		t.Errorf("did not receive StateSync frame")
	}
	if !sawExit {
		t.Errorf("did not receive Exit frame")
	}
	if exitCode != 0 {
		t.Errorf("exit code = %d, want 0", exitCode)
	}

	// ConPTY output includes ANSI escape sequences plus the echoed command
	// line (because ConPTY runs interactively). What we assert is that the
	// literal payload string appears somewhere in the rendered output.
	out := outputBuf.String()
	if !strings.Contains(out, "hello-world-from-conpty") {
		t.Errorf("expected Output to contain %q, got %q", "hello-world-from-conpty", out)
	}

	// Wait for the session to finish tearing itself down.
	select {
	case <-session.Done():
	case <-time.After(5 * time.Second):
		t.Errorf("timed out waiting for terminal session to finish")
	}
}

// readClientFrame reads a single HMP v1 frame from the client side of the
// connection (mirroring the server's readFrame helper but kept local to the
// test to avoid exporting internal helpers).
func readClientFrame(r net.Conn) (hmp1.FrameType, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	t := hmp1.FrameType(header[0])
	length := binary.LittleEndian.Uint32(header[1:5])
	if length > hmp1.MaxPayloadLength {
		return 0, nil, fmt.Errorf("frame length %d exceeds max %d", length, hmp1.MaxPayloadLength)
	}
	if length == 0 {
		return t, nil, nil
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return t, payload, nil
}
