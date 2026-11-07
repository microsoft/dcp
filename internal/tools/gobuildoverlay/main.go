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

// Remove this patch when DCP adopts the fix for kubernetes-sigs/controller-runtime#3629.
//
//go:embed controller-runtime-3629.patch
var controllerRuntimePatch []byte

const (
	controllerRuntimeModule  = "sigs.k8s.io/controller-runtime"
	controllerRuntimeVersion = "v0.25.0"
)

type overlayConfig struct {
	Replace map[string]string
}

type moduleInfo struct {
	Path    string
	Version string
	Dir     string
	Replace *moduleInfo
}

type sourcePatch struct {
	root         string
	name         string
	relativePath string
	contents     []byte
}

func main() {
	outputDirectory := flag.String("output-dir", "", "Directory where the build overlay is generated")
	targetOS := flag.String("target-os", runtime.GOOS, "Target operating system for the build overlay")
	flag.Parse()

	if *outputDirectory == "" {
		_, _ = fmt.Fprintln(os.Stderr, "--output-dir is required")
		os.Exit(2)
	}

	ctx := context.Background()
	dependency, dependencyErr := resolveControllerRuntime(ctx)
	if dependencyErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "could not resolve controller-runtime source: %v\n", dependencyErr)
		os.Exit(1)
	}

	moduleRoot, moduleRootErr := osutil.FindRootFor(osutil.FileTarget, "go.mod")
	if moduleRootErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "could not locate main module: %v\n", moduleRootErr)
		os.Exit(1)
	}

	generateErr := generateBuildOverlay(ctx, runtime.GOROOT(), *targetOS, dependency,
		filepath.Join(moduleRoot, "go.mod"), *outputDirectory)
	if generateErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "could not generate Go build overlay: %v\n", generateErr)
		os.Exit(1)
	}
}

func resolveControllerRuntime(ctx context.Context) (moduleInfo, error) {
	listCommand := exec.CommandContext(ctx, goExecutablePath(),
		"list", "-mod=readonly", "-find", "-json", controllerRuntimeModule+"/pkg/manager")
	listOutput, listErr := listCommand.Output()
	if listErr != nil {
		var exitErr *exec.ExitError
		if errors.As(listErr, &exitErr) {
			return moduleInfo{}, fmt.Errorf("locating module: %w: %s", listErr, exitErr.Stderr)
		}
		return moduleInfo{}, fmt.Errorf("locating module: %w", listErr)
	}

	var packageInfo struct {
		Module moduleInfo
	}
	if decodeErr := json.Unmarshal(listOutput, &packageInfo); decodeErr != nil {
		return moduleInfo{}, fmt.Errorf("decoding module information: %w", decodeErr)
	}
	return packageInfo.Module, nil
}

func goExecutablePath() string {
	goName := "go"
	if runtime.GOOS == "windows" {
		goName += ".exe"
	}
	return filepath.Join(runtime.GOROOT(), "bin", goName)
}

func generateBuildOverlay(
	ctx context.Context,
	goRoot string,
	targetOS string,
	dependency moduleInfo,
	moduleFile string,
	outputDirectory string,
) error {
	if dependency.Path != controllerRuntimeModule || dependency.Version != controllerRuntimeVersion ||
		dependency.Dir == "" || dependency.Replace != nil {
		return fmt.Errorf("expected unmodified %s %s source; found module %q version %q, directory %q, replaced: %t",
			controllerRuntimeModule, controllerRuntimeVersion, dependency.Path, dependency.Version,
			dependency.Dir, dependency.Replace != nil)
	}

	if targetOS != "darwin" && targetOS != "linux" && targetOS != "windows" {
		return fmt.Errorf("unsupported target operating system %q", targetOS)
	}

	absoluteOutputDirectory, absolutePathErr := filepath.Abs(outputDirectory)
	if absolutePathErr != nil {
		return fmt.Errorf("resolving output directory %q: %w", outputDirectory, absolutePathErr)
	}

	stagedDependency, stagingErr := stageControllerRuntime(dependency, absoluteOutputDirectory)
	if stagingErr != nil {
		return stagingErr
	}

	patches := []sourcePatch{{
		root:         stagedDependency,
		name:         "controller-runtime",
		relativePath: filepath.Join("pkg", "manager", "runnable_group.go"),
		contents:     controllerRuntimePatch,
	}}
	if targetOS == "darwin" {
		patches = append(patches, sourcePatch{
			root:         goRoot,
			name:         "go-runtime",
			relativePath: filepath.Join("src", "runtime", "os_darwin.go"),
			contents:     darwinRuntimePatch,
		})
	}

	replacements := make(map[string]string, len(patches))
	for _, patch := range patches {
		sourcePath := filepath.Join(patch.root, patch.relativePath)
		source, readErr := readFile(sourcePath)
		if readErr != nil {
			return fmt.Errorf("reading %q: %w", sourcePath, readErr)
		}

		patchedSource, patchRequired, patchErr := applySourcePatch(ctx, source, patch.relativePath, patch.contents)
		if patchErr != nil {
			return fmt.Errorf("patching %q: %w", sourcePath, patchErr)
		}
		if !patchRequired {
			continue
		}

		patchedSourcePath := filepath.Join(absoluteOutputDirectory, patch.name, patch.relativePath)
		makeSourceDirectoryErr := os.MkdirAll(filepath.Dir(patchedSourcePath), osutil.PermissionDirectoryOthersRead)
		if makeSourceDirectoryErr != nil {
			return fmt.Errorf("creating patched source directory: %w", makeSourceDirectoryErr)
		}

		writeSourceErr := writeFileIfChanged(patchedSourcePath, patchedSource)
		if writeSourceErr != nil {
			return fmt.Errorf("writing patched source %q: %w", patchedSourcePath, writeSourceErr)
		}

		replacements[sourcePath] = patchedSourcePath
	}

	makeOutputDirectoryErr := os.MkdirAll(absoluteOutputDirectory, osutil.PermissionDirectoryOthersRead)
	if makeOutputDirectoryErr != nil {
		return fmt.Errorf("creating output directory %q: %w", absoluteOutputDirectory, makeOutputDirectoryErr)
	}

	config := overlayConfig{Replace: replacements}
	configContents, marshalErr := json.MarshalIndent(config, "", "  ")
	if marshalErr != nil {
		return fmt.Errorf("encoding overlay configuration: %w", marshalErr)
	}
	configContents = append(configContents, '\n')

	configPath := filepath.Join(absoluteOutputDirectory, "overlay.json")
	writeConfigErr := writeFileIfChanged(configPath, configContents)
	if writeConfigErr != nil {
		return fmt.Errorf("writing overlay configuration %q: %w", configPath, writeConfigErr)
	}

	if moduleFileErr := generateModuleFile(ctx, moduleFile, stagedDependency, absoluteOutputDirectory); moduleFileErr != nil {
		return moduleFileErr
	}

	return nil
}

func stageControllerRuntime(dependency moduleInfo, outputDirectory string) (string, error) {
	stagedDirectory := filepath.Join(outputDirectory, "modules", "controller-runtime@"+dependency.Version)
	sourcePath := filepath.Join("pkg", "manager", "runnable_group.go")
	originalSource, originalReadErr := readFile(filepath.Join(dependency.Dir, sourcePath))
	if originalReadErr != nil {
		return "", fmt.Errorf("reading controller-runtime source: %w", originalReadErr)
	}
	stagedSource, stagedReadErr := readFile(filepath.Join(stagedDirectory, sourcePath))
	if stagedReadErr == nil {
		if !bytes.Equal(stagedSource, originalSource) {
			return "", fmt.Errorf("staged controller-runtime source differs from the pinned module; remove %q and regenerate", stagedDirectory)
		}
		return stagedDirectory, nil
	}
	if !errors.Is(stagedReadErr, os.ErrNotExist) {
		return "", fmt.Errorf("reading staged controller-runtime source: %w", stagedReadErr)
	}

	stagingParent := filepath.Dir(stagedDirectory)
	if parentErr := os.MkdirAll(stagingParent, osutil.PermissionDirectoryOthersRead); parentErr != nil {
		return "", fmt.Errorf("creating module staging directory: %w", parentErr)
	}
	temporaryDirectory, temporaryErr := os.MkdirTemp(stagingParent, ".controller-runtime-")
	if temporaryErr != nil {
		return "", fmt.Errorf("creating temporary module directory: %w", temporaryErr)
	}
	defer func() { _ = os.RemoveAll(temporaryDirectory) }()
	if copyErr := os.CopyFS(temporaryDirectory, os.DirFS(dependency.Dir)); copyErr != nil {
		return "", fmt.Errorf("staging controller-runtime source: %w", copyErr)
	}
	if renameErr := os.Rename(temporaryDirectory, stagedDirectory); renameErr != nil {
		// Another generator may have published the same immutable module copy.
		publishedSource, publishedReadErr := readFile(filepath.Join(stagedDirectory, sourcePath))
		if publishedReadErr != nil || !bytes.Equal(publishedSource, originalSource) {
			return "", fmt.Errorf("publishing staged module: %w", errors.Join(renameErr, publishedReadErr))
		}
	}
	return stagedDirectory, nil
}

func generateModuleFile(ctx context.Context, moduleFile string, stagedDependency string, outputDirectory string) error {
	moduleContents, moduleReadErr := readFile(moduleFile)
	if moduleReadErr != nil {
		return fmt.Errorf("reading main module file: %w", moduleReadErr)
	}
	sumContents, sumReadErr := readFile(filepath.Join(filepath.Dir(moduleFile), "go.sum"))
	if sumReadErr != nil {
		return fmt.Errorf("reading main module checksums: %w", sumReadErr)
	}
	temporaryDirectory, temporaryErr := os.MkdirTemp(outputDirectory, ".modfile-")
	if temporaryErr != nil {
		return fmt.Errorf("creating temporary modfile directory: %w", temporaryErr)
	}
	defer func() { _ = os.RemoveAll(temporaryDirectory) }()
	temporaryModuleFile := filepath.Join(temporaryDirectory, "go.mod")
	if writeErr := dcpio.WriteFile(temporaryModuleFile, moduleContents, osutil.PermissionOwnerReadWriteOthersRead); writeErr != nil {
		return fmt.Errorf("staging main module file: %w", writeErr)
	}
	editCommand := exec.CommandContext(ctx, goExecutablePath(), "mod", "edit", "-modfile="+temporaryModuleFile,
		"-replace="+controllerRuntimeModule+"@"+controllerRuntimeVersion+"="+stagedDependency)
	editOutput, editErr := editCommand.CombinedOutput()
	if editErr != nil {
		return fmt.Errorf("configuring staged dependency: %w: %s", editErr, editOutput)
	}
	editedModule, editedReadErr := readFile(temporaryModuleFile)
	if editedReadErr != nil {
		return fmt.Errorf("reading generated module file: %w", editedReadErr)
	}
	if moduleWriteErr := writeFileIfChanged(filepath.Join(outputDirectory, "go.mod"), editedModule); moduleWriteErr != nil {
		return fmt.Errorf("writing generated module file: %w", moduleWriteErr)
	}
	if sumWriteErr := writeFileIfChanged(filepath.Join(outputDirectory, "go.sum"), sumContents); sumWriteErr != nil {
		return fmt.Errorf("writing generated module checksums: %w", sumWriteErr)
	}
	return nil
}

func applySourcePatch(ctx context.Context, source []byte, relativePath string, patch []byte) ([]byte, bool, error) {
	stagingDirectory, createTempErr := os.MkdirTemp("", "dcp-go-build-overlay-")
	if createTempErr != nil {
		return nil, false, fmt.Errorf("creating patch staging directory: %w", createTempErr)
	}
	defer func() {
		_ = os.RemoveAll(stagingDirectory)
	}()

	stagedSourcePath := filepath.Join(stagingDirectory, relativePath)
	stagedSourceDirectory := filepath.Dir(stagedSourcePath)
	makeSourceDirectoryErr := os.MkdirAll(stagedSourceDirectory, osutil.PermissionDirectoryOthersRead)
	if makeSourceDirectoryErr != nil {
		return nil, false, fmt.Errorf("creating patch source directory: %w", makeSourceDirectoryErr)
	}

	writeSourceErr := dcpio.WriteFile(
		stagedSourcePath,
		source,
		osutil.PermissionOwnerReadWriteOthersRead,
	)
	if writeSourceErr != nil {
		return nil, false, fmt.Errorf("writing patch source: %w", writeSourceErr)
	}

	forwardCheckOutput, forwardCheckErr := runGitApply(ctx, stagingDirectory, patch, false, true)
	if forwardCheckErr == nil {
		applyOutput, applyErr := runGitApply(ctx, stagingDirectory, patch, false, false)
		if applyErr != nil {
			return nil, false, fmt.Errorf("applying upstream patch: %w: %s", applyErr, applyOutput)
		}

		patchedSource, patchedSourceErr := readFile(stagedSourcePath)
		if patchedSourceErr != nil {
			return nil, false, fmt.Errorf("reading patched source: %w", patchedSourceErr)
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

	reverseCheckOutput, reverseCheckErr := runGitApply(ctx, stagingDirectory, patch, true, true)
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
	patch []byte,
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
	normalizedPatch := bytes.ReplaceAll(patch, []byte("\r\n"), []byte("\n"))
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
