//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"github.com/microsoft/dcp/pkg/process"
)

// Verifies that native WSLC commands use a hidden isolated console without job breakaway or changes to their arguments.
func TestWslcCommandsIsolateConsoleWithoutDetachingLifetime(t *testing.T) {
	t.Parallel()

	command := makeWslcCommand("image", "inspect", "--format", "json", "test-image")
	process.DecoupleFromParent(command)

	require.NotNil(t, command.SysProcAttr)
	require.True(t, command.SysProcAttr.HideWindow)
	require.NotZero(t, command.SysProcAttr.CreationFlags&windows.CREATE_NEW_CONSOLE)
	require.Zero(t, command.SysProcAttr.CreationFlags&windows.CREATE_BREAKAWAY_FROM_JOB)
	require.Equal(t, []string{"image", "inspect", "--format", "json", "test-image"}, command.Args[1:])
}
