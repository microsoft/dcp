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
// Relative symlinks resolving within the source are preserved; absolute, dangling or escaping links and special files are rejected.
// executableFiles selects root-relative regular files that gain execute bits without modifying their sources.
// uid and gid apply to every entry. The caller finalizes the archive with Close or Buffer.
func (tw *TarWriter) CopyDirectory(ctx context.Context, source, destination string, uid, gid int32, executableFiles []string) error {
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
	root, resolveErr := filepath.EvalSymlinks(source)
	if resolveErr != nil {
		return fmt.Errorf("resolve directory source %q: %w", source, resolveErr)
	}
	rootInfo, statErr := os.Stat(root)
	if statErr != nil {
		return fmt.Errorf("inspect directory source %q: %w", source, statErr)
	}
	if !rootInfo.IsDir() {
		return fmt.Errorf("directory source %q is not a directory", source)
	}
	executables := make(map[string]struct{}, len(executableFiles))
	for _, name := range executableFiles {
		if name == "." || !fs.ValidPath(name) || strings.ContainsAny(name, "\\:\x00") {
			return fmt.Errorf("executable file %q must be a root-relative path using forward slashes without '.' or '..' components", name)
		}
		current := root
		components := strings.Split(name, "/")
		for index, component := range components {
			if cancellationErr := ctx.Err(); cancellationErr != nil {
				return cancellationErr
			}
			current = filepath.Join(current, component)
			info, inspectErr := os.Lstat(current)
			if inspectErr != nil {
				return fmt.Errorf("inspect executable file %q: %w", name, inspectErr)
			}
			if (index < len(components)-1 && !info.IsDir()) || (index == len(components)-1 && !info.Mode().IsRegular()) {
				return fmt.Errorf("executable file %q must identify a regular file without symlink traversal", name)
			}
		}
		executables[name] = struct{}{}
	}
	return filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if cancellationErr := ctx.Err(); cancellationErr != nil {
			return cancellationErr
		}
		if walkErr != nil {
			return fmt.Errorf("walk directory source %q: %w", current, walkErr)
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return fmt.Errorf("inspect directory entry %q: %w", current, infoErr)
		}
		relative, relativeErr := filepath.Rel(root, current)
		if relativeErr != nil {
			return fmt.Errorf("resolve archive entry %q: %w", current, relativeErr)
		}
		if relative == "." && prefix == "." {
			return nil
		}
		name := path.Join(prefix, filepath.ToSlash(relative))
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, linkErr := os.Readlink(current)
			if linkErr != nil {
				return fmt.Errorf("read source symlink %q: %w", current, linkErr)
			}
			archiveTarget := filepath.ToSlash(target)
			// Windows treats root-relative (\x) and drive-relative (C:x) targets as non-absolute.
			if filepath.IsAbs(target) || filepath.VolumeName(target) != "" || path.IsAbs(archiveTarget) {
				return fmt.Errorf("source symlink %q must be relative and remain within the source directory", current)
			}
			resolved, resolveLinkErr := filepath.EvalSymlinks(current)
			if resolveLinkErr != nil {
				return fmt.Errorf("source symlink %q must be relative and remain within the source directory: %w", current, resolveLinkErr)
			}
			contained, containedErr := filepath.Rel(root, resolved)
			if containedErr != nil || contained == ".." || strings.HasPrefix(contained, ".."+string(filepath.Separator)) {
				return errors.Join(fmt.Errorf("source symlink %q must be relative and remain within the source directory", current), containedErr)
			}
			return tw.WriteSymlink(name, archiveTarget, uid, gid, info.ModTime(), info.ModTime(), info.ModTime())
		case info.IsDir():
			return tw.WriteDir(name, uid, gid, info.Mode().Perm(), info.ModTime(), info.ModTime(), info.ModTime())
		case info.Mode().IsRegular():
			file, openErr := OpenFileReadOnly(current)
			if openErr != nil {
				return fmt.Errorf("open directory entry %q: %w", current, openErr)
			}
			copyCtx, cancelCopy := context.WithCancel(ctx)
			mode := info.Mode().Perm()
			if _, executable := executables[filepath.ToSlash(relative)]; executable {
				mode |= 0111
			}
			copyErr := tw.CopyFile(NewContextReader(copyCtx, file, false), info.Size(), name, uid, gid, mode, info.ModTime(), info.ModTime(), info.ModTime())
			cancelCopy()
			closeErr := file.Close()
			return errors.Join(copyErr, closeErr)
		default:
			return fmt.Errorf("unsupported directory entry %q with mode %s", current, info.Mode())
		}

	})
}
