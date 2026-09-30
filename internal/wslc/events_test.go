/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
)

const eventTestTimestamp = "2026-09-29T14:28:17.000000000-07:00"

// Verifies that text events preserve lifecycle actions and full object IDs, including network endpoint identity and action suffix normalization.
// Ambiguous labels are not interpreted as authoritative event attributes.
func TestParseWslcEvent(t *testing.T) {
	t.Parallel()

	containerID := strings.Repeat("a", 64)
	networkID := strings.Repeat("b", 64)
	for _, testCase := range []struct {
		name       string
		line       string
		source     containers.EventSource
		action     containers.EventAction
		actor      string
		attributes map[string]string
	}{
		{"create with ambiguous labels", fmt.Sprintf("container create %s (label=one, two=three (four), name=container)", containerID),
			containers.EventSourceContainer, containers.EventActionCreate, containerID, nil},
		{"spontaneous exit", fmt.Sprintf("container stop %s (exitCode=0, name=container)", containerID),
			containers.EventSourceContainer, containers.EventActionStop, containerID, nil},
		{"destroy without attributes", "container destroy " + containerID,
			containers.EventSourceContainer, containers.EventActionDestroy, containerID, nil},
		{"health action", "container health_status: healthy " + containerID + " (name=container)",
			containers.EventSourceContainer, containers.EventActionHealthStatus, containerID, nil},
		{"exec action", "container exec_create: sh -c exit " + containerID + " (name=container)",
			containers.EventSourceContainer, containers.EventActionExecCreate, containerID, nil},
		{"network create", "network create " + networkID + " (name=network, type=bridge)",
			containers.EventSourceNetwork, containers.EventActionCreate, networkID, nil},
		{"network destroy", "network destroy " + networkID + " (name=network, type=bridge)",
			containers.EventSourceNetwork, containers.EventActionDestroy, networkID, nil},
		{"network connect", fmt.Sprintf("network connect %s (container=%s, name=network, type=bridge)", networkID, containerID),
			containers.EventSourceNetwork, containers.EventActionConnect, networkID, map[string]string{"container": containerID}},
		{"network disconnect", fmt.Sprintf("network disconnect %s (container=%s, name=ambiguous, container=not-an-id)", networkID, containerID),
			containers.EventSourceNetwork, containers.EventActionDisconnect, networkID, map[string]string{"container": containerID}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			event, parseErr := parseWslcEvent(eventTestTimestamp+" "+testCase.line+"\r\n", logr.Discard())
			require.NoError(t, parseErr)
			require.Equal(t, testCase.source, event.Source)
			require.Equal(t, testCase.action, event.Action)
			require.Equal(t, testCase.actor, event.Actor.ID)
			require.Equal(t, testCase.attributes, event.Attributes)
		})
	}
}

// Verifies rejection of malformed framing, timestamps, sources, IDs, and missing or misleading network endpoint attributes.
func TestParseWslcEventRejectsInvalidIdentityAndFraming(t *testing.T) {
	t.Parallel()

	fullID := strings.Repeat("a", 64)
	for _, line := range []string{
		"",
		"not-an-event",
		"invalid-time container start " + fullID,
		eventTestTimestamp + " container start short-id",
		eventTestTimestamp + " container start " + strings.Repeat("g", 64),
		eventTestTimestamp + " image create " + fullID,
		eventTestTimestamp + " network connect " + fullID,
		eventTestTimestamp + " network disconnect " + fullID + " (container=short-id)",
		eventTestTimestamp + " network connect " + fullID + " (container=" + strings.Repeat("g", 64) + ")",
		eventTestTimestamp + " network connect " + fullID + " (name=container=" + fullID + ")",
		eventTestTimestamp + " network connect " + fullID + " (name=network, container=" + fullID + ")",
		eventTestTimestamp + " container create " + fullID + " (label=first\nsecond)",
		eventTestTimestamp + " container create " + fullID + " (unterminated",
	} {
		event, parseErr := parseWslcEvent(line, logr.Discard())
		require.Error(t, parseErr, line)
		require.Empty(t, event.Actor.ID, line)
	}
}

// Verifies that a forced parser panic is recovered through the shared utility, logged once with a stack, and returned without a partial event.
// Parsing a subsequent valid event must still succeed after the injected fault is removed.
func TestParseWslcEventRecoversAndLogsPanics(t *testing.T) {
	originalPattern := wslcEventPattern
	t.Cleanup(func() { wslcEventPattern = originalPattern })
	wslcEventPattern = nil

	var messages []string
	log := funcr.New(func(_, message string) { messages = append(messages, message) }, funcr.Options{})
	line := eventTestTimestamp + " container create " + strings.Repeat("a", 64)
	var event containers.EventMessage
	var parseErr error
	require.NotPanics(t, func() {
		event, parseErr = parseWslcEvent(line, log)
	})
	require.ErrorIs(t, parseErr, errWslcEventPanic)
	require.Equal(t, containers.EventMessage{}, event)
	require.Len(t, messages, 1)
	require.Contains(t, messages[0], "A goroutine ended prematurely due to panic")
	require.Contains(t, messages[0], "stack")
	require.Contains(t, messages[0], "parseWslcEvent")

	wslcEventPattern = originalPattern
	recoveredEvent, recoveredErr := parseWslcEvent(line, log)
	require.NoError(t, recoveredErr)
	require.Equal(t, strings.Repeat("a", 64), recoveredEvent.Actor.ID)
	require.Len(t, messages, 1)
}

// Verifies that an unexpected regular-expression capture layout returns a normal parse error instead of requiring panic recovery.
func TestParseWslcEventValidatesCaptureLayout(t *testing.T) {
	originalPattern := wslcEventPattern
	t.Cleanup(func() { wslcEventPattern = originalPattern })
	wslcEventPattern = regexp.MustCompile(`.+`)

	event, parseErr := parseWslcEvent("unexpected capture layout", logr.Discard())
	require.Error(t, parseErr)
	require.NotErrorIs(t, parseErr, errWslcEventPanic)
	require.Equal(t, containers.EventMessage{}, event)
}

// Verifies that oversized event input is rejected before parsing and yields no partial event or recovered panic.
func TestParseWslcEventRejectsOversizedInput(t *testing.T) {
	t.Parallel()

	event, parseErr := parseWslcEvent(strings.Repeat("x", maxEventSize+1), logr.Discard())
	require.ErrorContains(t, parseErr, "exceeds")
	require.NotErrorIs(t, parseErr, errWslcEventPanic)
	require.Equal(t, containers.EventMessage{}, event)
}

// Verifies that arbitrary event input never requires panic recovery, errors return empty events, and accepted events have valid identities.
func FuzzParseWslcEvent(f *testing.F) {
	fullID := strings.Repeat("a", 64)
	for _, seed := range []string{
		"",
		"\x00\xff\xfe\r\n",
		"not an event",
		eventTestTimestamp + " container create " + fullID,
		eventTestTimestamp + " container stop " + fullID + " (exitCode=0, label=one, two=three (four))",
		eventTestTimestamp + " container health_status: healthy " + fullID,
		eventTestTimestamp + " container create " + fullID + " (label=first\nsecond)",
		eventTestTimestamp + " network connect " + fullID + " (container=" + fullID + ", name=test)",
		eventTestTimestamp + " network disconnect " + fullID + " (name=test, container=" + fullID + ")",
		strings.Repeat("x", maxEventSize+1),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, line string) {
		event, parseErr := parseWslcEvent(line, logr.Discard())
		require.NotErrorIs(t, parseErr, errWslcEventPanic, "ordinary input must not require panic recovery")
		if parseErr != nil {
			require.Equal(t, containers.EventMessage{}, event)
			return
		}
		require.Contains(t, []containers.EventSource{containers.EventSourceContainer, containers.EventSourceNetwork}, event.Source)
		require.NotEmpty(t, event.Action)
		require.Len(t, event.Actor.ID, 64)
		_, actorDecodeErr := hex.DecodeString(event.Actor.ID)
		require.NoError(t, actorDecodeErr)
		if event.Source == containers.EventSourceNetwork &&
			(event.Action == containers.EventActionConnect || event.Action == containers.EventActionDisconnect) {
			require.Len(t, event.Attributes["container"], 64)
			_, containerDecodeErr := hex.DecodeString(event.Attributes["container"])
			require.NoError(t, containerDecodeErr)
		}
	})
}

type eventContextExecutor struct {
	process.Executor
	contexts chan context.Context
}

func (executor *eventContextExecutor) StartProcess(
	ctx context.Context,
	command *exec.Cmd,
	handler process.ProcessExitHandler,
	flags process.ProcessCreationFlag,
	create process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	executor.contexts <- ctx
	return executor.Executor.StartProcess(ctx, command, handler, flags, create)
}

type testEventCommand struct {
	ctx       context.Context
	execution *internal_testutil.ProcessExecution
	lines     chan string
}

func receiveTestValue[T any](t *testing.T, ctx context.Context, values <-chan T) T {
	t.Helper()
	select {
	case value, open := <-values:
		require.True(t, open, "channel closed before the expected value")
		return value
	case <-ctx.Done():
		t.Fatalf("waiting for event test value: %v", ctx.Err())
		var empty T
		return empty
	}
}

// Verifies that subscribers share one native stream, canceling one leaves others active, and canceling the last stops the command.
// A later subscription must launch a fresh stream and receive events.
func TestWatchEventsSharesSourceAndCancelsLastSubscriber(t *testing.T) {
	t.Parallel()

	for _, source := range []containers.EventSource{containers.EventSourceContainer, containers.EventSourceNetwork} {
		t.Run(string(source), func(t *testing.T) {
			t.Parallel()

			ctx, orchestrator, executor := newTestOrchestrator(t)
			observedExecutor := &eventContextExecutor{Executor: executor, contexts: make(chan context.Context, 4)}
			orchestrator.executor = observedExecutor
			commands := make(chan testEventCommand, 4)
			commandPrefix := []string{"wslc", "events", "--filter", "type=" + string(source)}
			executor.InstallAutoExecution(internal_testutil.AutoExecution{
				Condition: internal_testutil.ProcessSearchCriteria{Command: commandPrefix},
				RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
					commandCtx := receiveTestValue(t, ctx, observedExecutor.contexts)
					command := testEventCommand{ctx: commandCtx, execution: execution, lines: make(chan string, 4)}
					commands <- command
					for {
						select {
						case <-commandCtx.Done():
							return 0
						case line, open := <-command.lines:
							if !open {
								return 0
							}
							if _, writeErr := io.WriteString(execution.Cmd.Stdout, line); writeErr != nil {
								require.ErrorIs(t, writeErr, io.ErrClosedPipe)
								return 0
							}
						}
					}
				},
			})

			watch := orchestrator.WatchContainers
			if source == containers.EventSourceNetwork {
				watch = orchestrator.WatchNetworks
			}
			firstSink := make(chan containers.EventMessage, 4)
			secondSink := make(chan containers.EventMessage, 4)
			firstSubscription, firstErr := watch(firstSink)
			require.NoError(t, firstErr)
			t.Cleanup(firstSubscription.Cancel)
			secondSubscription, secondErr := watch(secondSink)
			require.NoError(t, secondErr)
			t.Cleanup(secondSubscription.Cancel)
			command := receiveTestValue(t, ctx, commands)
			require.Contains(t, command.execution.Cmd.Args, "--since")
			require.NotContains(t, command.execution.Cmd.Args, "--format")
			require.Len(t, executor.FindAll(commandPrefix, "", nil), 1)

			eventID := strings.Repeat("a", 64)
			line := fmt.Sprintf("%s %s create %s (name=test)\n", eventTestTimestamp, source, eventID)
			command.lines <- line
			require.Equal(t, eventID, receiveTestValue(t, ctx, firstSink).Actor.ID)
			require.Equal(t, eventID, receiveTestValue(t, ctx, secondSink).Actor.ID)

			firstSubscription.Cancel()
			_, firstOpen := <-firstSink
			require.False(t, firstOpen)
			require.NoError(t, command.ctx.Err())
			command.lines <- line
			require.Equal(t, eventID, receiveTestValue(t, ctx, secondSink).Actor.ID)

			secondSubscription.Cancel()
			select {
			case <-command.ctx.Done():
			case <-ctx.Done():
				t.Fatal("last subscription did not cancel the native event command")
			}
			handler, isConcurrentHandler := command.execution.ExitHandler.(*process.ConcurrentProcessExitHandler)
			require.True(t, isConcurrentHandler)
			select {
			case <-handler.Exited():
			case <-ctx.Done():
				t.Fatal("canceled event command did not finish")
			}

			thirdSink := make(chan containers.EventMessage, 4)
			thirdSubscription, thirdErr := watch(thirdSink)
			require.NoError(t, thirdErr)
			t.Cleanup(thirdSubscription.Cancel)
			restarted := receiveTestValue(t, ctx, commands)
			require.Len(t, executor.FindAll(commandPrefix, "", nil), 2)
			restarted.lines <- line
			require.Equal(t, eventID, receiveTestValue(t, ctx, thirdSink).Actor.ID)
			thirdSubscription.Cancel()
			restartedHandler, isRestartedHandler := restarted.execution.ExitHandler.(*process.ConcurrentProcessExitHandler)
			require.True(t, isRestartedHandler)
			select {
			case <-restartedHandler.Exited():
			case <-ctx.Done():
				t.Fatal("restarted event command did not finish")
			}
		})
	}
}

// Verifies that startup errors, nonzero exits, unexpected EOF, and oversized events are logged and release the stream context.
func TestWatchEventsReportsFailuresAndReleasesSource(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name       string
		stdout     string
		stderr     string
		exitCode   int32
		startError error
		wantLog    string
	}{
		{name: "startup", startError: errors.New("start failed"), wantLog: "Could not start WSLC event stream"},
		{name: "nonzero exit", stderr: "event source unavailable", exitCode: 23, wantLog: "event source unavailable"},
		{name: "unexpected EOF", wantLog: "WSLC event stream stopped unexpectedly"},
		{name: "oversized event", stdout: strings.Repeat("x", maxEventSize+1), wantLog: "Could not read WSLC event stream"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx, orchestrator, executor := newTestOrchestrator(t)
			observedExecutor := &eventContextExecutor{Executor: executor, contexts: make(chan context.Context, 1)}
			orchestrator.executor = observedExecutor
			messages := make(chan string, 8)
			orchestrator.log = funcr.New(func(_, message string) { messages <- message }, funcr.Options{})
			autoExecution := internal_testutil.AutoExecution{
				Condition: internal_testutil.ProcessSearchCriteria{Command: []string{"wslc", "events"}},
				RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
					_, _ = io.WriteString(execution.Cmd.Stdout, testCase.stdout)
					_, _ = io.WriteString(execution.Cmd.Stderr, testCase.stderr)
					return testCase.exitCode
				},
			}
			if testCase.startError != nil {
				autoExecution.StartupError = func(*internal_testutil.ProcessExecution) error { return testCase.startError }
			}
			executor.InstallAutoExecution(autoExecution)

			sink := make(chan containers.EventMessage, 1)
			subscription, subscribeErr := orchestrator.WatchContainers(sink)
			require.NoError(t, subscribeErr)
			t.Cleanup(subscription.Cancel)
			commandCtx := receiveTestValue(t, ctx, observedExecutor.contexts)
			require.Contains(t, receiveTestValue(t, ctx, messages), testCase.wantLog)
			select {
			case <-commandCtx.Done():
			case <-ctx.Done():
				t.Fatal("failed event source did not release its context")
			}
			if testCase.startError == nil {
				executions := executor.FindAll([]string{"wslc", "events"}, "", nil)
				require.Len(t, executions, 1)
				handler, isConcurrentHandler := executions[0].ExitHandler.(*process.ConcurrentProcessExitHandler)
				require.True(t, isConcurrentHandler)
				select {
				case <-handler.Exited():
				case <-ctx.Done():
					t.Fatal("failed event command did not finish")
				}
			}
		})
	}
}

// Verifies that a network watch ignores malformed and container events while delivering a subsequent valid network event.
func TestWatchEventsSkipsMalformedAndUnrelatedEvents(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	fullID := strings.Repeat("b", 64)
	output := "malformed\n" +
		fmt.Sprintf("%s container create %s\n", eventTestTimestamp, fullID) +
		fmt.Sprintf("%s network destroy %s (name=deleted)\n", eventTestTimestamp, fullID)
	installAutoCommand(t, executor, []string{"wslc", "events", "--filter", "type=network"}, output, "", 0)
	sink := make(chan containers.EventMessage, 4)
	subscription, subscribeErr := orchestrator.WatchNetworks(sink)
	require.NoError(t, subscribeErr)
	t.Cleanup(subscription.Cancel)

	event := receiveTestValue(t, ctx, sink)
	require.Equal(t, containers.EventSourceNetwork, event.Source)
	require.Equal(t, containers.EventActionDestroy, event.Action)
	require.Equal(t, fullID, event.Actor.ID)
	require.Empty(t, sink)
}

// Verifies that nil container or network event sinks are rejected without starting a CLI process.
func TestWatchEventsRejectsNilSinks(t *testing.T) {
	t.Parallel()

	_, orchestrator, executor := newTestOrchestrator(t)
	containerSubscription, containerErr := orchestrator.WatchContainers(nil)
	networkSubscription, networkErr := orchestrator.WatchNetworks(nil)
	require.Nil(t, containerSubscription)
	require.Error(t, containerErr)
	require.Nil(t, networkSubscription)
	require.Error(t, networkErr)
	require.Empty(t, executor.Executions)
}

// Verifies that event diagnostics retain only the configured byte limit, acknowledge all input, and mark truncation explicitly.
func TestEventDiagnosticsAreBounded(t *testing.T) {
	t.Parallel()

	diagnostics := &eventDiagnostics{}
	written, writeErr := diagnostics.Write([]byte(strings.Repeat("x", maxEventDiagnosticSize+100)))
	require.NoError(t, writeErr)
	require.Equal(t, maxEventDiagnosticSize+100, written)
	require.Len(t, diagnostics.data, maxEventDiagnosticSize)
	require.True(t, strings.HasSuffix(diagnostics.String(), "[truncated]"))
}
