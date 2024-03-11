package integration_test

import (
	"context"
	"fmt"
	"math/rand"
	"regexp"
	std_slices "slices"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	wait "k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil"
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
		verifyExeRunning  func(ctx context.Context, t *testing.T, exe *apiv1.Executable) string
		simulateRunEnding func(t *testing.T, runID string)
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
			verifyExeRunning: func(ctx context.Context, t *testing.T, exe *apiv1.Executable) string {
				pid, err := ensureProcessRunning(ctx, exe.Spec.ExecutablePath)
				if err != nil {
					t.Fatalf("Process could not be started: %v", err)
					return "" // make compiler happy
				} else {
					return strconv.Itoa(int(pid))
				}
			},
			simulateRunEnding: func(t *testing.T, runID string) {
				pid64, err := strconv.ParseInt(runID, 10, 32)
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
			verifyExeRunning: func(ctx context.Context, t *testing.T, exe *apiv1.Executable) string {
				runID, err := ensureIdeRunSessionStarted(ctx, exe.Spec.ExecutablePath)

				if err != nil {
					t.Fatalf("IDE run session could not be started: %v", err)
					return "" // make compiler happy
				}

				return runID
			},
			simulateRunEnding: func(t *testing.T, runID string) {
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

				if updatedExe.Status.ExecutionID == runID && updatedExe.Status.State == apiv1.ExecutableStateFinished && updatedExe.Status.ExitCode != apiv1.UnknownExitCode && *updatedExe.Status.ExitCode == int32(expectedEC) {
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
			exePatch := updatedExe.DeepCopy()
			exePatch.Spec.Stop = true
			if err := client.Patch(ctx, exePatch, ctrl_client.MergeFrom(updatedExe)); err != nil {
				t.Fatalf("Unable to update Executable: %v", err)
			}

			t.Logf("Waiting for process associated with Executable '%s' to be killed...", tc.exe.ObjectMeta.Name)
			tc.verifyRunEnded(ctx, t, tc.exe)

			updatedExe = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(tc.exe), func(currentExe *apiv1.Executable) (bool, error) {
				return currentExe.Status.State == apiv1.ExecutableStateFinished, nil
			})

			if err := client.Delete(ctx, updatedExe); err != nil {
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
			if err := client.Delete(ctx, updatedExe); err != nil {
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

func ensureIdeRunSessionStarted(ctx context.Context, cmdPath string) (string, error) {
	runID, err := findIdeRun(ctx, cmdPath)
	if err != nil {
		return "", err
	}

	randomPid, _ := process.IntToPidT(rand.Intn(12345) + 1)
	if err = ideRunner.SimulateRunStart(controllers.RunID(runID), randomPid); err != nil {
		return "", err
	}

	return runID, nil
}

func findIdeRun(ctx context.Context, cmdPath string) (string, error) {
	runID := ""

	ideRunSessionStarted := func(_ context.Context) (bool, error) {
		activeSessionsWithPath := ideRunner.FindAll(cmdPath, func(run ctrl_testutil.TestIdeRun) bool {
			return run.Running()
		})

		if len(activeSessionsWithPath) != 1 {
			return false, nil
		}

		runID = string(activeSessionsWithPath[0].ID)
		return true, nil
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, ideRunSessionStarted)
	if err != nil {
		return "", err
	}

	return runID, nil
}
