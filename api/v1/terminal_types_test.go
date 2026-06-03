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

func TestTerminalSocketModeNormalized(t *testing.T) {
	t.Parallel()

	require.Equal(t, TerminalSocketModeListen, TerminalSocketMode("").Normalized())
	require.Equal(t, TerminalSocketModeListen, TerminalSocketModeListen.Normalized())
	require.Equal(t, TerminalSocketModeConnect, TerminalSocketModeConnect.Normalized())
}

func TestTerminalSpecEqualTreatsEmptySocketModeAsListen(t *testing.T) {
	t.Parallel()

	withEmpty := &TerminalSpec{UDSPath: "/tmp/t.sock", Cols: 80, Rows: 24}
	withListen := &TerminalSpec{UDSPath: "/tmp/t.sock", SocketMode: TerminalSocketModeListen, Cols: 80, Rows: 24}
	withConnect := &TerminalSpec{UDSPath: "/tmp/t.sock", SocketMode: TerminalSocketModeConnect, Cols: 80, Rows: 24}

	require.True(t, withEmpty.Equal(withListen), "empty socket mode must equal explicit listen")
	require.False(t, withEmpty.Equal(withConnect), "listen must not equal connect")
	require.False(t, withListen.Equal(withConnect), "listen must not equal connect")
}

func TestTerminalSpecValidateSocketMode(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		mode    TerminalSocketMode
		wantErr bool
	}{
		{name: "empty defaults to listen", mode: "", wantErr: false},
		{name: "listen", mode: TerminalSocketModeListen, wantErr: false},
		{name: "connect", mode: TerminalSocketModeConnect, wantErr: false},
		{name: "invalid", mode: TerminalSocketMode("bogus"), wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := &TerminalSpec{UDSPath: "/tmp/t.sock", SocketMode: tc.mode}
			errs := ts.Validate(field.NewPath("spec", "terminal"))
			if tc.wantErr {
				require.NotEmpty(t, errs)
			} else {
				require.Empty(t, errs)
			}
		})
	}
}

func TestTerminalSpecValidateUpdateForbidsSocketModeChange(t *testing.T) {
	t.Parallel()

	specPath := field.NewPath("spec", "terminal")
	listen := &TerminalSpec{UDSPath: "/tmp/t.sock", SocketMode: TerminalSocketModeListen, Cols: 80, Rows: 24}
	connect := &TerminalSpec{UDSPath: "/tmp/t.sock", SocketMode: TerminalSocketModeConnect, Cols: 80, Rows: 24}

	require.NotEmpty(t, connect.ValidateUpdate(listen, specPath), "changing socket mode must be forbidden")

	// An empty new mode is equivalent to listen and must be accepted against an explicit listen.
	empty := &TerminalSpec{UDSPath: "/tmp/t.sock", Cols: 80, Rows: 24}
	require.Empty(t, empty.ValidateUpdate(listen, specPath), "empty mode must equal listen and be permitted")
}

func TestContainerSpecGetLifecycleKeyIncludesTerminalSocketMode(t *testing.T) {
	t.Parallel()

	newSpec := func(mode TerminalSocketMode) *ContainerSpec {
		return &ContainerSpec{
			Image:         "api:dev",
			ContainerName: "api",
			Terminal: &TerminalSpec{
				UDSPath:    "/tmp/t.sock",
				SocketMode: mode,
				Cols:       80,
				Rows:       24,
			},
		}
	}

	keyListen, _, errListen := newSpec(TerminalSocketModeListen).GetLifecycleKey()
	require.NoError(t, errListen)

	keyConnect, _, errConnect := newSpec(TerminalSocketModeConnect).GetLifecycleKey()
	require.NoError(t, errConnect)
	require.NotEqual(t, keyListen, keyConnect, "listen and connect must yield different lifecycle keys")

	keyEmpty, _, errEmpty := newSpec("").GetLifecycleKey()
	require.NoError(t, errEmpty)
	require.Equal(t, keyListen, keyEmpty, "empty socket mode must hash the same as listen")
}
