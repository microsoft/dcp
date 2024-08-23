package io_test

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
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
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex),
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
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex),
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex),
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex),
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
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex),
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex + " foo"),
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex + " bar"),
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
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex + " foo"),
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex + " bar"),
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex),
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
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex + " foo"),
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex),
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex),
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex + " bar"),
			},
		},
		{
			description: "split-lines",
			inputLines: [][]byte{
				[]byte("line 1 start..."),
				osutil.WithNewline([]byte("... line 1 end")),
				osutil.WithNewline([]byte("")),
				[]byte("line 2 start..."),
				osutil.WithNewline([]byte("... line 2 end")),
			},
			expectedLines: []*regexp.Regexp{
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex + ` line 1 start\.\.\.`),
				regexp.MustCompile(`\.\.\. line 1 end`),
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex),
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex + ` line 2 start\.\.\.`),
				regexp.MustCompile(`\.\.\. line 2 end`),
			},
		},
		{
			description: "compressed-lines",
			inputLines: [][]byte{
				append(osutil.WithNewline([]byte("line 1")), osutil.WithNewline([]byte("line 2"))...),
				append(osutil.WithNewline([]byte("")), osutil.WithNewline([]byte(""))...),
				append(osutil.WithNewline([]byte("")), osutil.WithNewline([]byte("line 3"))...),
				append(osutil.WithNewline([]byte("line 4")), osutil.WithNewline([]byte(""))...),
				append(append(osutil.WithNewline([]byte("line 5")), osutil.WithNewline([]byte(""))...), osutil.WithNewline([]byte("line 6"))...),
			},
			expectedLines: []*regexp.Regexp{
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex + " line 1" + newlineRegex + testutil.RFC3339MiliTimestampRegex + " line 2"),
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex + " " + newlineRegex + testutil.RFC3339MiliTimestampRegex),
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex + " " + newlineRegex + testutil.RFC3339MiliTimestampRegex + " line 3"),
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex + " line 4" + newlineRegex + testutil.RFC3339MiliTimestampRegex),
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex + " line 5" + newlineRegex + testutil.RFC3339MiliTimestampRegex + " " + newlineRegex + testutil.RFC3339MiliTimestampRegex + " line 6"),
			},
		},
		{
			description: "compressed-and-split-lines",
			inputLines: [][]byte{
				append(osutil.WithNewline([]byte("line 1")), []byte("line 2 start...")...),
				append(osutil.WithNewline([]byte("...line 2 end")), osutil.WithNewline([]byte(""))...),
				append(osutil.WithNewline([]byte("")), osutil.WithNewline([]byte("line 3 start..."))...),
				[]byte("line 3 continued..."),
				osutil.WithNewline([]byte("...line 3 end")),
			},
			expectedLines: []*regexp.Regexp{
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex + " line 1" + newlineRegex + testutil.RFC3339MiliTimestampRegex + ` line 2 start\.\.\.`),
				regexp.MustCompile(`\.\.\.line 2 end` + newlineRegex + testutil.RFC3339MiliTimestampRegex),
				regexp.MustCompile(testutil.RFC3339MiliTimestampRegex + " " + newlineRegex + testutil.RFC3339MiliTimestampRegex + ` line 3 start\.\.\.`),
				regexp.MustCompile(`line 3 continued\.\.\.`),
				regexp.MustCompile(`\.\.\.line 3 end`),
			},
		},
	}

	t.Parallel()

	for _, tc := range testcases {
		t.Run(tc.description, func(t *testing.T) {
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
}
