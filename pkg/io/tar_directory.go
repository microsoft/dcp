/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// CopyDirectory writes a directory's contents beneath destination in the archive, preserving permissions.
// Links in the source path itself are followed. Within the source, relative symlinks resolving inside it are
// preserved; absolute, dangling or escaping symlinks, Windows junctions, and other special files are rejected.
// executableFiles selects root-relative regular files that gain execute bits without modifying their sources.
// uid and gid apply to every entry. The caller finalizes the archive with Close or Buffer.
func (tw *TarWriter) CopyDirectory(ctx context.Context, source, destination string, uid, gid int32, executableFiles []string) (copyErr error) {
	if cancellationErr := ctx.Err(); cancellationErr != nil {
		return cancellationErr
	}
	prefix := path.Clean(destination)
	if path.IsAbs(prefix) || prefix == ".." || strings.HasPrefix(prefix, "../") {
		return fmt.Errorf("destination must be relative to the archive root")
	}
	if !filepath.IsAbs(source) {
		return fmt.Errorf("directory source must be absolute")
	}
	// os.Root confines link resolution to the source tree without resolving the source path's own ancestors,
	// which may include junctions that filepath.EvalSymlinks cannot traverse on Windows.
	root, openErr := os.OpenRoot(source)
	if openErr != nil {
		return fmt.Errorf("open directory source %q: %w", source, openErr)
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			copyErr = errors.Join(copyErr, fmt.Errorf("close directory source %q: %w", source, closeErr))
		}
	}()
	executables := make(map[string]struct{}, len(executableFiles))
	for _, name := range executableFiles {
		if name == "." || !fs.ValidPath(name) || strings.ContainsAny(name, "\\:\x00") {
			return fmt.Errorf("executable file %q must be a root-relative path using forward slashes without '.' or '..' components", name)
		}
		components := strings.Split(name, "/")
		for index := range components {
			if cancellationErr := ctx.Err(); cancellationErr != nil {
				return cancellationErr
			}
			info, inspectErr := root.Lstat(filepath.FromSlash(path.Join(components[:index+1]...)))
			if inspectErr != nil {
				return fmt.Errorf("inspect executable file %q: %w", name, inspectErr)
			}
			if (index < len(components)-1 && !info.IsDir()) || (index == len(components)-1 && !info.Mode().IsRegular()) {
				return fmt.Errorf("executable file %q must identify a regular file without symlink traversal", name)
			}
		}
		executables[name] = struct{}{}
	}
	return fs.WalkDir(root.FS(), ".", func(relative string, entry fs.DirEntry, walkErr error) error {
		if cancellationErr := ctx.Err(); cancellationErr != nil {
			return cancellationErr
		}
		current := filepath.Join(source, filepath.FromSlash(relative))
		if walkErr != nil {
			return fmt.Errorf("walk directory source %q: %w", current, walkErr)
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return fmt.Errorf("inspect directory entry %q: %w", current, infoErr)
		}
		if relative == "." && prefix == "." {
			return nil
		}
		name := path.Join(prefix, relative)
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, linkErr := root.Readlink(filepath.FromSlash(relative))
			if linkErr != nil {
				return fmt.Errorf("read source symlink %q: %w", current, linkErr)
			}
			archiveTarget := filepath.ToSlash(target)
			// Windows treats root-relative (\x) and drive-relative (C:x) targets as non-absolute.
			if filepath.IsAbs(target) || filepath.VolumeName(target) != "" || path.IsAbs(archiveTarget) {
				return fmt.Errorf("source symlink %q must be relative and remain within the source directory", current)
			}
			// Root.Stat follows links only within the source and fails for escaping or dangling targets.
			if _, resolveErr := root.Stat(filepath.FromSlash(relative)); resolveErr != nil {
				return fmt.Errorf("source symlink %q must be relative and remain within the source directory: %w", current, resolveErr)
			}
			return tw.WriteSymlink(name, archiveTarget, uid, gid, info.ModTime(), info.ModTime(), info.ModTime())
		case info.IsDir():
			return tw.WriteDir(name, uid, gid, info.Mode().Perm(), info.ModTime(), info.ModTime(), info.ModTime())
		case info.Mode().IsRegular():
			file, fileOpenErr := root.Open(filepath.FromSlash(relative))
			if fileOpenErr != nil {
				return fmt.Errorf("open directory entry %q: %w", current, fileOpenErr)
			}
			copyCtx, cancelCopy := context.WithCancel(ctx)
			mode := info.Mode().Perm()
			if _, executable := executables[relative]; executable {
				mode |= 0111
			}
			fileCopyErr := tw.CopyFile(NewContextReader(copyCtx, file, false), info.Size(), name, uid, gid, mode, info.ModTime(), info.ModTime(), info.ModTime())
			cancelCopy()
			closeErr := file.Close()
			return errors.Join(fileCopyErr, closeErr)
		default:
			// Includes Windows junctions, whose absolute targets cannot be represented inside the archive.
			return fmt.Errorf("unsupported directory entry %q with mode %s; only directories, regular files and relative symlinks are supported", current, info.Mode())
		}
	})
}
