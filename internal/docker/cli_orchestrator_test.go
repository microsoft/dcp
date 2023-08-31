package docker

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/smallnest/chanx"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"

	ct "github.com/microsoft/usvc-apiserver/internal/containers"
	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

const (
	actionTimeout   = 5 * time.Second
	actionPoll      = 200 * time.Millisecond
	pollImmediately = true // Dont't wait before polling for the first time
)

func TestExpectStringsSingleLine(t *testing.T) {
	b := bytes.Buffer{}

	// Empty output should produce error
	require.Error(t, expectStrings(&b, []string{"foo"}))

	b.WriteString("something\n else")
	// Must expect something
	require.Error(t, expectStrings(&b, []string{}))
	// Cannot expect empty string
	require.Error(t, expectStrings(&b, []string{"something", ""}))
	b.Reset()

	// Just the expected string
	b.WriteString("foo")
	require.NoError(t, expectStrings(&b, []string{"foo"}))
	b.Reset()

	// Expected string with some whitespace around it
	b.WriteString(" foo   ")
	require.NoError(t, expectStrings(&b, []string{"foo"}))
	b.Reset()

	// Expected string ending with line feed
	b.WriteString(" foo\n\n")
	require.NoError(t, expectStrings(&b, []string{"foo"}))
	b.Reset()

	// Expected string on the second line of text
	b.WriteString("  \nfoo\n")
	require.NoError(t, expectStrings(&b, []string{"foo"}))
	b.Reset()

	// Expected string does not match actual data (should fail)
	b.WriteString("bar")
	require.Error(t, expectStrings(&b, []string{"foo"}))
	b.Reset()
}

func TestExpectStringsMultiline(t *testing.T) {
	b := bytes.Buffer{}

	// Expected strings, exactly
	b.WriteString("foo\nbar \n  baz")
	require.NoError(t, expectStrings(&b, []string{"foo", "bar", "baz"}))
	b.Reset()

	// Some, but not all strings match
	// .. mismatch at the beginning
	b.WriteString("foo\nbar \n  baz")
	require.Error(t, expectStrings(&b, []string{"notFoo", "bar", "baz"}))
	b.Reset()
	// .. mismatch in the middle
	b.WriteString("foo\nbar \n  baz")
	require.Error(t, expectStrings(&b, []string{"foo", "notBar", "baz"}))
	b.Reset()
	// .. mismatch at the end
	b.WriteString("foo\nbar \n  baz")
	require.Error(t, expectStrings(&b, []string{"foo", "bar", "notBaz"}))
	b.Reset()

	// Less data than expected
	b.WriteString("foo\nbar")
	require.Error(t, expectStrings(&b, []string{"foo", "bar", "baz"}))
	b.Reset()

	// More data than expected is OK
	b.WriteString("foo\nbar \n  baz")
	require.NoError(t, expectStrings(&b, []string{"foo", "bar"}))
	b.Reset()
}

func TestInspectedContainerDeserialization(t *testing.T) {
	b := bytes.Buffer{}

	// Empty output should produce empty slice
	vols, err := asObjects(&b, unmarshalVolume)
	require.NoError(t, err)
	require.Empty(t, vols)

	// One record
	b.WriteString(`{"CreatedAt":"2023-01-06T23:29:43Z","Driver":"local","Labels":{},"Mountpoint":"/var/lib/docker/volumes/foo/_data","Name":"foo","Options":{},"Scope":"local"}`)
	vols, err = asObjects(&b, unmarshalVolume)
	require.NoError(t, err)
	require.Len(t, vols, 1)
	creationTime, _ := time.Parse(time.RFC3339, "2023-01-06T23:29:43Z")
	expected := ct.InspectedVolume{
		Name:       "foo",
		Driver:     "local",
		Scope:      "local",
		MountPoint: "/var/lib/docker/volumes/foo/_data",
		CreatedAt:  creationTime,
		Labels:     make(map[string]string),
	}
	require.EqualValues(t, expected, vols[0])
	b.Reset()

	// One record with whitespace around
	b.WriteString(` 
	{"CreatedAt":"2023-01-06T23:29:43Z","Driver":"local","Labels":{},"Mountpoint":"/var/lib/docker/volumes/foo/_data","Name":"foo","Options":{},"Scope":"local"}  
	
	`)
	vols, err = asObjects(&b, unmarshalVolume)
	require.NoError(t, err)
	require.Len(t, vols, 1)
	b.Reset()

	// Two records, with some whitespace
	b.WriteString(` 
	{"CreatedAt":"2023-01-06T23:29:43Z","Driver":"local","Labels":{},"Mountpoint":"/var/lib/docker/volumes/foo/_data","Name":"foo","Options":{},"Scope":"local"}  
	{"CreatedAt":"2022-12-22T17:45:33Z","Driver":"local","Labels":{"com.docker.compose.project":"db","com.docker.compose.version":"2.13.0","com.docker.compose.volume":"sql-data"},"Mountpoint":"/var/lib/docker/volumes/db_sql-data/_data","Name":"db_sql-data","Options":null,"Scope":"local"}  

	`)
	vols, err = asObjects(&b, unmarshalVolume)
	require.NoError(t, err)
	require.Len(t, vols, 2)
	b.Reset()

	// Two records, no whitespace (but records separated by newline)
	b.WriteString(`{"CreatedAt":"2023-01-06T23:29:43Z", "Driver":"local", "Labels":{}, "Mountpoint":"/var/lib/docker/volumes/foo/_data", "Name":"foo", "Options":{}, "Scope":"local" }
	{"CreatedAt":"2022-12-22T17:45:33Z", "Driver":"local", "Labels":{"com.docker.compose.project":"db","com.docker.compose.version":"2.13.0","com.docker.compose.volume":"sql-data"}, "Mountpoint":"/var/lib/docker/volumes/db_sql-data/_data","Name":"db_sql-data","Options":null,"Scope":"local"}`)
	vols, err = asObjects(&b, unmarshalVolume)
	require.NoError(t, err)
	require.Len(t, vols, 2)
	// Make sure the labels un-marshalled properly
	require.EqualValues(t,
		map[string]string{
			"com.docker.compose.project": "db",
			"com.docker.compose.version": "2.13.0",
			"com.docker.compose.volume":  "sql-data",
		}, vols[1].Labels)
}

func TestReportsContainerEvents(t *testing.T) {
	t.Parallel()

	pe := ctrl_testutil.NewTestProcessExecutor()
	dco := NewDockerCliOrchestrator(testr.New(t), pe)
	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()

	sub, evtC := subscribe(t, ctx, dco)

	// The first subscription should trigger "docker events" execution
	dockerExec := waitForDockerEventsExecution(t, ctx, pe, nil)

	// Simulate container start event
	w := dockerExec.Cmd.Stdout
	evtText := []byte(`{"status":"create","id":"f97d15","from":"nginx","Type":"container","Action":"create","Actor":{"ID":"f97d15","Attributes":{"image":"nginx","maintainer":"NGINX Docker Maintainers <docker-maint@nginx.com>","name":"dreamy_lamport"}},"scope":"local","time":1674517581,"timeNano":1674517581499098260}` + "\n")
	written, err := w.Write(evtText)
	require.NoError(t, err)
	require.Equal(t, len(evtText), written)

	evtMsg, err := waitForEvent(ctx, evtC)
	require.NoError(t, err)
	require.Equal(t, ct.EventActionCreate, evtMsg.Action)
	require.Equal(t, ct.EventSourceContainer, evtMsg.Source)
	require.Equal(t, "f97d15", evtMsg.Actor.ID)

	// Simulate container destroy event
	evtText = []byte(`{"status":"destroy","id":"e14fec","from":"nginx","Type":"container","Action":"destroy","Actor":{"ID":"e14fec","Attributes":{"image":"nginx","maintainer":"NGINX Docker Maintainers <docker-maint@nginx.com>","name":"epic_jepsen"}},"scope":"local","time":1674517605,"timeNano":1674517605994948172}` + "\n")
	written, err = w.Write(evtText)
	require.NoError(t, err)
	require.Equal(t, len(evtText), written)

	evtMsg, err = waitForEvent(ctx, evtC)
	require.NoError(t, err)
	require.Equal(t, ct.EventActionDestroy, evtMsg.Action)
	require.Equal(t, ct.EventSourceContainer, evtMsg.Source)
	require.Equal(t, "e14fec", evtMsg.Actor.ID)

	err = sub.Cancel()
	require.NoError(t, err)
	requireChanClosed(t, evtC, "The events channel should be closed when subscription is cancelled")

	// This is the only subscription--the "docker events" command should be terminated.
	waitForDockerEventsExecution(t, ctx, pe, func(exec *ctrl_testutil.ProcessExecution) bool {
		return exec.Finished() && exec.ExitCode == ctrl_testutil.KilledProcessExitCode
	})
}

// Stops reporting events when subscription is cancelled (but other subscriptions continue)
func TestDoesNotReportEventsWhenSubscriptionCancelled(t *testing.T) {
	t.Parallel()

	pe := ctrl_testutil.NewTestProcessExecutor()
	dco := NewDockerCliOrchestrator(testr.New(t), pe)
	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()

	sub, evtC := subscribe(t, ctx, dco)
	sub2, evtC2 := subscribe(t, ctx, dco)

	dockerExec := waitForDockerEventsExecution(t, ctx, pe, nil)
	w := dockerExec.Cmd.Stdout

	// Write a container event
	evtText := []byte(`{"status":"create","id":"f97d15","from":"nginx","Type":"container","Action":"create","Actor":{"ID":"f97d15","Attributes":{"image":"nginx","maintainer":"NGINX Docker Maintainers <docker-maint@nginx.com>","name":"dreamy_lamport"}},"scope":"local","time":1674517581,"timeNano":1674517581499098260}` + "\n")
	written, err := w.Write(evtText)
	require.NoError(t, err)
	require.Equal(t, len(evtText), written)

	// That event should be reported to both subscriptions
	_, err = waitForEvent(ctx, evtC)
	require.NoError(t, err)
	_, err = waitForEvent(ctx, evtC2)
	require.NoError(t, err)

	// Cancel first subscription, but keep the second
	err = sub.Cancel()
	require.NoError(t, err)
	requireChanClosed(t, evtC, "The events channel should be closed when first subscription is cancelled")

	// Write another event
	evtText = []byte(`{"status":"destroy","id":"e14fec","from":"nginx","Type":"container","Action":"destroy","Actor":{"ID":"e14fec","Attributes":{"image":"nginx","maintainer":"NGINX Docker Maintainers <docker-maint@nginx.com>","name":"epic_jepsen"}},"scope":"local","time":1674517605,"timeNano":1674517605994948172}` + "\n")
	written, err = w.Write(evtText)
	require.NoError(t, err)
	require.Equal(t, len(evtText), written)

	// The event should be delivered to the second subscription
	_, err = waitForEvent(ctx, evtC2)
	require.NoError(t, err)

	err = sub2.Cancel()
	require.NoError(t, err)
	requireChanClosed(t, evtC2, "The events channel should be closed when the second subscription is cancelled")

	/// The "docker events" command should be terminated after the last subscription is cancelled
	waitForDockerEventsExecution(t, ctx, pe, func(exec *ctrl_testutil.ProcessExecution) bool {
		return exec.Finished() && exec.ExitCode == ctrl_testutil.KilledProcessExitCode
	})
}

// Starts the event watcher when the first subscription is created, and stops it when the last subscription is cancelled.
// This cycle can be repeated more than once.
func TestStartsAndStopsEventWatcher(t *testing.T) {
	t.Parallel()

	pe := ctrl_testutil.NewTestProcessExecutor()
	dco := NewDockerCliOrchestrator(testr.New(t), pe)
	ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
	defer cancel()

	sub, evtC := subscribe(t, ctx, dco)

	// A suubscription should trigger "docker events" execution
	waitForDockerEventsExecution(t, ctx, pe, nil)

	err := sub.Cancel()
	require.NoError(t, err)
	requireChanClosed(t, evtC, "The events channel should be closed when the subscription is cancelled")
	// The "docker events" command should be terminated.
	waitForDockerEventsExecution(t, ctx, pe, func(exec *ctrl_testutil.ProcessExecution) bool {
		return exec.Finished() && exec.ExitCode == ctrl_testutil.KilledProcessExitCode
	})

	pe.ClearHistory()

	sub, evtC = subscribe(t, ctx, dco)
	waitForDockerEventsExecution(t, ctx, pe, nil)
	sub2, evtC2 := subscribe(t, ctx, dco)

	err = sub.Cancel()
	require.NoError(t, err)
	requireChanClosed(t, evtC, "The events channel should be closed when the first subscription is cancelled")

	err = sub2.Cancel()
	require.NoError(t, err)
	requireChanClosed(t, evtC2, "The events channel should be closed when the second subscription is cancelled")

	// The "docker events" command should be terminated again.
	waitForDockerEventsExecution(t, ctx, pe, func(exec *ctrl_testutil.ProcessExecution) bool {
		return exec.Finished() && exec.ExitCode == ctrl_testutil.KilledProcessExitCode
	})
}

func waitForDockerEventsExecution(t *testing.T, ctx context.Context, executor *ctrl_testutil.TestProcessExecutor, cond func(exec *ctrl_testutil.ProcessExecution) bool) ctrl_testutil.ProcessExecution {
	pe, err := ctrl_testutil.WaitForCommand(executor, ctx, []string{"docker", "events"}, "", cond)
	require.NoError(t, err)
	return pe
}

func waitForEvent(ctx context.Context, c <-chan ct.EventMessage) (ct.EventMessage, error) {
	actionCtx, cancel := context.WithTimeout(ctx, actionTimeout)
	defer cancel()

	var retval ct.EventMessage
	receivedEvent := func(_ context.Context) (bool, error) {
		select {
		case retval = <-c:
			return true, nil
		default:
			return false, nil
		}
	}

	err := wait.PollUntilContextCancel(actionCtx, actionPoll, pollImmediately, receivedEvent)
	if err != nil {
		return ct.EventMessage{}, fmt.Errorf("Failed to receive container event message: %w", err)
	} else {
		return retval, nil
	}
}

func subscribe(t *testing.T, ctx context.Context, dco *DockerCliOrchestrator) (ct.EventSubscription, <-chan ct.EventMessage) {
	const initialEventChannelCapacity = 5
	evtC := chanx.NewUnboundedChan[ct.EventMessage](ctx, initialEventChannelCapacity)
	sub, err := dco.WatchContainers(evtC.In)
	require.NoError(t, err)
	return sub, evtC.Out
}

func requireChanClosed[ElementT any](t *testing.T, c <-chan ElementT, errMsg string) {
	require.Condition(t, func() bool {
		_, open := <-c
		return !open
	}, errMsg)
}
