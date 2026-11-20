/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/syncmap"
)

// FileRegistry owns private temporary files, each in a separate restricted directory.
// Close stops creation; Cleanup also stops creation, then waits for accepted writes before removing remaining files.
type FileRegistry struct {
	// Parent directory for generated file directories.
	directory string
	// Gates creation and tracks accepted writes until their files have closed.
	writes *concurrency.CountdownLatch
	// Retains file and directory cleanup responsibility until removal succeeds.
	files syncmap.Map[string, struct{}]
}

// NewFileRegistry returns a FileRegistry that creates private per-file subdirectories under directory.
func NewFileRegistry(directory string) *FileRegistry {
	return &FileRegistry{
		directory: directory,
		writes:    concurrency.NewCountdownLatch(),
	}
}

// Create writes contents to fileName inside a unique, restricted directory named using prefix.
// It returns a closed file's path on success and retains cleanup responsibility for any remnants on failure.
func (registry *FileRegistry) Create(ctx context.Context, prefix, fileName string, contents []byte) (string, error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return "", contextErr
	}
	if !filepath.IsLocal(fileName) || fileName == "." || filepath.Base(fileName) != fileName {
		return "", fmt.Errorf("file name must be a single local path component: %q", fileName)
	}
	completeWrite, registered := registry.writes.Register()
	if !registered {
		return "", context.Canceled
	}
	defer completeWrite()
	if cancellationErr := ctx.Err(); cancellationErr != nil {
		return "", cancellationErr
	}

	fileDirectory, directoryErr := os.MkdirTemp(registry.directory, prefix)
	if directoryErr != nil {
		return "", fmt.Errorf("create registered file directory: %w", directoryErr)
	}
	path := filepath.Join(fileDirectory, fileName)
	registry.files.Store(path, struct{}{})
	restrictErr := EnsureRestrictedDirectory(fileDirectory, osutil.PermissionOnlyOwnerReadWriteTraverse)
	if restrictErr != nil {
		return "", errors.Join(
			fmt.Errorf("restrict registered file directory: %w", restrictErr),
			registry.Remove(path),
		)
	}
	file, createErr := CreateNewFile(path, osutil.PermissionOnlyOwnerReadWrite)
	if createErr != nil {
		// An invalid filename can prevent even removal by path; an empty directory needs only directory cleanup.
		emptyDirectoryErr := os.Remove(fileDirectory)
		if emptyDirectoryErr == nil || errors.Is(emptyDirectoryErr, os.ErrNotExist) {
			registry.files.Delete(path)
			return "", fmt.Errorf("create registered file: %w", createErr)
		}
		return "", errors.Join(
			fmt.Errorf("create registered file: %w", createErr),
			registry.Remove(path),
		)
	}
	_, writeErr := file.Write(contents)
	closeErr := file.Close()
	if completionErr := errors.Join(writeErr, closeErr, ctx.Err()); completionErr != nil {
		return "", errors.Join(
			fmt.Errorf("write registered file: %w", completionErr),
			registry.Remove(path),
		)
	}
	return path, nil
}

// Remove deletes a previously returned file and its directory, retaining tracking if removal fails.
// Repeated removal is safe when both paths are already gone.
func (registry *FileRegistry) Remove(path string) error {
	_, tracked := registry.files.Load(path)
	if !tracked {
		return nil
	}
	removeErr := os.Remove(path)
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("remove registered file %q: %w", path, removeErr)
	}
	directoryErr := os.Remove(filepath.Dir(path))
	if directoryErr != nil && !errors.Is(directoryErr, os.ErrNotExist) {
		return fmt.Errorf("remove registered file directory %q: %w", filepath.Dir(path), directoryErr)
	}
	registry.files.Delete(path)
	return nil
}

// Close stops accepting file creation without interrupting accepted writes.
func (registry *FileRegistry) Close() {
	registry.writes.Close()
}

// Cleanup stops accepting file creation, waits for accepted writes, and removes all remaining files,
// returning aggregated errors. Failed removals remain tracked for another cleanup attempt.
func (registry *FileRegistry) Cleanup() error {
	registry.writes.Close()
	registry.writes.Wait()
	var cleanupErrors []error
	registry.files.Range(func(path string, _ struct{}) bool {
		if removeErr := registry.Remove(path); removeErr != nil {
			cleanupErrors = append(cleanupErrors, removeErr)
		}
		return true
	})
	return errors.Join(cleanupErrors...)
}
