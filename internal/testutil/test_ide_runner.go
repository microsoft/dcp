package testutil

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type TestIdeRun struct {
	ID                   controllers.RunID
	Exe                  *apiv1.Executable
	Status               *apiv1.ExecutableStatus
	StartWaitingCalled   bool
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
	nextRunID int32
	Runs      []TestIdeRun
	m         *sync.RWMutex
}

func NewTestIdeRunner() *TestIdeRunner {
	return &TestIdeRunner{
		Runs: make([]TestIdeRun, 0),
		m:    &sync.RWMutex{},
	}
}

func (r *TestIdeRunner) StartRun(_ context.Context, exe *apiv1.Executable, runChangeHandler controllers.RunChangeHandler, _ logr.Logger) error {
	runID := controllers.RunID("run_" + strconv.Itoa(int(atomic.AddInt32(&r.nextRunID, 1))))
	r.m.Lock()
	defer r.m.Unlock()

	exe.Status.State = apiv1.ExecutableStateStarting
	exe.Status.ExecutionID = string(runID)

	run := TestIdeRun{
		ID:        runID,
		Exe:       exe,
		Status:    exe.Status.DeepCopy(),
		StartedAt: time.Now(),
		StartWaitingCallback: func() {
			r.m.Lock()
			defer r.m.Unlock()
			i := r.find(runID)
			run := r.Runs[i]
			run.StartWaitingCalled = true
			r.Runs[i] = run

			run.ChangeHandler.OnRunChanged(runID, run.PID, apiv1.UnknownExitCode, nil)
		},
		ChangeHandler: runChangeHandler,
	}

	r.Runs = append(r.Runs, run)

	return nil
}

func (r *TestIdeRunner) StopRun(_ context.Context, runID controllers.RunID, _ logr.Logger) error {
	return r.doStopRun(runID, KilledProcessExitCode)
}

func (r *TestIdeRunner) SimulateRunStart(runID controllers.RunID, pid process.Pid_t) error {
	return r.doChangeRun(runID, pid)
}

func (r *TestIdeRunner) SimulateRunEnd(runID controllers.RunID, exitCode int32) error {
	return r.doStopRun(runID, exitCode)
}

func (r *TestIdeRunner) FindAll(exePath string, cond func(run TestIdeRun) bool) []TestIdeRun {
	r.m.RLock()
	defer r.m.RUnlock()
	retval := make([]TestIdeRun, 0)

	for _, run := range r.Runs {
		if run.Exe.Spec.ExecutablePath == exePath && (cond == nil || cond(run)) {
			retval = append(retval, run)
		}
	}

	return retval
}

func (r *TestIdeRunner) doChangeRun(runID controllers.RunID, pid process.Pid_t) error {
	run, err := func() (TestIdeRun, error) {
		r.m.Lock()
		defer r.m.Unlock()

		i := r.find(runID)
		if i == NotFound {
			return TestIdeRun{}, fmt.Errorf("run '%s' was not found, cannot be stopped", runID)
		}

		run := r.Runs[i]
		run.PID = pid
		run.Status.State = apiv1.ExecutableStateRunning
		run.Status.StartupTimestamp = metav1.NewTime(run.StartedAt)
		r.Runs[i] = run

		return run, nil
	}()

	if err != nil {
		return err
	}

	if run.ChangeHandler != nil {
		done := make(chan struct{})
		go func() {
			run.ChangeHandler.OnStarted(run.Exe.NamespacedName(), runID, *run.Status, run.StartWaitingCallback)

			close(done)
		}()
		<-done
	}

	return nil
}

func (r *TestIdeRunner) doStopRun(runID controllers.RunID, exitCode int32) error {
	r.m.Lock()
	defer r.m.Unlock()

	i := r.find(runID)
	if i == NotFound {
		return fmt.Errorf("run '%s' was not found, cannot be stopped", runID)
	}

	run := r.Runs[i]
	run.EndedAt = time.Now()
	run.ExitCode = exitCode
	r.Runs[i] = run

	if run.ChangeHandler != nil {
		done := make(chan struct{})
		go func() {
			ec := new(int32)
			*ec = exitCode
			run.ChangeHandler.OnRunChanged(runID, run.PID, ec, nil)
			close(done)
		}()
		<-done
	}

	return nil
}

func (r *TestIdeRunner) find(runID controllers.RunID) int {
	for i, run := range r.Runs {
		if run.ID == runID {
			return i
		}
	}

	return NotFound
}

var _ controllers.ExecutableRunner = (*TestIdeRunner)(nil)
