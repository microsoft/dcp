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
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/microsoft/dcp/pkg/process"
)

const (
	defaultConsoleWidth  = 80
	defaultConsoleHeight = 24
	maxConsoleDimension  = 1<<15 - 1
	stillActiveExitCode  = 259
)

var (
	errConPTYUnsupported = errors.New("conpty is not available on this Windows version")

	kernel32DLL = windows.NewLazySystemDLL("kernel32.dll")

	requiredProcedures = []requiredProcedure{
		{name: "CreatePseudoConsole", proc: kernel32DLL.NewProc("CreatePseudoConsole")},
		{name: "ResizePseudoConsole", proc: kernel32DLL.NewProc("ResizePseudoConsole")},
		{name: "ClosePseudoConsole", proc: kernel32DLL.NewProc("ClosePseudoConsole")},
		{name: "InitializeProcThreadAttributeList", proc: kernel32DLL.NewProc("InitializeProcThreadAttributeList")},
		{name: "UpdateProcThreadAttribute", proc: kernel32DLL.NewProc("UpdateProcThreadAttribute")},
		{name: "DeleteProcThreadAttributeList", proc: kernel32DLL.NewProc("DeleteProcThreadAttributeList")},
	}
)

type requiredProcedure struct {
	name string
	proc *windows.LazyProc
}

// windowsPseudoConsoleConfig captures the process and initial terminal settings for a ConPTY-backed process.
type windowsPseudoConsoleConfig struct {
	CommandLine string
	Env         []string
	Dir         string
	Cols        int
	Rows        int
}

type handleIO struct {
	handle windows.Handle
}

func (h *handleIO) Read(p []byte) (int, error) {
	var bytesRead uint32
	readErr := windows.ReadFile(h.handle, p, &bytesRead, nil)
	return int(bytesRead), readErr
}

func (h *handleIO) Write(p []byte) (int, error) {
	var bytesWritten uint32
	writeErr := windows.WriteFile(h.handle, p, &bytesWritten, nil)
	return int(bytesWritten), writeErr
}

func (h *handleIO) Close() error {
	return closeHandle(h.handle)
}

// windowsPseudoConsole owns a ConPTY instance, the attached child process handle, and
// the pipe endpoints DCP uses to exchange terminal bytes with the child.
type windowsPseudoConsole struct {
	console windows.Handle
	pid     process.Pid_t

	inputWriter  *handleIO
	outputReader *handleIO

	consoleInput  windows.Handle
	consoleOutput windows.Handle

	closeOnce sync.Once
	closeErr  error

	processMu           sync.Mutex
	processHandle       windows.Handle
	processHandleClosed bool
}

// isWindowsPseudoConsoleAvailable reports whether this Windows version exports the APIs required for ConPTY.
func isWindowsPseudoConsoleAvailable() bool {
	return availabilityError() == nil
}

func availabilityError() error {
	for _, requiredProc := range requiredProcedures {
		findErr := requiredProc.proc.Find()
		if findErr != nil {
			return fmt.Errorf("%s not found: %w", requiredProc.name, findErr)
		}
	}
	return nil
}

// startWindowsPseudoConsole creates a Windows pseudo console, starts the configured process attached
// to it, and returns the ConPTY I/O handle owner.
func startWindowsPseudoConsole(config windowsPseudoConsoleConfig) (*windowsPseudoConsole, error) {
	availabilityErr := availabilityError()
	if availabilityErr != nil {
		return nil, fmt.Errorf("%w: %v", errConPTYUnsupported, availabilityErr)
	}
	if config.CommandLine == "" {
		return nil, errors.New("command line is empty")
	}

	consoleSize, sizeErr := consoleSize(config.Cols, config.Rows)
	if sizeErr != nil {
		return nil, sizeErr
	}

	var (
		consoleInputRead   windows.Handle
		consoleInputWrite  windows.Handle
		consoleOutputRead  windows.Handle
		consoleOutputWrite windows.Handle
	)
	inputPipeErr := windows.CreatePipe(&consoleInputRead, &consoleInputWrite, nil, 0)
	if inputPipeErr != nil {
		return nil, fmt.Errorf("create conpty input pipe: %w", inputPipeErr)
	}
	outputPipeErr := windows.CreatePipe(&consoleOutputRead, &consoleOutputWrite, nil, 0)
	if outputPipeErr != nil {
		_ = closeHandles(consoleInputRead, consoleInputWrite)
		return nil, fmt.Errorf("create conpty output pipe: %w", outputPipeErr)
	}

	var console windows.Handle
	createConsoleErr := windows.CreatePseudoConsole(consoleSize, consoleInputRead, consoleOutputWrite, 0, &console)
	if createConsoleErr != nil {
		_ = closeHandles(consoleInputRead, consoleInputWrite, consoleOutputRead, consoleOutputWrite)
		return nil, fmt.Errorf("create conpty: %w", createConsoleErr)
	}

	processInformation, createProcessErr := createProcessAttachedToConsole(console, config.CommandLine, config.Dir, config.Env)
	if createProcessErr != nil {
		_ = closeHandles(consoleInputRead, consoleInputWrite, consoleOutputRead, consoleOutputWrite)
		windows.ClosePseudoConsole(console)
		return nil, createProcessErr
	}

	closeThreadErr := closeHandle(processInformation.Thread)
	if closeThreadErr != nil {
		_ = closeHandles(consoleInputRead, consoleInputWrite, consoleOutputRead, consoleOutputWrite, processInformation.Process)
		windows.ClosePseudoConsole(console)
		return nil, fmt.Errorf("close process thread handle: %w", closeThreadErr)
	}

	return &windowsPseudoConsole{
		console:       console,
		pid:           process.Uint32_ToPidT(processInformation.ProcessId),
		inputWriter:   &handleIO{handle: consoleInputWrite},
		outputReader:  &handleIO{handle: consoleOutputRead},
		consoleInput:  consoleInputRead,
		consoleOutput: consoleOutputWrite,
		processHandle: processInformation.Process,
	}, nil
}

func createProcessAttachedToConsole(
	console windows.Handle,
	commandLine string,
	workDir string,
	env []string,
) (*windows.ProcessInformation, error) {
	commandLinePtr, commandLineErr := windows.UTF16PtrFromString(commandLine)
	if commandLineErr != nil {
		return nil, fmt.Errorf("encode command line: %w", commandLineErr)
	}

	var workDirPtr *uint16
	if workDir != "" {
		var workDirErr error
		workDirPtr, workDirErr = windows.UTF16PtrFromString(workDir)
		if workDirErr != nil {
			return nil, fmt.Errorf("encode working directory: %w", workDirErr)
		}
	}

	creationFlags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT)
	envBlock, envBlockErr := createEnvironmentBlock(env)
	if envBlockErr != nil {
		return nil, envBlockErr
	}
	var envBlockPtr *uint16
	if envBlock != nil {
		creationFlags |= windows.CREATE_UNICODE_ENVIRONMENT
		envBlockPtr = &envBlock[0]
	}

	attributeList, attributeListErr := windows.NewProcThreadAttributeList(1)
	if attributeListErr != nil {
		return nil, fmt.Errorf("create proc-thread attribute list: %w", attributeListErr)
	}
	defer attributeList.Delete()

	updateAttributeErr := attributeList.Update(
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		unsafe.Pointer(uintptr(console)),
		unsafe.Sizeof(console),
	)
	if updateAttributeErr != nil {
		return nil, fmt.Errorf("attach conpty attribute: %w", updateAttributeErr)
	}

	startupInfo := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:    uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Flags: windows.STARTF_USESTDHANDLES,
		},
		ProcThreadAttributeList: attributeList.List(),
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
		workDirPtr,
		&startupInfo.StartupInfo,
		&processInformation,
	)
	runtime.KeepAlive(envBlock)
	if createProcessErr != nil {
		return nil, fmt.Errorf("create conpty-attached process: %w", createProcessErr)
	}

	return &processInformation, nil
}

func consoleSize(cols, rows int) (windows.Coord, error) {
	effectiveCols := cols
	if effectiveCols <= 0 {
		effectiveCols = defaultConsoleWidth
	}
	effectiveRows := rows
	if effectiveRows <= 0 {
		effectiveRows = defaultConsoleHeight
	}

	if effectiveCols > maxConsoleDimension || effectiveRows > maxConsoleDimension {
		return windows.Coord{}, fmt.Errorf("invalid conpty dimensions cols=%d rows=%d", cols, rows)
	}

	return windows.Coord{X: int16(effectiveCols), Y: int16(effectiveRows)}, nil
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

func closeHandle(handle windows.Handle) error {
	if handle == 0 || handle == windows.InvalidHandle {
		return nil
	}
	return windows.CloseHandle(handle)
}

func closeHandles(handles ...windows.Handle) error {
	var firstCloseErr error
	for _, handle := range handles {
		closeErr := closeHandle(handle)
		if firstCloseErr == nil && closeErr != nil {
			firstCloseErr = closeErr
		}
	}
	return firstCloseErr
}

func (pc *windowsPseudoConsole) Read(p []byte) (int, error) {
	return pc.outputReader.Read(p)
}

func (pc *windowsPseudoConsole) Write(p []byte) (int, error) {
	return pc.inputWriter.Write(p)
}

func (pc *windowsPseudoConsole) Resize(cols, rows int) error {
	size, sizeErr := consoleSize(cols, rows)
	if sizeErr != nil {
		return sizeErr
	}

	resizeErr := windows.ResizePseudoConsole(pc.console, size)
	if resizeErr != nil {
		return fmt.Errorf("resize conpty: %w", resizeErr)
	}
	return nil
}

func (pc *windowsPseudoConsole) Close() error {
	pc.closeOnce.Do(func() {
		windows.ClosePseudoConsole(pc.console)
		pc.closeErr = errors.Join(
			pc.closeProcessHandle(),
			closeHandles(pc.consoleInput, pc.consoleOutput),
			pc.inputWriter.Close(),
			pc.outputReader.Close(),
		)
	})
	return pc.closeErr
}

func (pc *windowsPseudoConsole) closeProcessHandle() error {
	pc.processMu.Lock()
	defer pc.processMu.Unlock()

	if pc.processHandleClosed {
		return nil
	}

	closeErr := closeHandle(pc.processHandle)
	if closeErr == nil {
		pc.processHandleClosed = true
	}
	return closeErr
}

func (pc *windowsPseudoConsole) beginWait() (windows.Handle, error) {
	pc.processMu.Lock()
	defer pc.processMu.Unlock()

	if pc.processHandleClosed {
		return 0, errors.New("process handle is closed")
	}
	return pc.processHandle, nil
}

// wait waits for the attached process to exit and returns its Windows exit code.
func (pc *windowsPseudoConsole) wait(ctx context.Context) (uint32, error) {
	processHandle, handleErr := pc.beginWait()
	if handleErr != nil {
		return stillActiveExitCode, handleErr
	}

	for {
		contextErr := ctx.Err()
		if contextErr != nil {
			return stillActiveExitCode, fmt.Errorf("wait canceled: %w", contextErr)
		}

		waitResult, waitErr := windows.WaitForSingleObject(processHandle, 1000)
		if waitResult == uint32(windows.WAIT_TIMEOUT) {
			continue
		}
		if waitErr != nil {
			return stillActiveExitCode, fmt.Errorf("wait for process: %w", waitErr)
		}

		var exitCode uint32
		exitCodeErr := windows.GetExitCodeProcess(processHandle, &exitCode)
		if exitCodeErr != nil {
			return stillActiveExitCode, fmt.Errorf("get process exit code: %w", exitCodeErr)
		}
		return exitCode, nil
	}
}

func (pc *windowsPseudoConsole) processID() process.Pid_t {
	return pc.pid
}
