/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	usvc_io "github.com/microsoft/dcp/pkg/io"
)

// Hex1b Multiplex Protocol version 1 (HMP v1) is what DCP uses to expose
// pseudo-terminal output to clients. The wire format is intentionally minimal:
//
//	[type:1B][length:4B little-endian][payload:length bytes]
//
// Supported frame types are as follows.
// In the description below S is the server (DCP) wrapping a PTY that DCP-managed process/container is attached to;
// C is the client (e.g. a terminal emulator) using Hex1b protocol to interact with the terminal session.
//
//	0x01 Hello       JSON: {"version":1,"width":W,"height":H}     S->C
//	0x02 StateSync   raw ANSI bytes (current screen)              S->C
//	0x03 Output      raw ANSI bytes from PTY                      S->C
//	0x04 Input       raw bytes to deliver to PTY stdin            C->S
//	0x05 Resize      [width:4B LE][height:4B LE]                  bidirectional
//	0x06 Exit        [exitCode:4B LE]                             S->C (then close)
//
// HMP v1 specification:
// https://github.com/mitchdenny/hex1b/blob/main/docs/muxer-protocol.md

// Hmp1MaxPayloadLength is the maximum payload length (16 MiB, HMP v1 limit).
const Hmp1MaxPayloadLength = 16 * 1024 * 1024

type Hmp1FrameType byte

const (
	Hmp1FrameHello     Hmp1FrameType = 0x01
	Hmp1FrameStateSync Hmp1FrameType = 0x02
	Hmp1FrameOutput    Hmp1FrameType = 0x03
	Hmp1FrameInput     Hmp1FrameType = 0x04
	Hmp1FrameResize    Hmp1FrameType = 0x05
	Hmp1FrameExit      Hmp1FrameType = 0x06
)

// Hmp1HelloPayload is the JSON payload of a Hello frame.
type Hmp1HelloPayload struct {
	Version int    `json:"version"`
	Width   uint16 `json:"width"`
	Height  uint16 `json:"height"`
}

const (
	// Hmp1ExitCodeWaitTimeout bounds how long the server waits for an exit
	// code of a process that finished running before considering it lost.
	Hmp1ExitCodeWaitTimeout = 3 * time.Second

	// hmp1ExitCodeSendTimeout bounds how long the server tries to deliver
	// the process exit code to a terminal client before giving up.
	hmp1ExitCodeSendTimeout = 3 * time.Second

	// hmp1PtyOutputBufSize bounds the per-read chunk the Hmp1Server pulls
	// from the PTY before forwarding it as an Output frame.
	hmp1PtyOutputBufSize = 32 * 1024
)

// Hmp1ServerOptions configures a Hmp1Server invocation.
type Hmp1ServerOptions struct {
	// InitialCols / InitialRows are the dimensions sent in the Hello frame.
	// Reasonable defaults are applied if either is zero.
	InitialCols uint16
	InitialRows uint16

	// Log receives diagnostic messages from the server.
	Log logr.Logger
}

// Hmp1Server is the server side of the HMP v1 protocol that bridges a single
// pseudo-terminal to client connections established via Serve(). The server
// does not own the PTY lifetime — the caller does — so it can be reused
// across multiple Serve invocations (reconnects).
type Hmp1Server struct {
	// Whether the server is currently serving a connection. At most one connection is allowed at a time.
	serving *atomic.Bool

	// The pseudoterminal that the server bridges to the client connection.
	// The server does not own the PTY lifetime.
	pty PTY

	// CancellableReader for the PTY output. Ensures that no data is lost between Serve() invocations.
	ptyReader *usvc_io.CancellableReader

	// Lock for serializing writes to the client connection.
	writeLock *sync.Mutex
}

// NewHmp1Server creates a Hmp1Server that bridges the given pty to client connections established via Serve().
// When lifetime context is cancelled, the existing Serve() session is terminated and any subsequent Serve() call will fail
// (the Hmp1Server is effectively disposed).
func NewHmp1Server(lifetimeCtx context.Context, pty PTY) *Hmp1Server {
	return &Hmp1Server{
		serving:   &atomic.Bool{},
		pty:       pty,
		ptyReader: usvc_io.NewCancellableReader(lifetimeCtx, pty, hmp1PtyOutputBufSize),
		writeLock: &sync.Mutex{},
	}
}

// Serve runs an HMP v1 server on the given connection, bridging conn <-> pty
// until either the PTY exits, the connection closes, or the context is cancelled.
//
// The exitCode reported back to the client (via the Exit frame) is taken from exitCodeCh.
// If the channel is nil, the Exit frame is sent with exit code 0.
//
// Serve takes ownership of conn for the duration of the call: it will close conn before returning,
// both to release any goroutine blocked on conn.Read and to signal the viewer that the session is over.
// It does NOT close pty; the caller owns the PTY lifetime so it can be reused
// across multiple Serve invocations (reconnects).
func (s *Hmp1Server) Serve(ctx context.Context, conn net.Conn, exitCodeCh <-chan int32, opts Hmp1ServerOptions) error {
	if !s.serving.CompareAndSwap(false, true) {
		return fmt.Errorf("server is already serving a connection")
	}
	defer s.serving.Store(false)

	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			opts.Log.V(1).Info("error closing terminal server connection", "error", closeErr.Error())
		}
	}()

	cols := opts.InitialCols
	if cols <= 0 {
		cols = defaultInitialCols
	}
	rows := opts.InitialRows
	if rows <= 0 {
		rows = defaultInitialRows
	}

	helloBytes, marshalErr := json.Marshal(Hmp1HelloPayload{Version: 1, Width: cols, Height: rows})
	if marshalErr != nil {
		return fmt.Errorf("marshalling Hello payload failed: %w", marshalErr)
	}

	writeHelloErr := s.writeFrame(conn, Hmp1FrameHello, helloBytes)
	if writeHelloErr != nil {
		return fmt.Errorf("write Hello: %w", writeHelloErr)
	}

	// Request state sync immediately after Hello so the client can render a complete screen upon connecting.
	writeStateSyncErr := s.writeFrame(conn, Hmp1FrameStateSync, nil)
	if writeStateSyncErr != nil {
		return fmt.Errorf("write StateSync: %w", writeStateSyncErr)
	}

	ioCtx, ioCtxCancel := context.WithCancel(ctx)
	defer ioCtxCancel()

	var outputPumpErr, clientPumpErr error
	var clientSideEnded bool
	var wg sync.WaitGroup
	wg.Add(2)

	// Pump 1: PTY -> conn (Output frames).
	go func() {
		defer wg.Done()
		defer ioCtxCancel()
		outputPumpErr = s.pumpOutputFrames(ioCtx, conn)
	}()

	// Pump 2: conn -> PTY (Input/Resize). Reports whether it terminated because
	// of a client-side event so we can skip the exit-code dance on disconnect.
	go func() {
		defer wg.Done()
		defer ioCtxCancel()
		clientSideEnded, clientPumpErr = s.pumpClientFrames(ioCtx, conn, opts)
	}()

	wg.Wait()

	if !clientSideEnded {
		// Exiting because the process exited or PTY connection failed.
		exitCode := int32(0)
		if exitCodeCh != nil {
			select {
			case code, ok := <-exitCodeCh:
				if ok {
					exitCode = code
				}
			case <-time.After(Hmp1ExitCodeWaitTimeout):
				opts.Log.V(1).Info("timeout waiting for exit code")
			}
		}

		exitPayload := make([]byte, 4)
		binary.LittleEndian.PutUint32(exitPayload, uint32(exitCode))
		if d, ok := conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
			_ = d.SetWriteDeadline(time.Now().Add(hmp1ExitCodeSendTimeout))
		}
		_ = s.writeFrame(conn, Hmp1FrameExit, exitPayload)
	}

	return errors.Join(outputPumpErr, clientPumpErr)
}

// Sends output frames from the program attached to the PTY to the terminal client.
func (s *Hmp1Server) pumpOutputFrames(ioCtx context.Context, conn net.Conn) error {
	for {
		data, readErr := s.ptyReader.Read(ioCtx)
		if len(data) > 0 {
			if writeErr := s.writeFrame(conn, Hmp1FrameOutput, data); writeErr != nil {
				return fmt.Errorf("writing Output frame failed: %w", writeErr)
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, os.ErrClosed) ||
				errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
				// All expected errors indicating we are done reading from the PTY
				// (the process exited, the PTY was closed, or serving was cancelled).
				return nil
			}
			return fmt.Errorf("reading from PTY failed: %w", readErr)
		}
	}
}

// Processes client frames (input and resize, sending client input to the PTY).
// Returns clientSideEnded=true when termination is caused by the client side
// (conn EOF/closed or a fatal client-side frame error), otherwise false
// (pumping context was cancelled or the PTY write failed).
func (s *Hmp1Server) pumpClientFrames(ioCtx context.Context, conn net.Conn, opts Hmp1ServerOptions) (clientSideEnded bool, err error) {
	connReader := usvc_io.NewContextReader(ioCtx, conn, false /* do not close conn, parent will do it */)
	fr := newHmp1FrameReader(connReader)

	for {
		frameType, payload, readErr := fr.readFrame()
		if readErr != nil {
			if errors.Is(readErr, context.Canceled) {
				// Serving context cancelled (outer ctx or the other pump's
				// defer): not a client-side termination.
				return false, nil
			}
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, net.ErrClosed) {
				return true, nil
			}
			return true, fmt.Errorf("reading client frame: %w", readErr)
		}

		switch frameType {
		case Hmp1FrameInput:
			if _, writeErr := s.pty.Write(payload); writeErr != nil {
				// PTY-side failure, not a client-side termination.
				return false, fmt.Errorf("write PTY: %w", writeErr)
			}
		case Hmp1FrameResize:
			if len(payload) != 8 {
				opts.Log.V(1).Info("dropping malformed Resize frame", "length", len(payload))
				continue
			}

			w := int(binary.LittleEndian.Uint32(payload[0:4]))
			h := int(binary.LittleEndian.Uint32(payload[4:8]))

			if w <= 0 || h <= 0 || w > math.MaxUint16 || h > math.MaxUint16 {
				opts.Log.V(1).Info("dropping Resize frame with invalid dimensions", "width", w, "height", h)
				continue
			}

			if resizeErr := s.pty.Resize(uint16(w), uint16(h)); resizeErr != nil {
				opts.Log.V(1).Info("PTY resize failed", "err", resizeErr.Error(), "cols", w, "rows", h)
			}
		default:
			opts.Log.V(1).Info("ignoring unexpected frame from client", "type", byte(frameType), "length", len(payload))
		}
	}
}

func (s *Hmp1Server) writeFrame(w io.Writer, t Hmp1FrameType, payload []byte) error {
	if len(payload) > Hmp1MaxPayloadLength {
		return fmt.Errorf("hmp1: payload exceeds maximum (%d > %d)", len(payload), Hmp1MaxPayloadLength)
	}

	header := [5]byte{}
	header[0] = byte(t)
	binary.LittleEndian.PutUint32(header[1:5], uint32(len(payload)))

	s.writeLock.Lock()
	defer s.writeLock.Unlock()

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

// hmp1FrameReader reads HMP v1 frames from r, reusing a single payload scratch
// buffer across calls so a long-lived conn->PTY pump does not allocate per
// frame. The slice returned by readFrame is owned by the hmp1FrameReader and
// is only valid until the next call to readFrame; callers that need to retain
// the bytes must copy them.
type hmp1FrameReader struct {
	r       io.Reader
	payload []byte
}

func newHmp1FrameReader(r io.Reader) *hmp1FrameReader {
	// Pre-size the scratch to comfortably hold typical input frames
	// (keystrokes, 8-byte resize). It grows for larger paste payloads and
	// stays at the high-water mark thereafter.
	return &hmp1FrameReader{r: r, payload: make([]byte, 0, 4096)}
}

func (fr *hmp1FrameReader) readFrame() (Hmp1FrameType, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(fr.r, header[:]); err != nil {
		return 0, nil, err
	}
	t := Hmp1FrameType(header[0])
	length := binary.LittleEndian.Uint32(header[1:5])
	if length > Hmp1MaxPayloadLength {
		return 0, nil, fmt.Errorf("hmp1: payload length %d exceeds maximum %d", length, Hmp1MaxPayloadLength)
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
