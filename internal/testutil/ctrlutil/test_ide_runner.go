// Copyright (c) Microsoft Corporation. All rights reserved.

package ctrlutil

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/pointers"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/syncmap"
)

const AutoStartExecutableAnnotation = "test.usvc-dev.developer.microsoft.com/auto-start-executable"

type TestIdeRun struct {
	ID                   controllers.RunID
	Exe                  *apiv1.Executable
	FinishTimestamp      metav1.MicroTime
	ExitCode             *int32
	StartWaitingCalled   bool
	startWaitingChan     chan struct{}
	ChangeHandler        controllers.RunChangeHandler
	StartWaitingCallback func()
}

func (r *TestIdeRun) Running() bool {
	return r.FinishTimestamp.IsZero()
}
func (r *TestIdeRun) Finished() bool {
	return !r.FinishTimestamp.IsZero()
}

type TestIdeRunner struct {
	nextRunID   int32
	Runs        *syncmap.Map[types.NamespacedName, *TestIdeRun]
	m           *sync.RWMutex
	lifetimeCtx context.Context
}

func NewTestIdeRunner(lifetimeCtx context.Context) *TestIdeRunner {
	return &TestIdeRunner{
		Runs:        &syncmap.Map[types.NamespacedName, *TestIdeRun]{},
		m:           &sync.RWMutex{},
		lifetimeCtx: lifetimeCtx,
	}
}

func (r *TestIdeRunner) StartRun(
	_ context.Context,
	exe *apiv1.Executable,
	runChangeHandler controllers.RunChangeHandler,
	log logr.Logger,
) *controllers.ExecutableStartResult {
	r.m.Lock()
	defer r.m.Unlock()

	namespacedName := exe.NamespacedName()

	runID := controllers.RunID("run_" + strconv.Itoa(int(atomic.AddInt32(&r.nextRunID, 1))))

	run := &TestIdeRun{
		ID:  runID,
		Exe: exe,
		StartWaitingCallback: func() {
			r.m.Lock()
			defer r.m.Unlock()

			if run, found := r.Runs.Load(namespacedName); found {
				if !run.StartWaitingCalled {
					run.StartWaitingCalled = true
					if run.startWaitingChan != nil {
						close(run.startWaitingChan)
					}
				}
			}
		},
		ChangeHandler: runChangeHandler,
	}

	if runChangeHandler != nil {
		run.startWaitingChan = make(chan struct{})
	}

	r.Runs.Store(namespacedName, run)
	result := controllers.NewExecutableStartResult()

	if exe.Annotations != nil {
		if asea, ok := exe.Annotations[AutoStartExecutableAnnotation]; ok && asea == "true" {
			pid, err := randdata.MakeRandomInt64(math.MaxInt64 - 1)
			if err != nil {
				log.Error(err, "Failed to generate random PID for run")
				result.ExeState = apiv1.ExecutableStateFailedToStart
				result.StartupError = err
				return result
			}

			pid = pid + 1 // Ensure that the PID is positive
			go func() {
				startErr := r.SimulateSuccessfulRunStart(runID, process.Pid_t(pid))
				if startErr != nil {
					log.Error(startErr, "Failed to simulate run start")
				}
			}()
		}

	}

	result.ExeState = apiv1.ExecutableStateStarting
	result.RunID = runID
	return result
}

func (r *TestIdeRunner) StopRun(_ context.Context, runID controllers.RunID, _ logr.Logger) error {
	return r.doStopRun(runID, internal_testutil.KilledProcessExitCode)
}

func (r *TestIdeRunner) SimulateSuccessfulRunStart(runID controllers.RunID, pid process.Pid_t) error {
	return r.SimulateRunStart(
		func(_ types.NamespacedName, run *TestIdeRun) bool { return run.ID == runID },
		func(run *TestIdeRun, result *controllers.ExecutableStartResult) {
			pointers.SetValue(&result.Pid, int64(pid))
			result.RunID = runID
			result.ExeState = apiv1.ExecutableStateRunning
			result.CompletionTimestamp = metav1.NowMicro()
		},
	)
}

func (r *TestIdeRunner) SimulateFailedRunStart(exeName types.NamespacedName, startupError error) error {
	return r.SimulateRunStart(
		func(objName types.NamespacedName, run *TestIdeRun) bool { return objName == exeName },
		func(run *TestIdeRun, result *controllers.ExecutableStartResult) {
			run.ID = controllers.UnknownRunID
			result.RunID = controllers.UnknownRunID
			result.Pid = apiv1.UnknownPID
			result.ExeState = apiv1.ExecutableStateFailedToStart
			result.CompletionTimestamp = metav1.NowMicro()
		},
	)
}

func (r *TestIdeRunner) SimulateRunStart(
	isDesiredRun func(types.NamespacedName, *TestIdeRun) bool,
	changeToStarted func(*TestIdeRun, *controllers.ExecutableStartResult),
) error {
	// Do not take a lock here. findAndChangeRun() will take a lock internally for the duration of its working.

	var result controllers.ExecutableStartResult
	run, found := r.findAndChangeRun(isDesiredRun, func(run *TestIdeRun) {
		changeToStarted(run, &result)
	})
	if !found {
		return fmt.Errorf("run for Executable '%s' was not found", run.Exe.NamespacedName().String())
	}

	if run.ChangeHandler != nil {
		// Make sure OnStartupCompleted is called before we return and let the test proceed.
		done := make(chan struct{})
		go func() {
			result.StartWaitForRunCompletion = run.StartWaitingCallback
			run.ChangeHandler.OnStartupCompleted(run.Exe.NamespacedName(), &result)

			close(done)
		}()
		<-done
	}

	return nil
}

func (r *TestIdeRunner) SimulateRunEnd(runID controllers.RunID, exitCode int32) error {
	return r.doStopRun(runID, exitCode)
}

func (r *TestIdeRunner) FindAll(exePath string, cond func(run TestIdeRun) bool) []TestIdeRun {
	r.m.RLock()
	defer r.m.RUnlock()
	retval := make([]TestIdeRun, 0)

	r.Runs.Range(func(_ types.NamespacedName, run *TestIdeRun) bool {
		if run.Exe.Spec.ExecutablePath == exePath && (cond == nil || cond(*run)) {
			retval = append(retval, *run)
		}

		return true
	})

	return retval
}

func (r *TestIdeRunner) doStopRun(runID controllers.RunID, exitCode int32) error {
	// Do not take a lock here. findAndChangeRun() will take a lock internally for the duration of its working.

	var run, found = r.findAndChangeRun(
		func(_ types.NamespacedName, run *TestIdeRun) bool { return run.ID == runID },
		func(run *TestIdeRun) {
			run.FinishTimestamp = metav1.NowMicro()
			pointers.SetValue(&run.ExitCode, int32(exitCode))
		},
	)

	if !found {
		return fmt.Errorf("run '%s' was not found, cannot be stopped", runID)
	}

	if run.ChangeHandler != nil {
		done := make(chan struct{}) // Make sure OnRunCompleted is called before we return and let the test proceed.
		go func() {
			ec := new(int32)
			*ec = exitCode
			select {
			case <-r.lifetimeCtx.Done():
				return
			case <-run.startWaitingChan:
				run.ChangeHandler.OnRunCompleted(runID, ec, nil)
			}
			close(done)
		}()
		<-done
	}

	return nil
}

func (r *TestIdeRunner) findAndChangeRun(matches func(types.NamespacedName, *TestIdeRun) bool, change func(*TestIdeRun)) (TestIdeRun, bool) {
	r.m.Lock()
	defer r.m.Unlock()

	var foundRun *TestIdeRun
	r.Runs.Range(func(_ types.NamespacedName, run *TestIdeRun) bool {
		if matches(run.Exe.NamespacedName(), run) {
			change(run)
			foundRun = run
			return false
		}

		return true
	})

	if foundRun == nil {
		return TestIdeRun{}, false
	} else {
		return *foundRun, true
	}
}

var _ controllers.ExecutableRunner = (*TestIdeRunner)(nil)
