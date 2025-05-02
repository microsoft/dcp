package io_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/usvc-apiserver/pkg/io"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

var (
	lineSep = string(osutil.LineSep())
)

// Tests that IndexingWriter properly writes index entries with formatted timestamps and correct line/offset values
func TestIndexingWriterBasicIndexing(t *testing.T) {
	t.Parallel()
	dataWriter := testutil.NewBufferWriter()
	indexWriter := testutil.NewBufferWriter()

	stride := uint32(3)
	writer := usvc_io.NewIndexingWriterWithStride(dataWriter, indexWriter, stride)

	// Write several lines of text
	totalLines := 10
	for i := 0; i < totalLines; i++ {
		line := "Line " + strconv.Itoa(i) + lineSep
		n, err := writer.Write([]byte(line))
		require.NoError(t, err)
		require.Equal(t, len(line), n)
	}

	verifyIndex(t, dataWriter, indexWriter, stride)
}

// Tests that empty lines are indexed properly
func TestIndexingWriterEmptyLines(t *testing.T) {
	t.Parallel()
	dataWriter := testutil.NewBufferWriter()
	indexWriter := testutil.NewBufferWriter()

	// Use a small stride for easier testing
	stride := uint32(3)
	writer := usvc_io.NewIndexingWriterWithStride(dataWriter, indexWriter, stride)

	// Write a mix of empty and non-empty lines
	lines := []string{
		"Line 1" + lineSep,
		lineSep, // Empty line
		"Line 3" + lineSep,
		lineSep, // Empty line
		"Line 5" + lineSep,
		"Line 6" + lineSep,
		lineSep, // Empty line
		"Line 8" + lineSep,
		"Line 9" + lineSep,
	}

	for _, line := range lines {
		n, err := writer.Write([]byte(line))
		require.NoError(t, err)
		require.Equal(t, len(line), n)
	}

	verifyIndex(t, dataWriter, indexWriter, stride)
}

// Tests that write errors in the data writer set the indexInvalid flag
func TestIndexingWriterDataError(t *testing.T) {
	t.Parallel()
	dataWriter := testutil.NewBufferWriter()
	indexWriter := testutil.NewBufferWriter()

	stride := uint32(3)
	failWrite := 3 * int(stride)
	writer := usvc_io.NewIndexingWriterWithStride(dataWriter, indexWriter, stride)

	// Write some lines successfully
	for i := 0; i < failWrite; i++ {
		line := "Line " + strconv.Itoa(i) + lineSep
		n, err := writer.Write([]byte(line))
		require.NoError(t, err)
		require.Equal(t, len(line), n)
	}

	// Verify the index is valid so far
	require.False(t, writer.IndexInvalid(), "Index should be valid before errors")

	// Make the data writer fail
	writeErr := errors.New(t.Name() + ": write error")
	dataWriter.FailNextWrite(writeErr)
	_, expectedErr := writer.Write([]byte(fmt.Sprintf("Line %d%s", failWrite, lineSep)))
	require.Error(t, expectedErr)

	// Verify the index is now marked as invalid
	require.True(t, writer.IndexInvalid(), "Index should be invalid after data write error")

	// Write several more lines. Writing should still work, but no more index lines should be written.
	for i := failWrite; i < 2*failWrite; i++ {
		line := "Line " + strconv.Itoa(i) + lineSep
		n, err := writer.Write([]byte(line))
		require.NoError(t, err)
		require.Equal(t, len(line), n)
	}

	// Verify no more index lines were written after the failure
	indexLines := indexWriter.Lines(osutil.LineSep())
	expectedIndexLines := ceilIntDiv(failWrite, int(stride))
	require.Equal(t, expectedIndexLines, len(indexLines), "Did not get expected number of index lines")
}

// Tests that sync errors set the indexInvalid flag
func TestIndexingWriterSyncError(t *testing.T) {
	t.Parallel()
	dataWriter := testutil.NewBufferWriter()
	indexWriter := testutil.NewBufferWriter()

	stride := uint32(3)
	syncErrorWrite := 3 * int(stride) // The sequence number of the write that has sync error enabled.
	writer := usvc_io.NewIndexingWriterWithStride(dataWriter, indexWriter, stride)

	// Write some lines successfully
	for i := 0; i < syncErrorWrite; i++ {
		line := "Line " + strconv.Itoa(i) + lineSep
		n, err := writer.Write([]byte(line))
		require.NoError(t, err)
		require.Equal(t, len(line), n)
	}

	// Verify the index is valid so far
	require.False(t, writer.IndexInvalid(), "Index should be valid before errors")

	// Make the data writer fail on sync
	syncErr := errors.New(t.Name() + ": sync error")
	dataWriter.FailNextSync(syncErr)

	// Write more lines to trigger another sync
	for i := syncErrorWrite; i < 2*syncErrorWrite; i++ {
		line := "Line " + strconv.Itoa(i) + lineSep
		n, err := writer.Write([]byte(line))
		require.NoError(t, err)
		require.Equal(t, len(line), n)
	}

	// Verify the index is now marked as invalid
	require.True(t, writer.IndexInvalid(), "Index should be invalid after sync error")

	// Verify no more index lines were written after the failure
	indexLines := indexWriter.Lines(osutil.LineSep())
	expectedIndexLines := ceilIntDiv(syncErrorWrite, int(stride))
	require.Equal(t, expectedIndexLines, len(indexLines), "Did not get expected number of index lines")
}

// Tests that write errors in the index writer set the indexInvalid flag
func TestIndexingWriterIndexError(t *testing.T) {
	t.Parallel()
	dataWriter := testutil.NewBufferWriter()
	indexWriter := testutil.NewBufferWriter()

	stride := uint32(3)
	indexErrorWrite := 3*int(stride) + 1 // The sequence number of the DATA write that enables INDEX write error.
	writer := usvc_io.NewIndexingWriterWithStride(dataWriter, indexWriter, stride)

	// Write lines until we hit the one that will cause the index write error
	for i := 0; i < indexErrorWrite; i++ {
		line := "Line " + strconv.Itoa(i) + lineSep
		n, err := writer.Write([]byte(line))
		require.NoError(t, err)
		require.Equal(t, len(line), n)
	}

	// Verify the index is valid so far
	require.False(t, writer.IndexInvalid(), "Index should be valid")

	indexErr := errors.New(t.Name() + ": index write error")
	indexWriter.FailNextWrite(indexErr)

	// Write more lines to trigger the index write error
	for i := indexErrorWrite; i < 3*indexErrorWrite; i++ {
		line := "Line " + strconv.Itoa(i) + lineSep
		n, err := writer.Write([]byte(line))
		require.NoError(t, err)
		require.Equal(t, len(line), n)
	}

	require.True(t, writer.IndexInvalid(), "Index should be invalid after index write error")

	// Verify no more index lines were written after the failure
	indexLines := indexWriter.Lines(osutil.LineSep())
	expectedIndexLines := ceilIntDiv(indexErrorWrite, int(stride))
	require.Equal(t, expectedIndexLines, len(indexLines), "Did not get expected number of index lines")
}

// Tests that closing the writer closes both underlying writers
func TestIndexingWriterClose(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()

	dataWriter := testutil.NewBufferWriter()
	indexWriter := testutil.NewBufferWriter()

	writer := usvc_io.NewIndexingWriter(dataWriter, indexWriter)

	// Close the writer
	err := writer.Close()
	require.NoError(t, err)

	// Verify that both underlying writers were closed
	select {
	case <-dataWriter.Closed():
	case <-ctx.Done():
		t.Fatalf("Timed out waiting for data writer to close: %v", ctx.Err())
	}
	select {
	case <-indexWriter.Closed():
	case <-ctx.Done():
		t.Fatalf("Timed out waiting for index writer to close: %v", ctx.Err())
	}

	// Try to write after closing
	_, err = writer.Write([]byte("Should not work" + lineSep))
	require.ErrorIs(t, err, usvc_io.ErrClosedWriter)
}

func parseIndexLine(line []byte) (io.IndexLine, error) {
	parts := bytes.Split(bytes.TrimSpace(line), []byte(" "))
	if len(parts) != 3 {
		return io.IndexLine{}, errors.New("invalid index line format")
	}

	timestamp, tsParseErr := time.Parse(time.RFC3339, string(parts[0]))
	if tsParseErr != nil {
		return io.IndexLine{}, tsParseErr
	}

	lineNum, lnParseErr := strconv.ParseUint(string(parts[1]), 10, 32)
	if lnParseErr != nil {
		return io.IndexLine{}, lnParseErr
	}
	if lineNum > math.MaxUint32 {
		return io.IndexLine{}, errors.New("line number out of range")
	}

	offset, offsetParseErr := strconv.ParseUint(string(parts[2]), 10, 64)
	if offsetParseErr != nil {
		return io.IndexLine{}, offsetParseErr
	}

	return io.IndexLine{
		Timestamp: timestamp,
		Line:      uint32(lineNum),
		Offset:    offset,
	}, nil
}

// Verifies that "index lines" correctly describe the corresponding data lines.
// - Line numbers in the index should be multiples of the stride
// - The offset in the index should point to the start of the corresponding data line
// - The timestamp in the index should match the timestamp in the data line
func verifyIndex(t *testing.T, dataWriter, indexWriter *testutil.BufferWriter, stride uint32) {
	// The way bytes.Split() works is that it will return an empty slice as the last element
	// if the input data ends with a separator. So we need to trim it.
	dataLines := dataWriter.Lines(osutil.LineSep())
	indexLines := indexWriter.Lines(osutil.LineSep())

	expectedIndexLines := ceilIntDiv(len(dataLines), int(stride))
	require.Equal(t, expectedIndexLines, len(indexLines), "Did not get expected number of index lines")

	dataLineOffsets := make([]uint64, len(dataLines))
	dataLineOffsets[0] = 0
	for i := 1; i < len(dataLines); i++ {
		dataLineOffsets[i] = dataLineOffsets[i-1] + uint64(len(dataLines[i-1])) + uint64(len(lineSep))
	}

	for i, indexLine := range indexLines {
		il, err := parseIndexLine(indexLine)
		require.NoError(t, err, "Failed to parse index line: %s", string(indexLine))

		expectedLineNum := uint32(i) * stride
		require.Equal(t, expectedLineNum, il.Line, "Unexpected line number in index line")

		require.Equal(t, dataLineOffsets[expectedLineNum], il.Offset, "Data line offset does not match expected value")

		dataTimestamp, parseErr := time.Parse(time.RFC3339, string(bytes.Split(dataLines[expectedLineNum], []byte(" "))[0]))
		require.NoError(t, parseErr, "Failed to parse timestamp from data line")
		require.WithinDuration(t, il.Timestamp, dataTimestamp, time.Millisecond, "Index and data timestamps do not match")
	}
}

func ceilIntDiv(a, b int) int {
	if a%b == 0 {
		return a / b
	}
	return a/b + 1
}
