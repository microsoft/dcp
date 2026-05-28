//go:build !windows

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
	"golang.org/x/sys/unix"
)

// disableTerminalEcho clears the ECHO and ICANON bits on the controlling
// terminal so the child's own input does not get echoed back to the master
// before the test driver has a chance to assert on the output. ISIG is left
// alone so that SIGINT / SIGTERM behavior remains testable.
func disableTerminalEcho() error {
	fd := int(os.Stdin.Fd())
	termios, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return fmt.Errorf("ioctl get termios: %w", err)
	}
	termios.Lflag &^= unix.ECHO | unix.ICANON
	// Switch to single-byte read semantics so the reader doesn't block
	// waiting for a full line of input from a now-uncanonicalized stream.
	termios.Cc[unix.VMIN] = 1
	termios.Cc[unix.VTIME] = 0
	if err = unix.IoctlSetTermios(fd, ioctlSetTermios, termios); err != nil {
		return fmt.Errorf("ioctl set termios: %w", err)
	}
	return nil
}

// getTerminalSize returns the current dimensions of the controlling terminal
// using TIOCGWINSZ on the stdin fd.
func getTerminalSize() (cols, rows uint16, err error) {
	var ws struct {
		rows, cols, xpixel, ypixel uint16
	}
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		os.Stdin.Fd(),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)),
	)
	if errno != 0 {
		return 0, 0, fmt.Errorf("ioctl TIOCGWINSZ: %w", errno)
	}
	return ws.cols, ws.rows, nil
}

// registerSignals subscribes the supplied channel to every OS signal we may
// need to react to: SIGINT/SIGTERM/SIGQUIT for shutdown, and SIGHUP for the
// hang-up scenario. classifySignal interprets which kind a delivered signal
// represents.
func registerSignals(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP)
}

func classifySignal(sig os.Signal) signalKind {
	switch sig {
	case syscall.SIGHUP:
		return signalHup
	case syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT:
		return signalShutdown
	}
	return signalUnknown
}

// installResizeHandler installs a SIGWINCH handler that, on the first
// signal, prints the new terminal size and exits with the configured code.
func installResizeHandler(log logr.Logger, exitCode int, cancel context.CancelFunc) error {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(ch)
		_, ok := <-ch
		if !ok {
			return
		}
		cols, rows, err := getTerminalSize()
		if err != nil {
			log.Error(err, "Could not read terminal size after SIGWINCH")
			return
		}
		fmt.Fprintf(os.Stdout, "size:%dx%d\n", cols, rows)
		finalExitCode.Store(int32(exitCode))
		cancel()
	}()
	return nil
}
