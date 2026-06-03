//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/microsoft/dcp/pkg/process"
)

// ptyMaster represents the master side of a Unix pseudo-terminal
type ptyMaster struct {
	*os.File
}

// ptySlave represents the slave side of a Unix pseudo-terminal
type ptySlave struct {
	*os.File
}

// unixPTY is a PTY implementation for Unix-like systems.
// We currently support Linux and macOS.
// Read(), Write(), and Resize() are goroutine-safe.
// The Close() method is also goroutine-safe, but invoking Close() while other methods are in progress
// may lead to an I/O error.
type unixPTY struct {
	master *ptyMaster
	slave  *ptySlave
	lock   *sync.Mutex
}

func (up *unixPTY) Close() error {
	var err error = nil
	up.lock.Lock()
	defer up.lock.Unlock()

	if up.master != nil {
		tmpMaster := up.master
		up.master = nil
		closeErr := tmpMaster.Close()
		if closeErr != nil {
			err = fmt.Errorf("failed to close PTY master: %w", closeErr)
		}
	}

	if up.slave != nil {
		tmpSlave := up.slave
		up.slave = nil
		closeErr := tmpSlave.Close()
		if closeErr != nil {
			err = errors.Join(err, fmt.Errorf("failed to close PTY slave: %w", closeErr))
		}
	}

	return err
}

func (up *unixPTY) Read(p []byte) (n int, err error) {
	up.lock.Lock()
	if up.master == nil {
		up.lock.Unlock()
		return 0, os.ErrClosed
	}

	f := up.master
	up.lock.Unlock()

	return f.Read(p)
}

func (up *unixPTY) Write(p []byte) (n int, err error) {
	up.lock.Lock()
	if up.master == nil {
		up.lock.Unlock()
		return 0, os.ErrClosed
	}

	f := up.master
	up.lock.Unlock()

	return f.Write(p)
}

func (up *unixPTY) Resize(cols, rows uint16) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("invalid PTY dimensions cols=%d rows=%d", cols, rows)
	}

	up.lock.Lock()
	if up.master == nil {
		up.lock.Unlock()
		return os.ErrClosed
	}

	f := up.master
	up.lock.Unlock()

	ws := &winsize{rows: rows, cols: cols}
	return pty_set_size(f, ws)
}

func (up *unixPTY) closeSlave() error {
	if up.slave == nil {
		return nil
	}
	tmpSlave := up.slave
	up.slave = nil
	return tmpSlave.Close()
}

var _ = PTY((*unixPTY)(nil))

// startProcessWithTerminal allocates a Unix pseudo-terminal, spawns the command attached to the slave side,
// and returns a PseudoTerminalProcess whose PTY adapter is the master.
// The caller is responsible for invoking PTY.Close() once the session ends;
// that closes the master fd and the automatically sends SIGHUP to the spawned process.
func startProcessWithTerminal(ctx context.Context, pe process.Executor, spec *CommandSpec) (*PseudoTerminalProcess, error) {
	cols, rows := normalizeTerminalDimensions(spec.Cols, spec.Rows)

	master, slave, err := pty_open()
	if err != nil {
		return nil, fmt.Errorf("failed to allocate PTY: %w", err)
	}
	pty := &unixPTY{
		master: master,
		slave:  slave,
		lock:   &sync.Mutex{},
	}

	resizeErr := pty.Resize(cols, rows)
	if resizeErr != nil {
		_ = pty.Close() // best effort
		return nil, fmt.Errorf("failed to resize PTY: %w", resizeErr)
	}

	cmd := spec.Cmd
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true

	// Assign the embedded *os.File rather than the ptySlave wrapper. exec.Cmd
	// inspects Stdin/Stdout/Stderr via a *os.File type assertion to decide
	// whether to inherit the fd directly into the child; a wrapper struct
	// would fail that assertion and trigger a pipe fallback, leaving the
	// child without a tty on fd 0 (which then breaks Setctty with ENOTTY).
	if cmd.Stdout == nil {
		cmd.Stdout = slave.File
	}
	if cmd.Stderr == nil {
		cmd.Stderr = slave.File
	}
	if cmd.Stdin == nil {
		cmd.Stdin = slave.File
	}

	exitHandler := process.NewConcurrentProcessExitHandler()
	pid, startTime, startWait, startErr := pe.StartProcess(ctx, cmd, exitHandler, spec.CreationFlags, nil)
	if startErr != nil {
		_ = pty.Close() // best effort
		return nil, fmt.Errorf("failed to start process: %w", startErr)
	}

	// Close the slave end in the parent process. The child has already inherited
	// its own copy of the slave fd via fork+exec, so it remains usable on the
	// child side. Keeping the slave open in the parent would prevent
	// master.Read from ever returning EOF/EIO when the child exits, because the
	// kernel keeps the PTY pair alive as long as any process has the slave open.

	_ = pty.closeSlave() // best effort; master end remains usable either way

	ptp := PseudoTerminalProcess{
		PTY:              pty,
		PID:              pid,
		IdentityTime:     startTime,
		StartWaitForExit: startWait,
		ExitHandler:      exitHandler,
		Executor:         pe,
	}
	return &ptp, nil
}

// Allocates a pseudoterminal and returns the master and slave file descriptors.
func pty_open() (master *ptyMaster, slave *ptySlave, err error) {
	var masterFile *os.File
	masterFile, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("openpt: %w", err)
	}
	master = &ptyMaster{masterFile}
	defer func() {
		if err != nil {
			_ = master.Close() // best effort
			master = nil
		}
	}()

	if err = grantpt(master); err != nil {
		return nil, nil, fmt.Errorf("grantpt: %w", err)
	}
	if err = unlockpt(master); err != nil {
		return nil, nil, fmt.Errorf("unlockpt: %w", err)
	}

	name, err := ptsname(master)
	if err != nil {
		return nil, nil, fmt.Errorf("ptsname: %w", err)
	}

	var slaveFile *os.File
	slaveFile, err = os.OpenFile(name, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open slave %q: %w", name, err)
	}
	slave = &ptySlave{slaveFile}
	return master, slave, nil
}

// winsize describes the terminal size.
type winsize struct {
	rows uint16 // ws_row: Number of rows (in cells).
	cols uint16 // ws_col: Number of columns (in cells).
	x    uint16 // ws_xpixel: Width in pixels.
	y    uint16 // ws_ypixel: Height in pixels.
}

func pty_get_size(master *ptyMaster) (size *winsize, err error) {
	var ws winsize

	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		master.Fd(),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)),
	)
	if errno != 0 {
		return nil, errno
	}
	return &ws, nil
}

func pty_set_size(master *ptyMaster, size *winsize) error {
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		master.Fd(),
		uintptr(syscall.TIOCSWINSZ),
		uintptr(unsafe.Pointer(size)),
	)
	if errno != 0 {
		return errno
	}
	return nil
}
