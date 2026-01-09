// Copyright (c) Microsoft Corporation. All rights reserved.

package perftrace

import (
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/testutil"
)

func TestPerfTraceRequestParsed(t *testing.T) {
	logSink := testutil.NewMockLoggerSink()

	reqStr := "startup=30s,shutdown=15s"
	requests := parseProfilingRequests(reqStr, logr.New(logSink))

	expected := map[ProfileType]time.Duration{
		ProfileTypeStartup:  30 * time.Second,
		ProfileTypeShutdown: 15 * time.Second,
	}
	require.Equal(t, expected, requests)
}

func TestPerfTraceInvalidTraceType(t *testing.T) {
	logSink := testutil.NewMockLoggerSink()
	logSink.EnableErrorCall()

	reqStr := "unknown=30s"
	requests := parseProfilingRequests(reqStr, logr.New(logSink))

	require.Len(t, requests, 0)
	logSink.AssertNumberOfCalls(t, "Error", 1)
}

func TestPerfTraceInvalidTraceDuration(t *testing.T) {
	logSink := testutil.NewMockLoggerSink()
	logSink.EnableErrorCall()

	reqStr := "startup=xxx"
	requests := parseProfilingRequests(reqStr, logr.New(logSink))

	require.Len(t, requests, 0)
	logSink.AssertNumberOfCalls(t, "Error", 1)
}

func TestPerfTraceRequestInvalid(t *testing.T) {
	logSink := testutil.NewMockLoggerSink()
	logSink.EnableErrorCall()

	reqStr := "blah123"
	requests := parseProfilingRequests(reqStr, logr.New(logSink))

	require.Len(t, requests, 0)
	logSink.AssertNumberOfCalls(t, "Error", 1)
}

func TestPerfTraceRequestEmpty(t *testing.T) {
	logSink := testutil.NewMockLoggerSink()
	log := logr.New(logSink)

	reqStr := ""
	requests := parseProfilingRequests(reqStr, log)
	require.Len(t, requests, 0)

	reqStr = "   "
	requests = parseProfilingRequests(reqStr, log)
	require.Len(t, requests, 0)
}
