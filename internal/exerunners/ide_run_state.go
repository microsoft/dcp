package exerunners

import (
	"io"
	"sync"

	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

type runChangedNotifyArg struct {
	rs         *runState
	locker     sync.Locker
	notifyType notificationType
}

type runState struct {
	name          types.NamespacedName
	runID         controllers.RunID
	sessionURL    string // The URL of the run session resource in the IDE
	changeHandler controllers.RunChangeHandler
	handlerWG     *sync.WaitGroup
	pid           process.Pid_t
	finished      bool
	exitCode      *int32
	exitCh        chan struct{}
	stdOut        *usvc_io.BufferedWrappingWriter
	stdErr        *usvc_io.BufferedWrappingWriter
}

func NewRunState() *runState {
	rs := &runState{
		runID:         "",
		changeHandler: nil,
		handlerWG:     &sync.WaitGroup{},
		finished:      false,
		exitCode:      apiv1.UnknownExitCode,
		exitCh:        make(chan struct{}),
		stdOut:        usvc_io.NewBufferedWrappingWriter(),
		stdErr:        usvc_io.NewBufferedWrappingWriter(),
	}

	// Two things need to happen before the completion handler is called:
	// 1. The StartRun() method of the IDE runner must get a chance to set the completion handler.
	// 2. The IDE runner consumer must call startWaitForRunCompletion() for the run.
	rs.handlerWG.Add(2)

	return rs
}

func notifyRunChanged(arg runChangedNotifyArg) {
	rs := arg.rs
	rs.handlerWG.Wait()

	// Make sure the run state is not changed while we are gathering data for the change handler by locking the run state.
	// The notification always uses latest data stored in the run state, so we can safely "fire and forget" this method
	// via the debounce mechanism.

	arg.locker.Lock()
	runID := rs.runID
	pid := rs.pid
	exitCode := apiv1.UnknownExitCode
	if rs.exitCode != apiv1.UnknownExitCode {
		exitCode = new(int32)
		*exitCode = *rs.exitCode
	}
	arg.locker.Unlock()

	switch arg.notifyType {
	case notificationTypeProcessRestarted:
		rs.changeHandler.OnRunChanged(runID, pid, exitCode, nil)
	case notificationTypeSessionTerminated:
		rs.changeHandler.OnRunCompleted(runID, exitCode, nil)
	}
}

func (rs *runState) NotifyRunChangedAsync(locker sync.Locker) {
	go func() {
		notifyRunChanged(runChangedNotifyArg{
			rs:         rs,
			locker:     locker,
			notifyType: notificationTypeProcessRestarted,
		})
	}()
}

func (rs *runState) NotifyRunCompletedAsync(locker sync.Locker) {
	go func() {
		notifyRunChanged(runChangedNotifyArg{
			rs:         rs,
			locker:     locker,
			notifyType: notificationTypeSessionTerminated,
		})
	}()
}

func (rs *runState) IncreaseCompletionCallReadiness() {
	rs.handlerWG.Done()
}

func (rs *runState) SetOutputWriters(stdOut, stdErr io.WriteCloser) error {
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

func (rs *runState) CloseOutputWriters() {
	rs.stdOut.Close()
	rs.stdErr.Close()
}
