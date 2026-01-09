/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Test empty input
func TestTailReaderEmptyInput(t *testing.T) {
	t.Parallel()

	input := io.NopCloser(strings.NewReader(""))
	reader := usvc_io.NewTailReader(input, 3)
	require.False(t, reader.Filled.Frozen())

	var result bytes.Buffer
	_, err := io.Copy(&result, reader)
	require.NoError(t, err)
	require.Equal(t, "", result.String())

	require.True(t, reader.Filled.Frozen())
	require.Equal(t, usvc_io.TailReaderStats{TotalLines: 0, TailLines: 0}, *reader.FillStats.Load())

	require.NoError(t, reader.Close())
}

// Test single line with trailing newline
func TestTailReaderSingleLineWithNewline(t *testing.T) {
	t.Parallel()

	input := io.NopCloser(strings.NewReader("single line\n"))
	reader := usvc_io.NewTailReader(input, 3)

	var result bytes.Buffer
	_, err := io.Copy(&result, reader)
	require.NoError(t, err)
	require.Equal(t, "single line\n", result.String())

	require.True(t, reader.Filled.Frozen())
	require.Equal(t, usvc_io.TailReaderStats{TotalLines: 1, TailLines: 1}, *reader.FillStats.Load())

	require.NoError(t, reader.Close())
}

// Test single line without trailing newline
func TestTailReaderSingleLineWithoutNewline(t *testing.T) {
	t.Parallel()

	input := io.NopCloser(strings.NewReader("single line"))
	reader := usvc_io.NewTailReader(input, 3)

	var result bytes.Buffer
	_, err := io.Copy(&result, reader)
	require.NoError(t, err)
	require.Equal(t, "single line", result.String())

	require.True(t, reader.Filled.Frozen())
	require.Equal(t, usvc_io.TailReaderStats{TotalLines: 1, TailLines: 1}, *reader.FillStats.Load())

	require.NoError(t, reader.Close())
}

// Test fewer lines than tailSize
func TestTailReaderFewerLines(t *testing.T) {
	t.Parallel()

	input := io.NopCloser(strings.NewReader("line1\nline2\n"))
	reader := usvc_io.NewTailReader(input, 5)

	var result bytes.Buffer
	_, err := io.Copy(&result, reader)
	require.NoError(t, err)
	require.Equal(t, "line1\nline2\n", result.String())

	require.True(t, reader.Filled.Frozen())
	require.Equal(t, usvc_io.TailReaderStats{TotalLines: 2, TailLines: 2}, *reader.FillStats.Load())

	require.NoError(t, reader.Close())
}

// Test exactly tailSize lines
func TestTailReaderExactLines(t *testing.T) {
	t.Parallel()

	input := io.NopCloser(strings.NewReader("line1\nline2\nline3\n"))
	reader := usvc_io.NewTailReader(input, 3)

	var result bytes.Buffer
	_, err := io.Copy(&result, reader)
	require.NoError(t, err)
	require.Equal(t, "line1\nline2\nline3\n", result.String())

	require.NoError(t, reader.Close())
}

// Test more lines than tailSize
func TestTailReaderMoreLines(t *testing.T) {
	t.Parallel()

	input := io.NopCloser(strings.NewReader("line1\nline2\nline3\nline4\nline5\n"))
	reader := usvc_io.NewTailReader(input, 3)

	var result bytes.Buffer
	_, err := io.Copy(&result, reader)
	require.NoError(t, err)
	require.Equal(t, "line3\nline4\nline5\n", result.String())

	require.True(t, reader.Filled.Frozen())
	require.Equal(t, usvc_io.TailReaderStats{TotalLines: 5, TailLines: 3}, *reader.FillStats.Load())

	require.NoError(t, reader.Close())
}

// Test with empty lines
func TestTailReaderWithEmptyLines(t *testing.T) {
	t.Parallel()

	input := io.NopCloser(strings.NewReader("line1\n\nline3\n\nline5\n"))
	reader := usvc_io.NewTailReader(input, 3)

	var result bytes.Buffer
	_, err := io.Copy(&result, reader)
	require.NoError(t, err)
	require.Equal(t, "line3\n\nline5\n", result.String())

	require.True(t, reader.Filled.Frozen())
	require.Equal(t, usvc_io.TailReaderStats{TotalLines: 5, TailLines: 3}, *reader.FillStats.Load())

	require.NoError(t, reader.Close())
}

// Test with lines of varying lengths
func TestTailReaderVaryingLineLengths(t *testing.T) {
	t.Parallel()

	shortLine := "short"
	mediumLine := "this is a medium length line"
	longLine := strings.Repeat("long line with lots of repeated text ", 100)

	input := io.NopCloser(strings.NewReader(strings.Repeat(shortLine+"\n"+mediumLine+"\n"+longLine+"\n", 5)))
	reader := usvc_io.NewTailReader(input, 3)

	var result bytes.Buffer
	_, err := io.Copy(&result, reader)
	require.NoError(t, err)
	require.Equal(t, shortLine+"\n"+mediumLine+"\n"+longLine+"\n", result.String())

	require.True(t, reader.Filled.Frozen())
	require.Equal(t, usvc_io.TailReaderStats{TotalLines: 5 * 3, TailLines: 3}, *reader.FillStats.Load())

	require.NoError(t, reader.Close())
}

// Test reading with small buffer requiring multiple reads
func TestTailReaderSmallReadBuffer(t *testing.T) {
	t.Parallel()

	content := "line1\nline2\nline3\nline4\nline5\n"
	input := io.NopCloser(strings.NewReader(content))
	reader := usvc_io.NewTailReader(input, 3)

	// Using a very small buffer to force multiple reads
	buf := make([]byte, 4)
	var result bytes.Buffer

	for {
		n, err := reader.Read(buf)
		if n > 0 {
			result.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
	}

	require.Equal(t, "line3\nline4\nline5\n", result.String())
	require.NoError(t, reader.Close())

	require.True(t, reader.Filled.Frozen())
	require.Equal(t, usvc_io.TailReaderStats{TotalLines: 5, TailLines: 3}, *reader.FillStats.Load())
}

// Test reading with large buffer that can hold entire output
func TestTailReaderLargeReadBuffer(t *testing.T) {
	t.Parallel()

	content := "line1\nline2\nline3\nline4\nline5\n"
	input := io.NopCloser(strings.NewReader(content))
	reader := usvc_io.NewTailReader(input, 3)

	// Large buffer that can hold the entire output in one read
	buf := make([]byte, 1024)
	n, err := reader.Read(buf)
	require.NoError(t, err)

	result := string(buf[:n])
	_, err = reader.Read(buf) // This should return EOF
	require.Equal(t, io.EOF, err)

	require.Equal(t, "line3\nline4\nline5\n", result)
	require.NoError(t, reader.Close())

	require.True(t, reader.Filled.Frozen())
	require.Equal(t, usvc_io.TailReaderStats{TotalLines: 5, TailLines: 3}, *reader.FillStats.Load())
}

// Test with error during reading
func TestTailReaderErrorAfterSomeData(t *testing.T) {
	t.Parallel()

	expectedErr := errors.New("simulated error")
	content := "line1\nline2\nline3\nline4\nline5\n"

	// Create a mock reader that will return an error after reading 10 bytes
	input := testutil.NewTestReader()
	input.AddEntry(testutil.AsByteTimelineEntries([]byte(content[:10])...)...)
	input.AddEntry(testutil.AsErrorTimelineEntry(expectedErr))
	input.AddEntry(testutil.AsByteTimelineEntries([]byte(content[10:])...)...)

	reader := usvc_io.NewTailReader(input, 3)

	buf := make([]byte, 1024)
	n, err := reader.Read(buf)

	// We expect to get the error from the inner reader
	assert.Equal(t, expectedErr, err)
	assert.Equal(t, 0, n)

	// The TailReader should be able to recover from intermittent errors
	n, err = reader.Read(buf)
	require.NoError(t, err)
	result := string(buf[:n])
	require.Equal(t, "line3\nline4\nline5\n", result)

	require.True(t, reader.Filled.Frozen())
	require.Equal(t, usvc_io.TailReaderStats{TotalLines: 5, TailLines: 3}, *reader.FillStats.Load())
}

// Test reading after close
func TestTailReaderReadAfterClose(t *testing.T) {
	t.Parallel()

	input := io.NopCloser(strings.NewReader("line1\nline2\nline3\n"))
	reader := usvc_io.NewTailReader(input, 3)

	require.NoError(t, reader.Close())

	buf := make([]byte, 10)
	_, err := reader.Read(buf)
	assert.Equal(t, usvc_io.ErrClosedReader, err)

	require.False(t, reader.Filled.Frozen())
}

// Test with zero-sized read buffer
func TestTailReaderZeroSizedBuffer(t *testing.T) {
	t.Parallel()

	input := io.NopCloser(strings.NewReader("line1\nline2\nline3\n"))
	reader := usvc_io.NewTailReader(input, 3)

	buf := make([]byte, 0)
	_, err := reader.Read(buf)
	assert.Equal(t, io.ErrShortBuffer, err)

	require.NoError(t, reader.Close())
}

// Test with mixed line endings
func TestTailReaderMixedLineEndings(t *testing.T) {
	t.Parallel()

	// Mix of \n and \r\n line endings
	input := io.NopCloser(strings.NewReader("line1\nline2\r\nline3\nline4\r\n"))
	reader := usvc_io.NewTailReader(input, 3)

	var result bytes.Buffer
	_, err := io.Copy(&result, reader)
	require.NoError(t, err)

	// Note: This test checks how the TailReader handles mixed line endings
	require.Equal(t, "line2\r\nline3\nline4\r\n", result.String())

	require.True(t, reader.Filled.Frozen())
	require.Equal(t, usvc_io.TailReaderStats{TotalLines: 4, TailLines: 3}, *reader.FillStats.Load())

	require.NoError(t, reader.Close())
}

// Test with very long lines to ensure proper buffering
func TestTailReaderVeryLongLines(t *testing.T) {
	t.Parallel()

	longLine1 := strings.Repeat("a", 100*1024) // 100KB line
	longLine2 := strings.Repeat("b", 100*1024) // 100KB line
	longLine3 := strings.Repeat("c", 100*1024) // 100KB line

	input := io.NopCloser(strings.NewReader(longLine1 + "\n" + longLine2 + "\n" + longLine3 + "\n"))
	reader := usvc_io.NewTailReader(input, 2)

	var result bytes.Buffer
	_, err := io.Copy(&result, reader)
	require.NoError(t, err)

	expected := longLine2 + "\n" + longLine3 + "\n"
	require.Equal(t, expected, result.String())

	require.True(t, reader.Filled.Frozen())
	require.Equal(t, usvc_io.TailReaderStats{TotalLines: 3, TailLines: 2}, *reader.FillStats.Load())

	require.NoError(t, reader.Close())
}

// Test partial read
func TestTailReaderPartialRead(t *testing.T) {
	t.Parallel()

	input := io.NopCloser(strings.NewReader("line1\nline2\nline3\nline4\nline5\n"))
	reader := usvc_io.NewTailReader(input, 3)

	// Read only part of the first line
	buf := make([]byte, 4) // "line" without the trailing "3\n"
	n, err := reader.Read(buf)
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.Equal(t, "line", string(buf[:n]))

	// Read the rest
	var result bytes.Buffer
	result.Write(buf[:n])
	_, err = io.Copy(&result, reader)
	require.NoError(t, err)

	require.Equal(t, "line3\nline4\nline5\n", result.String())
	require.NoError(t, reader.Close())

	require.True(t, reader.Filled.Frozen())
	require.Equal(t, usvc_io.TailReaderStats{TotalLines: 5, TailLines: 3}, *reader.FillStats.Load())
}

// Test invalid tailSize
func TestTailReaderInvalidTailSize(t *testing.T) {
	t.Parallel()

	input := io.NopCloser(strings.NewReader(""))

	assert.Panics(t, func() {
		_ = usvc_io.NewTailReader(input, 0)
	})

	assert.Panics(t, func() {
		_ = usvc_io.NewTailReader(input, -1)
	})
}

// Test that the reader is usable after EOF from the inner reader.
func TestTailReaderUsableAfterEOF(t *testing.T) {
	t.Parallel()

	content1 := "line1\nline2\nline3\nline4\nline5\nline6\n"
	content2 := "line7\nline8\nline9\nline10\n"

	input := testutil.NewTestReader()
	input.AddEntry(testutil.AsByteTimelineEntries([]byte(content1)...)...)
	input.AddEntry(testutil.AsErrorTimelineEntry(io.EOF))
	input.AddEntry(testutil.AsByteTimelineEntries([]byte(content2)...)...)

	reader := usvc_io.NewTailReader(input, 2)
	var buf bytes.Buffer
	_, err := io.Copy(&buf, reader)
	require.NoError(t, err)

	// Expected because the middle EOF serves as a separator indicating where "most recent" lines end,
	// and will be consumed by the reader. content2 will then follow directly after.
	require.Equal(t, "line5\nline6\n"+content2, buf.String())
	require.True(t, reader.Filled.Frozen())
	require.Equal(t, usvc_io.TailReaderStats{TotalLines: 6, TailLines: 2}, *reader.FillStats.Load())

	// Now try the same but with two EOFs in a row.

	buf.Reset()
	input = testutil.NewTestReader()
	input.AddEntry(testutil.AsByteTimelineEntries([]byte(content1)...)...)
	input.AddEntry(testutil.AsErrorTimelineEntry(io.EOF))
	input.AddEntry(testutil.AsErrorTimelineEntry(io.EOF))
	input.AddEntry(testutil.AsByteTimelineEntries([]byte(content2)...)...)

	reader = usvc_io.NewTailReader(input, 2)
	_, err = io.Copy(&buf, reader)
	require.NoError(t, err)
	require.Equal(t, "line5\nline6\n", buf.String()) // Only lines from content1.
	require.True(t, reader.Filled.Frozen())
	require.Equal(t, usvc_io.TailReaderStats{TotalLines: 6, TailLines: 2}, *reader.FillStats.Load())

	// Reader should be usable and return data from content2.
	buf.Reset()
	_, err = io.Copy(&buf, reader)
	require.NoError(t, err)
	require.Equal(t, content2, buf.String())

	// FillStats should not change.
	require.True(t, reader.Filled.Frozen())
	require.Equal(t, usvc_io.TailReaderStats{TotalLines: 6, TailLines: 2}, *reader.FillStats.Load())
}
