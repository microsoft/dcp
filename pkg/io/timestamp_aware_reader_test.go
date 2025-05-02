package io_test

import (
	"bytes"
	"fmt"
	"io"
	"testing"
	"time"

	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
	"github.com/stretchr/testify/require"
)

func TestTimestampAwareReaderIncludesNonTimestampPrefix(t *testing.T) {
	t.Parallel()

	testText := "testinput "

	testReader := testutil.NewTestReader()
	testReader.AddEntry(testutil.AsByteTimelineEntries([]byte(testText)...)...)

	timestampReader := usvc_io.NewTimestampAwareReader(testReader, usvc_io.TimestampAwareReaderOptions{Timestamps: true})
	readBytes := make([]byte, len(testText))
	n, err := timestampReader.Read(readBytes)
	if err != nil && err != io.EOF {
		require.Fail(t, fmt.Sprintf("unexpected error: %s", err))
	}

	require.Equal(t, testText, string(readBytes[:n]))
}

func TestTimestampAwareReaderDoesNotIncludeTimestampPrefix(t *testing.T) {
	t.Parallel()

	expectedText := "this is the expected output"
	testText := time.Now().UTC().Format(osutil.RFC3339MiliTimestampFormat) + " " + expectedText

	testReader := testutil.NewTestReader()
	testReader.AddEntry(testutil.AsByteTimelineEntries([]byte(testText)...)...)

	timestampReader := usvc_io.NewTimestampAwareReader(testReader, usvc_io.TimestampAwareReaderOptions{Timestamps: false})
	readBytes := make([]byte, len(testText))
	n, err := timestampReader.Read(readBytes)
	if err != nil && err != io.EOF {
		require.Fail(t, fmt.Sprintf("unexpected error: %s", err))
	}

	require.Equal(t, expectedText, string(readBytes[:n]))
}

func TestTimestampAwareReaderIncludesTimestampPrefixIfRequested(t *testing.T) {
	t.Parallel()

	dataText := "this is the expected output"
	testText := time.Now().UTC().Format(osutil.RFC3339MiliTimestampFormat) + " " + dataText

	testReader := testutil.NewTestReader()
	testReader.AddEntry(testutil.AsByteTimelineEntries([]byte(testText)...)...)

	timestampReader := usvc_io.NewTimestampAwareReader(testReader, usvc_io.TimestampAwareReaderOptions{Timestamps: true})
	readBytes := make([]byte, len(testText))
	n, err := timestampReader.Read(readBytes)
	if err != nil && err != io.EOF {
		require.Fail(t, fmt.Sprintf("unexpected error: %s", err))
	}

	require.Equal(t, testText, string(readBytes[:n]))
}

// Ensures that the TimestampAwareReader correctly handles various cases of fully- and partially-written log lines
// (in timestamp-ignoring mode).
func TestTimestampAwareReaderInitialReadsIgnoringTimestamps(t *testing.T) {
	t.Parallel()

	type testcase struct {
		testText     string
		expectedText string
		description  string
	}

	timestamp := time.Now().UTC().Format(osutil.RFC3339MiliTimestampFormat)
	testCases := []testcase{
		{"", "", "nothing"},
		{"2025-03-", "", "partial timestamp"},
		{timestamp, "", "full timestamp"},
		{timestamp + " ", "", "full timestamp with single space"},
		{timestamp + "   ", "  ", "full timestamp with multiple spaces"},
		{timestamp + " data\r", "data\r", "data followed by CR"},
		{timestamp + " data\r\n", "data\r\n", "data followed by CRLF"},
		{timestamp + " data\n", "data\n", "data followed by LF"},
		{timestamp + " data\r\n2025-03-", "data\r\n", "data followed by CRLF and partial timestamp"},
		{timestamp + " data\n2025-03-", "data\n", "data followed by LF and partial timestamp"},
		{timestamp + " data\r\n" + timestamp, "data\r\n", "data followed by CRLF and full timestamp"},
		{timestamp + " data\n" + timestamp, "data\n", "data followed by LF and full timestamp"},
		{timestamp + " data\r\n" + timestamp + " ", "data\r\n", "data followed by CRLF and full timestamp and space"},
		{timestamp + " data\n" + timestamp + " ", "data\n", "data followed by LF and full timestamp and space"},
		{timestamp + " data\r\n" + timestamp + " data2\r\n", "data\r\ndata2\r\n", "data followed by CRLF and full timestamp and more data"},
		{timestamp + " data\n" + timestamp + " data2\n", "data\ndata2\n", "data followed by LF and full timestamp and more data"},
	}

	for _, tc := range testCases {
		testReader := testutil.NewTestReader()
		testReader.AddEntry(testutil.AsByteTimelineEntries([]byte(tc.testText)...)...)

		timestampReader := usvc_io.NewTimestampAwareReader(testReader, usvc_io.TimestampAwareReaderOptions{Timestamps: false})
		readBytes := make([]byte, len(tc.testText)+1)
		n, err := timestampReader.Read(readBytes)
		if err != nil && err != io.EOF {
			require.Fail(t, fmt.Sprintf("unexpected error: %s", err))
		}
		require.Equal(t, tc.expectedText, string(readBytes[:n]), "test case: '%s' resulted in unexpected output from the reader", tc.description)
	}
}

// Ensures that the TimestampAwareReader correctly resumes reading after encountering EOF
// in various cases of fully- and partially-written log lines (in timestamp-ignoring mode).
func TestTimestampAwareReaderEofHandlingIgnoringTimestamps(t *testing.T) {
	t.Parallel()

	type testcase struct {
		firstRead    string
		secondRead   string
		expectedText string
		description  string
	}

	timestamp := time.Now().UTC().Format(osutil.RFC3339MiliTimestampFormat)
	testCases := []testcase{
		{"", timestamp + " data", "data", "EOF before anything"},
		{timestamp[:5], timestamp[5:] + "  ", " ", "divided timestamp"},
		{timestamp, " data", "data", "EOF between timestamp and space"},
		{timestamp + " ", "data", "data", "EOF after space"},
		{timestamp + " data", "\n" + timestamp + " data2", "data\ndata2", "EOF before LF"},
		{timestamp + " data\n", timestamp + " data2", "data\ndata2", "EOF after LF"},
		{timestamp + " data", "\r\n" + timestamp + " data2", "data\r\ndata2", "EOF before CRLF"},
		{timestamp + " data\r", "\n" + timestamp + " data2", "data\r\ndata2", "EOF before CR and after LF"},
		{timestamp + " data\r\n", timestamp + " data2", "data\r\ndata2", "EOF after CRLF"},
	}

	for _, tc := range testCases {
		testReader := testutil.NewTestReader()
		testReader.AddEntry(testutil.AsByteTimelineEntries([]byte(tc.firstRead)...)...)
		testReader.AddEntry(testutil.AsErrorTimelineEntry(io.EOF))
		testReader.AddEntry(testutil.AsByteTimelineEntries([]byte(tc.secondRead)...)...)

		timestampReader := usvc_io.NewTimestampAwareReader(testReader, usvc_io.TimestampAwareReaderOptions{Timestamps: false})
		readBytes := make([]byte, len(tc.firstRead)+len(tc.secondRead)+1)
		n, err := timestampReader.Read(readBytes)
		if err != nil && err != io.EOF {
			require.Fail(t, fmt.Sprintf("unexpected error on first read: %s", err))
		}
		n2, err2 := timestampReader.Read(readBytes[n:])
		if err2 != nil && err2 != io.EOF {
			require.Fail(t, fmt.Sprintf("unexpected error on second read: %s", err2))
		}
		require.Equal(t, tc.expectedText, string(readBytes[:(n+n2)]), "test case: '%s' resulted in unexpected output from the reader", tc.description)
	}
}

func TestTimestampAwareReaderDoesNotOverfillBuffer(t *testing.T) {
	t.Parallel()

	expectedText := "this is the expected output"
	testText := time.Now().UTC().Format(osutil.RFC3339MiliTimestampFormat) + " " + expectedText
	testReader := testutil.NewTestReader()
	testReader.AddEntry(testutil.AsByteTimelineEntries([]byte(testText)...)...)

	timestampReader := usvc_io.NewTimestampAwareReader(testReader, usvc_io.TimestampAwareReaderOptions{Timestamps: false})
	buf := make([]byte, 5)
	result := ""

	for {
		n, err := timestampReader.Read(buf)
		if err != nil && err != io.EOF {
			require.Fail(t, fmt.Sprintf("unexpected error: %s", err))
		}
		if n == 0 {
			break
		}
		result += string(buf[:n])
	}

	require.Equal(t, expectedText, result)
}

func TestTimestampAwareReaderLimitOption(t *testing.T) {
	t.Parallel()

	type testcase struct {
		input        string
		options      usvc_io.TimestampAwareReaderOptions
		expectedText string
		description  string
	}

	timestamp := time.Now().UTC().Format(osutil.RFC3339MiliTimestampFormat)
	testCases := []testcase{
		{
			input: timestamp + " line1\n" + timestamp + " line2\n" + timestamp + " line3\n",
			options: usvc_io.TimestampAwareReaderOptions{
				Timestamps: false,
				Limit:      2,
			},
			expectedText: "line1\nline2\n",
			description:  "limit with timestamps stripped",
		},
		{
			input: timestamp + " line1\n" + timestamp + " line2\n" + timestamp + " line3\n",
			options: usvc_io.TimestampAwareReaderOptions{
				Timestamps: true,
				Limit:      2,
			},
			expectedText: timestamp + " line1\n" + timestamp + " line2\n",
			description:  "limit with timestamps included",
		},
		{
			input: "line1\nline2\nline3\n",
			options: usvc_io.TimestampAwareReaderOptions{
				Timestamps: false,
				Limit:      2,
			},
			expectedText: "line1\nline2\n",
			description:  "limit without timestamps",
		},
		{
			input: timestamp + " line1\n\n" + timestamp + " line3\n",
			options: usvc_io.TimestampAwareReaderOptions{
				Timestamps: false,
				Limit:      2,
			},
			expectedText: "line1\n\n",
			description:  "limit with empty line",
		},
		{
			input: "",
			options: usvc_io.TimestampAwareReaderOptions{
				Timestamps: false,
				Limit:      2,
			},
			expectedText: "",
			description:  "limit with empty input",
		},
		{
			input: timestamp + " line1",
			options: usvc_io.TimestampAwareReaderOptions{
				Timestamps: false,
				Limit:      1,
			},
			expectedText: "line1",
			description:  "limit with no newline",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			testReader := testutil.NewTestReader()
			testReader.AddEntry(testutil.AsByteTimelineEntries([]byte(tc.input)...)...)

			timestampReader := usvc_io.NewTimestampAwareReader(testReader, tc.options)

			var result bytes.Buffer
			buf := make([]byte, 1024)

			for {
				n, err := timestampReader.Read(buf)
				if n > 0 {
					result.Write(buf[:n])
				}
				if err != nil {
					if err != io.EOF {
						require.Fail(t, fmt.Sprintf("unexpected error: %s", err))
					}
					break
				}
			}

			require.Equal(t, tc.expectedText, result.String(), "test case: '%s' resulted in unexpected output", tc.description)
		})
	}
}

func TestTimestampAwareReaderSkipOption(t *testing.T) {
	t.Parallel()

	type testcase struct {
		input        string
		options      usvc_io.TimestampAwareReaderOptions
		expectedText string
		description  string
	}

	timestamp := time.Now().UTC().Format(osutil.RFC3339MiliTimestampFormat)
	testCases := []testcase{
		{
			input: timestamp + " line1\n" + timestamp + " line2\n" + timestamp + " line3\n",
			options: usvc_io.TimestampAwareReaderOptions{
				Timestamps: false,
				Skip:       1,
			},
			expectedText: "line2\nline3\n",
			description:  "skip with timestamps stripped",
		},
		{
			input: timestamp + " line1\n" + timestamp + " line2\n" + timestamp + " line3\n",
			options: usvc_io.TimestampAwareReaderOptions{
				Timestamps: true,
				Skip:       1,
			},
			expectedText: timestamp + " line2\n" + timestamp + " line3\n",
			description:  "skip with timestamps included",
		},
		{
			input: "line1\nline2\nline3\n",
			options: usvc_io.TimestampAwareReaderOptions{
				Timestamps: false,
				Skip:       2,
			},
			expectedText: "line3\n",
			description:  "skip without timestamps",
		},
		{
			input: timestamp + " line1\n\n" + timestamp + " line3\n",
			options: usvc_io.TimestampAwareReaderOptions{
				Timestamps: false,
				Skip:       2,
			},
			expectedText: "line3\n",
			description:  "skip with empty line",
		},
		{
			input: "",
			options: usvc_io.TimestampAwareReaderOptions{
				Timestamps: false,
				Skip:       2,
			},
			expectedText: "",
			description:  "skip with empty input",
		},
		{
			input: timestamp + " line1\n" + timestamp + " line2\n",
			options: usvc_io.TimestampAwareReaderOptions{
				Timestamps: false,
				Skip:       3,
			},
			expectedText: "",
			description:  "skip more lines than input has",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			testReader := testutil.NewTestReader()
			testReader.AddEntry(testutil.AsByteTimelineEntries([]byte(tc.input)...)...)

			timestampReader := usvc_io.NewTimestampAwareReader(testReader, tc.options)

			var result bytes.Buffer
			buf := make([]byte, 1024)

			for {
				n, err := timestampReader.Read(buf)
				if n > 0 {
					result.Write(buf[:n])
				}
				if err != nil {
					if err != io.EOF {
						require.Fail(t, fmt.Sprintf("unexpected error: %s", err))
					}
					break
				}
			}

			require.Equal(t, tc.expectedText, result.String(), "test case: '%s' resulted in unexpected output", tc.description)
		})
	}
}

func TestTimestampAwareReaderSkipAndLimitOptions(t *testing.T) {
	t.Parallel()

	type testcase struct {
		input        string
		options      usvc_io.TimestampAwareReaderOptions
		expectedText string
		description  string
	}

	timestamp := time.Now().UTC().Format(osutil.RFC3339MiliTimestampFormat)
	testCases := []testcase{
		{
			input: timestamp + " line1\n" + timestamp + " line2\n" + timestamp + " line3\n" + timestamp + " line4\n",
			options: usvc_io.TimestampAwareReaderOptions{
				Timestamps: false,
				Skip:       1,
				Limit:      2,
			},
			expectedText: "line2\nline3\n",
			description:  "skip and limit with timestamps stripped",
		},
		{
			input: timestamp + " line1\n" + timestamp + " line2\n" + timestamp + " line3\n" + timestamp + " line4\n",
			options: usvc_io.TimestampAwareReaderOptions{
				Timestamps: true,
				Skip:       1,
				Limit:      2,
			},
			expectedText: timestamp + " line2\n" + timestamp + " line3\n",
			description:  "skip and limit with timestamps included",
		},
		{
			input: "line1\nline2\nline3\nline4\n",
			options: usvc_io.TimestampAwareReaderOptions{
				Timestamps: false,
				Skip:       1,
				Limit:      2,
			},
			expectedText: "line2\nline3\n",
			description:  "skip and limit without timestamps",
		},
		{
			input: timestamp + " line1\n\n" + timestamp + " line3\n" + timestamp + " line4\n",
			options: usvc_io.TimestampAwareReaderOptions{
				Timestamps: false,
				Skip:       1,
				Limit:      2,
			},
			expectedText: "\nline3\n",
			description:  "skip and limit with empty line",
		},
		{
			input: "",
			options: usvc_io.TimestampAwareReaderOptions{
				Timestamps: false,
				Skip:       1,
				Limit:      2,
			},
			expectedText: "",
			description:  "skip and limit with empty input",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			testReader := testutil.NewTestReader()
			testReader.AddEntry(testutil.AsByteTimelineEntries([]byte(tc.input)...)...)

			timestampReader := usvc_io.NewTimestampAwareReader(testReader, tc.options)

			var result bytes.Buffer
			buf := make([]byte, 1024)

			for {
				n, err := timestampReader.Read(buf)
				if n > 0 {
					result.Write(buf[:n])
				}
				if err != nil {
					if err != io.EOF {
						require.Fail(t, fmt.Sprintf("unexpected error: %s", err))
					}
					break
				}
			}

			require.Equal(t, tc.expectedText, result.String(), "test case: '%s' resulted in unexpected output", tc.description)
		})
	}
}
