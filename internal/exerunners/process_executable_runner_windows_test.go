//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package exerunners

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/microsoft/dcp/api/v1"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that Executables using a terminal do not have their windows hidden,
// while non-terminal executables do. See ProcessExecutableRunner.makeProcessCommand
// for rationale why the latter is the case.
func TestProcessExecutableRunnerWindowsNotHiddenIfTerminalEnabledn(t *testing.T) {
	testCases := []struct {
		name             string
		terminal         *apiv1.TerminalSpec
		expectHideWindow bool
	}{
		{
			name:             "non-terminal executable hides window",
			terminal:         nil,
			expectHideWindow: true,
		},
		{
			name:             "terminal-backed executable does not hide window",
			terminal:         &apiv1.TerminalSpec{Cols: 120, Rows: 40},
			expectHideWindow: false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultExerunnerTestTimeout)
			defer cancel()

			runner := NewProcessExecutableRunner(internal_testutil.NewTestProcessExecutor(ctx))
			exe := &apiv1.Executable{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "api",
					Namespace: "default",
					UID:       "api-uid",
				},
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "/test/app",
					Terminal:       testCase.terminal,
				},
			}

			cmd, _, _ := runner.makeProcessCommand(ctx, exe)
			require.NotNil(t, cmd)

			hideWindow := cmd.SysProcAttr != nil && cmd.SysProcAttr.HideWindow
			require.Equal(t, testCase.expectHideWindow, hideWindow)
		})
	}
}
