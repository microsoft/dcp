/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
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
