/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package proxy

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

const lowercaseLetters = "abcdefghijklmnopqrstuvwxyz"

type ioResult struct {
	n   int
	err error
}

var testErrRepeatForever = errors.New("test error response returned forever")

// testReaderWriter is an implementation of DeadlineReaderWriter that returns predefined values
// from Read()/Write() calls. It is used for testing.
type testReaderWriter struct {
	lock            *sync.Mutex
	nextLetter      int
	nextReadResult  int
	nextWriteResult int
	readResults     []ioResult
	writeResults    []ioResult
	closed          bool
}

// Close implements DeadlineReaderWriter.
func (trw *testReaderWriter) Close() error {
	trw.lock.Lock()
	defer trw.lock.Unlock()

	if trw.closed {
		return net.ErrClosed
	}

	trw.closed = true

	return nil
}

func newTestReaderWriter(readResults, writeResults []ioResult) *testReaderWriter {
	return &testReaderWriter{
		lock:         &sync.Mutex{},
		readResults:  readResults,
		writeResults: writeResults,
	}
}

func (trw *testReaderWriter) Read(p []byte) (n int, err error) {
	trw.lock.Lock()
	defer trw.lock.Unlock()

	if trw.closed {
		return 0, net.ErrClosed
	}

	if trw.nextReadResult >= len(trw.readResults) {
		return 0, io.EOF
	}

	result := trw.readResults[trw.nextReadResult]
	if !errors.Is(result.err, testErrRepeatForever) {
		trw.nextReadResult++
	}

	toWrite := result.n
	if toWrite > len(p) {
		toWrite = len(p)
	}
	for i := 0; i < toWrite; i++ {
		p[i] = lowercaseLetters[trw.nextLetter]
	}
	trw.nextLetter = (trw.nextLetter + 1) % len(lowercaseLetters)

	if errors.Is(result.err, os.ErrDeadlineExceeded) {
		return result.n, nil
	}

	return result.n, result.err
}

func (trw *testReaderWriter) Write(p []byte) (n int, err error) {
	trw.lock.Lock()
	defer trw.lock.Unlock()

	if trw.closed {
		return 0, net.ErrClosed
	}

	if trw.nextWriteResult >= len(trw.writeResults) {
		panic("Write() called too many times")
	}

	result := trw.writeResults[trw.nextWriteResult]
	if !errors.Is(result.err, testErrRepeatForever) {
		trw.nextWriteResult++
	}

	if errors.Is(result.err, os.ErrDeadlineExceeded) {
		return result.n, nil
	}

	return result.n, result.err
}

func (trw *testReaderWriter) SetReadDeadline(_ time.Time) error {
	return nil
}

func (trw *testReaderWriter) SetWriteDeadline(_ time.Time) error {
	return nil
}

func (trw *testReaderWriter) SetDeadline(_ time.Time) error {
	return nil
}

var _ DeadlineReaderWriter = &testReaderWriter{}
