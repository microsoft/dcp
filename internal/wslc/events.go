/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/pubsub"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

const (
	maxEventSize           = 2 * 1024 * 1024
	maxEventDiagnosticSize = 4096
)

var (
	wslcEventPattern  = regexp.MustCompile(`^(\S+) (container|network) ([a-z_]+(?:: [^\r\n]+?)?) ([0-9a-fA-F]{64})(?: \(([^\r\n]*)\))?$`)
	errWslcEventPanic = errors.New("wslc event parser panicked")
)

func (wco *WslcCliOrchestrator) WatchContainers(
	sink chan<- containers.EventMessage,
) (*pubsub.Subscription[containers.EventMessage], error) {
	if sink == nil {
		return nil, fmt.Errorf("container event sink cannot be nil")
	}
	return wco.containerEvtWatcher.Subscribe(sink), nil
}

func (wco *WslcCliOrchestrator) WatchNetworks(
	sink chan<- containers.EventMessage,
) (*pubsub.Subscription[containers.EventMessage], error) {
	if sink == nil {
		return nil, fmt.Errorf("network event sink cannot be nil")
	}
	return wco.networkEvtWatcher.Subscribe(sink), nil
}

func (wco *WslcCliOrchestrator) doWatchContainers(ctx context.Context, subscriptions *pubsub.SubscriptionSet[containers.EventMessage]) {
	wco.watchEvents(ctx, subscriptions, containers.EventSourceContainer)
}

func (wco *WslcCliOrchestrator) doWatchNetworks(ctx context.Context, subscriptions *pubsub.SubscriptionSet[containers.EventMessage]) {
	wco.watchEvents(ctx, subscriptions, containers.EventSourceNetwork)
}

func (wco *WslcCliOrchestrator) watchEvents(
	watcherCtx context.Context,
	subscriptions *pubsub.SubscriptionSet[containers.EventMessage],
	source containers.EventSource,
) {
	streamCtx, streamCancel := context.WithCancel(watcherCtx)
	defer streamCancel()

	reader, writer := usvc_io.NewBufferedPipeWithMaxSize(maxEventSize)
	defer reader.Close()
	defer writer.Close()

	// Replay the startup boundary because WSLC timestamps have second-level precision.
	since := time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	command := makeWslcCommand("events", "--filter", "type="+string(source), "--since", since)
	diagnostics := &eventDiagnostics{}
	command.Stdout = usvc_io.NopWriteCloser(writer)
	command.Stderr = diagnostics
	exitHandler := process.NewConcurrentProcessExitHandler()
	wco.log.V(1).Info("Running WSLC command", "Command", command.String())
	_, startWaitForExit, startErr := wco.executor.StartProcess(
		streamCtx,
		command,
		exitHandler,
		process.CreationFlagEnsureKillOnDispose,
		nil,
	)
	if startErr != nil {
		wco.log.Error(startErr, "Could not start WSLC event stream", "Source", source)
		return
	}
	startWaitForExit()

	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		select {
		case <-exitHandler.Exited():
		case <-streamCtx.Done():
		}
		_ = writer.Close()
	}()
	defer func() {
		streamCancel()
		_ = reader.Close()
		<-exitHandler.Exited()
		<-streamDone
	}()

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxEventSize)
	parserLog := wco.log.WithValues("Source", source)
	for scanner.Scan() {
		if streamCtx.Err() != nil {
			return
		}
		event, parseErr := parseWslcEvent(scanner.Text(), parserLog)
		if parseErr != nil {
			if !errors.Is(parseErr, errWslcEventPanic) {
				parserLog.Error(parseErr, "Could not parse WSLC event")
			}
			continue
		}
		if event.Source == source {
			subscriptions.Notify(event)
		}
	}

	if watcherCtx.Err() != nil {
		return
	}
	if scanErr := scanner.Err(); scanErr != nil {
		wco.log.Error(scanErr, "Could not read WSLC event stream", "Source", source)
		return
	}

	<-exitHandler.Exited()
	exitInfo := exitHandler.ExitInfo()
	streamErr := errors.Join(exitInfo.Err, fmt.Errorf("wslc event stream ended with exit code %d", exitInfo.ExitCode))
	wco.log.Error(streamErr, "WSLC event stream stopped unexpectedly", "Source", source, "Stderr", diagnostics.String())
}

func parseWslcEvent(line string, log logr.Logger) (event containers.EventMessage, parseErr error) {
	defer func() {
		panicErr := resiliency.MakePanicError(recover(), log)
		if panicErr != nil {
			event = containers.EventMessage{}
			parseErr = errors.Join(errWslcEventPanic, panicErr)
		}
	}()

	if len(line) > maxEventSize {
		return containers.EventMessage{}, fmt.Errorf("wslc event exceeds %d bytes", maxEventSize)
	}
	match := wslcEventPattern.FindStringSubmatch(strings.TrimSpace(line))
	if len(match) != 6 {
		return containers.EventMessage{}, fmt.Errorf("invalid WSLC event format or incomplete object ID")
	}
	if _, timestampErr := time.Parse(time.RFC3339Nano, match[1]); timestampErr != nil {
		return containers.EventMessage{}, fmt.Errorf("parsing WSLC event timestamp: %w", timestampErr)
	}

	action, _, _ := strings.Cut(match[3], ":")
	event = containers.EventMessage{
		Source: containers.EventSource(match[2]),
		Action: containers.EventAction(action),
		Actor:  containers.EventActor{ID: match[4]},
	}
	if event.Source == containers.EventSourceNetwork &&
		(event.Action == containers.EventActionConnect || event.Action == containers.EventActionDisconnect) {
		// Endpoint identity is the first native attribute; unescaped names must not supply it.
		firstAttribute, _, _ := strings.Cut(match[5], ", ")
		containerID, hasContainer := strings.CutPrefix(firstAttribute, "container=")
		if !hasContainer || len(containerID) != 64 {
			return containers.EventMessage{}, fmt.Errorf("WSLC network event is missing a complete container ID")
		}
		if _, decodeErr := hex.DecodeString(containerID); decodeErr != nil {
			return containers.EventMessage{}, fmt.Errorf("decoding WSLC network event container ID: %w", decodeErr)
		}
		event.Attributes = map[string]string{"container": containerID}
	}
	return event, nil
}

type eventDiagnostics struct {
	data      []byte
	truncated bool
}

func (diagnostics *eventDiagnostics) Write(data []byte) (int, error) {
	retained := min(len(data), maxEventDiagnosticSize-len(diagnostics.data))
	diagnostics.data = append(diagnostics.data, data[:retained]...)
	diagnostics.truncated = diagnostics.truncated || retained < len(data)
	return len(data), nil
}

func (diagnostics *eventDiagnostics) String() string {
	message := strings.TrimSpace(string(diagnostics.data))
	if diagnostics.truncated {
		message += " [truncated]"
	}
	return message
}
