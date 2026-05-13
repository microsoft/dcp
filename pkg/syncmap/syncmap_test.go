/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package syncmap

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestComparableMapCompareAndDelete(t *testing.T) {
	t.Parallel()

	m := ComparableMap[string, *int]{}
	value := 42
	other := 43

	m.Store("key", &value)

	require.False(t, m.CompareAndDelete("key", &other))
	_, found := m.Load("key")
	require.True(t, found)

	require.True(t, m.CompareAndDelete("key", &value))
	_, found = m.Load("key")
	require.False(t, found)
}
