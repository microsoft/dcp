/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io_test

import (
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestTimestampWriterAppliesTimestamps(t *testing.T) {
	const newlineRegex = `\r?\n`

	type testcase struct {
		description   string
		inputLines    [][]byte
		expectedLines []*regexp.Regexp
	}

	testcases := []testcase{
		{
			description: "one-empty-line",
			inputLines: [][]byte{
				osutil.WithNewline([]byte("")),
			},
			expectedLines: []*regexp.Regexp{
				regexp.MustCompile(linePrefixRegex(1)),
			},
		},
		{
			description: "three-empty-lines",
			inputLines: [][]byte{
				osutil.WithNewline([]byte("")),
				osutil.WithNewline([]byte("")),
				osutil.WithNewline([]byte("")),
			},
			expectedLines: []*regexp.Regexp{
				regexp.MustCompile(linePrefixRegex(1)),
				regexp.MustCompile(linePrefixRegex(2)),
				regexp.MustCompile(linePrefixRegex(3)),
			},
		},
		{
			description: "leading-empty-line",
			inputLines: [][]byte{
				osutil.WithNewline([]byte("")),
				osutil.WithNewline([]byte("foo")),
				osutil.WithNewline([]byte("bar")),
			},
			expectedLines: []*regexp.Regexp{
				regexp.MustCompile(linePrefixRegex(1)),
				regexp.MustCompile(linePrefixRegex(2) + " foo"),
				regexp.MustCompile(linePrefixRegex(3) + " bar"),
			},
		},
		{
			description: "trailing-empty-line",
			inputLines: [][]byte{
				osutil.WithNewline([]byte("foo")),
				osutil.WithNewline([]byte("bar")),
				osutil.WithNewline([]byte("")),
			},
			expectedLines: []*regexp.Regexp{
				regexp.MustCompile(linePrefixRegex(1) + " foo"),
				regexp.MustCompile(linePrefixRegex(2) + " bar"),
				regexp.MustCompile(linePrefixRegex(3)),
			},
		},
		{
			description: "empty-lines-in-the-middle",
			inputLines: [][]byte{
				osutil.WithNewline([]byte("foo")),
				osutil.WithNewline([]byte("")),
				osutil.WithNewline([]byte("")),
				osutil.WithNewline([]byte("bar")),
			},
			expectedLines: []*regexp.Regexp{
				regexp.MustCompile(linePrefixRegex(1) + " foo"),
				regexp.MustCompile(linePrefixRegex(2)),
				regexp.MustCompile(linePrefixRegex(3)),
				regexp.MustCompile(linePrefixRegex(4) + " bar"),
			},
		},
		{
			description: "split-lines",
			inputLines: [][]byte{
				[]byte("line 1 start..."),
				osutil.WithNewline([]byte("... line 1 end")),
				osutil.WithNewline([]byte("")),
				[]byte("line 3 start..."),
				osutil.WithNewline([]byte("... line 3 end")),
			},
			expectedLines: []*regexp.Regexp{
				regexp.MustCompile(linePrefixRegex(1) + ` line 1 start\.\.\.`),
				regexp.MustCompile(`\.\.\. line 1 end`),
				regexp.MustCompile(linePrefixRegex(2)),
				regexp.MustCompile(linePrefixRegex(3) + ` line 3 start\.\.\.`),
				regexp.MustCompile(`\.\.\. line 3 end`),
			},
		},
		{
			description: "compressed-lines",
			inputLines: [][]byte{
				append(osutil.WithNewline([]byte("line 1")), osutil.WithNewline([]byte("line 2"))...),
				append(osutil.WithNewline([]byte("")), osutil.WithNewline([]byte(""))...),
				append(osutil.WithNewline([]byte("")), osutil.WithNewline([]byte("line 6"))...),
				append(osutil.WithNewline([]byte("line 7")), osutil.WithNewline([]byte(""))...),
				append(append(osutil.WithNewline([]byte("line 9")), osutil.WithNewline([]byte(""))...), osutil.WithNewline([]byte("line 11"))...),
			},
			expectedLines: []*regexp.Regexp{
				regexp.MustCompile(linePrefixRegex(1) + " line 1" + newlineRegex + linePrefixRegex(2) + " line 2"),
				regexp.MustCompile(linePrefixRegex(3) + " " + newlineRegex + linePrefixRegex(4)),
				regexp.MustCompile(linePrefixRegex(5) + " " + newlineRegex + linePrefixRegex(6) + " line 6"),
				regexp.MustCompile(linePrefixRegex(7) + " line 7" + newlineRegex + linePrefixRegex(8)),
				regexp.MustCompile(linePrefixRegex(9) + " line 9" + newlineRegex + linePrefixRegex(10) + " " + newlineRegex + linePrefixRegex(11) + " line 11"),
			},
		},
		{
			description: "compressed-and-split-lines",
			inputLines: [][]byte{
				append(osutil.WithNewline([]byte("line 1")), []byte("line 2 start...")...),
				append(osutil.WithNewline([]byte("...line 2 end")), osutil.WithNewline([]byte(""))...),
				append(osutil.WithNewline([]byte("")), osutil.WithNewline([]byte("line 5 start..."))...),
				[]byte("line 5 continued..."),
				osutil.WithNewline([]byte("...line 5 end")),
			},
			expectedLines: []*regexp.Regexp{
				regexp.MustCompile(linePrefixRegex(1) + " line 1" + newlineRegex + linePrefixRegex(2) + ` line 2 start\.\.\.`),
				regexp.MustCompile(`\.\.\.line 2 end` + newlineRegex + linePrefixRegex(3)),
				regexp.MustCompile(linePrefixRegex(4) + " " + newlineRegex + linePrefixRegex(5) + ` line 5 start\.\.\.`),
				regexp.MustCompile(`line 5 continued\.\.\.`),
				regexp.MustCompile(`\.\.\.line 5 end`),
			},
		},
	}

	t.Parallel()

	// Run testcases with TimestampWriter
	for _, tc := range testcases {
		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()

			sink := testutil.NewBufferWriter()
			tw := usvc_io.NewTimestampWriter(sink)

			for _, line := range tc.inputLines {
				_, err := tw.Write(line)
				require.NoError(t, err)
			}

			output := sink.Bytes()
			chunks := sink.Chunks()

			for i, expectedLine := range tc.expectedLines {
				require.Regexp(
					t, expectedLine, string(output[chunks[i].Offset:chunks[i].Offset+chunks[i].Length]),
					"write with index %d did not match expected", i,
				)
			}
		})
	}

	// Run the same testcases with indexingWriter
	for _, tc := range testcases {
		t.Run(tc.description+"_indexing-writer", func(t *testing.T) {
			t.Parallel()

			sink := testutil.NewBufferWriter()
			indexSink := testutil.NewBufferWriter()
			iw := usvc_io.NewIndexingWriter(sink, indexSink)

			for _, line := range tc.inputLines {
				_, err := iw.Write(line)
				require.NoError(t, err)
			}

			output := sink.Bytes()
			chunks := sink.Chunks()

			for i, expectedLine := range tc.expectedLines {
				require.Regexp(
					t, expectedLine, string(output[chunks[i].Offset:chunks[i].Offset+chunks[i].Length]),
					"write with index %d did not match expected", i,
				)
			}
		})
	}
}

func linePrefixRegex(lineNum uint64) string {
	return strconv.FormatUint(lineNum, 10) + " " + osutil.RFC3339MiliTimestampRegex
}
