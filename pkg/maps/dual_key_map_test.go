package maps

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Should be possible to update changing either key, in addition to the normal Store() method.
func TestSyncMapKeyUpdatingMethods(t *testing.T) {
	m := NewSynchronizedDualKeyMap[string, string, int]()
	m.Store("one", "uno", 1)

	k2, val, found := m.FindByFirstKey("one")
	require.True(t, found)
	require.Equal(t, "uno", k2)
	require.Equal(t, 1, val)

	k1, val, found := m.FindBySecondKey("uno")
	require.True(t, found)
	require.Equal(t, "one", k1)
	require.Equal(t, 1, val)

	succeeded := m.UpdateChangingFirstKey("one", "eins", "uno", 1)
	require.True(t, succeeded)

	_, _, found = m.FindByFirstKey("one")
	require.False(t, found)

	k2, val, found = m.FindByFirstKey("eins")
	require.True(t, found)
	require.Equal(t, "uno", k2)
	require.Equal(t, 1, val)

	k1, val, found = m.FindBySecondKey("uno")
	require.True(t, found)
	require.Equal(t, "eins", k1)
	require.Equal(t, 1, val)

	succeeded = m.UpdateChangingSecondKey("eins", "uno", "jeden", 1)
	require.True(t, succeeded)

	_, _, found = m.FindBySecondKey("uno")
	require.False(t, found)

	k2, val, found = m.FindByFirstKey("eins")
	require.True(t, found)
	require.Equal(t, "jeden", k2)
	require.Equal(t, 1, val)

	k1, val, found = m.FindBySecondKey("jeden")
	require.True(t, found)
	require.Equal(t, "eins", k1)
	require.Equal(t, 1, val)

	// Updating with non-existing keys should fail
	succeeded = m.UpdateChangingFirstKey("zero", "null", "cero", 0)
	require.False(t, succeeded)
	succeeded = m.UpdateChangingSecondKey("zero", "null", "cero", 0)
	require.False(t, succeeded)
}

// Update should succeed only if the entry is already present.
func TestSyncMapUpdateVersusStore(t *testing.T) {
	m := NewSynchronizedDualKeyMap[string, string, int]()
	m.Store("one", "uno", 1)

	updated := m.Update("one", "uno", 2)
	require.True(t, updated)

	_, val, found := m.FindByFirstKey("one")
	require.True(t, found)
	require.Equal(t, 2, val)

	updated = m.Update("one", "jeden", 3)
	require.False(t, updated, "Update should fail if the second key does not match")

	updated = m.Update("eins", "uno", 4)
	require.False(t, updated, "Update should fail if the first key does not match")

	updated = m.Update("something", "else", 0)
	require.False(t, updated, "Update should fail if neither key matches")
}

// One can delete by either key.
func TestSyncMapDeletes(t *testing.T) {
	m := NewSynchronizedDualKeyMap[string, string, int]()
	m.Store("one", "uno", 1)
	m.Store("two", "dos", 2)

	_, _, found := m.FindByFirstKey("one")
	require.True(t, found)
	_, _, found = m.FindBySecondKey("uno")
	require.True(t, found)
	_, _, found = m.FindByFirstKey("two")
	require.True(t, found)
	_, _, found = m.FindBySecondKey("dos")
	require.True(t, found)

	m.DeleteByFirstKey("one")

	_, _, found = m.FindByFirstKey("one")
	require.False(t, found)
	_, _, found = m.FindBySecondKey("uno")
	require.False(t, found)
	_, _, found = m.FindByFirstKey("two")
	require.True(t, found)
	_, _, found = m.FindBySecondKey("dos")
	require.True(t, found)

	m.DeleteBySecondKey("dos")

	_, _, found = m.FindByFirstKey("one")
	require.False(t, found)
	_, _, found = m.FindBySecondKey("uno")
	require.False(t, found)
	_, _, found = m.FindByFirstKey("two")
	require.False(t, found)
	_, _, found = m.FindBySecondKey("dos")
	require.False(t, found)
}

// Deferred operations should be executed if present.
// If there are no deferred operations, the attempt to execute them should be a no-op.
func TestSyncMapDeferredOps(t *testing.T) {
	m := NewSynchronizedDualKeyMap[string, string, int]()
	m.Store("one", "uno", 1)
	m.Store("two", "dos", 2)

	// Queue first entry update
	m.QueueDeferredOp("one", func(mm *DualKeyMap[string, string, int]) {
		mm.Update("one", "uno", 3)
	})

	// Queue another first entry update
	m.QueueDeferredOp("one", func(mm *DualKeyMap[string, string, int]) {
		mm.Update("one", "uno", 4)
	})

	m.RunDeferredOps("one")
	_, val, found := m.FindByFirstKey("one")
	require.True(t, found)
	require.Equal(t, 4, val)

	require.NotPanics(t, func() { m.RunDeferredOps("nonexistent") }, "Should be able to RunDeferredOps() when there are none")

	m.RunDeferredOps("two")
	// There were no deferred ops for "two", so the value should be unchanged
	_, val, found = m.FindByFirstKey("two")
	require.True(t, found)
	require.Equal(t, 2, val)
}
