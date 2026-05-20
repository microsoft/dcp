//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"unsafe"

	"github.com/go-logr/logr"
	"golang.org/x/sys/windows"
)

var (
	kernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procReadConsoleInputW = kernel32.NewProc("ReadConsoleInputW")
)

// disableTerminalEcho clears ENABLE_ECHO_INPUT on the console input device
// so the echo loop test scenario can observe its own input without
// interference from the host pseudo-console echoing typed bytes back to the
// output stream.
//
// ENABLE_LINE_INPUT is left set on purpose. In ConPTY the child's stdin is a
// pipe written by conhost.exe; conhost only forwards cooked text to that
// pipe when the console is in line-input mode. Switching the console to raw
// input mode (clearing ENABLE_LINE_INPUT) makes the input only available
// via ReadConsoleInput against CONIN$, which would break readers that go
// through os.Stdin / ReadFile. Per Windows docs, ENABLE_ECHO_INPUT only has
// any effect when ENABLE_LINE_INPUT is set, so clearing just the echo flag
// is sufficient to silence the host's echo.
//
// CONIN$ (not STD_INPUT_HANDLE) is opened because inside a ConPTY the std
// handles are pipes, against which console-mode APIs return "invalid handle".
func disableTerminalEcho() error {
	hConIn, err := openConIn()
	if err != nil {
		return fmt.Errorf("open CONIN$: %w", err)
	}
	defer func() { _ = windows.CloseHandle(hConIn) }()

	var mode uint32
	if getErr := windows.GetConsoleMode(hConIn, &mode); getErr != nil {
		return fmt.Errorf("get console mode: %w", getErr)
	}
	mode &^= windows.ENABLE_ECHO_INPUT
	if setErr := windows.SetConsoleMode(hConIn, mode); setErr != nil {
		return fmt.Errorf("set console mode: %w", setErr)
	}
	return nil
}

// getTerminalSize returns the size of the console window attached to the
// pseudo-console (or the parent console, if running outside a ConPTY).
//
// Inside a ConPTY, STD_OUTPUT_HANDLE is wired to a pipe — console APIs do
// not work against it. CONOUT$ opens the actual console screen buffer
// that the (pseudo-)console exposes; this is what we want for dimension
// queries.
func getTerminalSize() (cols, rows uint16, err error) {
	hConOut, err := openConOut()
	if err != nil {
		return 0, 0, fmt.Errorf("open CONOUT$: %w", err)
	}
	defer func() { _ = windows.CloseHandle(hConOut) }()

	var info windows.ConsoleScreenBufferInfo
	if infoErr := windows.GetConsoleScreenBufferInfo(hConOut, &info); infoErr != nil {
		return 0, 0, fmt.Errorf("get screen buffer info: %w", infoErr)
	}
	// info.Window holds the visible viewport. Its width/height is what the
	// child should treat as the current terminal size.
	cols = uint16(info.Window.Right - info.Window.Left + 1)
	rows = uint16(info.Window.Bottom - info.Window.Top + 1)
	return cols, rows, nil
}

// openConOut opens the console output device (CONOUT$). The handle must be
// closed by the caller.
func openConOut() (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString("CONOUT$")
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(
		name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
}

// openConIn opens the console input device (CONIN$). The handle must be
// closed by the caller.
func openConIn() (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString("CONIN$")
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(
		name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
}

// registerSignals subscribes the supplied channel to every console control
// event that we may need to react to. On Windows the Go runtime maps
// CTRL_C_EVENT to os.Interrupt and CTRL_BREAK_EVENT / CTRL_CLOSE_EVENT (plus
// LOGOFF and SHUTDOWN) to syscall.SIGTERM. classifySignal narrows the
// observed signal to the kind we care about.
func registerSignals(ch chan<- os.Signal) {
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
}

// classifySignal interprets the observed signal. On Windows we cannot
// distinguish between CTRL_BREAK / CTRL_CLOSE because both surface as
// syscall.SIGTERM. We classify SIGTERM as a hup-like event because:
//   - When --hup-exit-code is configured, the test scenario expects the
//     close event (ClosePseudoConsole / closed console window) to drive the
//     exit; the dispatcher uses the override.
//   - When --hup-exit-code is not configured, the dispatcher's signalHup
//     branch falls through to a normal shutdown.
//
// os.Interrupt (CTRL_C_EVENT) is always treated as a shutdown-only signal.
func classifySignal(sig os.Signal) signalKind {
	switch sig {
	case syscall.SIGTERM:
		return signalHup
	case os.Interrupt:
		return signalShutdown
	}
	return signalUnknown
}

// installResizeHandler spawns a goroutine that watches CONIN$ for
// WINDOW_BUFFER_SIZE_EVENT records. On the first such record, it reads the
// new terminal size, prints it, sets the configured exit code, and cancels
// the wait context.
//
// ConPTY surfaces host-driven resizes through the same console input stream
// that delivers key events. ENABLE_WINDOW_INPUT must be set on the input
// mode so the events are not filtered out. CONIN$ is opened explicitly
// (rather than going through STD_INPUT_HANDLE) because the std handles are
// pipes inside a ConPTY and console APIs only work against the console
// device.
func installResizeHandler(log logr.Logger, exitCode int, cancel context.CancelFunc) error {
	hConIn, err := openConIn()
	if err != nil {
		return fmt.Errorf("open CONIN$: %w", err)
	}

	var mode uint32
	if getErr := windows.GetConsoleMode(hConIn, &mode); getErr != nil {
		_ = windows.CloseHandle(hConIn)
		return fmt.Errorf("get console mode: %w", getErr)
	}
	mode |= windows.ENABLE_WINDOW_INPUT
	if setErr := windows.SetConsoleMode(hConIn, mode); setErr != nil {
		_ = windows.CloseHandle(hConIn)
		return fmt.Errorf("set console mode (enable window input): %w", setErr)
	}

	go func() {
		defer func() { _ = windows.CloseHandle(hConIn) }()
		// inputRecord mirrors the Win32 INPUT_RECORD struct. The only field
		// we touch is EventType; the rest is a union large enough to hold the
		// biggest event variant (KEY_EVENT_RECORD). On a 64-bit platform the
		// union is 16 bytes, plus the leading 2-byte EventType and 2 bytes
		// of padding to align the union — total 20 bytes. Sizing it
		// generously (24 bytes) is safe even if Windows changes the layout
		// in the future.
		type inputRecord struct {
			EventType uint16
			_         [22]byte
		}

		buf := make([]inputRecord, 16)
		for {
			var read uint32
			r1, _, callErr := procReadConsoleInputW.Call(
				uintptr(hConIn),
				uintptr(unsafe.Pointer(&buf[0])),
				uintptr(len(buf)),
				uintptr(unsafe.Pointer(&read)),
			)
			if r1 == 0 {
				log.Error(callErr, "ReadConsoleInputW failed")
				return
			}
			for i := uint32(0); i < read; i++ {
				if buf[i].EventType != windows.WINDOW_BUFFER_SIZE_EVENT {
					continue
				}
				cols, rows, sizeErr := getTerminalSize()
				if sizeErr != nil {
					log.Error(sizeErr, "Could not read terminal size after WINDOW_BUFFER_SIZE_EVENT")
					return
				}
				fmt.Fprintf(os.Stdout, "size:%dx%d\n", cols, rows)
				finalExitCode.Store(int32(exitCode))
				cancel()
				return
			}
		}
	}()
	return nil
}
