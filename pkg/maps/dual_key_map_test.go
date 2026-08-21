/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package maps

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDualKeyMapStoreReplacesEntryByFirstKey(t *testing.T) {
	t.Parallel()

	m := NewDualKeyMap[string, int, string]()
	m.Store("one", 1, "first")
	m.Store("one", 2, "second")

	secondKey, value, found := m.FindByFirstKey("one")
	require.True(t, found)
	require.Equal(t, 2, secondKey)
	require.Equal(t, "second", value)

	_, _, found = m.FindBySecondKey(1)
	require.False(t, found)
}

func TestDualKeyMapStoreReplacesEntryBySecondKey(t *testing.T) {
	t.Parallel()

	m := NewDualKeyMap[string, int, string]()
	m.Store("one", 1, "first")
	m.Store("two", 1, "second")

	firstKey, value, found := m.FindBySecondKey(1)
	require.True(t, found)
	require.Equal(t, "two", firstKey)
	require.Equal(t, "second", value)

	_, _, found = m.FindByFirstKey("one")
	require.False(t, found)
}

func TestDualKeyMapStoreReplacesConflictingEntries(t *testing.T) {
	t.Parallel()

	m := NewDualKeyMap[string, int, string]()
	m.Store("one", 1, "first")
	m.Store("two", 2, "second")
	m.Store("one", 2, "replacement")

	secondKey, value, found := m.FindByFirstKey("one")
	require.True(t, found)
	require.Equal(t, 2, secondKey)
	require.Equal(t, "replacement", value)

	_, _, found = m.FindBySecondKey(1)
	require.False(t, found)
	_, _, found = m.FindByFirstKey("two")
	require.False(t, found)
}
