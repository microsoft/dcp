//go:build windows

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
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/microsoft/dcp/pkg/process"
)

// windowsPTY is a PTY implementation for Windows OS that leverages the Windows Console API.
// Read(), Write(), and Resize() are goroutine-safe.
// The Close() method is also goroutine-safe, but invoking Close() while other methods are in progress
// may lead to an I/O error.
type windowsPTY struct {
	hConsole     windows.Handle
	outputRead   windows.Handle
	inputWrite   windows.Handle
	otherHandles []windows.Handle
	lock         *sync.Mutex
}

func (wp *windowsPTY) Read(p []byte) (int, error) {
	wp.lock.Lock()
	if wp.outputRead == 0 || wp.outputRead == windows.InvalidHandle {
		wp.lock.Unlock()
		return 0, os.ErrClosed
	}
	rh := wp.outputRead
	wp.lock.Unlock()

	var bytesRead uint32
	readErr := windows.ReadFile(rh, p, &bytesRead, nil)
	return int(bytesRead), readErr
}

func (wp *windowsPTY) Write(p []byte) (int, error) {
	wp.lock.Lock()
	if wp.inputWrite == 0 || wp.inputWrite == windows.InvalidHandle {
		wp.lock.Unlock()
		return 0, os.ErrClosed
	}
	wh := wp.inputWrite
	wp.lock.Unlock()

	var bytesWritten uint32
	writeErr := windows.WriteFile(wh, p, &bytesWritten, nil)
	return int(bytesWritten), writeErr
}

func (wp *windowsPTY) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return fmt.Errorf("invalid PTY dimensions cols=%d rows=%d", cols, rows)
	}

	wp.lock.Lock()
	if wp.hConsole == 0 || wp.hConsole == windows.InvalidHandle {
		wp.lock.Unlock()
		return os.ErrClosed
	}

	hConsole := wp.hConsole
	wp.lock.Unlock()

	// windowsConsoleSize/normalizeTerminalDimensions substitutes defaults for
	// zero dimensions; we reject zeros above so only the upper-bound clamp matters here.
	consoleSize := windowsConsoleSize(cols, rows)
	return windows.ResizePseudoConsole(hConsole, consoleSize)
}

func (wp *windowsPTY) Close() error {
	wp.lock.Lock()
	defer wp.lock.Unlock()

	if wp.hConsole != 0 && wp.hConsole != windows.InvalidHandle {
		windows.ClosePseudoConsole(wp.hConsole)
		wp.hConsole = windows.InvalidHandle
	}

	toClose := append(wp.otherHandles, wp.inputWrite, wp.outputRead)
	wp.inputWrite = windows.InvalidHandle
	wp.outputRead = windows.InvalidHandle
	wp.otherHandles = nil

	return closeHandles(toClose...)
}

var _ = PTY((*windowsPTY)(nil))

// startProcessWithTerminal allocates a new Windows console and starts a process attached to it.
func startProcessWithTerminal(ctx context.Context, pe process.Executor, spec *CommandSpec) (*PseudoTerminalProcess, error) {
	var inputRead, inputWrite, outputRead, outputWrite windows.Handle
	inputPipeErr := windows.CreatePipe(&inputRead, &inputWrite, nil, 0)
	if inputPipeErr != nil {
		return nil, fmt.Errorf("failed to create Windows console input pipe: %w", inputPipeErr)
	}
	outputPipeErr := windows.CreatePipe(&outputRead, &outputWrite, nil, 0)
	if outputPipeErr != nil {
		_ = closeHandles(inputRead, inputWrite)
		return nil, fmt.Errorf("failed to create Windows console output pipe: %w", outputPipeErr)
	}

	initialCols, initialRows := normalizeTerminalDimensions(spec.Cols, spec.Rows)
	var hConsole windows.Handle
	consoleSize := windowsConsoleSize(initialCols, initialRows)
	createConsoleErr := windows.CreatePseudoConsole(consoleSize, inputRead, outputWrite, 0, &hConsole)
	if createConsoleErr != nil {
		_ = closeHandles(inputRead, inputWrite, outputRead, outputWrite)
		return nil, fmt.Errorf("failed to create Windows console: %w", createConsoleErr)
	}

	exitHandler := process.NewConcurrentProcessExitHandler()
	pid, startTime, startWait, startErr := pe.StartProcess(
		ctx,
		spec.Cmd,
		exitHandler,
		spec.CreationFlags,
		func(cmd *exec.Cmd) (process.Pid_t, process.Waitable, error) {
			return createProcessWithConsole(cmd, hConsole, spec.CreationFlags)
		})
	if startErr != nil {
		windows.ClosePseudoConsole(hConsole)
		_ = closeHandles(inputRead, inputWrite, outputRead, outputWrite)
		return nil, fmt.Errorf("failed to start process: %w", startErr)
	}

	ptp := &PseudoTerminalProcess{
		PTY: &windowsPTY{
			hConsole:     hConsole,
			outputRead:   outputRead,
			inputWrite:   inputWrite,
			otherHandles: []windows.Handle{inputRead, outputWrite},
			lock:         &sync.Mutex{},
		},
		InitialCols:      initialCols,
		InitialRows:      initialRows,
		PID:              pid,
		IdentityTime:     startTime,
		StartWaitForExit: startWait,
		ExitHandler:      exitHandler,
		Executor:         pe,
	}

	return ptp, nil
}

func createProcessWithConsole(
	cmd *exec.Cmd,
	hConsole windows.Handle,
	flags process.ProcessCreationFlag,
) (process.Pid_t, process.Waitable, error) {
	if len(cmd.Args) == 0 || cmd.Args[0] != cmd.Path || cmd.Path == "" {
		return process.UnknownPID, nil, fmt.Errorf("missing or invalid command path")
	}

	commandLine := windows.ComposeCommandLine(cmd.Args)
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.CmdLine != "" {
		commandLine = cmd.SysProcAttr.CmdLine
	}
	commandLinePtr, commandLineErr := windows.UTF16PtrFromString(commandLine)
	if commandLineErr != nil {
		return process.UnknownPID, nil, fmt.Errorf("could not convert command line to UTF-16: %w", commandLineErr)
	}

	var workingDir *uint16
	if cmd.Dir != "" {
		var workingDirErr error
		workingDir, workingDirErr = windows.UTF16PtrFromString(cmd.Dir)
		if workingDirErr != nil {
			return process.UnknownPID, nil, fmt.Errorf("could not convert working directory to UTF-16: %w", workingDirErr)
		}
	}

	attributeList, attributeListErr := windows.NewProcThreadAttributeList(1)
	if attributeListErr != nil {
		return process.UnknownPID, nil, fmt.Errorf("could not create new thread attribute list: %w", attributeListErr)
	}
	defer attributeList.Delete()

	updateAttributeErr := attributeList.Update(
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		unsafe.Pointer(uintptr(hConsole)),
		unsafe.Sizeof(hConsole),
	)
	if updateAttributeErr != nil {
		return process.UnknownPID, nil, fmt.Errorf("could not update thread attribute list: %w", updateAttributeErr)
	}

	startupInfoEx := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			// Cb must include the trailing ProcThreadAttributeList field, which is unique
			// to StartupInfoEx (and not part of StartupInfo). The OS uses this size to
			// detect that an extended StartupInfo is being supplied alongside
			// EXTENDED_STARTUPINFO_PRESENT in CreateProcess's creation flags.
			Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})),

			// STARTF_USESTDHANDLES is set with hStd{Input,Output,Error} left at zero.
			// In combination with PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE in the attribute
			// list, this tells the loader that the child must not inherit the parent's
			// console handles; the pseudoconsole supplies the child's stdio instead.
			// Microsoft's reference C++ sample omits this flag, but real-world Go
			// ConPTY implementations (e.g. github.com/UserExistsError/conpty) all set
			// it because dropping it causes the child to fall back to inheriting the
			// parent's console, defeating the pseudoconsole.
			Flags: windows.STARTF_USESTDHANDLES,
		},
		ProcThreadAttributeList: attributeList.List(),
	}

	// As of now we (DCP) only use cmd.SystProcAttr.HideWindow and cmd. SystProcAttr.CreationFlags,
	// so this is what is used below.
	// If we ever decide to use other SysProcAttr fields (like ProcessAttributes, ThreadAttributes, Token,
	// or AdditionalInheritedHandles), we will need to update this code accordingly.

	creationFlags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT)

	if cmd.SysProcAttr != nil {
		creationFlags |= uint32(cmd.SysProcAttr.CreationFlags)

		if cmd.SysProcAttr.HideWindow {
			startupInfoEx.Flags |= windows.STARTF_USESHOWWINDOW
			startupInfoEx.ShowWindow = windows.SW_HIDE
		}
	}

	envBlock, envBlockErr := createEnvironmentBlock(cmd.Env)
	if envBlockErr != nil {
		return process.UnknownPID, nil, envBlockErr
	}
	var envBlockPtr *uint16
	if envBlock != nil {
		creationFlags |= windows.CREATE_UNICODE_ENVIRONMENT
		envBlockPtr = &envBlock[0]
	}

	var processInformation windows.ProcessInformation
	createProcessErr := windows.CreateProcess(
		nil,
		commandLinePtr,
		nil,
		nil,
		false,
		creationFlags,
		envBlockPtr,
		workingDir,
		&startupInfoEx.StartupInfo,
		&processInformation,
	)
	if createProcessErr != nil {
		return process.UnknownPID, nil, fmt.Errorf("could not start process: %w", createProcessErr)
	}

	runtime.KeepAlive(envBlock)
	runtime.KeepAlive(startupInfoEx)

	_ = closeHandles(processInformation.Thread) // best effort

	pid := process.Uint32_ToPidT(processInformation.ProcessId)
	waitable := newWindowsProcessHandle(processInformation.Process, cmd, flags)

	return pid, waitable, nil
}

func closeHandles(handles ...windows.Handle) error {
	var err error = nil

	for _, handle := range handles {
		if handle == 0 || handle == windows.InvalidHandle {
			continue
		}

		err = errors.Join(err, windows.CloseHandle(handle))
	}

	return err
}

func windowsConsoleSize(cols, rows uint16) windows.Coord {
	cols, rows = normalizeTerminalDimensions(cols, rows)
	return windows.Coord{X: int16(cols), Y: int16(rows)}
}

func createEnvironmentBlock(env []string) ([]uint16, error) {
	if env == nil {
		return nil, nil
	}

	entries := append([]string(nil), env...)
	for index, entry := range entries {
		if strings.ContainsRune(entry, 0) {
			return nil, fmt.Errorf("environment entry at index %d contains NUL", index)
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return strings.ToUpper(entries[i]) < strings.ToUpper(entries[j])
	})

	block := strings.Join(entries, "\x00") + "\x00\x00"
	return utf16.Encode([]rune(block)), nil
}

// windowsProcessHandle represents a Windows process handle.
// Implements process.Waitable (specifically process.WaitableWithExitCode) and io.Closer.
// All methods are goroutine-safe.
type windowsProcessHandle struct {
	hProcess windows.Handle
	info     string
	flags    process.ProcessCreationFlag
	waitOnce func() error
	lock     *sync.Mutex

	// exitCode is populated by waitOnce after WaitForSingleObject returns.
	// Reads of the int32 itself are atomic on Windows, but the lock is taken
	// for consistency with the rest of the type.
	exitCode int32
}

func newWindowsProcessHandle(hProcess windows.Handle, cmd *exec.Cmd, flags process.ProcessCreationFlag) *windowsProcessHandle {
	wph := &windowsProcessHandle{
		hProcess: hProcess,
		info:     cmd.String(),
		flags:    flags,
		lock:     &sync.Mutex{},
		exitCode: process.UnknownExitCode,
	}

	wph.waitOnce = sync.OnceValue(func() error {
		wph.lock.Lock()
		h := wph.hProcess
		wph.lock.Unlock()
		if h == windows.InvalidHandle {
			return os.ErrClosed
		}

		_, waitErr := windows.WaitForSingleObject(h, windows.INFINITE)
		if waitErr != nil {
			// The process is in an unknown state; release the handle and bail out.
			_ = wph.Close()
			return fmt.Errorf("WaitForSingleObject: %w", waitErr)
		}

		var rawExitCode uint32
		exitCodeErr := windows.GetExitCodeProcess(h, &rawExitCode)
		if exitCodeErr == nil {
			wph.lock.Lock()
			wph.exitCode = int32(rawExitCode)
			wph.lock.Unlock()
		}

		closeErr := wph.Close()
		return errors.Join(exitCodeErr, closeErr)
	})

	return wph
}

func (wph *windowsProcessHandle) Wait() error {
	return wph.waitOnce()
}

func (wph *windowsProcessHandle) ExitCode() int32 {
	wph.lock.Lock()
	defer wph.lock.Unlock()
	return wph.exitCode
}

func (wph *windowsProcessHandle) Info() string {
	return wph.info
}

func (wph *windowsProcessHandle) Flags() process.ProcessCreationFlag {
	return wph.flags
}

func (wph *windowsProcessHandle) Close() error {
	wph.lock.Lock()
	defer wph.lock.Unlock()
	if wph.hProcess == windows.InvalidHandle {
		return nil
	}
	err := closeHandles(wph.hProcess)
	wph.hProcess = windows.InvalidHandle
	return err
}

var _ process.Waitable = (*windowsProcessHandle)(nil)
var _ process.ExitCodeSource = (*windowsProcessHandle)(nil)
