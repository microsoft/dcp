/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestTerminalSpecValidateUDSPathRequirement(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		spec        TerminalSpec
		expectError bool
	}{
		{
			name: "empty UDSPath allowed in default (listen) mode",
			spec: TerminalSpec{Cols: 80, Rows: 24},
		},
		{
			name: "empty UDSPath allowed in explicit listen mode",
			spec: TerminalSpec{SocketMode: TerminalSocketModeListen, Cols: 80, Rows: 24},
		},
		{
			name:        "empty UDSPath rejected in connect mode",
			spec:        TerminalSpec{SocketMode: TerminalSocketModeConnect, Cols: 80, Rows: 24},
			expectError: true,
		},
		{
			name: "non-empty UDSPath allowed in listen mode",
			spec: TerminalSpec{UDSPath: "/tmp/app.sock", SocketMode: TerminalSocketModeListen, Cols: 80, Rows: 24},
		},
		{
			name: "non-empty UDSPath allowed in connect mode",
			spec: TerminalSpec{UDSPath: "/tmp/app.sock", SocketMode: TerminalSocketModeConnect, Cols: 80, Rows: 24},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			errs := testCase.spec.Validate(field.NewPath("spec", "terminal"))
			if testCase.expectError {
				require.NotEmpty(t, errs)
				require.Equal(t, "spec.terminal.udsPath", errs[0].Field)
			} else {
				require.Empty(t, errs)
			}
		})
	}
}
