/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// containerprobe supplies deterministic operations for true container-runtime tests.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

const filePollInterval = 50 * time.Millisecond

func main() {
	if runErr := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); runErr != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", runErr)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("command is required")
	}

	switch args[0] {
	case "wait":
		return runWait(args[1:])
	case "exit":
		return runExit(args[1:])
	case "report":
		return runReport(args[1:], stdout, stderr)
	case "inspect-files":
		return runInspectFiles(args[1:], stdout)
	case "emit":
		return runEmit(args[1:], stdout, stderr)
	case "write-and-wait":
		return runWriteAndWait(args[1:])
	case "wait-read":
		return runWaitRead(args[1:], stdout)
	case "cat":
		return runCat(args[1:], stdout)
	case "interactive":
		return runInteractive(args[1:], stdin, stdout)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runWait(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("wait requires a duration")
	}
	duration, parseErr := time.ParseDuration(args[0])
	if parseErr != nil {
		return fmt.Errorf("parsing wait duration: %w", parseErr)
	}
	time.Sleep(duration)
	return nil
}

func runExit(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("exit does not accept arguments")
	}
	return nil
}

func runReport(args []string, stdout io.Writer, stderr io.Writer) error {
	if len(args) != 2 {
		return fmt.Errorf("report requires an environment variable name and stderr text")
	}
	workingDirectory, workingDirectoryErr := os.Getwd()
	if workingDirectoryErr != nil {
		return fmt.Errorf("getting working directory: %w", workingDirectoryErr)
	}
	if _, stdoutErr := fmt.Fprintf(stdout, "%s:%s", os.Getenv(args[0]), workingDirectory); stdoutErr != nil {
		return fmt.Errorf("writing report stdout: %w", stdoutErr)
	}
	if _, stderrErr := io.WriteString(stderr, args[1]); stderrErr != nil {
		return fmt.Errorf("writing report stderr: %w", stderrErr)
	}
	return nil
}

func runInspectFiles(args []string, stdout io.Writer) error {
	if len(args) != 3 {
		return fmt.Errorf("inspect-files requires two file paths and one symbolic link path")
	}

	firstContents, firstReadErr := readFile(args[0])
	if firstReadErr != nil {
		return fmt.Errorf("reading first file: %w", firstReadErr)
	}
	secondContents, secondReadErr := readFile(args[1])
	if secondReadErr != nil {
		return fmt.Errorf("reading second file: %w", secondReadErr)
	}
	linkTarget, linkReadErr := os.Readlink(args[2])
	if linkReadErr != nil {
		return fmt.Errorf("reading symbolic link: %w", linkReadErr)
	}

	if _, writeErr := fmt.Fprintf(stdout, "%s|%s|%s", firstContents, secondContents, linkTarget); writeErr != nil {
		return fmt.Errorf("writing inspected files: %w", writeErr)
	}
	return nil
}

func runEmit(args []string, stdout io.Writer, stderr io.Writer) error {
	if len(args) != 2 {
		return fmt.Errorf("emit requires stdout and stderr text")
	}
	if _, stdoutErr := io.WriteString(stdout, args[0]); stdoutErr != nil {
		return fmt.Errorf("writing stdout: %w", stdoutErr)
	}
	if _, stderrErr := io.WriteString(stderr, args[1]); stderrErr != nil {
		return fmt.Errorf("writing stderr: %w", stderrErr)
	}
	return nil
}

func runWriteAndWait(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("write-and-wait requires a path, contents, and duration")
	}
	duration, parseErr := time.ParseDuration(args[2])
	if parseErr != nil {
		return fmt.Errorf("parsing wait duration: %w", parseErr)
	}
	if writeErr := usvc_io.WriteFile(args[0], []byte(args[1]), osutil.PermissionOnlyOwnerReadWrite); writeErr != nil {
		return fmt.Errorf("writing file: %w", writeErr)
	}
	time.Sleep(duration)
	return nil
}

func runWaitRead(args []string, stdout io.Writer) error {
	if len(args) != 2 {
		return fmt.Errorf("wait-read requires a path and timeout")
	}
	timeout, parseErr := time.ParseDuration(args[1])
	if parseErr != nil {
		return fmt.Errorf("parsing read timeout: %w", parseErr)
	}

	deadline := time.Now().Add(timeout)
	for {
		contents, readErr := readFile(args[0])
		if readErr == nil {
			if _, writeErr := stdout.Write(contents); writeErr != nil {
				return fmt.Errorf("writing file contents: %w", writeErr)
			}
			return nil
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			return fmt.Errorf("reading file: %w", readErr)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timed out waiting for %q", args[0])
		}
		time.Sleep(filePollInterval)
	}
}

func runCat(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("cat requires a path")
	}
	contents, readErr := readFile(args[0])
	if readErr != nil {
		return fmt.Errorf("reading file: %w", readErr)
	}
	if _, writeErr := stdout.Write(contents); writeErr != nil {
		return fmt.Errorf("writing file contents: %w", writeErr)
	}
	return nil
}

func runInteractive(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("interactive does not accept arguments")
	}

	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "exit" {
			return nil
		}
		if _, writeErr := fmt.Fprintf(stdout, "probe:%s\n", line); writeErr != nil {
			return fmt.Errorf("writing interactive response: %w", writeErr)
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return fmt.Errorf("reading interactive input: %w", scanErr)
	}
	return nil
}

func readFile(path string) ([]byte, error) {
	file, openErr := usvc_io.OpenFileReadOnly(path)
	if openErr != nil {
		return nil, openErr
	}
	defer file.Close()

	contents, readErr := io.ReadAll(file)
	if readErr != nil {
		return nil, readErr
	}
	return contents, nil
}
