/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build windows

package exerunners

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"

	apiv1 "github.com/microsoft/dcp/api/v1"
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

	exe := &apiv1.Executable{
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: `C:\Windows\System32\cmd.exe`,
			Terminal: &apiv1.TerminalSpec{
				Enabled: true,
				UDSPath: udsPath,
				Cols:    100,
				Rows:    30,
			},
		},
		Status: apiv1.ExecutableStatus{
			EffectiveArgs: []string{"/c", "echo hello-world-from-conpty && exit 0"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tp, err := startTerminalProcess(ctx, exe)
	if err != nil {
		t.Fatalf("startTerminalProcess: %v", err)
	}

	session, err := startTerminalSession(ctx, exe, tp, log)
	if err != nil {
		_ = tp.pty.Close()
		t.Fatalf("startTerminalSession: %v", err)
	}
	defer session.Close()

	// Give the listener a moment to actually bind. acceptLoop runs in a
	// goroutine; on Windows UDS a Dial too soon can race the Listen.
	if err := waitForUDS(udsPath, 2*time.Second); err != nil {
		t.Fatalf("waiting for UDS to appear: %v", err)
	}

	conn, err := net.Dial("unix", udsPath)
	if err != nil {
		t.Fatalf("dial UDS: %v", err)
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
			if err := json.Unmarshal(payload, &p); err != nil {
				t.Fatalf("Hello payload was not valid JSON (%v): %q", err, string(payload))
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

func waitForUDS(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("UDS %q did not appear within %s", path, timeout)
}
