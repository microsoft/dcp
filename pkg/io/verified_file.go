/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// OpenSHA256VerifiedFile opens a regular file, verifies its SHA256, and rewinds it for reading.
// The caller closes the returned file.
func OpenSHA256VerifiedFile(ctx context.Context, source, expectedSHA256 string) (*os.File, error) {
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if cancellationErr := ctx.Err(); cancellationErr != nil {
		return nil, cancellationErr
	}
	// Opening a FIFO blocks until a writer appears and cannot be cancelled, so reject special files first.
	sourceInfo, sourceStatErr := os.Stat(source)
	if sourceStatErr != nil {
		return nil, fmt.Errorf("inspect file %q: %w", source, sourceStatErr)
	}
	if !sourceInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("file %q is not a regular file", source)
	}
	file, openErr := OpenFileReadOnly(source)
	if openErr != nil {
		return nil, fmt.Errorf("open file %q: %w", source, openErr)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		return nil, errors.Join(fmt.Errorf("inspect file %q: %w", source, statErr), file.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(fmt.Errorf("file %q is not a regular file", source), file.Close())
	}
	hash := sha256.New()
	if _, hashErr := io.Copy(hash, NewContextReader(readCtx, file, false)); hashErr != nil {
		return nil, errors.Join(fmt.Errorf("hash file %q: %w", source, hashErr), file.Close())
	}
	expected := strings.TrimSpace(expectedSHA256)
	if strings.HasPrefix(strings.ToLower(expected), "sha256:") {
		expected = expected[len("sha256:"):]
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, expected) {
		return nil, errors.Join(fmt.Errorf("SHA256 mismatch: expected %s, got %s", expectedSHA256, actual), file.Close())
	}
	if _, rewindErr := file.Seek(0, io.SeekStart); rewindErr != nil {
		return nil, errors.Join(fmt.Errorf("rewind file %q: %w", source, rewindErr), file.Close())
	}
	return file, nil
}
