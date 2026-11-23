/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that FileRegistry creates closed files in separate directories, removes
// one file without affecting another, and cleans up the remaining files after closing.
func TestFileRegistryOwnsCreatedFiles(t *testing.T) {
	t.Parallel()
	registry := usvc_io.NewFileRegistry(t.TempDir())
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	firstPath, firstErr := registry.Create(ctx, "first-", "first.json", []byte("first credentials"))
	require.NoError(t, firstErr)
	secondPath, secondErr := registry.Create(ctx, "second-", "second.json", []byte("second credentials"))
	require.NoError(t, secondErr)
	require.NotEqual(t, filepath.Dir(firstPath), filepath.Dir(secondPath))
	firstContents, readErr := os.ReadFile(firstPath)
	require.NoError(t, readErr)
	require.Equal(t, "first credentials", string(firstContents))
	require.NoError(t, registry.Remove(firstPath))
	require.NoError(t, registry.Remove(firstPath))
	require.NoFileExists(t, firstPath)
	require.NoDirExists(t, filepath.Dir(firstPath))
	require.FileExists(t, secondPath)

	registry.Close()
	registry.Close()
	require.NoError(t, registry.Cleanup())
	require.NoError(t, registry.Cleanup())
	require.NoFileExists(t, secondPath)
	require.NoDirExists(t, filepath.Dir(secondPath))
	_, afterCloseErr := registry.Create(ctx, "closed-", "unused.json", nil)
	require.ErrorIs(t, afterCloseErr, context.Canceled)
}

// Verifies that FileRegistry.Cleanup completes without a prior Close, removes created files,
// and stops accepting new files.
func TestFileRegistryCleanupStopsCreationWithoutClose(t *testing.T) {
	t.Parallel()
	registry := usvc_io.NewFileRegistry(t.TempDir())
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	path, createErr := registry.Create(ctx, "unclosed-", "security.json", []byte("credentials"))
	require.NoError(t, createErr)

	cleanupResult := make(chan error, 1)
	go func() {
		cleanupResult <- registry.Cleanup()
	}()
	select {
	case cleanupErr := <-cleanupResult:
		require.NoError(t, cleanupErr)
	case <-ctx.Done():
		t.Fatal("Cleanup did not complete without a prior Close")
	}
	require.NoFileExists(t, path)
	require.NoDirExists(t, filepath.Dir(path))
	_, afterCleanupErr := registry.Create(ctx, "after-cleanup-", "unused.json", nil)
	require.ErrorIs(t, afterCleanupErr, context.Canceled)
}

// Verifies that FileRegistry retains failed directory removals for later cleanup
// and still removes unrelated files when one removal fails.
func TestFileRegistryRetriesFailedCleanup(t *testing.T) {
	t.Parallel()
	registry := usvc_io.NewFileRegistry(t.TempDir())
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	blockedPath, blockedErr := registry.Create(ctx, "blocked-", "security.json", []byte("credentials"))
	require.NoError(t, blockedErr)
	otherPath, otherErr := registry.Create(ctx, "other-", "security.json", nil)
	require.NoError(t, otherErr)
	blockerPath := filepath.Join(filepath.Dir(blockedPath), "block-removal")
	blocker, blockerErr := usvc_io.CreateNewFile(blockerPath, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, blockerErr)
	require.NoError(t, blocker.Close())

	require.Error(t, registry.Remove(blockedPath))
	registry.Close()
	require.Error(t, registry.Cleanup())
	require.NoFileExists(t, blockedPath)
	require.DirExists(t, filepath.Dir(blockedPath))
	require.NoDirExists(t, filepath.Dir(otherPath))
	require.NoError(t, os.Remove(blockerPath))
	require.NoError(t, registry.Cleanup())
	require.NoDirExists(t, filepath.Dir(blockedPath))
}

// Verifies that FileRegistry rejects invalid filenames, canceled creation, and
// directory allocation failures without leaving files to be cleaned up.
func TestFileRegistryRejectsFailedCreation(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	registry := usvc_io.NewFileRegistry(directory)
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	for _, fileName := range []string{"", ".", "..", filepath.Join("nested", "file.json")} {
		path, createErr := registry.Create(ctx, "invalid-", fileName, nil)
		require.Empty(t, path)
		require.Error(t, createErr)
	}
	_, longNameErr := registry.Create(ctx, "long-name-", strings.Repeat("x", 300), nil)
	require.Error(t, longNameErr, "Failed file creation must still clean up its allocated directory")
	canceledCtx, stopCreation := context.WithCancel(ctx)
	stopCreation()
	_, cancellationErr := registry.Create(canceledCtx, "canceled-", "security.json", nil)
	require.ErrorIs(t, cancellationErr, context.Canceled)
	missingDirectory := usvc_io.NewFileRegistry(filepath.Join(directory, "missing"))
	_, directoryErr := missingDirectory.Create(ctx, "missing-", "security.json", nil)
	require.Error(t, directoryErr)
	missingDirectory.Close()
	require.NoError(t, missingDirectory.Cleanup())
	registry.Close()
	require.NoError(t, registry.Cleanup())
	remaining, readErr := os.ReadDir(directory)
	require.NoError(t, readErr)
	require.Empty(t, remaining)
}

// Verifies that FileRegistry cleanup waits for accepted writes while creation
// races with closing, leaving neither accepted files nor generated directories behind.
func TestFileRegistryClosesDuringConcurrentCreation(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	registry := usvc_io.NewFileRegistry(directory)
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	const creators = 32
	var completed sync.WaitGroup
	completed.Add(creators)
	start := make(chan struct{})
	for range creators {
		go func() {
			defer completed.Done()
			select {
			case <-start:
			case <-ctx.Done():
				return
			}
			_, createErr := registry.Create(ctx, "concurrent-", "security.json", []byte("credentials"))
			if createErr != nil && !errors.Is(createErr, context.Canceled) {
				t.Errorf("Unexpected creation failure: %v", createErr)
			}
		}()
	}
	close(start)
	registry.Close()
	require.NoError(t, registry.Cleanup())
	completed.Wait()
	remaining, readErr := os.ReadDir(directory)
	require.NoError(t, readErr)
	require.Empty(t, remaining)
}
