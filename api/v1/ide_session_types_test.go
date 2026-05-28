/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdeSessionStateCanUpdateTo(t *testing.T) {
	t.Parallel()

	allStates := []IdeSessionState{
		IdeSessionStateInitial,
		IdeSessionStateStarting,
		IdeSessionStateRunning,
		IdeSessionStateStopping,
		IdeSessionStateStopped,
		IdeSessionStateFailed,
	}

	allowed := map[IdeSessionState]map[IdeSessionState]bool{
		IdeSessionStateInitial: {
			IdeSessionStateInitial:  true, // no-op transitions are always allowed
			IdeSessionStateStarting: true,
		},
		IdeSessionStateStarting: {
			IdeSessionStateStarting: true,
			IdeSessionStateRunning:  true,
			IdeSessionStateStopping: true,
			IdeSessionStateStopped:  true,
			IdeSessionStateFailed:   true,
		},
		IdeSessionStateRunning: {
			IdeSessionStateRunning:  true,
			IdeSessionStateStopping: true,
			IdeSessionStateStopped:  true,
			IdeSessionStateFailed:   true,
		},
		IdeSessionStateStopping: {
			IdeSessionStateStopping: true,
			IdeSessionStateStopped:  true,
			IdeSessionStateFailed:   true,
		},
		// Terminal states allow only a no-op transition to themselves.
		IdeSessionStateStopped: {
			IdeSessionStateStopped: true,
		},
		IdeSessionStateFailed: {
			IdeSessionStateFailed: true,
		},
	}

	for _, from := range allStates {
		for _, to := range allStates {
			expected := allowed[from][to]
			actual := from.CanUpdateTo(to)
			assert.Equalf(t, expected, actual, "Transition from %q to %q: expected allowed=%v, got %v", from, to, expected, actual)
		}
	}
}

func TestIdeSessionStateIsTerminal(t *testing.T) {
	t.Parallel()

	cases := map[IdeSessionState]bool{
		IdeSessionStateInitial:  false,
		IdeSessionStateStarting: false,
		IdeSessionStateRunning:  false,
		IdeSessionStateStopping: false,
		IdeSessionStateStopped:  true,
		IdeSessionStateFailed:   true,
	}
	for state, expected := range cases {
		assert.Equalf(t, expected, state.IsTerminal(), "IsTerminal for %q", state)
	}
}

func TestIdeSessionStateIsValidDesiredState(t *testing.T) {
	t.Parallel()

	cases := map[IdeSessionState]bool{
		IdeSessionStateInitial:  true,
		IdeSessionStateRunning:  true,
		IdeSessionStateStopped:  true,
		IdeSessionStateStarting: false,
		IdeSessionStateStopping: false,
		IdeSessionStateFailed:   false,
		IdeSessionState("nonsense"): false,
	}
	for state, expected := range cases {
		assert.Equalf(t, expected, state.IsValidDesiredState(), "IsValidDesiredState for %q", state)
	}
}

func TestIdeSessionValidate(t *testing.T) {
	t.Parallel()

	const validLC = `[{"type": "project", "projectPath": "/foo/bar"}]`

	testCases := []struct {
		name        string
		spec        IdeSessionSpec
		expectError bool
		errSubstr   string
	}{
		{
			name: "valid - initial desired state",
			spec: IdeSessionSpec{
				LaunchConfigurations: validLC,
			},
			expectError: false,
		},
		{
			name: "valid - desired state Running",
			spec: IdeSessionSpec{
				LaunchConfigurations: validLC,
				DesiredState:         IdeSessionStateRunning,
			},
			expectError: false,
		},
		{
			name: "valid - desired state Stopped",
			spec: IdeSessionSpec{
				LaunchConfigurations: validLC,
				DesiredState:         IdeSessionStateStopped,
			},
			expectError: false,
		},
		{
			name:        "missing launch configurations",
			spec:        IdeSessionSpec{},
			expectError: true,
			errSubstr:   "launchConfigurations",
		},
		{
			name: "launch configurations not JSON",
			spec: IdeSessionSpec{
				LaunchConfigurations: "not json",
			},
			expectError: true,
			errSubstr:   "valid JSON",
		},
		{
			name: "desired state Starting is not user-settable",
			spec: IdeSessionSpec{
				LaunchConfigurations: validLC,
				DesiredState:         IdeSessionStateStarting,
			},
			expectError: true,
			errSubstr:   "desiredState",
		},
		{
			name: "desired state Failed is not user-settable",
			spec: IdeSessionSpec{
				LaunchConfigurations: validLC,
				DesiredState:         IdeSessionStateFailed,
			},
			expectError: true,
			errSubstr:   "desiredState",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := &IdeSession{
				Spec: tc.spec,
			}
			errs := s.Validate(context.Background())
			if tc.expectError {
				require.NotEmpty(t, errs, "Expected validation errors for case %q", tc.name)
				if tc.errSubstr != "" {
					var found bool
					for _, e := range errs {
						if assert.ObjectsAreEqual(true, e.Error() != "") && containsString(e.Error(), tc.errSubstr) {
							found = true
							break
						}
					}
					assert.Truef(t, found, "Expected one of the validation errors to mention %q. Got: %v", tc.errSubstr, errs)
				}
			} else {
				assert.Empty(t, errs, "Expected no validation errors, got: %v", errs)
			}
		})
	}
}

func TestIdeSessionValidateUpdate(t *testing.T) {
	t.Parallel()

	const lc1 = `[{"type": "project", "projectPath": "/foo"}]`
	const lc2 = `[{"type": "project", "projectPath": "/bar"}]`

	testCases := []struct {
		name         string
		oldSpec      IdeSessionSpec
		newSpec      IdeSessionSpec
		expectErrors []string
	}{
		{
			name:    "no changes",
			oldSpec: IdeSessionSpec{LaunchConfigurations: lc1, DesiredState: IdeSessionStateRunning},
			newSpec: IdeSessionSpec{LaunchConfigurations: lc1, DesiredState: IdeSessionStateRunning},
		},
		{
			name:    "transition empty to Running",
			oldSpec: IdeSessionSpec{LaunchConfigurations: lc1},
			newSpec: IdeSessionSpec{LaunchConfigurations: lc1, DesiredState: IdeSessionStateRunning},
		},
		{
			name:    "transition Running to Stopped",
			oldSpec: IdeSessionSpec{LaunchConfigurations: lc1, DesiredState: IdeSessionStateRunning},
			newSpec: IdeSessionSpec{LaunchConfigurations: lc1, DesiredState: IdeSessionStateStopped},
		},
		{
			name:         "changing launch configurations is forbidden",
			oldSpec:      IdeSessionSpec{LaunchConfigurations: lc1},
			newSpec:      IdeSessionSpec{LaunchConfigurations: lc2},
			expectErrors: []string{"launchConfigurations"},
		},
		{
			name:         "transition Stopped back to Running is forbidden",
			oldSpec:      IdeSessionSpec{LaunchConfigurations: lc1, DesiredState: IdeSessionStateStopped},
			newSpec:      IdeSessionSpec{LaunchConfigurations: lc1, DesiredState: IdeSessionStateRunning},
			expectErrors: []string{"desiredState"},
		},
		{
			name:         "transition Stopped to empty is forbidden",
			oldSpec:      IdeSessionSpec{LaunchConfigurations: lc1, DesiredState: IdeSessionStateStopped},
			newSpec:      IdeSessionSpec{LaunchConfigurations: lc1, DesiredState: IdeSessionStateInitial},
			expectErrors: []string{"desiredState"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			oldObj := &IdeSession{Spec: tc.oldSpec}
			newObj := &IdeSession{Spec: tc.newSpec}
			errs := newObj.ValidateUpdate(context.Background(), oldObj)
			if len(tc.expectErrors) == 0 {
				assert.Empty(t, errs, "Expected no errors but got: %v", errs)
				return
			}
			require.NotEmptyf(t, errs, "Expected validation errors for case %q", tc.name)
			for _, want := range tc.expectErrors {
				var found bool
				for _, e := range errs {
					if containsString(e.Error(), want) {
						found = true
						break
					}
				}
				assert.Truef(t, found, "Expected validation errors to mention %q. Got: %v", want, errs)
			}
		})
	}
}

func TestIdeSessionDone(t *testing.T) {
	t.Parallel()

	cases := map[IdeSessionState]bool{
		IdeSessionStateInitial:  false,
		IdeSessionStateStarting: false,
		IdeSessionStateRunning:  false,
		IdeSessionStateStopping: false,
		IdeSessionStateStopped:  true,
		IdeSessionStateFailed:   true,
	}
	for state, expected := range cases {
		s := &IdeSession{Status: IdeSessionStatus{State: state}}
		assert.Equalf(t, expected, s.Done(), "Done() for state %q", state)
	}
}

// containsString is a tiny substring helper so the tests do not depend on the strings package
// being imported elsewhere.
func containsString(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
