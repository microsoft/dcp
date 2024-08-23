package integration_test

import (
	"bufio"
	"context"
	"fmt"
	"math/rand"
	"regexp"
	std_slices "slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	wait "k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil/ctrlutil"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

// Ensure that a process or IDE run session is started when new Executable object appears
func TestExecutableIsStarted(t *testing.T) {
	type testcase struct {
		description   string
		exe           *apiv1.Executable
		verifyRunning func(ctx context.Context, t *testing.T, exe *apiv1.Executable)
	}

	testcases := []testcase{
		{
			description: "executable starts process",
			exe: &apiv1.Executable{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "executable-starts-process",
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/executable-starts-process",
				},
			},
			verifyRunning: func(ctx context.Context, t *testing.T, exe *apiv1.Executable) {
				if _, err := ensureProcessRunning(ctx, exe.Spec.ExecutablePath); err != nil {
					t.Fatalf("Process could not be started: %v", err)
				}
			},
		},
		{
			description: "executable starts IDE run session",
			exe: &apiv1.Executable{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "executable-starts-ide-run-session",
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/executable-starts-ide-run-session",
					ExecutionType:  apiv1.ExecutionTypeIDE,
				},
			},
			verifyRunning: func(ctx context.Context, t *testing.T, exe *apiv1.Executable) {
				if _, err := ensureIdeRunSessionStarted(ctx, exe.Spec.ExecutablePath); err != nil {
					t.Fatalf("IDE run session could not be started: %v", err)
				}
			},
		},
	}

	t.Parallel()

	for _, tc := range testcases {
		tc := tc // capture range variable for use in closures/goroutines

		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()

			t.Logf("Creating Executable '%s'", tc.exe.ObjectMeta.Name)
			if err := client.Create(ctx, tc.exe); err != nil {
				t.Fatalf("Could not create Executable: %v", err)
			}

			t.Log("Checking if Executable has been started...")
			tc.verifyRunning(ctx, t, tc.exe)
		})
	}
}

// Ensure exit code of processes/run sessions are captured correctly
func TestExecutableExitCodeCaptured(t *testing.T) {
	type testcase struct {
		description       string
		exe               *apiv1.Executable
		verifyExeRunning  func(ctx context.Context, t *testing.T, exe *apiv1.Executable) controllers.RunID
		simulateRunEnding func(t *testing.T, runID controllers.RunID)
	}

	const expectedEC = 12

	testcases := []testcase{
		{
			description: "process exit code captured",
			exe: &apiv1.Executable{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "exit-code-captured-process",
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/exit-code-captured-process",
				},
			},
			verifyExeRunning: func(ctx context.Context, t *testing.T, exe *apiv1.Executable) controllers.RunID {
				pid, err := ensureProcessRunning(ctx, exe.Spec.ExecutablePath)
				if err != nil {
					t.Fatalf("Process could not be started: %v", err)
					return "" // make compiler happy
				} else {
					return controllers.RunID(strconv.Itoa(int(pid)))
				}
			},
			simulateRunEnding: func(t *testing.T, runID controllers.RunID) {
				pid64, err := strconv.ParseInt(string(runID), 10, 32)
				require.NoError(t, err)
				pid, err := process.Int64ToPidT(pid64)
				require.NoError(t, err)
				testProcessExecutor.SimulateProcessExit(t, pid, int32(expectedEC))
			},
		},
		{
			description: "IDE exit code captured",
			exe: &apiv1.Executable{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "exit-code-captured-ide",
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/exit-code-captured-ide",
					ExecutionType:  apiv1.ExecutionTypeIDE,
				},
			},
			verifyExeRunning: func(ctx context.Context, t *testing.T, exe *apiv1.Executable) controllers.RunID {
				runID, err := ensureIdeRunSessionStarted(ctx, exe.Spec.ExecutablePath)

				if err != nil {
					t.Fatalf("IDE run session could not be started: %v", err)
					return controllers.UnknownRunID // make compiler happy
				}

				return runID
			},
			simulateRunEnding: func(t *testing.T, runID controllers.RunID) {
				err := ideRunner.SimulateRunEnd(controllers.RunID(runID), expectedEC)
				require.NoError(t, err)
			},
		},
	}

	t.Parallel()

	for _, tc := range testcases {
		tc := tc // capture range variable for use in closures/goroutines

		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()

			t.Logf("Creating Executable '%s'", tc.exe.ObjectMeta.Name)
			if err := client.Create(ctx, tc.exe); err != nil {
				t.Fatalf("Could not create Executable: %v", err)
			}

			t.Log("Waiting for process to start...")
			runID := tc.verifyExeRunning(ctx, t, tc.exe)

			t.Log("Executable started, shutting the underlying run down...")
			tc.simulateRunEnding(t, runID)

			exitCodeCaptured := func(ctx context.Context) (bool, error) {
				var updatedExe apiv1.Executable
				if err := client.Get(ctx, ctrl_client.ObjectKeyFromObject(tc.exe), &updatedExe); err != nil {
					t.Fatalf("Unable to fetch updated Executable: %v", err)
					return false, err
				}

				if updatedExe.Status.ExecutionID == string(runID) && updatedExe.Status.State == apiv1.ExecutableStateFinished && updatedExe.Status.ExitCode != apiv1.UnknownExitCode && *updatedExe.Status.ExitCode == int32(expectedEC) {
					return true, nil
				}

				return false, nil
			}
			err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, exitCodeCaptured)
			require.NoError(t, err, "Exit codes could not be captured for Executable")
		})
	}
}

// Ensure the run is terminated if stop is set to true
func TestExecutableStopState(t *testing.T) {
	type testcase struct {
		description      string
		exe              *apiv1.Executable
		verifyExeRunning func(ctx context.Context, t *testing.T, exe *apiv1.Executable)
		verifyRunEnded   func(ctx context.Context, t *testing.T, exe *apiv1.Executable)
	}

	testcases := []testcase{
		{
			description: "verify process-based Executable run is terminated",
			exe: &apiv1.Executable{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "executable-stop-state-process",
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/executable-stop-state-process",
				},
			},
			verifyExeRunning: func(ctx context.Context, t *testing.T, exe *apiv1.Executable) {
				_, err := ensureProcessRunning(ctx, exe.Spec.ExecutablePath)
				if err != nil {
					t.Fatalf("Process could not be started: %v", err)
				}
			},
			verifyRunEnded: func(ctx context.Context, t *testing.T, exe *apiv1.Executable) {
				processKilled := func(_ context.Context) (bool, error) {
					killedProcesses := testProcessExecutor.FindAll([]string{exe.Spec.ExecutablePath}, "", func(pe *ctrl_testutil.ProcessExecution) bool {
						return pe.Finished() && pe.ExitCode == ctrl_testutil.KilledProcessExitCode
					})
					return len(killedProcesses) == 1, nil
				}
				err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, processKilled)
				require.NoError(t, err, "Process was not terminated as expected")
			},
		},
		{
			description: "verify IDE-based Executable run is terminated",
			exe: &apiv1.Executable{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "executable-stop-state-ide",
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/executable-stop-state-ide",
					ExecutionType:  apiv1.ExecutionTypeIDE,
				},
			},
			verifyExeRunning: func(ctx context.Context, t *testing.T, exe *apiv1.Executable) {
				if _, err := ensureIdeRunSessionStarted(ctx, exe.Spec.ExecutablePath); err != nil {
					t.Fatalf("IDE run session could not be started: %v", err)
				}
			},
			verifyRunEnded: func(ctx context.Context, t *testing.T, exe *apiv1.Executable) {
				runEnded := func(_ context.Context) (bool, error) {
					endedRuns := ideRunner.FindAll(exe.Spec.ExecutablePath, func(run ctrl_testutil.TestIdeRun) bool {
						return run.Finished() && run.ExitCode == ctrl_testutil.KilledProcessExitCode
					})
					return len(endedRuns) == 1, nil
				}
				err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, runEnded)
				require.NoError(t, err, "The IDE run was not terminated as expected")
			},
		},
	}

	t.Parallel()

	for _, tc := range testcases {
		tc := tc // capture range variable for use in closures/goroutines

		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()

			t.Logf("Creating Executable '%s'", tc.exe.ObjectMeta.Name)
			if err := client.Create(ctx, tc.exe); err != nil {
				t.Fatalf("Could not create Executable: %v", err)
			}

			t.Logf("Waiting for Executable '%s' run to start...", tc.exe.ObjectMeta.Name)
			tc.verifyExeRunning(ctx, t, tc.exe)

			updatedExe := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(tc.exe), func(currentExe *apiv1.Executable) (bool, error) {
				return currentExe.Status.State != "", nil
			})

			t.Logf("Setting Executable '%s' stop to 'true'...", tc.exe.ObjectMeta.Name)
			err := retryOnConflict(ctx, updatedExe.NamespacedName(), func(ctx context.Context, currentExe *apiv1.Executable) error {
				exePatch := currentExe.DeepCopy()
				exePatch.Spec.Stop = true
				return client.Patch(ctx, exePatch, ctrl_client.MergeFromWithOptions(currentExe, ctrl_client.MergeFromWithOptimisticLock{}))
			})
			if err != nil {
				t.Fatalf("Unable to update Executable: %v", err)
			}

			t.Logf("Waiting for process associated with Executable '%s' to be killed...", tc.exe.ObjectMeta.Name)
			tc.verifyRunEnded(ctx, t, tc.exe)

			t.Logf("Verify Executable '%s' reaches Finished state...", tc.exe.ObjectMeta.Name)
			updatedExe = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(tc.exe), func(currentExe *apiv1.Executable) (bool, error) {
				return currentExe.Status.State == apiv1.ExecutableStateFinished, nil
			})

			err = retryOnConflict(ctx, updatedExe.NamespacedName(), func(ctx context.Context, currentExe *apiv1.Executable) error {
				return client.Delete(ctx, currentExe)
			})
			if err != nil {
				t.Fatalf("Unable to delete Executable: %v", err)
			}
		})
	}
}

// Ensure the run is terminated if Executable is deleted
func TestExecutableDeletion(t *testing.T) {
	type testcase struct {
		description      string
		exe              *apiv1.Executable
		verifyExeRunning func(ctx context.Context, t *testing.T, exe *apiv1.Executable)
		verifyRunEnded   func(ctx context.Context, t *testing.T, exe *apiv1.Executable)
	}

	testcases := []testcase{
		{
			description: "verify process-based Executable run is terminated",
			exe: &apiv1.Executable{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "executable-deletion-process",
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/executable-deletion-process",
				},
			},
			verifyExeRunning: func(ctx context.Context, t *testing.T, exe *apiv1.Executable) {
				_, err := ensureProcessRunning(ctx, exe.Spec.ExecutablePath)
				if err != nil {
					t.Fatalf("Process could not be started: %v", err)
				}
			},
			verifyRunEnded: func(ctx context.Context, t *testing.T, exe *apiv1.Executable) {
				processKilled := func(_ context.Context) (bool, error) {
					killedProcesses := testProcessExecutor.FindAll([]string{exe.Spec.ExecutablePath}, "", func(pe *ctrl_testutil.ProcessExecution) bool {
						return pe.Finished() && pe.ExitCode == ctrl_testutil.KilledProcessExitCode
					})
					return len(killedProcesses) == 1, nil
				}
				err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, processKilled)
				require.NoError(t, err, "Process was not terminated as expected")
			},
		},
		{
			description: "verify IDE-based Executable run is terminated",
			exe: &apiv1.Executable{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "executable-deletion-ide",
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/executable-deletion-ide",
					ExecutionType:  apiv1.ExecutionTypeIDE,
				},
			},
			verifyExeRunning: func(ctx context.Context, t *testing.T, exe *apiv1.Executable) {
				if _, err := ensureIdeRunSessionStarted(ctx, exe.Spec.ExecutablePath); err != nil {
					t.Fatalf("IDE run session could not be started: %v", err)
				}
			},
			verifyRunEnded: func(ctx context.Context, t *testing.T, exe *apiv1.Executable) {
				runEnded := func(_ context.Context) (bool, error) {
					endedRuns := ideRunner.FindAll(exe.Spec.ExecutablePath, func(run ctrl_testutil.TestIdeRun) bool {
						return run.Finished() && run.ExitCode == ctrl_testutil.KilledProcessExitCode
					})
					return len(endedRuns) == 1, nil
				}
				err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, runEnded)
				require.NoError(t, err, "The IDE run was not terminated as expected")
			},
		},
	}

	t.Parallel()

	for _, tc := range testcases {
		tc := tc // capture range variable for use in closures/goroutines

		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()

			t.Logf("Creating Executable '%s'", tc.exe.ObjectMeta.Name)
			if err := client.Create(ctx, tc.exe); err != nil {
				t.Fatalf("Could not create Executable: %v", err)
			}

			t.Logf("Waiting for Executable '%s' run to start...", tc.exe.ObjectMeta.Name)
			tc.verifyExeRunning(ctx, t, tc.exe)

			updatedExe := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(tc.exe), func(currentExe *apiv1.Executable) (bool, error) {
				return currentExe.Status.State != "", nil
			})

			t.Logf("Deleting Executable '%s' object...", tc.exe.ObjectMeta.Name)
			err := retryOnConflict(ctx, updatedExe.NamespacedName(), func(ctx context.Context, currentExe *apiv1.Executable) error {
				return client.Delete(ctx, currentExe)
			})
			if err != nil {
				t.Fatalf("Unable to delete Executable: %v", err)
			}

			t.Logf("Waiting for process associated with Executable '%s' to be killed...", tc.exe.ObjectMeta.Name)
			tc.verifyRunEnded(ctx, t, tc.exe)
		})
	}
}

// Ensure that Executable ends up in "failed to start" state with FinishTimestamp set if the run fails
// Uses OS process runner
func TestExecutableStartupFailureProcess(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	exe := &apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "executable-startup-failure-process",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/executable-startup-failure-process",
		},
	}

	t.Logf("Preparing run for Executable '%s'... (should fail)", exe.ObjectMeta.Name)
	testProcessExecutor.InstallAutoExecution(ctrl_testutil.AutoExecution{
		Condition: ctrl_testutil.ProcessSearchCriteria{
			Command: []string{exe.Spec.ExecutablePath},
		},
		StartupError: fmt.Errorf("simulated startup failure for Executable '%s'", exe.ObjectMeta.Name),
	})
	defer testProcessExecutor.RemoveAutoExecution(ctrl_testutil.ProcessSearchCriteria{
		Command: []string{exe.Spec.ExecutablePath},
	})

	t.Logf("Creating Executable '%s'", exe.ObjectMeta.Name)
	if err := client.Create(ctx, exe); err != nil {
		t.Fatalf("Could not create Executable: %v", err)
	}

	t.Logf("Waiting for Executable '%s' to end up in 'failed to start' state...", exe.ObjectMeta.Name)
	_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(exe), func(currentExe *apiv1.Executable) (bool, error) {
		stateFailedToStart := currentExe.Status.State == apiv1.ExecutableStateFailedToStart
		finishTimestampSet := !currentExe.Status.FinishTimestamp.IsZero()
		return stateFailedToStart && finishTimestampSet, nil
	})
}

// Ensure that Executable ends up in "failed to start" state with FinishTimestamp set if the run fails
// Uses IDE process runner
func TestExecutableStartupFailureIDE(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	exe := &apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "executable-startup-failure-ide",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/executable-startup-failure-ide",
			ExecutionType:  apiv1.ExecutionTypeIDE,
		},
	}

	t.Logf("Creating Executable '%s'", exe.ObjectMeta.Name)
	if err := client.Create(ctx, exe); err != nil {
		t.Fatalf("Could not create Executable: %v", err)
	}

	t.Logf("Simulating run for Executable '%s'... (should fail)", exe.ObjectMeta.Name)
	_, err := findIdeRun(ctx, exe.Spec.ExecutablePath) // Need to give the Executable controller time to create the run session
	require.NoError(t, err, "IDE run session for Executable '%s' could not be found", exe.ObjectMeta.Name)
	err = ideRunner.SimulateFailedRunStart(exe.NamespacedName(), fmt.Errorf("simulated startup failure for Executable '%s'", exe.ObjectMeta.Name))
	require.NoError(t, err, "IDE run session for Executable '%s' could not be simulated to fail", exe.ObjectMeta.Name)

	t.Logf("Waiting for Executable '%s' to end up in 'failed to start' state...", exe.ObjectMeta.Name)
	_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(exe), func(currentExe *apiv1.Executable) (bool, error) {
		stateFailedToStart := currentExe.Status.State == apiv1.ExecutableStateFailedToStart
		finishTimestampSet := !currentExe.Status.FinishTimestamp.IsZero()
		return stateFailedToStart && finishTimestampSet, nil
	})
}

// Verify ports are injected into Executable environment variables via portForServing template function.
// Service ports are specified via service-producer annotation.
func TestExecutableServingPortInjected(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-executable-serving-port-injected-env-var-svc",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol: apiv1.TCP,
			Address:  "127.22.33.44",
			Port:     7732,
		},
	}

	t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
	err := client.Create(ctx, &svc)
	require.NoError(t, err, "Could not create the Service")

	exe := apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-executable-serving-port-injected-env-var",
			Namespace:   metav1.NamespaceNone,
			Annotations: map[string]string{"service-producer": fmt.Sprintf(`[{"serviceName":"%s","address":"127.0.0.1","port":7733}]`, svc.ObjectMeta.Name)},
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/test-executable-serving-port-injected-env-var",
			Env: []apiv1.EnvVar{
				{
					Name:  "SVC_PORT",
					Value: fmt.Sprintf(`{{- portForServing "%s" -}}`, svc.ObjectMeta.Name),
				},
			},
		},
	}

	t.Logf("Creating Executable '%s' that is producing the Service '%s'...", exe.ObjectMeta.Name, svc.ObjectMeta.Name)
	err = client.Create(ctx, &exe)
	require.NoError(t, err, "Could not create the Executable")

	t.Logf("Ensure Executable '%s' is running...", exe.ObjectMeta.Name)
	pid, err := ensureProcessRunning(ctx, exe.Spec.ExecutablePath)
	require.NoError(t, err, "Process could not be started")

	t.Logf("Ensure the Executable '%s' process got the service port injected...", exe.ObjectMeta.Name)
	pe, found := testProcessExecutor.FindByPid(pid)
	require.True(t, found, "Could not find Executable '%s' process with PID %d", exe.ObjectMeta.Name, pid)
	require.True(t, slices.Contains(pe.Cmd.Env, "SVC_PORT=7733"), "Port was not injected into the Executable '%s' process environment variables. The process environment variables are: %v", exe.ObjectMeta.Name, pe.Cmd.Env)

	t.Logf("Ensure the Status.EffectiveEnv for Executable '%s' contains the injected port...", exe.ObjectMeta.Name)
	updatedExe := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exe), func(currentExe *apiv1.Executable) (bool, error) {
		return len(currentExe.Status.EffectiveEnv) > 0, nil
	})
	effectiveEnv := slices.Map[apiv1.EnvVar, string](updatedExe.Status.EffectiveEnv, func(v apiv1.EnvVar) string {
		return fmt.Sprintf("%s=%s", v.Name, v.Value)
	})
	require.True(t, slices.Contains(effectiveEnv, "SVC_PORT=7733"), "The Executable '%s' effective environment does not contain expected port information. The effective environment is %v", exe.ObjectMeta.Name, effectiveEnv)

	t.Logf("Ensure service exposed by Executable '%s' gets to Ready state...", exe.ObjectMeta.Name)
	waitServiceReady(t, ctx, &svc)
}

// Verify ports are injected into Executable environment variables via portForServing template function.
// Service ports are automatically allocated.
func TestExecutableServingPortAllocatedAndInjected(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-executable-serving-port-allocated-injected-env-var-svc",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol: apiv1.TCP,
			Address:  "127.22.33.45",
			Port:     7740,
		},
	}

	t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
	err := client.Create(ctx, &svc)
	require.NoError(t, err, "Could not create the Service")

	exe := apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-executable-serving-port-allocated-injected-env-var",
			Namespace: metav1.NamespaceNone,
			// No address and no port information
			Annotations: map[string]string{"service-producer": fmt.Sprintf(`[{"serviceName":"%s"}]`, svc.ObjectMeta.Name)},
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/test-executable-serving-port-allocated-injected-env-var",
			Env: []apiv1.EnvVar{
				{
					Name:  "SVC_PORT",
					Value: fmt.Sprintf(`{{- portForServing "%s" -}}`, svc.ObjectMeta.Name),
				},
			},
		},
	}

	t.Logf("Creating Executable '%s' that is producing the Service '%s'...", exe.ObjectMeta.Name, svc.ObjectMeta.Name)
	err = client.Create(ctx, &exe)
	require.NoError(t, err, "Could not create an Executable")

	t.Logf("Ensure Executable '%s' is running...", exe.ObjectMeta.Name)
	pid, err := ensureProcessRunning(ctx, exe.Spec.ExecutablePath)
	require.NoError(t, err, "Process could not be started")

	t.Logf("Ensure the process for Executable '%s' is running...", exe.ObjectMeta.Name)
	pe, found := testProcessExecutor.FindByPid(pid)
	require.True(t, found, "Could not find the Executable '%s' process with PID %d", exe.ObjectMeta.Name, pid)

	// Validate the injected port
	matches := testutil.FindAllMatching(pe.Cmd.Env, regexp.MustCompile(`SVC_PORT=(\d+)`))
	require.Len(t, matches, 1, "Port was not injected into the Executable '%s' process environment variables. The process environment variables are: %v", exe.ObjectMeta.Name, pe.Cmd.Env)
	port, err := strconv.ParseInt(matches[0][1], 10, 32)
	require.NoError(t, err, "The injected port '%s' is not a valid integer", matches[0][1])
	require.Greater(t, int32(port), int32(0), "The injected port '%d' is not a valid port number", port)
	require.Less(t, int32(port), int32(65536), "The injected port '%d' is not a valid port number", port)

	t.Logf("Ensure that the Endpoint for the Service '%s' and Executable '%s' is created...", svc.ObjectMeta.Name, exe.ObjectMeta.Name)
	executableEndpoint := waitEndpointExists(t, ctx, func(e *apiv1.Endpoint) (bool, error) {
		forCorrectService := e.Spec.ServiceName == svc.ObjectMeta.Name && e.Spec.ServiceNamespace == svc.ObjectMeta.Namespace
		controllerOwner := metav1.GetControllerOf(e)
		hasOwner := controllerOwner != nil && controllerOwner.Name == exe.ObjectMeta.Name && controllerOwner.Kind == "Executable"
		return forCorrectService && hasOwner, nil
	})
	require.Equal(t, int32(port), executableEndpoint.Spec.Port, "The port injected into the Executable '%s' process environment variables (%d) does not match the port of the Endpoint (%d)", exe.ObjectMeta.Name, port, executableEndpoint.Spec.Port)

	t.Logf("Ensure the Status.EffectiveEnv for Executable '%s' contains the injected port...", exe.ObjectMeta.Name)
	updatedExe := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exe), func(currentExe *apiv1.Executable) (bool, error) {
		return len(currentExe.Status.EffectiveEnv) > 0, nil
	})
	effectiveEnv := slices.Map[apiv1.EnvVar, string](updatedExe.Status.EffectiveEnv, func(v apiv1.EnvVar) string {
		return fmt.Sprintf("%s=%s", v.Name, v.Value)
	})
	expectedEnvVar := fmt.Sprintf("SVC_PORT=%d", port)
	require.True(t, slices.Contains(effectiveEnv, expectedEnvVar), "The Executable '%s' effective environment does not contain expected port information. The effective environemtn is %v", exe.ObjectMeta.Name, effectiveEnv)

	t.Logf("Ensure service exposed by Executable '%s' gets to Ready state...", exe.ObjectMeta.Name)
	waitServiceReady(t, ctx, &svc)
}

// Verify ports are injected into Executable using startup parameters and portForServing template function.
// Specific port to use is indicated via service-producer annotation.
func TestExecutableServingPortInjectedViaStartupParameter(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-executable-serving-port-injected-startup-param-svc",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol: apiv1.TCP,
			Address:  "127.22.33.66",
			Port:     7745,
		},
	}

	t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
	err := client.Create(ctx, &svc)
	require.NoError(t, err, "Could not create the Service")

	exe := apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-executable-serving-port-injected-startup-param",
			Namespace:   metav1.NamespaceNone,
			Annotations: map[string]string{"service-producer": fmt.Sprintf(`[{"serviceName":"%s","address":"127.0.0.1","port":7746}]`, svc.ObjectMeta.Name)},
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/test-executable-serving-port-injected-startup-param",
			Args: []string{
				"--port",
				fmt.Sprintf(`{{- portForServing "%s" -}}`, svc.ObjectMeta.Name),
			},
		},
	}

	t.Logf("Creating Executable '%s' that is producing the Service '%s'...", exe.ObjectMeta.Name, svc.ObjectMeta.Name)
	err = client.Create(ctx, &exe)
	require.NoError(t, err, "Could not create Executable '%s'", exe.ObjectMeta.Name)

	t.Logf("Ensure Executable '%s' is running...", exe.ObjectMeta.Name)
	pid, err := ensureProcessRunning(ctx, exe.Spec.ExecutablePath)
	require.NoError(t, err, "Process for Executable '%s' could not be started", exe.ObjectMeta.Name)

	// Note: arguments for the process follow C/Unix convention, that is, args[0] is the path to the program.

	t.Logf("Ensure the Executable '%s' process got the service port injected...", exe.ObjectMeta.Name)
	pe, found := testProcessExecutor.FindByPid(pid)
	require.True(t, found, "Could not find Executable '%s' process with PID %d", exe.ObjectMeta.Name, pid)
	require.Equal(t, "7746", pe.Cmd.Args[2], "Port was not injected into the Executable '%s' process startup parameters. The process startup parameters are: %v", exe.ObjectMeta.Name, pe.Cmd.Args)

	t.Logf("Ensure the Status.EffectiveArgs for Executable '%s' contains the injected port...", exe.ObjectMeta.Name)
	updatedExe := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exe), func(currentExe *apiv1.Executable) (bool, error) {
		return len(currentExe.Status.EffectiveArgs) == 2, nil
	})
	require.Equal(t, updatedExe.Status.EffectiveArgs[1], "7746", "The Executable '%s' startup parameters do not include expected port. The startup parameters are %v", exe.ObjectMeta.Name, updatedExe.Status.EffectiveArgs)

	t.Logf("Ensure service exposed by Executable '%s' gets to Ready state...", exe.ObjectMeta.Name)
	waitServiceReady(t, ctx, &svc)
}

// Verify ports are injected into Executable using startup parameters and portForServing template function.
// Service port is automatically allocated.
func TestExecutableServingPortAllocatedInjectedViaStartupParameter(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-executable-serving-port-allocated-injected-startup-params-svc",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol: apiv1.TCP,
			Address:  "127.22.31.31",
			Port:     7492,
		},
	}

	t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
	err := client.Create(ctx, &svc)
	require.NoError(t, err, "Could not create the Service")

	exe := apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-executable-serving-port-allocated-injected-startup-param",
			Namespace: metav1.NamespaceNone,
			// No address and no port information
			Annotations: map[string]string{"service-producer": fmt.Sprintf(`[{"serviceName":"%s"}]`, svc.ObjectMeta.Name)},
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/test-executable-serving-port-allocated-injected-startup-param",
			Args: []string{
				"--port",
				fmt.Sprintf(`{{- portForServing "%s" -}}`, svc.ObjectMeta.Name),
			},
		},
	}

	t.Logf("Creating Executable '%s' that is producing the Service '%s'...", exe.ObjectMeta.Name, svc.ObjectMeta.Name)
	err = client.Create(ctx, &exe)
	require.NoError(t, err, "Could not create Executable '%s'", exe.ObjectMeta.Name)

	t.Logf("Ensure Executable '%s' is running...", exe.ObjectMeta.Name)
	pid, err := ensureProcessRunning(ctx, exe.Spec.ExecutablePath)
	require.NoError(t, err, "Process for Executable '%s' could not be started", exe.ObjectMeta.Name)

	t.Logf("Ensure the process for Executable '%s' got the service port injected...", exe.ObjectMeta.Name)
	pe, found := testProcessExecutor.FindByPid(pid)
	require.True(t, found, "Could not find the Executable '%s' process with PID %d", exe.ObjectMeta.Name, pid)

	// Validate the injected port
	// Note: arguments for the process follow C/Unix convention, that is, args[0] is the path to the program.
	require.Len(t, pe.Cmd.Args, 3, "Expected 3 startup parameters for the Executable '%s' process. The process startup parameters are: %v", exe.ObjectMeta.Name, pe.Cmd.Args)
	portStr := pe.Cmd.Args[2]
	port, err := strconv.ParseInt(portStr, 10, 32)
	require.NoError(t, err, "The injected port '%s' is not a valid integer", portStr)
	require.Greater(t, int32(port), int32(0), "The injected port '%d' is not a valid port number", port)
	require.Less(t, int32(port), int32(65536), "The injected port '%d' is not a valid port number", port)

	t.Logf("Ensure that the Endpoint for the Service '%s' and Executable '%s' is created...", svc.ObjectMeta.Name, exe.ObjectMeta.Name)
	executableEndpoint := waitEndpointExists(t, ctx, func(e *apiv1.Endpoint) (bool, error) {
		forCorrectService := e.Spec.ServiceName == svc.ObjectMeta.Name && e.Spec.ServiceNamespace == svc.ObjectMeta.Namespace
		controllerOwner := metav1.GetControllerOf(e)
		hasOwner := controllerOwner != nil && controllerOwner.Name == exe.ObjectMeta.Name && controllerOwner.Kind == "Executable"
		return forCorrectService && hasOwner, nil
	})
	require.Equal(t, int32(port), executableEndpoint.Spec.Port, "The port injected into the Executable '%s' (%d) does not match the port of the Endpoint (%d)", exe.ObjectMeta.Name, port, executableEndpoint.Spec.Port)

	t.Logf("Ensure the Status.EffectiveArgs for Executable '%s' contains the injected port...", exe.ObjectMeta.Name)
	updatedExe := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exe), func(currentExe *apiv1.Executable) (bool, error) {
		return len(currentExe.Status.EffectiveArgs) == 2, nil
	})
	require.Equal(t, updatedExe.Status.EffectiveArgs[1], portStr, "The Executable '%s' startup parameters do not include expected port. The startup parameters are %v", exe.ObjectMeta.Name, updatedExe.Status.EffectiveArgs)

	t.Logf("Ensure service exposed by Executable '%s' gets to Ready state...", exe.ObjectMeta.Name)
	waitServiceReady(t, ctx, &svc)
}

// Verify ports are injected into Executable using a combination of startup parameters and environment variables.
// Some ports are pre-determined by service-producer annotation, others are auto-allocated.
func TestExecutableMultipleServingPortsInjected(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const IPAddr = "127.22.132.10"
	services := map[string]apiv1.Service{
		"svc-a": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-executable-multiple-serving-ports-injected-svc-a",
				Namespace: metav1.NamespaceNone,
			},
			Spec: apiv1.ServiceSpec{
				Protocol: apiv1.TCP,
				Address:  IPAddr,
				Port:     11740,
			},
		},
		"svc-b": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-executable-multiple-serving-ports-injected-svc-b",
				Namespace: metav1.NamespaceNone,
			},
			Spec: apiv1.ServiceSpec{
				Protocol: apiv1.TCP,
				Address:  IPAddr,
				Port:     11741,
			},
		},
		"svc-c": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-executable-multiple-serving-ports-injected-svc-c",
				Namespace: metav1.NamespaceNone,
			},
			Spec: apiv1.ServiceSpec{
				Protocol: apiv1.TCP,
				Address:  IPAddr,
				Port:     11742,
			},
		},
		"svc-d": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-executable-multiple-serving-ports-injected-svc-d",
				Namespace: metav1.NamespaceNone,
			},
			Spec: apiv1.ServiceSpec{
				Protocol: apiv1.TCP,
				Address:  IPAddr,
				Port:     11743,
			},
		},
	}

	for _, svc := range services {
		t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
		err := client.Create(ctx, &svc)
		require.NoError(t, err, "Could not create Service '%s'", svc.ObjectMeta.Name)
	}

	const svcAExpectedPort = 11750
	const svcBExpectedPort = 11751
	var spAnn strings.Builder
	spAnn.WriteString("[")

	// Use specific port, inject via env var
	spAnn.WriteString(fmt.Sprintf(`{"serviceName":"%s","address":"%s","port":%d}`, services["svc-a"].ObjectMeta.Name, IPAddr, svcAExpectedPort))
	spAnn.WriteString(",")

	// Use specific port, inject via startup parameter
	spAnn.WriteString(fmt.Sprintf(`{"serviceName":"%s","address":"%s","port":%d}`, services["svc-b"].ObjectMeta.Name, IPAddr, svcBExpectedPort))
	spAnn.WriteString(",")

	// Use auto-allocated port, inject via env var
	spAnn.WriteString(fmt.Sprintf(`{"serviceName":"%s"}`, services["svc-c"].ObjectMeta.Name))
	spAnn.WriteString(",")

	// Use auto-allocated port, inject via startup parameter
	spAnn.WriteString(fmt.Sprintf(`{"serviceName":"%s"}`, services["svc-d"].ObjectMeta.Name))

	spAnn.WriteString("]")

	exe := apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-executable-multiple-serving-ports-injected",
			Namespace:   metav1.NamespaceNone,
			Annotations: map[string]string{"service-producer": spAnn.String()},
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/test-executable-multiple-serving-ports-injected",
			Env: []apiv1.EnvVar{
				{
					Name:  "SVC_A_PORT",
					Value: fmt.Sprintf(`{{- portForServing "%s" -}}`, services["svc-a"].ObjectMeta.Name),
				},
				{
					Name:  "SVC_C_PORT",
					Value: fmt.Sprintf(`{{- portForServing "%s" -}}`, services["svc-c"].ObjectMeta.Name),
				},
			},
			Args: []string{
				fmt.Sprintf(`--svc-b-port={{- portForServing "%s" -}}`, services["svc-b"].ObjectMeta.Name),
				fmt.Sprintf(`--svc-d-port={{- portForServing "%s" -}}`, services["svc-d"].ObjectMeta.Name),
			},
		},
	}

	t.Logf("Creating Executable '%s'...", exe.ObjectMeta.Name)
	err := client.Create(ctx, &exe)
	require.NoError(t, err, "Could not create Executable '%s'", exe.ObjectMeta.Name)

	t.Logf("Ensure Executable '%s' is running...", exe.ObjectMeta.Name)
	pid, err := ensureProcessRunning(ctx, exe.Spec.ExecutablePath)
	require.NoError(t, err, "Process for Executable '%s' could not be started", exe.ObjectMeta.Name)

	t.Logf("Ensure the process for Executable '%s' is running...", exe.ObjectMeta.Name)
	pe, found := testProcessExecutor.FindByPid(pid)
	require.True(t, found, "Could not find the Executable '%s' process with PID %d", exe.ObjectMeta.Name, pid)

	// VALIDATION PART 1: validate ports injected via environment variables

	t.Logf("Ensure the Status.EffectiveEnv for Executable '%s' contains the injected ports...", exe.ObjectMeta.Name)
	updatedExe := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exe), func(currentExe *apiv1.Executable) (bool, error) {
		return len(currentExe.Status.EffectiveEnv) > 0, nil
	})
	effectiveEnv := slices.Map[apiv1.EnvVar, string](updatedExe.Status.EffectiveEnv, func(v apiv1.EnvVar) string {
		return fmt.Sprintf("%s=%s", v.Name, v.Value)
	})
	expectedEnvVar := fmt.Sprintf("SVC_A_PORT=%d", svcAExpectedPort)
	require.True(t, slices.Contains(effectiveEnv, expectedEnvVar), "The Executable '%s' effective environment does not contain expected port information for service A. The effective environemtn is %v", exe.ObjectMeta.Name, effectiveEnv)
	svcCEnvVarRegex := regexp.MustCompile(`SVC_C_PORT=(\d+)`)
	require.True(
		t,
		slices.Any(effectiveEnv, func(s string) bool {
			return svcCEnvVarRegex.MatchString(s)
		}),
		"The Executable '%s' effective environment does not contain expected port information for service C. The effective environemtn is %v",
		exe.ObjectMeta.Name,
		effectiveEnv,
	)

	validateAndParsePortStr := func(portStr string) int32 {
		port, parsingErr := strconv.ParseInt(portStr, 10, 32)
		require.NoError(t, parsingErr, "The injected port '%s' is not a valid integer", portStr)
		require.Greater(t, int32(port), int32(0), "The injected port '%d' is not a valid port number", port)
		require.Less(t, int32(port), int32(65536), "The injected port '%d' is not a valid port number", port)
		return int32(port)
	}

	matches := testutil.FindAllMatching(pe.Cmd.Env, regexp.MustCompile(`SVC_[AC]_PORT=(\d+)`))
	require.Len(t, matches, 2, "Expected ports not injected into the Executable '%s' process environment variables. The process environment variables are: %v", exe.ObjectMeta.Name, pe.Cmd.Env)
	var svcAPortStr, svcCPortStr string
	if strings.HasPrefix(matches[0][0], "SVC_A_PORT") {
		svcAPortStr = matches[0][1]
		svcCPortStr = matches[1][1]
	} else {
		svcAPortStr = matches[1][1]
		svcCPortStr = matches[0][1]
	}

	svcAPort := validateAndParsePortStr(svcAPortStr)
	require.Equal(t, int32(svcAExpectedPort), svcAPort)
	svcCPort := validateAndParsePortStr(svcCPortStr) // Service C port is auto-allocated, so as long as it parses OK, we are good.

	// VALIDATION PART 2: validate ports injected via startup parameters

	svcBPortExpectedArg := fmt.Sprintf("--svc-b-port=%d", svcBExpectedPort)
	require.Equal(
		t,
		svcBPortExpectedArg,
		pe.Cmd.Args[1],
		"Port for service B was not injected into the Executable '%s' process startup parameters. The process startup parameters are: %v",
		exe.ObjectMeta.Name,
		pe.Cmd.Args,
	)

	svcDPortArgRegex := regexp.MustCompile(`--svc-d-port=(\d+)`)
	matches = testutil.FindAllMatching(pe.Cmd.Args, svcDPortArgRegex)
	require.Len(t, matches, 1, "Port for service D was not injected into the Executable '%s' process startup parameters. The process startup parameters are: %v", exe.ObjectMeta.Name, pe.Cmd.Args)
	svcDPortStr := matches[0][1]
	svcDPort := validateAndParsePortStr(svcDPortStr)

	t.Logf("Ensure the Status.EffectiveArgs for Executable '%s' contains the injected port...", exe.ObjectMeta.Name)
	require.Equal(t, updatedExe.Status.EffectiveArgs[0], svcBPortExpectedArg, "The Executable '%s' startup parameters do not include expected port for service B. The startup parameters are %v", exe.ObjectMeta.Name, updatedExe.Status.EffectiveArgs)
	require.Equal(t, fmt.Sprintf("--svc-d-port=%d", svcDPort), updatedExe.Status.EffectiveArgs[1], "The Executable '%s' startup parameters do not include expected port for service D. The startup parameters are %v", exe.ObjectMeta.Name, updatedExe.Status.EffectiveArgs)

	// VALIDATION PART 3: validate endpoints

	t.Logf("Ensure that all expected Endpoints for Executable '%s' are created...", exe.ObjectMeta.Name)
	endpoints := slices.MapConcurrent[apiv1.Service, *apiv1.Endpoint](maps.Values(services), func(svc apiv1.Service) *apiv1.Endpoint {
		executableEndpoint := waitEndpointExists(t, ctx, func(e *apiv1.Endpoint) (bool, error) {
			forCorrectService := e.Spec.ServiceName == svc.ObjectMeta.Name && e.Spec.ServiceNamespace == svc.ObjectMeta.Namespace
			controllerOwner := metav1.GetControllerOf(e)
			hasOwner := controllerOwner != nil && controllerOwner.Name == exe.ObjectMeta.Name && controllerOwner.Kind == "Executable"
			return forCorrectService && hasOwner, nil
		})
		return executableEndpoint
	}, slices.MaxConcurrency)
	std_slices.SortFunc(endpoints, func(e1, e2 *apiv1.Endpoint) int {
		if e1.Spec.ServiceName < e2.Spec.ServiceName {
			return -1
		} else if e1.Spec.ServiceName > e2.Spec.ServiceName {
			return 1
		} else {
			return 0
		}
	})

	expectedPorts := []int32{svcAPort, svcBExpectedPort, svcCPort, svcDPort}
	actualPorts := slices.Map[*apiv1.Endpoint, int32](endpoints, func(e *apiv1.Endpoint) int32 { return e.Spec.Port })
	require.ElementsMatch(t, expectedPorts, actualPorts, "Some ports used by Endpoints of Executable '%s' are not matching what was injected into the Executable", exe.ObjectMeta.Name)

	t.Logf("Ensure services exposed by Executable '%s' gets to Ready state...", exe.ObjectMeta.Name)
	for _, svc := range services {
		waitServiceReady(t, ctx, &svc)
	}
}

// Verify ports are injected into Executable environment variables via portFor template function.
func TestClientExecutablePortForInjected(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	var expectedPort int32 = 7750

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-client-service-port-for-injected",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol: apiv1.TCP,
			Address:  "127.22.33.45",
			Port:     expectedPort,
		},
	}

	t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
	err := client.Create(ctx, &svc)
	require.NoError(t, err, "Could not create the Service")

	exe := apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-client-executable-port-for-injected",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/test-client-executable-port-for-injected",
			Env: []apiv1.EnvVar{
				{
					Name:  "UPSTREAM_SVC_PORT",
					Value: fmt.Sprintf(`{{- portFor "%s" -}}`, svc.ObjectMeta.Name),
				},
			},
		},
	}

	t.Logf("Creating Executable '%s' that is producing the Service '%s'...", exe.ObjectMeta.Name, svc.ObjectMeta.Name)
	err = client.Create(ctx, &exe)
	require.NoError(t, err, "Could not create an Executable")

	t.Logf("Ensure Executable '%s' is running...", exe.ObjectMeta.Name)
	pid, err := ensureProcessRunning(ctx, exe.Spec.ExecutablePath)
	require.NoError(t, err, "Process could not be started", pid)

	t.Logf("Ensure the Executable.Status.EffectiveEnv contains the injected port...")
	updatedExe := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exe), func(currentExe *apiv1.Executable) (bool, error) {
		return len(currentExe.Status.EffectiveEnv) > 0, nil
	})
	effectiveEnv := slices.Map[apiv1.EnvVar, string](updatedExe.Status.EffectiveEnv, func(v apiv1.EnvVar) string {
		return fmt.Sprintf("%s=%s", v.Name, v.Value)
	})
	expectedEnvVar := fmt.Sprintf("UPSTREAM_SVC_PORT=%d", expectedPort)
	require.True(t, slices.Contains(effectiveEnv, expectedEnvVar), "The Executable effective environment does not contain expected port information. The effective environment is %v", effectiveEnv)
}

func TestExecutableTemplatedEnvVarsInjected(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	exe := apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-executable-templated-env-vars-injected",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/test-executable-templated-env-vars-injected",
			Env: []apiv1.EnvVar{
				{
					Name:  "EXE_NAME",
					Value: `{{- .Name -}}`,
				},
				{
					Name:  "EXE_SPEC_EXECUTABLE_PATH",
					Value: `{{- .Spec.ExecutablePath -}}`,
				},
			},
		},
	}

	t.Logf("Creating Executable '%s' that has injected env vars...", exe.ObjectMeta.Name)
	err := client.Create(ctx, &exe)
	require.NoError(t, err, "Could not create an Executable")

	t.Logf("Ensure Executable '%s' is running...", exe.ObjectMeta.Name)
	pid, err := ensureProcessRunning(ctx, exe.Spec.ExecutablePath)
	require.NoError(t, err, "Process could not be started")

	t.Logf("Ensure the Executable process got templated env vars injected...")
	pe, found := testProcessExecutor.FindByPid(pid)
	require.True(t, found, "Could not find the process with PID %d", pid)

	// Validate the templated env vars
	// Validate EXE_NAME
	matches := testutil.FindAllMatching(pe.Cmd.Env, regexp.MustCompile(`EXE_NAME=(.+)`))
	require.Len(t, matches, 1, "EXE_NAME was not injected into the Executable process environment variables. The process environment variables are: %v", pe.Cmd.Env)

	// Validate EXE_SPEC_EXECUTABLE_PATH
	matches = testutil.FindAllMatching(pe.Cmd.Env, regexp.MustCompile(`EXE_SPEC_EXECUTABLE_PATH=(.+)`))
	require.Len(t, matches, 1, "EXE_SPEC_EXECUTABLE_PATH was not injected into the Executable process environment variables. The process environment variables are: %v", pe.Cmd.Env)

	t.Logf("Ensure the Executable.Status.EffectiveEnv contains the injected templated env vars...")
	updatedExe := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exe), func(currentExe *apiv1.Executable) (bool, error) {
		return len(currentExe.Status.EffectiveEnv) > 0, nil
	})
	effectiveEnv := slices.Map[apiv1.EnvVar, string](updatedExe.Status.EffectiveEnv, func(v apiv1.EnvVar) string {
		return fmt.Sprintf("%s=%s", v.Name, v.Value)
	})
	expectedEnvVar := fmt.Sprintf("EXE_NAME=%s", updatedExe.Name)
	require.True(t, slices.Contains(effectiveEnv, expectedEnvVar), "The Executable effective environment does not contain expected EXE_NAME. The effective environment is %v", effectiveEnv)

	expectedEnvVar2 := fmt.Sprintf("EXE_SPEC_EXECUTABLE_PATH=%s", updatedExe.Spec.ExecutablePath)
	require.True(t, slices.Contains(effectiveEnv, expectedEnvVar2), "The Executable effective environment does not contain expected EXE_SPEC_EXECUTABLE_PATH. The effective environment is %v", effectiveEnv)
}

// Verify that envrioment variables are handled according to OS conventions:
// in a case-insensitive manner on Windows, and in a case-sensitive manner everywhere else.
func TestExecutableEnvironmentVariablesHandling(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	exe := apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-executable-environment-variables-handling",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/test-executable-environment-variables-handling",
			Env: []apiv1.EnvVar{
				{
					Name:  "AlphaVar",
					Value: `alpha var pascal case`,
				},
				{
					Name:  "betaVar",
					Value: `beta var camel case`,
				},
				{
					Name:  "alphavar",
					Value: `alpha var all lowercase`,
				},
			},
		},
	}

	t.Logf("Creating Executable '%s'...", exe.ObjectMeta.Name)
	err := client.Create(ctx, &exe)
	require.NoError(t, err, "Could not create an Executable '%s'", exe.ObjectMeta.Name)

	t.Logf("Ensure Status.EffectiveEnv for Executable '%s' contains expected env vars...", exe.ObjectMeta.Name)
	updatedExe := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exe), func(currentExe *apiv1.Executable) (bool, error) {
		return len(currentExe.Status.EffectiveEnv) > 0, nil
	})

	if osutil.IsWindows() {
		// There should be just one "alpha var"
		require.Equal(t, 1, slices.LenIf(updatedExe.Status.EffectiveEnv, func(v apiv1.EnvVar) bool {
			return v.Name == "AlphaVar" || v.Name == "alphavar"
		}))
	} else {
		require.Equal(t, 2, slices.LenIf(updatedExe.Status.EffectiveEnv, func(v apiv1.EnvVar) bool {
			return v.Name == "AlphaVar" || v.Name == "alphavar"
		}))
	}
}

func TestExecutableServingAddressInjected(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-executable-serving-address-injected"

	const IPAddr = "127.63.29.2"
	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-svc",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol: apiv1.TCP,
			Address:  IPAddr,
			Port:     26010,
		},
	}

	t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
	err := client.Create(ctx, &svc)
	require.NoError(t, err, "Could not create Service '%s'", svc.ObjectMeta.Name)

	var spAnn strings.Builder
	spAnn.WriteString("[")
	spAnn.WriteString(fmt.Sprintf(`{"serviceName":"%s", "address": "%s", "port": 26011}`, svc.ObjectMeta.Name, IPAddr))
	spAnn.WriteString("]")

	server := apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testName + "-server",
			Namespace:   metav1.NamespaceNone,
			Annotations: map[string]string{"service-producer": spAnn.String()},
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/" + testName + "-server",
			Env: []apiv1.EnvVar{
				{
					Name:  "ADDRESS",
					Value: fmt.Sprintf(`{{- addressForServing "%s" -}}`, svc.ObjectMeta.Name),
				},
				{
					Name:  "PORT",
					Value: fmt.Sprintf(`{{- portForServing "%s" -}}`, svc.ObjectMeta.Name),
				},
			},
		},
	}

	t.Logf("Creating Executable '%s'...", server.ObjectMeta.Name)
	err = client.Create(ctx, &server)
	require.NoError(t, err, "Could not create Executable '%s'", server.ObjectMeta.Name)

	t.Logf("Ensure the Status.EffectiveEnv for Executable '%s' contains the injected address...", server.ObjectMeta.Name)
	updatedExe := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&server), func(currentExe *apiv1.Executable) (bool, error) {
		return len(currentExe.Status.EffectiveEnv) > 0, nil
	})
	effectiveEnv := slices.Map[apiv1.EnvVar, string](updatedExe.Status.EffectiveEnv, func(v apiv1.EnvVar) string {
		return fmt.Sprintf("%s=%s", v.Name, v.Value)
	})
	expectedEnvVar := fmt.Sprintf("ADDRESS=%s", IPAddr)
	require.True(t, slices.Contains(effectiveEnv, expectedEnvVar), "The Executable '%s' effective environment does not contain expected address information. The effective environemtn is %v", server.ObjectMeta.Name, effectiveEnv)

	consumer := apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-consumer",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/" + testName + "-consumer",
			Args: []string{
				fmt.Sprintf(`--server-address={{- addressFor "%s" -}}`, svc.ObjectMeta.Name),
			},
		},
	}

	t.Logf("Creating Executable '%s'...", consumer.ObjectMeta.Name)
	err = client.Create(ctx, &consumer)
	require.NoError(t, err, "Could not create Executable '%s'", consumer.ObjectMeta.Name)

	t.Logf("Ensure Executable '%s' is running...", consumer.ObjectMeta.Name)
	pid, err := ensureProcessRunning(ctx, consumer.Spec.ExecutablePath)
	require.NoError(t, err, "Process for Executable '%s' could not be started", consumer.ObjectMeta.Name)

	t.Logf("Ensure the process for Executable '%s' is running...", consumer.ObjectMeta.Name)
	pe, found := testProcessExecutor.FindByPid(pid)
	require.True(t, found, "Could not find the Executable '%s' process with PID %d", consumer.ObjectMeta.Name, pid)

	serverAddressExpectedArg := fmt.Sprintf("--server-address=%s", IPAddr)
	require.Equal(
		t,
		serverAddressExpectedArg,
		pe.Cmd.Args[1],
		"Address for service '%s' was not injected into the Executable '%s' process startup parameters. The process startup parameters are: %v",
		svc.ObjectMeta.Name,
		consumer.ObjectMeta.Name,
		pe.Cmd.Args,
	)

	t.Logf("Ensure service exposed by Executable '%s' gets to Ready state...", server.ObjectMeta.Name)
	waitServiceReady(t, ctx, &svc)
}

// Verify that all changes to the Executable status made by IDE executable runner are refleced in the Executable status.
func TestExecutableStatusUpdatedByIdeRunner(t *testing.T) {
	type testcase struct {
		description    string
		exe            *apiv1.Executable
		performStartup func(*ctrl_testutil.TestIdeRun)
		verifyExe      func(*apiv1.Executable) (bool, error)
	}

	const successfulStartExeName = "test-executable-status-updated-by-ide-runner-successful-start"
	const failedStartExeName = "test-executable-status-updated-by-ide-runner-failed-start"

	randomPid, pidCreationErr := process.IntToPidT(rand.Intn(60000) + 1)
	require.NoError(t, pidCreationErr, "Could not generate random PID for successful run")
	desiredSuccessfulExeStatus := apiv1.ExecutableStatus{
		PID:        (*int64)(&randomPid),
		State:      apiv1.ExecutableStateRunning,
		StdOutFile: "stdout/log/for/" + successfulStartExeName,
		StdErrFile: "stderr/log/for/" + successfulStartExeName,
	}
	desiredFailedExeStatus := apiv1.ExecutableStatus{
		PID:   nil,
		State: apiv1.ExecutableStateFailedToStart,
	}

	verifyExeStatus := func(currentExe *apiv1.Executable, desiredStatus *apiv1.ExecutableStatus) (bool, error) {
		hasPID := (currentExe.Status.PID == nil && desiredStatus.PID == nil) ||
			(currentExe.Status.PID != nil && *currentExe.Status.PID == *desiredStatus.PID)

		stateMatches := currentExe.Status.State == desiredStatus.State

		// metav1.Time uses RFC 3339 format and truncates the time to seconds during serialization.
		// This is why we only check that the desired and actual timestamp are within a second of each other.
		hasStartupTimestamp := !currentExe.Status.StartupTimestamp.IsZero() && testutil.Within(currentExe.Status.StartupTimestamp.Time, desiredStatus.StartupTimestamp.Time, time.Second)

		hasFinishTimestamp := testutil.Within(currentExe.Status.FinishTimestamp.Time, desiredStatus.FinishTimestamp.Time, time.Second)

		hasStdOutFile := currentExe.Status.StdOutFile == desiredStatus.StdOutFile
		hasStdErrFile := currentExe.Status.StdErrFile == desiredStatus.StdErrFile

		return hasPID && stateMatches && hasStartupTimestamp && hasFinishTimestamp && hasStdOutFile && hasStdErrFile, nil
	}

	testcases := []testcase{
		{
			description: "successful start",
			exe: &apiv1.Executable{
				ObjectMeta: metav1.ObjectMeta{
					Name:      successfulStartExeName,
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/" + successfulStartExeName,
					ExecutionType:  apiv1.ExecutionTypeIDE,
				},
			},
			performStartup: func(r *ctrl_testutil.TestIdeRun) {
				r.PID = randomPid
				desiredSuccessfulExeStatus.StartupTimestamp = metav1.NewTime(r.StartedAt)
				desiredSuccessfulExeStatus.DeepCopyInto(r.Status)
			},
			verifyExe: func(currentExe *apiv1.Executable) (bool, error) {
				return verifyExeStatus(currentExe, &desiredSuccessfulExeStatus)
			},
		},
		{
			description: "failed start",
			exe: &apiv1.Executable{
				ObjectMeta: metav1.ObjectMeta{
					Name:      failedStartExeName,
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/" + failedStartExeName,
					ExecutionType:  apiv1.ExecutionTypeIDE,
				},
			},
			performStartup: func(r *ctrl_testutil.TestIdeRun) {
				r.PID = process.UnknownPID
				r.ID = controllers.UnknownRunID
				desiredFailedExeStatus.StartupTimestamp = metav1.NewTime(r.StartedAt)
				r.EndedAt = time.Now()
				desiredFailedExeStatus.FinishTimestamp = metav1.NewTime(r.EndedAt)
				desiredFailedExeStatus.DeepCopyInto(r.Status)
			},
			verifyExe: func(currentExe *apiv1.Executable) (bool, error) {
				return verifyExeStatus(currentExe, &desiredFailedExeStatus)
			},
		},
	}

	t.Parallel()

	for _, tc := range testcases {
		tc := tc

		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()

			if exeCreationErr := client.Create(ctx, tc.exe); exeCreationErr != nil {
				t.Fatalf("Could not create Executable '%s': %v", tc.exe.ObjectMeta.Name, exeCreationErr)
			}

			runID, runFindErr := findIdeRun(ctx, tc.exe.Spec.ExecutablePath)
			require.NoError(t, runFindErr, "Could not find IDE run for Executable '%s'", tc.exe.ObjectMeta.Name)

			runStartErr := ideRunner.SimulateRunStart(
				func(_ types.NamespacedName, r *ctrl_testutil.TestIdeRun) bool { return r.ID == runID },
				tc.performStartup,
			)
			require.NoError(t, runStartErr, "Could not simulate IDE run start for Executable '%s'", tc.exe.ObjectMeta.Name)

			t.Logf("Ensure the Status for Executable '%s' has been updated with data from the IDE runner...", tc.exe.ObjectMeta.Name)
			_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(tc.exe), tc.verifyExe)
		})
	}
}

// Verify that stdout and stderr logs can be captured in non-follow mode.
// The two sub-tests are verifying that logs can be captured when Executable is runnning,
// and when it has finished.
func TestExecutableLogsNonFollow(t *testing.T) {
	type testcase struct {
		description     string
		exeName         string
		finishExecution *concurrency.AutoResetEvent
		readyToReadLogs func(*apiv1.Executable) (bool, error)
	}

	const runningExeName = "test-executable-logs-non-follow-running"
	const finishedExeName = "test-executable-logs-non-follow-finished"

	testcases := []testcase{
		{
			description:     "running",
			exeName:         runningExeName,
			finishExecution: concurrency.NewAutoResetEvent(false), // Do not exit until told so
			readyToReadLogs: func(currentExe *apiv1.Executable) (bool, error) {
				return currentExe.Status.State == apiv1.ExecutableStateRunning, nil
			},
		},
		{
			description:     "finished",
			exeName:         finishedExeName,
			finishExecution: concurrency.NewAutoResetEvent(true), // Exit immediately
			readyToReadLogs: func(currentExe *apiv1.Executable) (bool, error) {
				return currentExe.Status.State == apiv1.ExecutableStateFinished, nil
			},
		},
	}

	t.Parallel()

	for _, tc := range testcases {
		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()

			// Ensure test cases will not block forever, but instead exit after context expires
			defer tc.finishExecution.Set()

			exe := apiv1.Executable{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1.GroupVersion.Version,
					Kind:       "Executable",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      tc.exeName,
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/" + tc.exeName,
				},
			}

			testProcessExecutor.InstallAutoExecution(ctrl_testutil.AutoExecution{
				Condition: ctrl_testutil.ProcessSearchCriteria{
					Command: []string{exe.Spec.ExecutablePath},
				},
				RunCommand: func(pe *ctrl_testutil.ProcessExecution) int32 {
					_, stdoutErr := pe.Cmd.Stdout.Write(osutil.WithNewline([]byte("Standard output log line 1")))
					require.NoError(t, stdoutErr, "Could not write to stdout of Executable '%s'", exe.ObjectMeta.Name)
					_, stderrErr := pe.Cmd.Stderr.Write(osutil.WithNewline([]byte("Standard error log line 1")))
					require.NoError(t, stderrErr, "Could not write to stderr of Executable '%s'", exe.ObjectMeta.Name)

					<-tc.finishExecution.Wait()
					return 0
				},
			})
			defer testProcessExecutor.RemoveAutoExecution(ctrl_testutil.ProcessSearchCriteria{
				Command: []string{exe.Spec.ExecutablePath},
			})

			t.Logf("Creating Executable '%s'...", exe.ObjectMeta.Name)
			err := client.Create(ctx, exe.DeepCopy())
			require.NoError(t, err, "Could not create Executable '%s'", exe.ObjectMeta.Name)

			t.Logf("Ensure Executable '%s' is in the expected state...", exe.ObjectMeta.Name)
			_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exe), tc.readyToReadLogs)

			t.Logf("Ensure logs for Executable '%s' can be captured...", exe.ObjectMeta.Name)
			opts := apiv1.LogOptions{
				Follow:     false,
				Source:     "stdout",
				Timestamps: false,
			}
			err = waitForObjectLogs(ctx, &exe, opts, [][]byte{[]byte("Standard output log line 1")}, nil)
			require.NoError(t, err)

			opts = apiv1.LogOptions{
				Follow:     false,
				Source:     "stderr",
				Timestamps: false,
			}
			err = waitForObjectLogs(ctx, &exe, opts, [][]byte{[]byte("Standard error log line 1")}, nil)
			require.NoError(t, err)
		})
	}
}

// Verify stdout and stderr logs can be captured in follow mode,
// The two sub-tests are verifying that logs can be captured when Executable is runnning,
// and when it has finished.
func TestExecutableLogsFollow(t *testing.T) {
	type testcase struct {
		description     string
		exeName         string
		startWriting    *concurrency.AutoResetEvent
		finishExecution *concurrency.AutoResetEvent
		readyToReadLogs func(*apiv1.Executable) (bool, error)
	}

	const runningExeName = "test-executable-logs-follow-running"
	const finishedExeName = "test-executable-logs-follow-finished"

	testcases := []testcase{
		{
			description: "running",
			exeName:     runningExeName,
			// In the running case we want the log stream to be open BEFORE any logs were written,
			// to ensure that all logs are captured and delivered.
			startWriting:    concurrency.NewAutoResetEvent(false),
			finishExecution: concurrency.NewAutoResetEvent(false),
			// Logs in follow mode should be captured even if the stream is open before Executable is running
			readyToReadLogs: func(currentExe *apiv1.Executable) (bool, error) {
				return true, nil
			},
		},
		{
			description: "finished",
			exeName:     finishedExeName,
			// In the "finished" case we want the log stream to be opened AFTER the logs were written
			// and the Executable finished running.
			startWriting:    concurrency.NewAutoResetEvent(true),
			finishExecution: concurrency.NewAutoResetEvent(true),
			readyToReadLogs: func(currentExe *apiv1.Executable) (bool, error) {
				return currentExe.Status.State == apiv1.ExecutableStateFinished, nil
			},
		},
	}

	lines := [][]byte{
		[]byte("Standard output log line 1"),
		[]byte("Standard output log line 2"),
		[]byte("Standard output log line 3"),
	}

	t.Parallel()

	for _, tc := range testcases {
		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()

			// Ensure test cases will not block forever, but instead exit after context expires
			defer tc.finishExecution.Set()
			defer tc.startWriting.Set()

			exe := apiv1.Executable{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1.GroupVersion.Version,
					Kind:       "Executable",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      tc.exeName,
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/" + tc.exeName,
				},
			}

			testProcessExecutor.InstallAutoExecution(ctrl_testutil.AutoExecution{
				Condition: ctrl_testutil.ProcessSearchCriteria{
					Command: []string{exe.Spec.ExecutablePath},
				},
				RunCommand: func(pe *ctrl_testutil.ProcessExecution) int32 {
					<-tc.startWriting.Wait()

					for i, line := range lines {
						_, stdoutErr := pe.Cmd.Stdout.Write(osutil.WithNewline(line))
						require.NoError(t, stdoutErr, "Could not write line %d to stdout of Executable '%s'", i, exe.ObjectMeta.Name)
					}

					<-tc.finishExecution.Wait()
					return 0
				},
			})
			defer testProcessExecutor.RemoveAutoExecution(ctrl_testutil.ProcessSearchCriteria{
				Command: []string{exe.Spec.ExecutablePath},
			})

			t.Logf("Creating Executable '%s'...", exe.ObjectMeta.Name)
			err := client.Create(ctx, exe.DeepCopy())
			require.NoError(t, err, "Could not create Executable '%s'", exe.ObjectMeta.Name)

			t.Logf("Ensure Executable '%s' is in the expected state...", exe.ObjectMeta.Name)
			_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exe), tc.readyToReadLogs)

			t.Logf("Start following Executable '%s' logs...", exe.ObjectMeta.Name)
			logsErrCh := make(chan error, 1)
			logStreamOpen := concurrency.NewAutoResetEvent(false)
			opts := apiv1.LogOptions{
				Follow:     true,
				Source:     "stdout",
				Timestamps: false,
			}
			go func() {
				// Run this in a separate goroutine to make sure we open the log stream before the Executable starts writing logs
				logsErrCh <- waitForObjectLogs(ctx, &exe, opts, lines, logStreamOpen)
			}()

			<-logStreamOpen.Wait()
			tc.startWriting.Set()
			err = <-logsErrCh
			require.NoError(t, err, "Could not follow logs for Executable '%s'", exe.ObjectMeta.Name)
		})
	}
}

// Verify that additional logs are reported in follow mode as soon as they are written
// This is similar to TestExecutableLogsFollow, but we write logs one line at a time, and verify each line separately,
// including measuring time-to-arrive.
func TestExecutableLogsFollowIncremental(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const exeName = "test-executable-logs-follow-incremental"

	exe := apiv1.Executable{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1.GroupVersion.Version,
			Kind:       "Executable",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      exeName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/" + exeName,
		},
	}

	type logLine struct {
		content []byte
		written time.Time
	}
	lines := []*logLine{
		{content: []byte("Standard output log line 1")},
		{content: []byte("Standard output log line 2")},
		{content: []byte("Standard output log line 3")},
	}
	// The lock is necessary to avoid race conditions when various parts of the test are reading and writing line timestamps
	linesLock := &sync.Mutex{}
	writeLine := concurrency.NewAutoResetEvent(false)

	testProcessExecutor.InstallAutoExecution(ctrl_testutil.AutoExecution{
		Condition: ctrl_testutil.ProcessSearchCriteria{
			Command: []string{exe.Spec.ExecutablePath},
		},
		RunCommand: func(pe *ctrl_testutil.ProcessExecution) int32 {
			for i, line := range lines {
				<-writeLine.Wait()
				linesLock.Lock()
				line.written = time.Now()
				linesLock.Unlock()
				_, stdoutErr := pe.Cmd.Stdout.Write(osutil.WithNewline(line.content))
				require.NoError(t, stdoutErr, "Could not write line %d to stdout of Executable '%s'", i, exe.ObjectMeta.Name)
			}
			return 0
		},
	})
	defer testProcessExecutor.RemoveAutoExecution(ctrl_testutil.ProcessSearchCriteria{
		Command: []string{exe.Spec.ExecutablePath},
	})

	t.Logf("Creating Executable '%s'...", exe.ObjectMeta.Name)
	err := client.Create(ctx, exe.DeepCopy())
	require.NoError(t, err, "Could not create Executable '%s'", exe.ObjectMeta.Name)

	// We might need to tweak this value if it turns that the test is unreliable on slow CI machines.
	const maxLogCaptureDelay time.Duration = 2 * time.Second

	t.Logf("Start following Executable '%s' logs...", exe.ObjectMeta.Name)
	opts := apiv1.LogOptions{
		Follow:     true,
		Source:     "stdout",
		Timestamps: false,
	}
	logStream, logStreamErr := openLogStream(ctx, &exe, opts, nil)
	require.NoError(t, logStreamErr, "Could not open log stream for Executable '%s'", exe.ObjectMeta.Name)

	scanner := bufio.NewScanner(usvc_io.NewContextReader(ctx, logStream, true /* leverageReadCloser */))
	for i, line := range lines {
		writeLine.Set()
		gotLine := scanner.Scan()
		require.True(t, gotLine, "Could not read line %d from log stream for Executable '%s', the reported error was %v", i, exe.ObjectMeta.Name, scanner.Err())
		linesLock.Lock()
		logCaptureDelay := time.Since(line.written)
		linesLock.Unlock()
		require.Equal(t, string(line.content), scanner.Text(), "Log line %d does not match expected content for Executable '%s'", i, exe.ObjectMeta.Name)
		require.Lessf(t, logCaptureDelay, maxLogCaptureDelay, "Log line %d took too long to arrive for Executable '%s'", i, exe.ObjectMeta.Name)
	}
}

// Verify that stdout and stderr logs can be timestamped.
func TestExecutableLogsTimestamped(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const exeName = "test-executable-logs-timestamped"

	exe := apiv1.Executable{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1.GroupVersion.Version,
			Kind:       "Executable",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      exeName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/" + exeName,
		},
	}

	testProcessExecutor.InstallAutoExecution(ctrl_testutil.AutoExecution{
		Condition: ctrl_testutil.ProcessSearchCriteria{
			Command: []string{exe.Spec.ExecutablePath},
		},
		RunCommand: func(pe *ctrl_testutil.ProcessExecution) int32 {
			_, stdoutErr := pe.Cmd.Stdout.Write(osutil.WithNewline([]byte("Standard output log line 1")))
			require.NoError(t, stdoutErr, "Could not write to stdout of Executable '%s'", exe.ObjectMeta.Name)
			_, stderrErr := pe.Cmd.Stderr.Write(osutil.WithNewline([]byte("Standard error log line 1")))
			require.NoError(t, stderrErr, "Could not write to stderr of Executable '%s'", exe.ObjectMeta.Name)
			return 0
		},
	})
	defer testProcessExecutor.RemoveAutoExecution(ctrl_testutil.ProcessSearchCriteria{
		Command: []string{exe.Spec.ExecutablePath},
	})

	t.Logf("Creating Executable '%s'...", exe.ObjectMeta.Name)
	err := client.Create(ctx, exe.DeepCopy())
	require.NoError(t, err, "Could not create Executable '%s'", exe.ObjectMeta.Name)

	t.Logf("Ensure logs for Executable '%s' can be captured...", exe.ObjectMeta.Name)
	opts := apiv1.LogOptions{
		Follow:     true,
		Source:     "stdout",
		Timestamps: true,
	}
	err = waitForObjectLogs(ctx, &exe, opts, [][]byte{[]byte(testutil.RFC3339MiliTimestampRegex + " " + "Standard output log line 1")}, nil)
	require.NoError(t, err)

	opts = apiv1.LogOptions{
		Follow:     true,
		Source:     "stderr",
		Timestamps: true,
	}
	err = waitForObjectLogs(ctx, &exe, opts, [][]byte{[]byte(testutil.RFC3339MiliTimestampRegex + " " + "Standard error log line 1")}, nil)
	require.NoError(t, err)
}

// Verify that logs in follow mode end when Executable is deleted
func TestExecutableLogsFollowStreamEndsOnDelete(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const exeName = "test-executable-logs-follow-stream-ends-on-delete"

	exe := apiv1.Executable{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1.GroupVersion.Version,
			Kind:       "Executable",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      exeName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/" + exeName,
		},
	}

	finishExecution := concurrency.NewAutoResetEvent(false)
	defer finishExecution.Set()

	testProcessExecutor.InstallAutoExecution(ctrl_testutil.AutoExecution{
		Condition: ctrl_testutil.ProcessSearchCriteria{
			Command: []string{exe.Spec.ExecutablePath},
		},
		RunCommand: func(pe *ctrl_testutil.ProcessExecution) int32 {
			_, stdoutErr := pe.Cmd.Stdout.Write(osutil.WithNewline([]byte("Standard output log line 1")))
			require.NoError(t, stdoutErr, "Could not write first line to stdout of Executable '%s'", exe.ObjectMeta.Name)
			_, stdoutErr = pe.Cmd.Stdout.Write(osutil.WithNewline([]byte("Standard output log line 2")))
			require.NoError(t, stdoutErr, "Could not write second line to stdout of Executable '%s'", exe.ObjectMeta.Name)

			<-finishExecution.Wait()
			return 0
		},
	})
	defer testProcessExecutor.RemoveAutoExecution(ctrl_testutil.ProcessSearchCriteria{
		Command: []string{exe.Spec.ExecutablePath},
	})

	t.Logf("Creating Executable '%s'...", exe.ObjectMeta.Name)
	err := client.Create(ctx, exe.DeepCopy())
	require.NoError(t, err, "Could not create Executable '%s'", exe.ObjectMeta.Name)

	t.Logf("Start following Executable '%s' logs...", exe.ObjectMeta.Name)
	opts := apiv1.LogOptions{
		Follow:     true,
		Source:     "stdout",
		Timestamps: false,
	}
	logStream, logStreamErr := openLogStream(ctx, &exe, opts, nil)
	require.NoError(t, logStreamErr, "Could not open log stream for Executable '%s'", exe.ObjectMeta.Name)

	t.Logf("Ensure Executable '%s' logs are captured...", exe.ObjectMeta.Name)
	scanner := bufio.NewScanner(usvc_io.NewContextReader(ctx, logStream, true /* leverageReadCloser */))
	gotLine := scanner.Scan()
	require.True(t, gotLine, "Could not read first line from log stream for Executable '%s', the reported error was %v", exe.ObjectMeta.Name, scanner.Err())
	require.Equal(t, "Standard output log line 1", scanner.Text(), "First log line does not match expected content for Executable '%s'", exe.ObjectMeta.Name)
	gotLine = scanner.Scan()
	require.True(t, gotLine, "Could not read second line from log stream for Executable '%s', the reported error was %v", exe.ObjectMeta.Name, scanner.Err())
	require.Equal(t, "Standard output log line 2", scanner.Text(), "Second log line does not match expected content for Executable '%s'", exe.ObjectMeta.Name)

	t.Logf("Deleting Executable '%s'...", exe.ObjectMeta.Name)
	err = client.Delete(ctx, exe.DeepCopy())
	require.NoError(t, err, "Could not delete Executable '%s'", exe.ObjectMeta.Name)

	t.Logf("Ensure log stream for Executable '%s' has ended...", exe.ObjectMeta.Name)
	gotLine = scanner.Scan()
	require.False(t, gotLine, "Unexpectedly read another line from log stream for Executable '%s'", exe.ObjectMeta.Name)
	require.NoError(t, scanner.Err(), "The log stream for Executable '%s' did not end gracefully", exe.ObjectMeta.Name)
}

// Ensure that Executable health status changes according to its state (no health probes).
// When running the Executable should be Healthy.
// If the Executable fails to start, it should be Unhealthy.
// Finished with zero exit code--Caution. Finished with non-zero exit code--Unhealthy.
func TestExecutableHealthBasic(t *testing.T) {
	type testcase struct {
		description            string
		exeName                string
		simulateStartupFailure bool
		exitCode               int32
		expectedState          apiv1.ExecutableState
		expectedHealthStatus   apiv1.HealthStatus
	}

	testcases := []testcase{
		{
			description:            "exit-zero",
			exeName:                "test-executable-health-basic-exit-zero",
			simulateStartupFailure: false,
			exitCode:               0,
			expectedState:          apiv1.ExecutableStateFinished,
			expectedHealthStatus:   apiv1.HealthStatusCaution,
		},
		{
			description:            "exit-non-zero",
			exeName:                "test-executable-health-basic-exit-non-zero",
			simulateStartupFailure: false,
			exitCode:               1,
			expectedState:          apiv1.ExecutableStateFinished,
			expectedHealthStatus:   apiv1.HealthStatusUnhealthy,
		},
		{
			description:            "startup-failure",
			exeName:                "test-executable-health-basic-startup-failure",
			simulateStartupFailure: true,
			expectedState:          apiv1.ExecutableStateFailedToStart,
			expectedHealthStatus:   apiv1.HealthStatusUnhealthy,
		},
	}

	t.Parallel()

	for _, tc := range testcases {
		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()

			exe := apiv1.Executable{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tc.exeName,
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "path/to/" + tc.exeName,
				},
			}

			if tc.simulateStartupFailure {
				t.Logf("Simulating Executable '%s' startup failure...", exe.ObjectMeta.Name)
				testProcessExecutor.InstallAutoExecution(ctrl_testutil.AutoExecution{
					Condition: ctrl_testutil.ProcessSearchCriteria{
						Command: []string{exe.Spec.ExecutablePath},
					},
					StartupError: fmt.Errorf("simulated statup failure for Executable '%s'", exe.ObjectMeta.Name),
				})
				defer testProcessExecutor.RemoveAutoExecution(ctrl_testutil.ProcessSearchCriteria{
					Command: []string{exe.Spec.ExecutablePath},
				})
			}

			t.Logf("Creating Executable '%s'...", exe.ObjectMeta.Name)
			err := client.Create(ctx, exe.DeepCopy())
			require.NoError(t, err, "Could not create Executable '%s'", exe.ObjectMeta.Name)

			if !tc.simulateStartupFailure {
				t.Logf("Ensure Executable '%s' is running...", exe.ObjectMeta.Name)
				_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exe), func(currentExe *apiv1.Executable) (bool, error) {
					return currentExe.Status.State == apiv1.ExecutableStateRunning, nil
				})
				pid, processErr := ensureProcessRunning(ctx, exe.Spec.ExecutablePath)
				require.NoError(t, processErr, "Process for Executable '%s' could not be started", exe.ObjectMeta.Name)

				t.Logf("Simulate Executable '%s' run finish...", exe.ObjectMeta.Name)
				testProcessExecutor.SimulateProcessExit(t, pid, tc.exitCode)
			}

			t.Logf("Ensure Executable '%s' is in the expected state, including health status...", exe.ObjectMeta.Name)
			_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exe), func(currentExe *apiv1.Executable) (bool, error) {
				hasFinishTimestamp := !currentExe.Status.FinishTimestamp.IsZero()
				inExpectedState := currentExe.Status.State == tc.expectedState
				hasExpectedHealthStatus := currentExe.Status.HealthStatus == tc.expectedHealthStatus
				return hasFinishTimestamp && inExpectedState && hasExpectedHealthStatus, nil
			})
		})
	}
}

func ensureProcessRunning(ctx context.Context, cmdPath string) (process.Pid_t, error) {
	pid := process.UnknownPID

	processStarted := func(_ context.Context) (bool, error) {
		runningProcessesWithPath := testProcessExecutor.FindAll([]string{cmdPath}, "", func(pe *ctrl_testutil.ProcessExecution) bool {
			return pe.Running()
		})

		if len(runningProcessesWithPath) != 1 {
			return false, nil
		}

		pid = runningProcessesWithPath[0].PID
		return true, nil
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, processStarted)
	if err != nil {
		return process.UnknownPID, err
	} else {
		return pid, nil
	}
}

func ensureIdeRunSessionStarted(ctx context.Context, cmdPath string) (controllers.RunID, error) {
	runID, err := findIdeRun(ctx, cmdPath)
	if err != nil {
		return controllers.UnknownRunID, err
	}

	randomPid, _ := process.IntToPidT(rand.Intn(12345) + 1)
	if err = ideRunner.SimulateSuccessfulRunStart(controllers.RunID(runID), randomPid); err != nil {
		return controllers.UnknownRunID, err
	}

	return runID, nil
}

func findIdeRun(ctx context.Context, cmdPath string) (controllers.RunID, error) {
	var runID controllers.RunID = controllers.UnknownRunID

	ideRunSessionStarted := func(_ context.Context) (bool, error) {
		activeSessionsWithPath := ideRunner.FindAll(cmdPath, func(run ctrl_testutil.TestIdeRun) bool {
			return run.Running()
		})

		if len(activeSessionsWithPath) != 1 {
			return false, nil
		}

		runID = activeSessionsWithPath[0].ID
		return true, nil
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ideRunSessionStarted)
	if err != nil {
		return controllers.UnknownRunID, err
	}

	return runID, nil
}
