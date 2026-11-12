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
			linkRelative, linkRelativeErr := filepath.Rel(root, filepath.Join(filepath.Dir(current), target))
			if filepath.IsAbs(target) || linkRelativeErr != nil || linkRelative == ".." ||
				strings.HasPrefix(linkRelative, ".."+string(filepath.Separator)) {
				return fmt.Errorf("source symlink %q must be relative and remain within the source directory", current)
			}
			return tw.WriteSymlink(name, filepath.ToSlash(target), uid, gid, info.ModTime(), info.ModTime(), info.ModTime())
		case info.IsDir():
			return tw.WriteDir(name, uid, gid, info.Mode().Perm(), info.ModTime(), info.ModTime(), info.ModTime())
		case info.Mode().IsRegular():
			file, openErr := OpenFileReadOnly(current)
			if openErr != nil {
				return fmt.Errorf("open directory entry %q: %w", current, openErr)
			}
			copyErr := tw.CopyFile(NewContextReader(ctx, file, false), info.Size(), name, uid, gid, info.Mode().Perm(), info.ModTime(), info.ModTime(), info.ModTime())
			closeErr := file.Close()
			return errors.Join(copyErr, closeErr)
		default:
			return fmt.Errorf("unsupported directory entry %q with mode %s", current, info.Mode())
		}
	})
}
