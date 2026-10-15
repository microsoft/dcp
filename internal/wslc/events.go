/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcpproc"
	"github.com/microsoft/dcp/internal/pubsub"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/process"
)

const (
	maxEventSize           = 2 * 1024 * 1024
	maxEventDiagnosticSize = 4096
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

func (wco *WslcCliOrchestrator) watchEvents(
	watcherCtx context.Context,
	subscriptions *pubsub.SubscriptionSet[containers.EventMessage],
	source containers.EventSource,
) {
	reader, writer := usvc_io.NewBufferedPipe()
	defer writer.Close()

	// Replay the startup boundary so events emitted while the stream starts are not missed.
	since := time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	command := makeWslcCommand(
		"events",
		"--filter", "type="+string(source),
		"--since", since,
		"--format", "json",
	)
	diagnostics := &eventDiagnostics{}
	command.Stdout = writer
	command.Stderr = diagnostics

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, maxEventDiagnosticSize), maxEventSize)
	eventLog := wco.log.WithValues("Source", source)
	go func() {
		for scanner.Scan() {
			if watcherCtx.Err() != nil {
				return
			}
			event, unmarshalErr := unmarshalWslcEvent(scanner.Bytes())
			if unmarshalErr != nil {
				eventLog.Error(unmarshalErr, "Could not parse WSLC event", "EventData", scanner.Text())
				continue
			}
			if event.Source == source {
				subscriptions.Notify(event)
			}
		}
		if scanErr := scanner.Err(); scanErr != nil && watcherCtx.Err() == nil {
			eventLog.Error(scanErr, "Could not read WSLC event stream")
		}
	}()

	exitCh := make(chan process.ProcessExitInfo, 1)
	exitHandler := process.NewChannelProcessExitHandler(exitCh)
	wco.log.V(1).Info("Running WSLC command", "Command", command.String())
	handle, startWaitForExit, startErr := wco.executor.StartProcess(
		watcherCtx,
		command,
		exitHandler,
		process.CreationFlagsNone,
		nil,
	)
	if startErr != nil {
		wco.log.Error(startErr, "Could not start WSLC event stream", "Source", source)
		return
	}

	dcpproc.RunProcessWatcher(wco.executor, handle, wco.log)
	startWaitForExit()

	select {
	case exitInfo, open := <-exitCh:
		if !open {
			eventLog.Error(fmt.Errorf("process exit notification channel closed without a result"), "WSLC event stream stopped unexpectedly")
			return
		}
		if watcherCtx.Err() != nil {
			return
		}
		streamErr := errors.Join(exitInfo.Err, fmt.Errorf("wslc event stream ended with exit code %d", exitInfo.ExitCode))
		eventLog.Error(streamErr, "WSLC event stream stopped unexpectedly", "Stderr", diagnostics.String())
	case <-watcherCtx.Done():
		eventLog.V(1).Info("Stopping 'wslc events' command", "PID", handle.Pid)
	}
}

type wslcEventActor struct {
	ID         string            `json:"ID,omitempty"`
	Attributes map[string]string `json:"Attributes,omitempty"`
}

type wslcEventMessage struct {
	Source containers.EventSource `json:"Type"`
	Action containers.EventAction `json:"Action"`
	Actor  wslcEventActor         `json:"Actor,omitempty"`
}

func unmarshalWslcEvent(data []byte) (containers.EventMessage, error) {
	var wslcEvent wslcEventMessage
	if unmarshalErr := json.Unmarshal(data, &wslcEvent); unmarshalErr != nil {
		return containers.EventMessage{}, unmarshalErr
	}
	if strings.HasPrefix(string(wslcEvent.Action), string(containers.EventActionHealthStatus)+":") {
		wslcEvent.Action = containers.EventActionHealthStatus
	}

	return containers.EventMessage{
		Source:     wslcEvent.Source,
		Action:     wslcEvent.Action,
		Actor:      containers.EventActor{ID: wslcEvent.Actor.ID},
		Attributes: wslcEvent.Actor.Attributes,
	}, nil
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
