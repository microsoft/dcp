package perftrace

import (
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

func TestPerfTraceRequestParsed(t *testing.T) {
	logSink := &testutil.MockLoggerSink{}
	logSink.On("Init", mock.AnythingOfType("logr.RuntimeInfo")).Return()
	// Not setting up any other calls, Error in particular, since we do not expect any errors.

	reqStr := "startup=30s,shutdown=15s"
	requests := parseProfilingRequests(reqStr, logr.New(logSink))

	expected := map[ProfileType]time.Duration{
		ProfileTypeStartup:  30 * time.Second,
		ProfileTypeShutdown: 15 * time.Second,
	}
	require.Equal(t, expected, requests)
}

func TestPerfTraceInvalidTraceType(t *testing.T) {
	logSink := &testutil.MockLoggerSink{}
	logSink.On("Init", mock.AnythingOfType("logr.RuntimeInfo")).Return()
	logSink.On("Error", mock.AnythingOfType("*errors.errorString"), mock.AnythingOfType("string"), mock.Anything).Return()

	reqStr := "unknown=30s"
	requests := parseProfilingRequests(reqStr, logr.New(logSink))

	require.Len(t, requests, 0)
	logSink.AssertNumberOfCalls(t, "Error", 1)
}

func TestPerfTraceInvalidTraceDuration(t *testing.T) {
	logSink := &testutil.MockLoggerSink{}
	logSink.On("Init", mock.AnythingOfType("logr.RuntimeInfo")).Return()
	logSink.On("Error", mock.AnythingOfType("*errors.errorString"), mock.AnythingOfType("string"), mock.Anything).Return()

	reqStr := "startup=xxx"
	requests := parseProfilingRequests(reqStr, logr.New(logSink))

	require.Len(t, requests, 0)
	logSink.AssertNumberOfCalls(t, "Error", 1)
}

func TestPerfTraceRequestInvalid(t *testing.T) {
	logSink := &testutil.MockLoggerSink{}
	logSink.On("Init", mock.AnythingOfType("logr.RuntimeInfo")).Return()
	logSink.On("Error", mock.AnythingOfType("*errors.errorString"), mock.AnythingOfType("string"), mock.Anything).Return()

	reqStr := "blah123"
	requests := parseProfilingRequests(reqStr, logr.New(logSink))

	require.Len(t, requests, 0)
	logSink.AssertNumberOfCalls(t, "Error", 1)
}

func TestPerfTraceRequestEmpty(t *testing.T) {
	logSink := &testutil.MockLoggerSink{}
	logSink.On("Init", mock.AnythingOfType("logr.RuntimeInfo")).Return()
	// Not setting up any other calls, Error in particular, since we do not expect any errors.
	log := logr.New(logSink)

	reqStr := ""
	requests := parseProfilingRequests(reqStr, log)
	require.Len(t, requests, 0)

	reqStr = "   "
	requests = parseProfilingRequests(reqStr, log)
	require.Len(t, requests, 0)
}
