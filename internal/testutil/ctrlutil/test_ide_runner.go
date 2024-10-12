package ctrlutil

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const AutoStartExecutableAnnotation = "test.usvc-dev.developer.microsoft.com/auto-start-executable"

type TestIdeRun struct {
	ID                   controllers.RunID
	Exe                  *apiv1.Executable
	RunInfo              *controllers.ExecutableRunInfo
	StartWaitingCalled   bool
	startWaitingChan     chan struct{}
	StartedAt            time.Time
	EndedAt              time.Time
	ChangeHandler        controllers.RunChangeHandler
	StartWaitingCallback func()
	PID                  process.Pid_t
	ExitCode             int32
}

func (r *TestIdeRun) Running() bool {
	return r.EndedAt.IsZero()
}
func (r *TestIdeRun) Finished() bool {
	return !r.EndedAt.IsZero()
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

func (r *TestIdeRunner) StartRun(_ context.Context, exe *apiv1.Executable, runInfo *controllers.ExecutableRunInfo, runChangeHandler controllers.RunChangeHandler, log logr.Logger) error {
	r.m.Lock()
	defer r.m.Unlock()

	namespacedName := exe.NamespacedName()

	runID := controllers.RunID("run_" + strconv.Itoa(int(atomic.AddInt32(&r.nextRunID, 1))))

	runInfo.State = apiv1.ExecutableStateStarting
	runInfo.ExecutionID = string(runID)

	run := &TestIdeRun{
		ID:        runID,
		Exe:       exe,
		RunInfo:   runInfo,
		StartedAt: time.Now(),
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

	if exe.Annotations != nil {
		if asea, ok := exe.Annotations[AutoStartExecutableAnnotation]; ok && asea == "true" {
			pid, err := randdata.MakeRandomInt64(math.MaxInt64 - 1)
			if err != nil {
				log.Error(err, "failed to generate random PID for run")
				return err
			}

			pid = pid + 1 // Ensure that the PID is positive
			go func() {
				startErr := r.SimulateSuccessfulRunStart(runID, process.Pid_t(pid))
				if startErr != nil {
					log.Error(startErr, "failed to simulate run start")
				}
			}()
		}

	}

	return nil
}

func (r *TestIdeRunner) StopRun(_ context.Context, runID controllers.RunID, _ logr.Logger) error {
	return r.doStopRun(runID, KilledProcessExitCode)
}

func (r *TestIdeRunner) SimulateSuccessfulRunStart(runID controllers.RunID, pid process.Pid_t) error {
	return r.SimulateRunStart(
		func(_ types.NamespacedName, run *TestIdeRun) bool { return run.ID == runID },
		func(run *TestIdeRun) {
			run.RunInfo.Lock()
			defer run.RunInfo.Unlock()

			run.PID = pid
			run.RunInfo.SetState(apiv1.ExecutableStateRunning)
			run.RunInfo.StartupTimestamp = metav1.NewMicroTime(run.StartedAt)
		},
	)
}

func (r *TestIdeRunner) SimulateFailedRunStart(exeName types.NamespacedName, startupError error) error {
	return r.SimulateRunStart(
		func(objName types.NamespacedName, run *TestIdeRun) bool { return objName == exeName },
		func(run *TestIdeRun) {
			run.RunInfo.Lock()
			defer run.RunInfo.Unlock()

			run.ID = controllers.UnknownRunID
			run.PID = process.UnknownPID
			run.RunInfo.SetState(apiv1.ExecutableStateFailedToStart)
			run.RunInfo.StartupTimestamp = metav1.NewMicroTime(run.StartedAt)
		},
	)
}

func (r *TestIdeRunner) SimulateRunStart(
	isDesiredRun func(types.NamespacedName, *TestIdeRun) bool,
	changeToStarted func(*TestIdeRun),
) error {
	run, found := r.findAndChangeRun(isDesiredRun, changeToStarted)
	if !found {
		return fmt.Errorf("run for Executable '%s' was not found", run.Exe.NamespacedName().String())
	}

	if run.ChangeHandler != nil {
		// Make sure OnStartingCompleted is called before we return and let the test proceed.
		done := make(chan struct{})
		go func() {
			run.RunInfo.Lock()
			defer run.RunInfo.Unlock()

			run.ChangeHandler.OnStartingCompleted(run.Exe.NamespacedName(), run.ID, run.RunInfo, run.StartWaitingCallback)

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
			run.EndedAt = time.Now()
			run.ExitCode = exitCode
		},
	)

	if !found {
		return fmt.Errorf("run '%s' was not found, cannot be stopped", runID)
	}

	if run.ChangeHandler != nil {
		go func() {
			ec := new(int32)
			*ec = exitCode
			run.ChangeHandler.OnRunCompleted(runID, ec, nil)
		}()
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
