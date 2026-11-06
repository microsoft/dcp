/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	dcpio "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

const (
	unpatchedRuntimeSource = `package runtime

func before() {}

type sigset uint32

var sigset_all = ^sigset(0)

//go:nosplit
//go:nowritebarrierrec
func setsig(i uint32, fn uintptr) {
	var sa usigactiont
	sa.sa_flags = _SA_SIGINFO | _SA_ONSTACK | _SA_RESTART
	sa.sa_mask = ^uint32(0)
	if fn == abi.FuncPCABIInternal(sighandler) { // abi.FuncPCABIInternal(sighandler) matches the callers in signal_unix.go
		if iscgo {
		}
	}
}

func after() {}
`
	patchedRuntimeSource = `package runtime

func before() {}

type sigset uint32

var sigset_all = ^sigset(0)

//go:nosplit
//go:nowritebarrierrec
func setsig(i uint32, fn uintptr) {
	var sa usigactiont

	sa.sa_flags = _SA_ONSTACK | _SA_RESTART
	// SA_SIGINFO should not be set when assigning SIG_DFL or SIG_IGN
	if fn != _SIG_DFL && fn != _SIG_IGN {
		sa.sa_flags |= _SA_SIGINFO
	}
	sa.sa_mask = ^uint32(0)
	if fn == abi.FuncPCABIInternal(sighandler) { // abi.FuncPCABIInternal(sighandler) matches the callers in signal_unix.go
		if iscgo {
		}
	}
}

func after() {}
`
)

// Verifies that generateBuildOverlay patches the pinned dependency on every supported OS and preserves the Darwin runtime fix.
func TestGenerateBuildOverlay(t *testing.T) {
	t.Parallel()

	for _, targetOS := range []string{"darwin", "linux", "windows"} {
		t.Run(targetOS, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			dependency, dependencyErr := resolveControllerRuntime(ctx)
			require.NoError(t, dependencyErr)
			sourcePath := filepath.Join(dependency.Dir, "pkg", "manager", "runnable_group.go")
			originalSource, sourceReadErr := readFile(sourcePath)
			require.NoError(t, sourceReadErr)

			goRoot := t.TempDir()
			runtimeSourcePath := filepath.Join(goRoot, "src", "runtime", "os_darwin.go")
			require.NoError(t, os.MkdirAll(filepath.Dir(runtimeSourcePath), osutil.PermissionDirectoryOthersRead))
			require.NoError(t, dcpio.WriteFile(runtimeSourcePath, []byte(unpatchedRuntimeSource), osutil.PermissionOwnerReadWriteOthersRead))
			moduleFile := filepath.Join(t.TempDir(), "go.mod")
			require.NoError(t, dcpio.WriteFile(moduleFile,
				[]byte("module example.com/overlaytest\n\ngo 1.27.1\n\nrequire "+controllerRuntimeModule+" "+controllerRuntimeVersion+"\n"),
				osutil.PermissionOwnerReadWriteOthersRead))
			require.NoError(t, dcpio.WriteFile(filepath.Join(filepath.Dir(moduleFile), "go.sum"), nil,
				osutil.PermissionOwnerReadWriteOthersRead))
			outputDirectory := filepath.Join(t.TempDir(), "overlay with spaces")
			require.NoError(t, generateBuildOverlay(ctx, goRoot, targetOS, dependency, moduleFile, outputDirectory))

			configPath := filepath.Join(outputDirectory, "overlay.json")
			configContents, configReadErr := readFile(configPath)
			require.NoError(t, configReadErr)
			var config overlayConfig
			require.NoError(t, json.Unmarshal(configContents, &config))
			stagedDependency := filepath.Join(outputDirectory, "modules", "controller-runtime@"+dependency.Version)
			stagedSourcePath := filepath.Join(stagedDependency, "pkg", "manager", "runnable_group.go")
			patchedSource, patchedReadErr := readFile(config.Replace[stagedSourcePath])
			require.NoError(t, patchedReadErr)
			require.NotContains(t, string(patchedSource), "defer close(r.startReadyCh)")
			require.Contains(t, string(patchedSource), "retErr = err\n\t\t\t\t}\n\t\t\t\treturn")
			require.Contains(t, string(patchedSource), "case r.startReadyCh <- rn:\n\t\t\t\t\t\tcase <-r.ctx.Done():")
			unmodifiedSource, unmodifiedReadErr := readFile(sourcePath)
			require.NoError(t, unmodifiedReadErr)
			require.Equal(t, originalSource, unmodifiedSource)
			stagedSource, stagedSourceErr := readFile(stagedSourcePath)
			require.NoError(t, stagedSourceErr)
			require.Equal(t, originalSource, stagedSource)
			generatedModule, moduleReadErr := readFile(filepath.Join(outputDirectory, "go.mod"))
			require.NoError(t, moduleReadErr)
			require.Contains(t, string(generatedModule), controllerRuntimeModule+" "+controllerRuntimeVersion+" => ")
			require.NotContains(t, string(configContents), dependency.Dir)

			if targetOS == "darwin" {
				require.Len(t, config.Replace, 2)
				patchedRuntime, runtimeReadErr := readFile(config.Replace[runtimeSourcePath])
				require.NoError(t, runtimeReadErr)
				require.Equal(t, patchedRuntimeSource, string(patchedRuntime))
			} else {
				require.Len(t, config.Replace, 1)
			}

			require.NoError(t, generateBuildOverlay(ctx, goRoot, targetOS, dependency, moduleFile, outputDirectory))
			repeatedConfig, repeatedReadErr := readFile(configPath)
			require.NoError(t, repeatedReadErr)
			require.Equal(t, configContents, repeatedConfig)
			repeatedModule, repeatedModuleReadErr := readFile(filepath.Join(outputDirectory, "go.mod"))
			require.NoError(t, repeatedModuleReadErr)
			require.Equal(t, generatedModule, repeatedModule)

			require.NoError(t, dcpio.WriteFile(stagedSourcePath, []byte("unexpected staged source\n"),
				osutil.PermissionOwnerReadWriteOthersRead))
			corruptedStageErr := generateBuildOverlay(ctx, goRoot, targetOS, dependency, moduleFile, outputDirectory)
			require.ErrorContains(t, corruptedStageErr, "staged controller-runtime source differs")
			require.NoError(t, dcpio.WriteFile(stagedSourcePath, originalSource, osutil.PermissionOwnerReadWriteOthersRead))

			patchedDependency := dependency
			patchedDependency.Dir = stagedDependency
			require.NoError(t, dcpio.WriteFile(stagedSourcePath, patchedSource, osutil.PermissionOwnerReadWriteOthersRead))
			alreadyPatchedOutput := t.TempDir()
			require.NoError(t, generateBuildOverlay(ctx, goRoot, "linux", patchedDependency, moduleFile, alreadyPatchedOutput))
			alreadyPatchedConfig, alreadyPatchedReadErr := readFile(filepath.Join(alreadyPatchedOutput, "overlay.json"))
			require.NoError(t, alreadyPatchedReadErr)
			require.JSONEq(t, `{"Replace": {}}`, string(alreadyPatchedConfig))
		})
	}
}

// Verifies that applySourcePatch recognizes a toolchain already containing golang/go#81009 without changing its source.
func TestDarwinRuntimePatchAlreadyApplied(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	source, required, patchErr := applySourcePatch(ctx, []byte(patchedRuntimeSource),
		filepath.Join("src", "runtime", "os_darwin.go"), darwinRuntimePatch)
	require.NoError(t, patchErr)
	require.False(t, required)
	require.Equal(t, patchedRuntimeSource, string(source))
}

// Verifies that applySourcePatch rejects unexpected dependency and Go runtime source rather than silently skipping a fix.
func TestBuildOverlayRejectsUnexpectedSource(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	for _, patch := range []sourcePatch{
		{relativePath: filepath.Join("src", "runtime", "os_darwin.go"), contents: darwinRuntimePatch},
		{relativePath: filepath.Join("pkg", "manager", "runnable_group.go"), contents: controllerRuntimePatch},
	} {
		_, _, patchErr := applySourcePatch(ctx, []byte("package unexpected\n"), patch.relativePath, patch.contents)
		require.ErrorContains(t, patchErr, "upstream patch applies neither forward nor in reverse")
	}
}

// Verifies that generateBuildOverlay rejects dependency replacements, version drift, missing source, and unsupported targets.
func TestBuildOverlayRejectsUnexpectedModule(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	expected := moduleInfo{Path: controllerRuntimeModule, Version: controllerRuntimeVersion, Dir: t.TempDir()}
	for _, mutation := range []func(*moduleInfo){
		func(info *moduleInfo) { info.Version = "v0.25.2" },
		func(info *moduleInfo) { info.Path = "example.com/other" },
		func(info *moduleInfo) { info.Dir = "" },
		func(info *moduleInfo) { info.Replace = &moduleInfo{Dir: t.TempDir()} },
	} {
		dependency := expected
		mutation(&dependency)
		generateErr := generateBuildOverlay(ctx, runtime.GOROOT(), "linux", dependency, "", t.TempDir())
		require.ErrorContains(t, generateErr, "expected unmodified "+controllerRuntimeModule)
	}
	targetErr := generateBuildOverlay(ctx, runtime.GOROOT(), "unsupported", expected, "", t.TempDir())
	require.ErrorContains(t, targetErr, "unsupported target operating system")

	expected.Dir = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(expected.Dir, "pkg", "manager"), osutil.PermissionDirectoryOthersRead))
	require.NoError(t, dcpio.WriteFile(filepath.Join(expected.Dir, "pkg", "manager", "runnable_group.go"),
		[]byte(strings.Repeat("unexpected source\n", 3)), osutil.PermissionOwnerReadWriteOthersRead))
	sourceErr := generateBuildOverlay(ctx, runtime.GOROOT(), "linux", expected, "", t.TempDir())
	require.ErrorContains(t, sourceErr, "upstream patch applies neither forward nor in reverse")
}
