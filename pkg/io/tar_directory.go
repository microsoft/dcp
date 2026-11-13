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
// Relative symlinks within the source are preserved; absolute or escaping links and special files are rejected.
// uid and gid apply to every entry. The caller finalizes the archive with Close or Buffer.
func (tw *TarWriter) CopyDirectory(ctx context.Context, source, destination string, uid, gid int32) error {
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
			if filepath.IsAbs(target) {
				return fmt.Errorf("source symlink %q must be relative and remain within the source directory", current)
			}
			if traversalErr := validateDirectorySymlink(ctx, root, relative); traversalErr != nil {
				return fmt.Errorf("source symlink %q must be relative and remain within the source directory: %w", current, traversalErr)
			}
			return tw.WriteSymlink(name, filepath.ToSlash(target), uid, gid, info.ModTime(), info.ModTime(), info.ModTime())
		case info.IsDir():
			return tw.WriteDir(name, uid, gid, info.Mode().Perm(), info.ModTime(), info.ModTime(), info.ModTime())
		case info.Mode().IsRegular():
			file, openErr := OpenFileReadOnly(current)
			if openErr != nil {
				return fmt.Errorf("open directory entry %q: %w", current, openErr)
			}
			copyCtx, cancelCopy := context.WithCancel(ctx)
			copyErr := tw.CopyFile(NewContextReader(copyCtx, file, false), info.Size(), name, uid, gid, info.Mode().Perm(), info.ModTime(), info.ModTime(), info.ModTime())
			cancelCopy()
			closeErr := file.Close()
			return errors.Join(copyErr, closeErr)
		default:
			return fmt.Errorf("unsupported directory entry %q with mode %s", current, info.Mode())
		}

	})
}

// Resolve symlinks before processing ".."; cleaning the path first changes filesystem traversal.
func validateDirectorySymlink(ctx context.Context, root, relative string) error {
	pending := strings.Split(relative, string(filepath.Separator))
	var resolved []string
	links := 0
	for len(pending) != 0 {
		if cancellationErr := ctx.Err(); cancellationErr != nil {
			return cancellationErr
		}
		component := pending[0]
		pending = pending[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return fmt.Errorf("link traversal escapes source directory")
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		candidate := filepath.Join(root, filepath.Join(resolved...), component)
		info, inspectErr := os.Lstat(candidate)
		if inspectErr != nil && !errors.Is(inspectErr, os.ErrNotExist) {
			return fmt.Errorf("inspect link target %q: %w", candidate, inspectErr)
		}
		if inspectErr == nil && info.Mode()&fs.ModeSymlink != 0 {
			links++
			if links > 255 {
				return fmt.Errorf("too many symbolic links")
			}
			target, readErr := os.Readlink(candidate)
			if readErr != nil {
				return fmt.Errorf("read link target %q: %w", candidate, readErr)
			}
			if filepath.IsAbs(target) {
				return fmt.Errorf("link traversal contains an absolute target")
			}
			pending = append(strings.Split(target, string(filepath.Separator)), pending...)
			continue
		}
		resolved = append(resolved, component)
	}
	return nil
}
