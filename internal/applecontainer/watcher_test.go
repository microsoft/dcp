/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package applecontainer

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"

	ct "github.com/microsoft/dcp/internal/containers"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

func listedContainerJson(id string, state string) string {
	startedDate := ""
	if state != "created" {
		startedDate = `"startedDate":"2026-06-12T15:00:00Z",`
	}
	return fmt.Sprintf(
		`{"configuration":{"id":"%[1]s","image":{"reference":"docker.io/library/alpine:latest"},"labels":{},"networks":[{"network":"default","options":{"hostname":"%[1]s"}}]},"id":"%[1]s","status":{"networks":[],%[2]s"state":"%[3]s"}}`,
		id, startedDate, state)
}

// installContainerListAutoExecution makes every `container ls --all --format json` invocation
// respond with the current value of the provided snapshot function.
func installContainerListAutoExecution(pe *internal_testutil.TestProcessExecutor, snapshot func() string) {
	pe.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{"container", "ls", "--all"},
		},
		RunCommand: func(p *internal_testutil.ProcessExecution) int32 {
			_, err := p.Cmd.Stdout.Write([]byte(snapshot()))
			if err != nil {
				return 1
			}
			return 0
		},
	})
}

func waitForEvent(ctx context.Context, evtC <-chan ct.EventMessage) (ct.EventMessage, error) {
	select {
	case <-ctx.Done():
		return ct.EventMessage{}, fmt.Errorf("timed out waiting for an event")
	case evt, ok := <-evtC:
		if !ok {
			return ct.EventMessage{}, fmt.Errorf("the event channel was closed")
		}
		return evt, nil
	}
}

func TestSynthesizesContainerEventsFromPolling(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 60*time.Second)
	defer cancel()
	pe := internal_testutil.NewTestProcessExecutor(ctx)
	defer func() {
		require.NoError(t, pe.Close())
	}()

	var lock sync.Mutex
	snapshot := "[]"
	firstPollDone := false
	setSnapshot := func(s string) {
		lock.Lock()
		defer lock.Unlock()
		snapshot = s
	}
	getSnapshot := func() string {
		lock.Lock()
		defer lock.Unlock()
		if !firstPollDone {
			// The watcher primes its state from the first poll without emitting events,
			// so the first poll must observe an empty container list regardless of when
			// the test changes the snapshot.
			firstPollDone = true
			return "[]"
		}
		return snapshot
	}

	installContainerListAutoExecution(pe, getSnapshot)

	aco := NewAppleContainerCliOrchestrator(testr.New(t), pe)

	evtC := make(chan ct.EventMessage, 10)
	sub, err := aco.WatchContainers(evtC)
	require.NoError(t, err)
	defer sub.Cancel()

	// A new running container appears: expect "create" followed by "start"
	setSnapshot("[" + listedContainerJson("ctr-1", "running") + "]")

	evt, err := waitForEvent(ctx, evtC)
	require.NoError(t, err)
	require.Equal(t, ct.EventSourceContainer, evt.Source)
	require.Equal(t, ct.EventActionCreate, evt.Action)
	require.Equal(t, "ctr-1", evt.Actor.ID)

	evt, err = waitForEvent(ctx, evtC)
	require.NoError(t, err)
	require.Equal(t, ct.EventActionStart, evt.Action)
	require.Equal(t, "ctr-1", evt.Actor.ID)

	// The container stops: expect "die"
	setSnapshot("[" + listedContainerJson("ctr-1", "stopped") + "]")

	evt, err = waitForEvent(ctx, evtC)
	require.NoError(t, err)
	require.Equal(t, ct.EventActionDie, evt.Action)
	require.Equal(t, "ctr-1", evt.Actor.ID)

	// The container is removed: expect "destroy"
	setSnapshot("[]")

	evt, err = waitForEvent(ctx, evtC)
	require.NoError(t, err)
	require.Equal(t, ct.EventActionDestroy, evt.Action)
	require.Equal(t, "ctr-1", evt.Actor.ID)
}
