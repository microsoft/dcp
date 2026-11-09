/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessHandle_Comparable(t *testing.T) {
	t.Parallel()

	now := time.Now()
	h1 := NewHandle(Uint32_ToPidT(100), now)
	h2 := NewHandle(Uint32_ToPidT(100), now)
	h3 := NewHandle(Uint32_ToPidT(200), now)

	assert.Equal(t, h1, h2)
	assert.NotEqual(t, h1, h3)

	zeroHandle := ProcessHandle{Pid: UnknownPID}
	assert.NotEqual(t, zeroHandle, h1)

	m := map[ProcessHandle]string{
		h1: "first",
		h3: "second",
	}
	assert.Equal(t, "first", m[h2])
	assert.Equal(t, "second", m[h3])
}

// Verifies that ResolveProcessHandle captures a missing identity but never replaces a supplied stale identity.
func TestResolveProcessHandleCapturesOnlyMissingIdentity(t *testing.T) {
	t.Parallel()

	pid := Pid_t(os.Getpid())
	currentHandle, currentErr := FindProcessHandle(pid)
	require.NoError(t, currentErr)

	capturedHandle, captureErr := ResolveProcessHandle(pid, time.Time{})
	require.NoError(t, captureErr)
	assert.Equal(t, currentHandle, capturedHandle)

	staleIdentity := currentHandle.IdentityTime.Add(-time.Hour)
	preservedHandle, preserveErr := ResolveProcessHandle(pid, staleIdentity)
	require.NoError(t, preserveErr)
	assert.Equal(t, NewHandle(pid, staleIdentity), preservedHandle)
}

// Verifies that ProcessHandle.WallClockStartTime rejects invalid PIDs and missing identities.
func TestProcessHandleWallClockStartTimeRejectsInvalidHandle(t *testing.T) {
	t.Parallel()

	invalidPID := NewHandle(UnknownPID, time.Now())
	invalidTime, invalidErr := invalidPID.WallClockStartTime()
	assert.ErrorIs(t, invalidErr, ErrInvalidProcessHandle)
	assert.True(t, invalidTime.IsZero())

	missingIdentity := NewHandle(100, time.Time{})
	missingTime, missingErr := missingIdentity.WallClockStartTime()
	assert.ErrorIs(t, missingErr, ErrInvalidProcessHandle)
	assert.True(t, missingTime.IsZero())
}
