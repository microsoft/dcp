/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package testutil

import (
	"bytes"
	"io"
	"sync"
)

// ThreadSafeBuffer is an io.ReadWriter that is safe for concurrent use by multiple goroutines.
// It uses an internal bytes.Buffer to store data and a sync.Mutex to ensure thread safety.
type ThreadSafeBuffer struct {
	buf  *bytes.Buffer
	lock *sync.Mutex
}

func (b *ThreadSafeBuffer) Write(p []byte) (n int, err error) {
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.buf.Write(p)
}

func (b *ThreadSafeBuffer) Read(p []byte) (n int, err error) {
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.buf.Read(p)
}

// NewThreadSafeBuffer creates and returns a new ThreadSafeBuffer instance.
func NewThreadSafeBuffer() *ThreadSafeBuffer {
	return &ThreadSafeBuffer{
		buf:  &bytes.Buffer{},
		lock: &sync.Mutex{},
	}
}

var _ io.ReadWriter = (*ThreadSafeBuffer)(nil)
