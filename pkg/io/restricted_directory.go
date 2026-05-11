/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"errors"
	"fmt"
	"os"
)

// EnsureRestrictedDirectory ensures the final path exists as a real directory owned by the
// current user, then applies the requested permissions. Parent directories are not created.
func EnsureRestrictedDirectory(dir string, perm os.FileMode) error {
	info, statErr := os.Lstat(dir)
	if errors.Is(statErr, os.ErrNotExist) {
		if mkdirErr := os.Mkdir(dir, perm); mkdirErr != nil {
			return fmt.Errorf("could not create directory '%s': %w", dir, mkdirErr)
		}
		info, statErr = os.Lstat(dir)
	}
	if statErr != nil {
		return fmt.Errorf("could not inspect directory '%s': %w", dir, statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("directory '%s' is a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("path '%s' is not a directory", dir)
	}
	if ownershipErr := validateRestrictedDirectoryOwner(dir, info); ownershipErr != nil {
		return fmt.Errorf("directory '%s' has invalid ownership: %w", dir, ownershipErr)
	}
	if chmodErr := os.Chmod(dir, perm); chmodErr != nil {
		return fmt.Errorf("could not restrict directory '%s': %w", dir, chmodErr)
	}

	return nil
}
