/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"errors"
	"io"
)

type Syncer interface {
	Sync() error
}

type WriteSyncerCloser interface {
	io.WriteCloser
	Syncer
}

var (
	ErrClosedWriter = errors.New("writer is closed")
	ErrClosedReader = errors.New("reader is closed")
)
