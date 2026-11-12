/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package ports

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsValidPort(t *testing.T) {
	t.Parallel()

	require.False(t, IsValidPort(0))
	require.True(t, IsValidPort(1))
	require.True(t, IsValidPort(65535))
	require.False(t, IsValidPort(65536))
}

func TestIsBindablePort(t *testing.T) {
	t.Parallel()

	require.True(t, IsBindablePort(0))
	require.True(t, IsBindablePort(1))
	require.True(t, IsBindablePort(65535))
	require.False(t, IsBindablePort(65536))
}
