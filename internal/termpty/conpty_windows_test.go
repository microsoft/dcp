//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestCreateEnvironmentBlock(t *testing.T) {
	block, blockErr := createEnvironmentBlock([]string{"z=last", "A=first"})
	if blockErr != nil {
		t.Fatalf("createEnvironmentBlock returned error: %v", blockErr)
	}

	got := string(utf16.Decode(block))
	want := "A=first\x00z=last\x00\x00"
	if got != want {
		t.Fatalf("environment block = %q, want %q", got, want)
	}
}

func TestCreateEnvironmentBlockNilInheritsEnvironment(t *testing.T) {
	block, blockErr := createEnvironmentBlock(nil)
	if blockErr != nil {
		t.Fatalf("createEnvironmentBlock(nil) returned error: %v", blockErr)
	}
	if block != nil {
		t.Fatalf("createEnvironmentBlock(nil) = %v, want nil", block)
	}
}

func TestCreateEnvironmentBlockRejectsNUL(t *testing.T) {
	_, blockErr := createEnvironmentBlock([]string{"A=B\x00C=D"})
	if blockErr == nil {
		t.Fatalf("createEnvironmentBlock returned nil error for entry containing NUL")
	}
}

func TestConsoleSizeDefaultsAndBounds(t *testing.T) {
	size, sizeErr := consoleSize(0, 0)
	if sizeErr != nil {
		t.Fatalf("consoleSize returned error: %v", sizeErr)
	}
	if size.X != defaultConsoleWidth || size.Y != defaultConsoleHeight {
		t.Fatalf("consoleSize(0, 0) = %dx%d, want %dx%d", size.X, size.Y, defaultConsoleWidth, defaultConsoleHeight)
	}

	_, sizeErr = consoleSize(maxConsoleDimension+1, 24)
	if sizeErr == nil {
		t.Fatalf("consoleSize returned nil error for an out-of-range width")
	}
}

func TestStartEcho(t *testing.T) {
	if !isWindowsPseudoConsoleAvailable() {
		t.Skip("ConPTY is not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pc, startErr := startWindowsPseudoConsole(windowsPseudoConsoleConfig{
		CommandLine: `C:\Windows\System32\cmd.exe /c echo hello-from-internal-conpty`,
		Cols:        80,
		Rows:        24,
	})
	if startErr != nil {
		t.Fatalf("Start returned error: %v", startErr)
	}
	defer pc.Close()

	outputCh := make(chan string, 1)
	readErrCh := make(chan error, 1)
	go func() {
		var output bytes.Buffer
		buffer := make([]byte, 4096)
		for {
			readCount, readErr := pc.Read(buffer)
			if readCount > 0 {
				output.Write(buffer[:readCount])
				if strings.Contains(output.String(), "hello-from-internal-conpty") {
					outputCh <- output.String()
					return
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					outputCh <- output.String()
					return
				}
				readErrCh <- readErr
				return
			}
		}
	}()

	waitCh := make(chan uint32, 1)
	waitErrCh := make(chan error, 1)
	go func() {
		exitCode, waitErr := pc.wait(ctx)
		if waitErr != nil {
			waitErrCh <- waitErr
			return
		}
		waitCh <- exitCode
	}()

	select {
	case output := <-outputCh:
		if !strings.Contains(output, "hello-from-internal-conpty") {
			t.Fatalf("output = %q, want hello marker", output)
		}
	case readErr := <-readErrCh:
		t.Fatalf("Read returned error: %v", readErr)
	case waitErr := <-waitErrCh:
		t.Fatalf("Wait returned error before output: %v", waitErr)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for output: %v", ctx.Err())
	}

	select {
	case exitCode := <-waitCh:
		if exitCode != 0 {
			t.Fatalf("exit code = %d, want 0", exitCode)
		}
	case waitErr := <-waitErrCh:
		t.Fatalf("Wait returned error: %v", waitErr)
	case <-ctx.Done():
		t.Fatalf("timed out waiting for process exit: %v", ctx.Err())
	}
}
