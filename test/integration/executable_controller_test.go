package integration_test

import (
	"context"
	"fmt"
	"math/rand"
	"regexp"
	"strconv"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	wait "k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil"
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
				processExecutor.SimulateProcessExit(t, pid, int32(expectedEC))
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

				randomPid, _ := process.IntToPidT(rand.Intn(12345) + 1)
				err = ideRunner.SimulateRunStart(controllers.RunID(runID), randomPid)
				require.NoError(t, err)

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

				if updatedExe.Status.ExecutionID == runID && updatedExe.Status.State == apiv1.ExecutableStateFinished && updatedExe.Status.ExitCode == int32(expectedEC) {
					return true, nil
				}

				return false, nil
			}
			err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, exitCodeCaptured)
			require.NoError(t, err, "Exit codes could not be captured for Executable")
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
					killedProcesses := processExecutor.FindAll(exe.Spec.ExecutablePath, func(pe ctrl_testutil.ProcessExecution) bool {
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

			t.Log("Waiting for Executable run to start...")
			tc.verifyExeRunning(ctx, t, tc.exe)

			updatedExe := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(tc.exe), func(currentExe *apiv1.Executable) (bool, error) {
				return !currentExe.Status.StartupTimestamp.IsZero(), nil
			})

			t.Log("Deleting Executable object...")
			if err := client.Delete(ctx, updatedExe); err != nil {
				t.Fatalf("Unable to delete Executable: %v", err)
			}

			t.Log("Waiting for process to be killed...")
			tc.verifyRunEnded(ctx, t, tc.exe)
		})
	}
}

// Verify ports are injected into Executable environment variables via portForServing template function.
// Service ports are specified via service-producer annotation.
func TestExecutableServingPortInjected(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-executable-serving-port-injected",
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
			Name:        "test-executable-serving-port-injected",
			Namespace:   metav1.NamespaceNone,
			Annotations: map[string]string{"service-producer": fmt.Sprintf(`[{"serviceName":"%s","address":"127.0.0.1","port":7733}]`, svc.ObjectMeta.Name)},
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/test-executable-serving-port-injected",
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

	t.Logf("Ensure the Executable process got the service port injected...")
	pe, found := processExecutor.FindByPid(pid)
	require.True(t, found, "Could not find the process with PID %d", pid)
	require.True(t, slices.Contains(pe.Cmd.Env, "SVC_PORT=7733"), "Port was not injected into the Executable process environment variables. The process environment variables are: %v", pe.Cmd.Env)

	t.Logf("Ensure the Executable.Status.EffectiveEnv contains the injected port...")
	updatedExe := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exe), func(currentExe *apiv1.Executable) (bool, error) {
		return len(currentExe.Status.EffectiveEnv) > 0, nil
	})
	effectiveEnv := slices.Map[apiv1.EnvVar, string](updatedExe.Status.EffectiveEnv, func(v apiv1.EnvVar) string {
		return fmt.Sprintf("%s=%s", v.Name, v.Value)
	})
	require.True(t, slices.Contains(effectiveEnv, "SVC_PORT=7733"), "The Executable effective environment does not contain expected port information. The effective environemtn is %v", effectiveEnv)
}

// Verify ports are injected into Executable environment variables via portForServing template function.
// Service ports are automatically allocated.
func TestExecutableServingPortAllocatedAndInjected(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-executable-serving-port-allocated-injected",
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
			Name:      "test-executable-serving-port-allocated-injected",
			Namespace: metav1.NamespaceNone,
			// No address and no port information
			Annotations: map[string]string{"service-producer": fmt.Sprintf(`[{"serviceName":"%s"}]`, svc.ObjectMeta.Name)},
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/test-executable-serving-port-allocated-injected",
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

	t.Logf("Ensure the Executable process got the service port injected...")
	pe, found := processExecutor.FindByPid(pid)
	require.True(t, found, "Could not find the process with PID %d", pid)

	// Validate the injected port
	matches := testutil.FindAllMatching(pe.Cmd.Env, regexp.MustCompile(`SVC_PORT=(\d+)`))
	require.Len(t, matches, 1, "Port was not injected into the Executable process environment variables. The process environment variables are: %v", pe.Cmd.Env)
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
	require.Equal(t, int32(port), executableEndpoint.Spec.Port, "The port injected into the Executable process environment variables (%d) does not match the port of the Endpoint (%d)", port, executableEndpoint.Spec.Port)

	t.Logf("Ensure the Executable.Status.EffectiveEnv contains the injected port...")
	updatedExe := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exe), func(currentExe *apiv1.Executable) (bool, error) {
		return len(currentExe.Status.EffectiveEnv) > 0, nil
	})
	effectiveEnv := slices.Map[apiv1.EnvVar, string](updatedExe.Status.EffectiveEnv, func(v apiv1.EnvVar) string {
		return fmt.Sprintf("%s=%s", v.Name, v.Value)
	})
	expectedEnvVar := fmt.Sprintf("SVC_PORT=%d", port)
	require.True(t, slices.Contains(effectiveEnv, expectedEnvVar), "The Executable effective environment does not contain expected port information. The effective environemtn is %v", effectiveEnv)
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
	pe, found := processExecutor.FindByPid(pid)
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

func ensureProcessRunning(ctx context.Context, cmdPath string) (process.Pid_t, error) {
	pid := process.UnknownPID

	processStarted := func(_ context.Context) (bool, error) {
		runningProcessesWithPath := processExecutor.FindAll(cmdPath, func(pe ctrl_testutil.ProcessExecution) bool {
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
	} else {
		return runID, nil
	}
}
