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
	ID                 controllers.RunID
	Exe                *apiv1.Executable
	StartWaitingCalled bool
	StartedAt          time.Time
	EndedAt            time.Time
	ChangeHandler      controllers.RunChangeHandler
	PID                process.Pid_t
	ExitCode           int32
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

func (r *TestIdeRunner) StartRun(_ context.Context, exe *apiv1.Executable, runChangeHandler controllers.RunChangeHandler, _ logr.Logger) (controllers.RunID, func(), error) {
	runID := controllers.RunID("run_" + strconv.Itoa(int(atomic.AddInt32(&r.nextRunID, 1))))
	r.m.Lock()
	defer r.m.Unlock()

	run := TestIdeRun{
		ID:            runID,
		Exe:           exe,
		StartedAt:     time.Now(),
		ChangeHandler: runChangeHandler,
	}

	r.Runs = append(r.Runs, run)

	startWaitForCompletion := func() {
		r.m.Lock()
		defer r.m.Unlock()
		i := r.find(runID)
		run := r.Runs[i]
		run.StartWaitingCalled = true
		r.Runs[i] = run
	}

	exe.Status.ExecutionID = string(runID)
	exe.Status.State = apiv1.ExecutableStateRunning
	exe.Status.StartupTimestamp = metav1.NewTime(run.StartedAt)

	return runID, startWaitForCompletion, nil
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
	r.m.Lock()
	defer r.m.Unlock()

	i := r.find(runID)
	if i == NotFound {
		return fmt.Errorf("run '%s' was not found, cannot be stopped", runID)
	}

	run := r.Runs[i]
	run.PID = pid
	r.Runs[i] = run

	if run.ChangeHandler != nil {
		done := make(chan struct{})
		go func() {
			run.ChangeHandler.OnRunChanged(runID, pid, process.UnknownExitCode, nil)
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
			run.ChangeHandler.OnRunChanged(runID, run.PID, exitCode, nil)
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
