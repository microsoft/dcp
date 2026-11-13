/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// OpenTarArchive opens a hash-verified source file or decodes base64 archive contents.
// Exactly one input must be supplied. The caller closes the returned reader.
func OpenTarArchive(ctx context.Context, source, expectedSHA256, rawContents string) (io.ReadCloser, error) {
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if cancellationErr := ctx.Err(); cancellationErr != nil {
		return nil, cancellationErr
	}
	if source != "" && rawContents != "" {
		return nil, fmt.Errorf("archive source and raw contents are mutually exclusive")
	}
	if source == "" {
		if rawContents == "" {
			return nil, fmt.Errorf("archive source or raw contents is required")
		}
		var contents bytes.Buffer
		decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(rawContents))
		if _, decodeErr := io.Copy(&contents, NewContextReader(readCtx, decoder, false)); decodeErr != nil {
			return nil, fmt.Errorf("decode archive contents: %w", decodeErr)
		}
		return io.NopCloser(bytes.NewReader(contents.Bytes())), nil
	}
	file, openErr := OpenFileReadOnly(source)
	if openErr != nil {
		return nil, fmt.Errorf("open archive %q: %w", source, openErr)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		return nil, errors.Join(fmt.Errorf("inspect archive %q: %w", source, statErr), file.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(fmt.Errorf("archive %q is not a regular file", source), file.Close())
	}
	hash := sha256.New()
	if _, hashErr := io.Copy(hash, NewContextReader(readCtx, file, false)); hashErr != nil {
		return nil, errors.Join(fmt.Errorf("hash archive %q: %w", source, hashErr), file.Close())
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
		return nil, errors.Join(fmt.Errorf("rewind archive %q: %w", source, rewindErr), file.Close())
	}
	return file, nil
}
