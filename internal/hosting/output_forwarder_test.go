/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package hosting

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that forwardLines preserves line endings and empty or whitespace-only lines, and terminates
// an unterminated final line with the platform line separator.
func Test_ForwardLines_PreservesLineEndings(t *testing.T) {
	t.Parallel()

	var dst bytes.Buffer
	forwardLines(strings.NewReader("first\nsecond\r\n\n \t \nlast"), &dst, "stdout", logr.Discard())

	require.Equal(t, "first\nsecond\r\n\n \t \nlast"+string(osutil.LineSep()), dst.String())
}

// Verifies that forwardLines writes any bytes returned with a source error before stopping.
func Test_ForwardLines_WritesDataBeforeReadError(t *testing.T) {
	t.Parallel()

	var dst bytes.Buffer
	forwardLines(readErrorReader{err: errors.New("source failed")}, &dst, "stdout", logr.Discard())

	require.Equal(t, "partial"+string(osutil.LineSep()), dst.String())
}

// Verifies that forwardLines splits lines longer than the maximum line size into platform-terminated pieces,
// forwards all line content, logs a warning for every split, and resumes normal forwarding afterwards.
func Test_ForwardLines_SplitsLongLines(t *testing.T) {
	t.Parallel()

	const tailLen = 10
	longLine := strings.Repeat("a", 2*maxForwardedLineSize+tailLen)
	src := strings.NewReader(longLine + "\nnext\n")

	var warnings atomic.Int32
	log := funcr.New(func(_, args string) {
		if strings.Contains(args, "maximum line size") {
			warnings.Add(1)
		}
	}, funcr.Options{})

	var dst bytes.Buffer
	forwardLines(src, &dst, "stderr", log)

	piece := strings.Repeat("a", maxForwardedLineSize) + string(osutil.LineSep())
	expected := piece + piece + strings.Repeat("a", tailLen) + "\n" + "next\n"
	require.Equal(t, expected, dst.String())
	require.Equal(t, int32(2), warnings.Load())
}

// Verifies that forwardLines keeps draining its source after destination writes fail,
// so that the producer is never blocked, and returns when the source is closed.
func Test_ForwardLines_DrainsSourceAfterWriteErrors(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, hostingTestTimeout)
	defer cancel()

	srcReader, srcWriter := io.Pipe()
	forwardDone := make(chan struct{})
	go func() {
		defer close(forwardDone)
		forwardLines(srcReader, failingWriter{}, "stdout", logr.Discard())
	}()

	// io.Pipe writes block until the data is read, so these writes complete only if the forwarder keeps reading.
	producerDone := make(chan error, 1)
	go func() {
		for i := 0; i < 100; i++ {
			if _, writeErr := srcWriter.Write([]byte("some output line\n")); writeErr != nil {
				producerDone <- writeErr
				return
			}
		}
		producerDone <- srcWriter.Close()
	}()

	select {
	case producerErr := <-producerDone:
		require.NoError(t, producerErr)
	case <-ctx.Done():
		require.FailNow(t, "the producer was blocked by the output forwarder")
	}

	select {
	case <-forwardDone:
	case <-ctx.Done():
		require.FailNow(t, "the output forwarder did not return after its source was closed")
	}
}

// failingWriter simulates a destination whose reader is gone.
type failingWriter struct{}

func (failingWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("broken pipe")
}

type readErrorReader struct {
	err error
}

func (r readErrorReader) Read(p []byte) (int, error) {
	return copy(p, "partial"), r.err
}
