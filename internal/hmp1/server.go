/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package hmp1 implements the server side of the Hex1b Multiplex Protocol
// version 1 (HMP v1) that DCP uses to expose pseudo-terminal output to the
// Aspire terminal host. The wire format is intentionally minimal:
//
//	[type:1B][length:4B little-endian][payload:length bytes]
//
// Frame types (the producer / "server" side that DCP plays):
//
//	0x01 Hello       JSON: {"version":1,"width":W,"height":H}     S->C
//	0x02 StateSync   raw ANSI bytes (current screen)              S->C
//	0x03 Output      raw ANSI bytes from PTY                      S->C
//	0x04 Input       raw bytes to deliver to PTY stdin            C->S
//	0x05 Resize      [width:4B LE][height:4B LE]                  bidirectional
//	0x06 Exit        [exitCode:4B LE]                             S->C (then close)
//
// The protocol is a strict superset of what the Hex1b client needs to render
// a terminal session and forward keystrokes/resize. Any frames the server
// receives outside of {Input, Resize} are logged at debug level and dropped.
package hmp1

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// Maximum payload length (16 MiB). Matches the Hex1b implementation.
const MaxPayloadLength = 16 * 1024 * 1024

// FrameType is the 1-byte frame discriminator at the head of every HMP v1 frame.
type FrameType byte

const (
	FrameHello     FrameType = 0x01
	FrameStateSync FrameType = 0x02
	FrameOutput    FrameType = 0x03
	FrameInput     FrameType = 0x04
	FrameResize    FrameType = 0x05
	FrameExit      FrameType = 0x06
)

// HelloPayload is the JSON payload of a Hello frame.
type HelloPayload struct {
	Version int `json:"version"`
	Width   int `json:"width"`
	Height  int `json:"height"`
}

// PTY is the abstraction the server needs over the underlying pseudo-terminal.
// Implementations must be safe for concurrent use by separate goroutines for
// Read, Write, Resize, and Wait (each call site uses a single goroutine).
type PTY interface {
	// Read reads bytes from the PTY's master side (i.e. process stdout/stderr).
	// Should return io.EOF when the process exits and the PTY is closed.
	Read(p []byte) (int, error)

	// Write writes bytes to the PTY's master side (i.e. process stdin).
	Write(p []byte) (int, error)

	// Resize updates the PTY window size.
	Resize(cols, rows int) error

	// Close closes the PTY master.
	Close() error
}

// ServerOptions configures a Server invocation.
type ServerOptions struct {
	// InitialCols / InitialRows are the dimensions sent in the Hello frame.
	// Reasonable defaults are applied if either is zero.
	InitialCols int
	InitialRows int

	// Log receives diagnostic messages from the server. May be the zero
	// logr.Logger to discard.
	Log logr.Logger
}

// Serve runs an HMP v1 server on the given connection, bridging conn <-> pty
// until either the PTY exits, the connection closes, or the context is
// cancelled. The exitCode reported back to the client (via the Exit frame) is
// taken from waitExit; it is invoked exactly once, after the PTY's read loop
// returns, and may block until the underlying process exits. If waitExit is
// nil the Exit frame is sent with exit code 0.
//
// Serve takes ownership of conn for the duration of the call: it will close
// conn before returning, both to release any goroutine blocked on conn.Read
// and to signal the viewer that the session is over. It does NOT close pty;
// the caller owns the PTY lifetime so it can be reused across multiple Serve
// invocations (reconnects).
func Serve(ctx context.Context, conn net.Conn, pty PTY, waitExit func() int32, opts ServerOptions) error {
	defer conn.Close()
	cols := opts.InitialCols
	if cols <= 0 {
		cols = 80
	}
	rows := opts.InitialRows
	if rows <= 0 {
		rows = 24
	}

	helloBytes, err := json.Marshal(HelloPayload{Version: 1, Width: cols, Height: rows})
	if err != nil {
		return fmt.Errorf("marshal Hello payload: %w", err)
	}

	// Serialize frame writes; both Output and Exit are written from different
	// goroutines (the read pump and the exit watcher).
	var writeMu sync.Mutex
	writeFrame := func(t FrameType, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return writeFrameLocked(conn, t, payload)
	}

	if err := writeFrame(FrameHello, helloBytes); err != nil {
		return fmt.Errorf("write Hello: %w", err)
	}
	// Empty StateSync: the headless terminal in the Aspire terminal host
	// rebuilds state from subsequent Output frames.
	if err := writeFrame(FrameStateSync, nil); err != nil {
		return fmt.Errorf("write StateSync: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// When the context is cancelled (either externally or by one of our pumps),
	// nudge the connection's blocked Read to return immediately. This is the
	// only way to unstick the conn->pty pump that is blocked inside
	// readFrame's io.ReadFull. We do not close conn (the caller owns its
	// lifetime), we just expire its read deadline. context.AfterFunc runs
	// the callback in its own goroutine the moment ctx is cancelled, and the
	// returned stop func cancels the arrangement when Serve returns normally
	// — saving us from spinning up a watcher goroutine of our own.
	stopWatcher := context.AfterFunc(ctx, func() {
		if d, ok := conn.(interface{ SetReadDeadline(time.Time) error }); ok {
			_ = d.SetReadDeadline(time.Unix(1, 0))
		}
	})
	defer stopWatcher()

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	// Pump 1: PTY -> conn (Output frames). Exits when PTY EOFs / errors.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		buf := make([]byte, 32*1024)
		for {
			n, readErr := pty.Read(buf)
			if n > 0 {
				if writeErr := writeFrame(FrameOutput, buf[:n]); writeErr != nil {
					errCh <- fmt.Errorf("write Output: %w", writeErr)
					return
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					return
				}
				errCh <- fmt.Errorf("read PTY: %w", readErr)
				return
			}
		}
	}()

	// Pump 2: conn -> PTY (Input/Resize). Exits when connection closes / errors
	// or the context is cancelled. The frameReader owns a growable scratch
	// buffer that is reused for every payload to keep this hot path
	// allocation-free for the typical frame sizes (keystrokes, 8-byte resize).
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		fr := newFrameReader(conn)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			frameType, payload, readErr := fr.Read()
			if readErr != nil {
				// Shutdown signals (EOF, closed conn, our own deadline nudge)
				// are silent. Other errors are real protocol failures and
				// must propagate even if the context happens to be cancelled
				// concurrently (e.g. because the PTY exited at the same time
				// as we received an invalid frame).
				if errors.Is(readErr, io.EOF) || errors.Is(readErr, net.ErrClosed) || errors.Is(readErr, os.ErrDeadlineExceeded) {
					return
				}
				errCh <- fmt.Errorf("read frame: %w", readErr)
				return
			}

			switch frameType {
			case FrameInput:
				if _, writeErr := pty.Write(payload); writeErr != nil {
					errCh <- fmt.Errorf("write PTY: %w", writeErr)
					return
				}
			case FrameResize:
				if len(payload) != 8 {
					opts.Log.V(1).Info("dropping malformed Resize frame", "length", len(payload))
					continue
				}
				w := int(binary.LittleEndian.Uint32(payload[0:4]))
				h := int(binary.LittleEndian.Uint32(payload[4:8]))
				if resizeErr := pty.Resize(w, h); resizeErr != nil {
					opts.Log.V(1).Info("PTY resize failed", "err", resizeErr.Error(), "cols", w, "rows", h)
				}
			default:
				opts.Log.V(1).Info("ignoring unexpected frame from client", "type", byte(frameType), "length", len(payload))
			}
		}
	}()

	wg.Wait()

	// Send a best-effort Exit frame. If the remote side has already gone away
	// (protocol error, dropped TCP, etc.) the write will block until the
	// deadline fires; either way we move on.
	var exitCode int32
	if waitExit != nil {
		exitCode = waitExit()
	}
	exitPayload := make([]byte, 4)
	binary.LittleEndian.PutUint32(exitPayload, uint32(exitCode))
	if d, ok := conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = d.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	}
	_ = writeFrame(FrameExit, exitPayload)

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func writeFrameLocked(w io.Writer, t FrameType, payload []byte) error {
	if len(payload) > MaxPayloadLength {
		return fmt.Errorf("hmp1: payload exceeds maximum (%d > %d)", len(payload), MaxPayloadLength)
	}
	header := [5]byte{}
	header[0] = byte(t)
	binary.LittleEndian.PutUint32(header[1:5], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func readFrame(r io.Reader) (FrameType, []byte, error) {
	fr := newFrameReader(r)
	t, payload, err := fr.Read()
	if err != nil {
		return 0, nil, err
	}
	if payload == nil {
		return t, nil, nil
	}
	// Detach the payload from the (single-shot) frameReader's scratch buffer
	// so callers of the package-level helper own the bytes outright.
	out := make([]byte, len(payload))
	copy(out, payload)
	return t, out, nil
}

// frameReader reads HMP v1 frames from r, reusing a single payload scratch
// buffer across calls so a long-lived conn->PTY pump does not allocate per
// frame. The slice returned by Read is owned by the frameReader and is only
// valid until the next call to Read; callers that need to retain the bytes
// must copy them.
type frameReader struct {
	r       io.Reader
	payload []byte
}

func newFrameReader(r io.Reader) *frameReader {
	// Pre-size the scratch to comfortably hold typical input frames
	// (keystrokes, 8-byte resize). It grows for larger paste payloads and
	// stays at the high-water mark thereafter.
	return &frameReader{r: r, payload: make([]byte, 0, 4096)}
}

func (fr *frameReader) Read() (FrameType, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(fr.r, header[:]); err != nil {
		return 0, nil, err
	}
	t := FrameType(header[0])
	length := binary.LittleEndian.Uint32(header[1:5])
	if length > MaxPayloadLength {
		return 0, nil, fmt.Errorf("hmp1: payload length %d exceeds maximum %d", length, MaxPayloadLength)
	}
	if length == 0 {
		return t, nil, nil
	}
	if cap(fr.payload) < int(length) {
		fr.payload = make([]byte, length)
	} else {
		fr.payload = fr.payload[:length]
	}
	if _, err := io.ReadFull(fr.r, fr.payload); err != nil {
		return 0, nil, err
	}
	return t, fr.payload, nil
}
