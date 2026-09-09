/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPhysicalProcessValidate(t *testing.T) {
	validPID := int64(42)
	zeroPID := int64(0)
	largePID := int64(math.MaxUint32) + 1
	testCases := []struct {
		name          string
		process       PhysicalProcess
		expectedError string
	}{
		{
			name: "valid created process",
			process: PhysicalProcess{
				ObjectMeta: metav1.ObjectMeta{Name: "test-process", Namespace: "test-namespace"},
				Spec: PhysicalProcessSpec{
					Process: &PhysicalProcessConfig{
						ExecutablePath:   "test-command",
						Args:             []string{"one", "two"},
						WorkingDirectory: "/tmp",
					},
				},
			},
		},
		{
			name: "valid tracked process",
			process: PhysicalProcess{
				ObjectMeta: metav1.ObjectMeta{Name: "test-process", Namespace: "test-namespace"},
				Spec:       PhysicalProcessSpec{PID: &validPID},
			},
		},
		{
			name: "missing namespace",
			process: PhysicalProcess{
				ObjectMeta: metav1.ObjectMeta{Name: "test-process"},
				Spec:       PhysicalProcessSpec{PID: &validPID},
			},
			expectedError: "metadata.namespace",
		},
		{
			name: "missing process source",
			process: PhysicalProcess{
				ObjectMeta: metav1.ObjectMeta{Name: "test-process", Namespace: "test-namespace"},
			},
			expectedError: "spec",
		},
		{
			name: "pid and config",
			process: PhysicalProcess{
				ObjectMeta: metav1.ObjectMeta{Name: "test-process", Namespace: "test-namespace"},
				Spec: PhysicalProcessSpec{
					PID:     &validPID,
					Process: &PhysicalProcessConfig{ExecutablePath: "test-command"},
				},
			},
			expectedError: "spec.process",
		},
		{
			name: "zero pid",
			process: PhysicalProcess{
				ObjectMeta: metav1.ObjectMeta{Name: "test-process", Namespace: "test-namespace"},
				Spec:       PhysicalProcessSpec{PID: &zeroPID},
			},
			expectedError: "spec.pid",
		},
		{
			name: "pid too large",
			process: PhysicalProcess{
				ObjectMeta: metav1.ObjectMeta{Name: "test-process", Namespace: "test-namespace"},
				Spec:       PhysicalProcessSpec{PID: &largePID},
			},
			expectedError: "spec.pid",
		},
		{
			name: "missing executable path",
			process: PhysicalProcess{
				ObjectMeta: metav1.ObjectMeta{Name: "test-process", Namespace: "test-namespace"},
				Spec:       PhysicalProcessSpec{Process: &PhysicalProcessConfig{}},
			},
			expectedError: "spec.process.executablePath",
		},
		{
			name: "whitespace executable path",
			process: PhysicalProcess{
				ObjectMeta: metav1.ObjectMeta{Name: "test-process", Namespace: "test-namespace"},
				Spec:       PhysicalProcessSpec{Process: &PhysicalProcessConfig{ExecutablePath: " "}},
			},
			expectedError: "spec.process.executablePath",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			errorList := testCase.process.Validate(context.Background())
			if testCase.expectedError == "" {
				require.Empty(t, errorList)
			} else {
				require.NotEmpty(t, errorList)
				require.Contains(t, errorList.ToAggregate().Error(), testCase.expectedError)
			}
		})
	}
}

func TestPhysicalProcessValidateUpdate(t *testing.T) {
	oldProcess := &PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "test-process", Namespace: "test-namespace"},
		Spec: PhysicalProcessSpec{
			Process: &PhysicalProcessConfig{ExecutablePath: "test-command"},
		},
	}

	t.Run("allows stop request", func(t *testing.T) {
		newProcess := oldProcess.DeepCopy()
		newProcess.Spec.Stop = true
		require.Empty(t, newProcess.ValidateUpdate(context.Background(), oldProcess))
	})

	t.Run("rejects clearing stop request", func(t *testing.T) {
		stoppedProcess := oldProcess.DeepCopy()
		stoppedProcess.Spec.Stop = true
		newProcess := stoppedProcess.DeepCopy()
		newProcess.Spec.Stop = false
		require.Error(t, newProcess.ValidateUpdate(context.Background(), stoppedProcess).ToAggregate())
	})

	t.Run("rejects creation field changes", func(t *testing.T) {
		newProcess := oldProcess.DeepCopy()
		newProcess.Spec.Process.Args = []string{"changed"}
		require.Error(t, newProcess.ValidateUpdate(context.Background(), oldProcess).ToAggregate())
	})
}
