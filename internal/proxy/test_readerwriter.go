// Copyright (c) Microsoft Corporation. All rights reserved.

package proxy

import (
	"sync"
	"time"
)

const lowercaseLetters = "abcdefghijklmnopqrstuvwxyz"

type ioResult struct {
	n   int
	err error
}

// testReaderWriter is an implementation of DeadlineReader/DeadlineWriter that returns predefined values
// from Read()/Write() calls. It is used for testing.
type testReaderWriter struct {
	lock            *sync.Mutex
	nextLetter      int
	nextReadResult  int
	nextWriteResult int
	readResults     []ioResult
	writeResults    []ioResult
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

	if trw.nextReadResult >= len(trw.readResults) {
		panic("Read() called too many times")
	}

	result := trw.readResults[trw.nextReadResult]
	trw.nextReadResult++

	toWrite := result.n
	if toWrite > len(p) {
		toWrite = len(p)
	}
	for i := 0; i < toWrite; i++ {
		p[i] = lowercaseLetters[trw.nextLetter]
	}
	trw.nextLetter = (trw.nextLetter + 1) % len(lowercaseLetters)

	return result.n, result.err
}

func (trw *testReaderWriter) Write(p []byte) (n int, err error) {
	trw.lock.Lock()
	defer trw.lock.Unlock()

	if trw.nextWriteResult >= len(trw.writeResults) {
		panic("Write() called too many times")
	}

	result := trw.writeResults[trw.nextWriteResult]
	trw.nextWriteResult++

	return result.n, result.err
}

func (trw *testReaderWriter) SetReadDeadline(_ time.Time) error {
	return nil
}

func (trw *testReaderWriter) SetWriteDeadline(_ time.Time) error {
	return nil
}

var _ DeadlineReader = &testReaderWriter{}
var _ DeadlineWriter = &testReaderWriter{}
