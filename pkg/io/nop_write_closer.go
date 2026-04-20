/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"io"
)

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error {
	return nil
}

type nopWriteCloserReaderFrom struct {
	nopWriteCloser
	io.ReaderFrom
}

// NopWriteCloser returns an io.WriteCloser that wraps the provided io.Writer, with a no-op Close method.
// If the provided writer is nil, it returns nil.
func NopWriteCloser(w io.Writer) io.WriteCloser {
	if w == nil {
		return nil
	} else if r, ok := w.(io.ReaderFrom); ok {
		return nopWriteCloserReaderFrom{nopWriteCloser{w}, r}
	} else {
		return nopWriteCloser{w}
	}
}
