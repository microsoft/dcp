/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package exerunners

import (
	"context"
	"io"
	"sync"

	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

type runState uint32

const (
	runStateNotStarted    runState = 0
	runStateRunning       runState = 0x1
	runStateCompleted     runState = 0x2
	runStateFailedToStart runState = 0x3

	// Two things need to happen before the run change handler is called:
	// 1. The StartRun() method of the IDE runner must get a chance to set the change handler.
	// 2. The IDE runner consumer must call startWaitForRunCompletion() for the run.
	RequiredChangeHandlerReadiness = 2
)

type runData struct {
	name                    types.NamespacedName
	runID                   controllers.RunID
	sessionURL              string // The URL of the run session resource in the IDE
	changeHandler           controllers.RunChangeHandler
	changeHandlerWG         *sync.WaitGroup
	pid                     process.Pid_t
	state                   runState
	startRunMethodCompleted bool
	exitCode                *int32
	exitCh                  chan struct{} // Channel used to confirm the receipt of run termination notification from the IDE
	stdOut                  *usvc_io.BufferedWrappingWriter
	stdErr                  *usvc_io.BufferedWrappingWriter
	changeNotifyQueue       *resiliency.WorkQueue
}

func NewRunData(lifetimeCtx context.Context) *runData {
	rs := &runData{
		runID:             "",
		changeHandler:     nil,
		changeHandlerWG:   &sync.WaitGroup{},
		state:             runStateNotStarted,
		exitCode:          apiv1.UnknownExitCode,
		exitCh:            make(chan struct{}),
		stdOut:            usvc_io.NewBufferedWrappingWriter(),
		stdErr:            usvc_io.NewBufferedWrappingWriter(),
		changeNotifyQueue: resiliency.NewWorkQueue(lifetimeCtx, 1), // For a particular run, change notifications are serialized
	}

	rs.changeHandlerWG.Add(RequiredChangeHandlerReadiness)

	return rs
}

func (rs *runData) notifyRunChanged(locker sync.Locker, notifyType notificationType) {
	rs.changeHandlerWG.Wait()

	// Make sure the run state is not changed while we are gathering data for the change handler by locking the run state.
	// The notification always uses latest data stored in the run state.

	locker.Lock()

	if rs.changeHandler == nil {
		// We might get some notifications during startup sequence, and then the startup may fail, leaving us with no change handler.
		locker.Unlock()
		return
	}

	runID := rs.runID
	pid := rs.pid
	exitCode := apiv1.UnknownExitCode
	if rs.exitCode != apiv1.UnknownExitCode {
		exitCode = new(int32)
		*exitCode = *rs.exitCode
	}
	locker.Unlock()

	switch notifyType {
	case notificationTypeProcessRestarted:
		rs.changeHandler.OnMainProcessChanged(runID, pid)
	case notificationTypeSessionTerminated:
		rs.changeHandler.OnRunCompleted(runID, exitCode, nil)
	}
}

func (rs *runData) NotifyRunChangedAsync(locker sync.Locker) {
	if rs.state == runStateNotStarted || rs.state == runStateRunning {
		// Errors only if lifetime context is cancelled
		_ = rs.changeNotifyQueue.Enqueue(func(_ context.Context) {
			rs.notifyRunChanged(locker, notificationTypeProcessRestarted)
		})
	}
}

func (rs *runData) NotifyRunCompletedAsync(locker sync.Locker) {
	// Errors only if lifetime context is cancelled
	_ = rs.changeNotifyQueue.Enqueue(func(_ context.Context) {
		rs.notifyRunChanged(locker, notificationTypeSessionTerminated)
	})
}

func (rs *runData) SendRunMessageAsync(locker sync.Locker, message string, level controllers.RunMessageLevel) {
	_ = rs.changeNotifyQueue.Enqueue(func(_ context.Context) {
		locker.Lock()
		if rs.changeHandler == nil {
			// We might get some notifications during startup sequence, and then the startup may fail, leaving us with no change handler.
			locker.Unlock()
			return
		}

		ch := rs.changeHandler
		runID := rs.runID
		locker.Unlock()

		ch.OnRunMessage(runID, level, message)
	})
}

func (rs *runData) IncreaseChangeHandlerReadiness() {
	rs.changeHandlerWG.Done()
}

func (rs *runData) DisableChangeHandlerReadiness() {
	for i := 0; i < RequiredChangeHandlerReadiness; i++ {
		rs.changeHandlerWG.Done()
	}
}

func (rs *runData) SetOutputWriters(stdOut, stdErr usvc_io.WriteSyncerCloser) error {
	var err error
	if stdOut != nil {
		err = rs.stdOut.SetTarget(usvc_io.NewTimestampWriter(stdOut))
	} else {
		err = rs.stdOut.SetTarget(io.Discard)
	}
	if err != nil {
		return err
	}

	if stdErr != nil {
		err = rs.stdErr.SetTarget(usvc_io.NewTimestampWriter(stdErr))
	} else {
		err = rs.stdErr.SetTarget(io.Discard)
	}
	if err != nil {
		return err
	}

	return nil
}

func (rs *runData) CloseOutputWriters() {
	rs.stdOut.Close()
	rs.stdErr.Close()
}
