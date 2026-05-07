/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package exerunners

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
)

func TestProcessExecutableRunnerSkipsMonitorForPersistentExecutable(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name                  string
		persistent            bool
		expectedMonitorStarts int
	}{
		{
			name:                  "non-persistent executable starts monitor",
			expectedMonitorStarts: 1,
		},
		{
			name:       "persistent executable skips monitor",
			persistent: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			dcppaths.EnableTestPathProbing()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			processExecutor := testutil.NewTestProcessExecutor(ctx)
			runner := NewProcessExecutableRunner(processExecutor)
			exe := &apiv1.Executable{
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "/test/app",
					Persistent:     testCase.persistent,
				},
			}

			result := runner.StartRun(ctx, exe, &testRunChangeHandler{}, logr.Discard())

			require.Equal(t, apiv1.ExecutableStateRunning, result.ExeState)
			require.Len(t, processExecutor.FindAll([]string{"/test/app"}, "", nil), 1)
			require.Len(t, processExecutor.FindAll([]string{"dcp", "monitor-process"}, "", nil), testCase.expectedMonitorStarts)
		})
	}
}

type testRunChangeHandler struct{}

func (*testRunChangeHandler) OnMainProcessChanged(controllers.RunID, process.Pid_t) {}

func (*testRunChangeHandler) OnRunCompleted(controllers.RunID, *int32, error) {}

func (*testRunChangeHandler) OnStartupCompleted(types.NamespacedName, *controllers.ExecutableStartResult) {
}

func (*testRunChangeHandler) OnRunMessage(controllers.RunID, controllers.RunMessageLevel, string) {}
