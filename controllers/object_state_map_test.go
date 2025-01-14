// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stretchr/testify/require"
)

type testObjectState struct {
	tag   string
	count int
}

func (t *testObjectState) Clone() *testObjectState {
	return &testObjectState{
		tag:   t.tag,
		count: t.count,
	}
}

func (t *testObjectState) UpdateFrom(other *testObjectState) bool {
	updated := false
	if t.tag != other.tag {
		t.tag = other.tag
		updated = true
	}
	if t.count != other.count {
		t.count = other.count
		updated = true
	}
	return updated
}

var _ Cloner[*testObjectState] = (*testObjectState)(nil)
var _ UpdateableFrom[*testObjectState] = (*testObjectState)(nil)

type testObjectStateMap = ObjectStateMap[string, testObjectState, *testObjectState]

// Should be possible to borrow using either key.
func TestObjectStateMapBorrowing(t *testing.T) {
	t.Parallel()

	m := NewObjectStateMap[string, testObjectState]()

	m.Store(types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone}, "one", &testObjectState{tag: "uno", count: 1})

	key, os := m.BorrowByNamespacedName(types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone})
	require.NotNil(t, os)
	require.Equal(t, "one", key)
	require.Equal(t, "uno", os.tag)
	require.Equal(t, 1, os.count)

	ns, os := m.BorrowByStateKey("one")
	require.NotNil(t, os)
	require.Equal(t, types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone}, ns)
	require.Equal(t, "uno", os.tag)
	require.Equal(t, 1, os.count)

	// Borrowing with non-existing keys should fail
	_, os = m.BorrowByNamespacedName(types.NamespacedName{Name: "not-there", Namespace: metav1.NamespaceNone})
	require.Nil(t, os)

	_, os = m.BorrowByStateKey("still-not-there")
	require.Nil(t, os)
}

// Tests various update scenarios.
func TestObjectStateMapUpdating(t *testing.T) {
	t.Parallel()

	type testcase struct {
		description    string
		update         func(*testObjectStateMap, types.NamespacedName, string, string, *testObjectState) bool
		verifyStateKey func(string)
	}

	testcases := []testcase{
		{
			description: "regular update",
			update: func(m *testObjectStateMap, ns types.NamespacedName, stateKey string, _ string, os *testObjectState) bool {
				return m.Update(ns, stateKey, os)
			},
			verifyStateKey: func(stateKey string) {
				require.Equal(t, "one", stateKey)
			},
		},
		{
			description: "update with changed state key",
			update: func(m *testObjectStateMap, ns types.NamespacedName, stateKey string, newStateKey string, os *testObjectState) bool {
				return m.UpdateChangingStateKey(ns, stateKey, newStateKey, os)
			},
			verifyStateKey: func(stateKey string) {
				require.Equal(t, "two", stateKey)
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()

			m := NewObjectStateMap[string, testObjectState]()

			m.Store(types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone}, "one", &testObjectState{tag: "uno", count: 1})

			// Update with non-existing key(s) should be a no-op
			updated := tc.update(m, types.NamespacedName{Name: "not-there", Namespace: metav1.NamespaceNone}, "one", "two", &testObjectState{tag: "uno", count: 2})
			require.False(t, updated)
			updated = tc.update(m, types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone}, "not-there", "two", &testObjectState{tag: "uno", count: 2})
			require.False(t, updated)

			// Update with no changes should be a no-op
			updated = tc.update(m, types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone}, "one", "two", &testObjectState{tag: "uno", count: 1})
			require.False(t, updated)

			// Update with changes should succeed
			updated = tc.update(m, types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone}, "one", "two", &testObjectState{tag: "uno", count: 2})
			require.True(t, updated)

			// Borrowing the updated object should return the updated values
			key, os := m.BorrowByNamespacedName(types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone})
			require.NotNil(t, os)
			tc.verifyStateKey(key)
			require.Equal(t, "uno", os.tag)
			require.Equal(t, 2, os.count)
		})
	}
}

// Should be able to delete by either key.
func TestObjectStateMapDeleting(t *testing.T) {
	t.Parallel()

	m := NewObjectStateMap[string, testObjectState]()

	m.Store(types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone}, "one", &testObjectState{tag: "uno", count: 1})
	m.Store(types.NamespacedName{Name: "zwei", Namespace: metav1.NamespaceNone}, "two", &testObjectState{tag: "dos", count: 1})

	_, os := m.BorrowByNamespacedName(types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone})
	require.NotNil(t, os)
	_, os = m.BorrowByStateKey("one")
	require.NotNil(t, os)
	_, os = m.BorrowByNamespacedName(types.NamespacedName{Name: "zwei", Namespace: metav1.NamespaceNone})
	require.NotNil(t, os)
	_, os = m.BorrowByStateKey("two")
	require.NotNil(t, os)

	// Deleting by non-existent keys should be a no-op (and should not panic)
	m.DeleteByNamespacedName(types.NamespacedName{Name: "not-there", Namespace: metav1.NamespaceNone})
	m.DeleteByStateKey("still- not-there")

	_, os = m.BorrowByNamespacedName(types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone})
	require.NotNil(t, os)
	_, os = m.BorrowByStateKey("one")
	require.NotNil(t, os)
	_, os = m.BorrowByNamespacedName(types.NamespacedName{Name: "zwei", Namespace: metav1.NamespaceNone})
	require.NotNil(t, os)
	_, os = m.BorrowByStateKey("two")
	require.NotNil(t, os)

	// Valid delete by namespaced name.
	m.DeleteByNamespacedName(types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone})

	_, os = m.BorrowByNamespacedName(types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone})
	require.Nil(t, os)
	_, os = m.BorrowByStateKey("one")
	require.Nil(t, os)
	_, os = m.BorrowByNamespacedName(types.NamespacedName{Name: "zwei", Namespace: metav1.NamespaceNone})
	require.NotNil(t, os)
	_, os = m.BorrowByStateKey("two")
	require.NotNil(t, os)

	// Valid delete by state key.
	m.DeleteByStateKey("two")

	_, os = m.BorrowByNamespacedName(types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone})
	require.Nil(t, os)
	_, os = m.BorrowByStateKey("one")
	require.Nil(t, os)
	_, os = m.BorrowByNamespacedName(types.NamespacedName{Name: "zwei", Namespace: metav1.NamespaceNone})
	require.Nil(t, os)
	_, os = m.BorrowByStateKey("two")
	require.Nil(t, os)

	// Deleting from empty map should ba no-op (and should not panic)
	m.DeleteByNamespacedName(types.NamespacedName{Name: "zwei", Namespace: metav1.NamespaceNone})
	m.DeleteByStateKey("one")
}

// Deferred operations should be executed if present.
// If there are no deferred operations, the attempt to execute them should be a no-op.
func TestObjectStateMapDeferredOps(t *testing.T) {
	t.Parallel()

	m := NewObjectStateMap[string, testObjectState]()

	m.Store(types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone}, "one", &testObjectState{tag: "uno", count: 1})
	m.Store(types.NamespacedName{Name: "zwei", Namespace: metav1.NamespaceNone}, "two", &testObjectState{tag: "dos", count: 1})

	// Queue update of the first object state
	m.QueueDeferredOp(types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone}, func(name types.NamespacedName, stateKey string) {
		_, os := m.BorrowByNamespacedName(name)
		require.NotNil(t, os)
		os.tag = "uno-updated"
		m.Update(name, stateKey, os)
	})

	// Queue another update of the first object state
	m.QueueDeferredOp(types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone}, func(name types.NamespacedName, stateKey string) {
		_, os := m.BorrowByNamespacedName(name)
		require.NotNil(t, os)
		os.count = 2
		m.Update(name, stateKey, os)
	})

	m.RunDeferredOps(types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone})

	// Borrowing the updated object should return the updated values
	stateKey, os := m.BorrowByNamespacedName(types.NamespacedName{Name: "eins", Namespace: metav1.NamespaceNone})
	require.NotNil(t, os)
	require.Equal(t, "one", stateKey)
	require.Equal(t, "uno-updated", os.tag)
	require.Equal(t, 2, os.count)

	require.NotPanics(t, func() {
		m.RunDeferredOps(types.NamespacedName{Name: "not-there", Namespace: metav1.NamespaceNone})
	})

	// There were no deferred operations for the second object state, so the values should be unchanged
	// after running deferred operations for it.
	m.RunDeferredOps(types.NamespacedName{Name: "zwei", Namespace: metav1.NamespaceNone})

	stateKey, os = m.BorrowByNamespacedName(types.NamespacedName{Name: "zwei", Namespace: metav1.NamespaceNone})
	require.NotNil(t, os)
	require.Equal(t, "two", stateKey)
	require.Equal(t, "dos", os.tag)
	require.Equal(t, 1, os.count)
}
