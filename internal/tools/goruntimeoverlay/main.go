/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	dcpio "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

//go:embed go-81009.patch
var darwinRuntimePatch []byte

type overlayConfig struct {
	Replace map[string]string
}

func main() {
	outputDirectory := flag.String("output-dir", "", "Directory where the runtime overlay is generated")
	flag.Parse()

	if *outputDirectory == "" {
		_, _ = fmt.Fprintln(os.Stderr, "--output-dir is required")
		os.Exit(2)
	}

	generateErr := generateRuntimeOverlay(context.Background(), runtime.GOROOT(), *outputDirectory)
	if generateErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "could not generate Go runtime overlay: %v\n", generateErr)
		os.Exit(1)
	}
}

func generateRuntimeOverlay(ctx context.Context, goRoot string, outputDirectory string) error {
	runtimeSourcePath := filepath.Join(goRoot, "src", "runtime", "os_darwin.go")
	runtimeSource, readErr := readFile(runtimeSourcePath)
	if readErr != nil {
		return fmt.Errorf("reading %q: %w", runtimeSourcePath, readErr)
	}

	patchedSource, patchRequired, patchErr := applyDarwinRuntimePatch(ctx, runtimeSource)
	if patchErr != nil {
		return fmt.Errorf("patching %q: %w", runtimeSourcePath, patchErr)
	}

	makeOutputDirectoryErr := os.MkdirAll(outputDirectory, osutil.PermissionDirectoryOthersRead)
	if makeOutputDirectoryErr != nil {
		return fmt.Errorf("creating output directory %q: %w", outputDirectory, makeOutputDirectoryErr)
	}

	replacements := make(map[string]string, 1)
	if patchRequired {
		patchedSourceDirectory := filepath.Join(outputDirectory, "runtime")
		makeSourceDirectoryErr := os.MkdirAll(
			patchedSourceDirectory,
			osutil.PermissionDirectoryOthersRead,
		)
		if makeSourceDirectoryErr != nil {
			return fmt.Errorf("creating patched runtime directory %q: %w", patchedSourceDirectory, makeSourceDirectoryErr)
		}

		patchedSourcePath := filepath.Join(patchedSourceDirectory, "os_darwin.go")
		writeSourceErr := writeFileIfChanged(patchedSourcePath, patchedSource)
		if writeSourceErr != nil {
			return fmt.Errorf("writing patched runtime source %q: %w", patchedSourcePath, writeSourceErr)
		}

		replacements[runtimeSourcePath] = patchedSourcePath
	}

	config := overlayConfig{Replace: replacements}
	configContents, marshalErr := json.MarshalIndent(config, "", "  ")
	if marshalErr != nil {
		return fmt.Errorf("encoding overlay configuration: %w", marshalErr)
	}
	configContents = append(configContents, '\n')

	configPath := filepath.Join(outputDirectory, "overlay.json")
	writeConfigErr := writeFileIfChanged(configPath, configContents)
	if writeConfigErr != nil {
		return fmt.Errorf("writing overlay configuration %q: %w", configPath, writeConfigErr)
	}

	return nil
}

func applyDarwinRuntimePatch(ctx context.Context, source []byte) ([]byte, bool, error) {
	stagingDirectory, createTempErr := os.MkdirTemp("", "dcp-go-runtime-overlay-")
	if createTempErr != nil {
		return nil, false, fmt.Errorf("creating patch staging directory: %w", createTempErr)
	}
	defer func() {
		_ = os.RemoveAll(stagingDirectory)
	}()

	stagedSourceDirectory := filepath.Join(stagingDirectory, "src", "runtime")
	makeSourceDirectoryErr := os.MkdirAll(stagedSourceDirectory, osutil.PermissionDirectoryOthersRead)
	if makeSourceDirectoryErr != nil {
		return nil, false, fmt.Errorf("creating patch source directory: %w", makeSourceDirectoryErr)
	}

	stagedSourcePath := filepath.Join(stagedSourceDirectory, "os_darwin.go")
	writeSourceErr := dcpio.WriteFile(
		stagedSourcePath,
		source,
		osutil.PermissionOwnerReadWriteOthersRead,
	)
	if writeSourceErr != nil {
		return nil, false, fmt.Errorf("writing patch source: %w", writeSourceErr)
	}

	forwardCheckOutput, forwardCheckErr := runGitApply(ctx, stagingDirectory, false, true)
	if forwardCheckErr == nil {
		applyOutput, applyErr := runGitApply(ctx, stagingDirectory, false, false)
		if applyErr != nil {
			return nil, false, fmt.Errorf("applying upstream patch: %w: %s", applyErr, applyOutput)
		}

		patchedSource, patchedSourceErr := readFile(stagedSourcePath)
		if patchedSourceErr != nil {
			return nil, false, fmt.Errorf("reading patched runtime source: %w", patchedSourceErr)
		}

		return patchedSource, true, nil
	}
	if !gitApplyRejectedPatch(forwardCheckErr) {
		return nil, false, fmt.Errorf(
			"checking whether the upstream patch applies: %w: %s",
			forwardCheckErr,
			forwardCheckOutput,
		)
	}

	reverseCheckOutput, reverseCheckErr := runGitApply(ctx, stagingDirectory, true, true)
	if reverseCheckErr == nil {
		return source, false, nil
	}
	if !gitApplyRejectedPatch(reverseCheckErr) {
		return nil, false, fmt.Errorf(
			"checking whether the upstream patch was already applied: %w: %s",
			reverseCheckErr,
			reverseCheckOutput,
		)
	}

	return nil, false, fmt.Errorf(
		"upstream patch applies neither forward nor in reverse (forward: %v: %s; reverse: %v: %s)",
		forwardCheckErr,
		forwardCheckOutput,
		reverseCheckErr,
		reverseCheckOutput,
	)
}

func runGitApply(
	ctx context.Context,
	workingDirectory string,
	reverse bool,
	check bool,
) ([]byte, error) {
	args := []string{
		"-c", "core.autocrlf=false",
		"-c", "core.eol=lf",
		"-C", workingDirectory,
		"apply",
		"--whitespace=nowarn",
	}
	if reverse {
		args = append(args, "--reverse")
	}
	if check {
		args = append(args, "--check")
	}
	args = append(args, "-")

	applyCommand := exec.CommandContext(ctx, "git", args...)
	normalizedPatch := bytes.ReplaceAll(darwinRuntimePatch, []byte("\r\n"), []byte("\n"))
	applyCommand.Stdin = bytes.NewReader(normalizedPatch)
	output, applyErr := applyCommand.CombinedOutput()
	return output, applyErr
}

func gitApplyRejectedPatch(applyErr error) bool {
	var exitErr *exec.ExitError
	return errors.As(applyErr, &exitErr)
}

func readFile(path string) ([]byte, error) {
	file, openErr := dcpio.OpenFileReadOnly(path)
	if openErr != nil {
		return nil, openErr
	}

	contents, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if fileErr := errors.Join(readErr, closeErr); fileErr != nil {
		return nil, fileErr
	}

	return contents, nil
}

func writeFileIfChanged(path string, contents []byte) error {
	existingContents, readErr := readFile(path)
	switch {
	case readErr == nil && bytes.Equal(existingContents, contents):
		return nil
	case readErr == nil:
	case errors.Is(readErr, os.ErrNotExist):
	default:
		return readErr
	}

	return dcpio.WriteFile(path, contents, osutil.PermissionOwnerReadWriteOthersRead)
}
