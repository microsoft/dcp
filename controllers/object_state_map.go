/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"

	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/maps"
)

type Cloner[T any] interface {
	// Performs a deep copy of the object (to the extent necessary).
	Clone() T
}

type UpdateableFrom[T any] interface {
	// Updates the object from another object of the same type.
	// Returns true if the target object was updated in any way, false otherwise.
	UpdateFrom(other T) bool
}

type PInMemoryObjectState[IMOS any] interface {
	*IMOS
	Cloner[*IMOS]
	UpdateableFrom[*IMOS]
}

type DeferredMapOperation[StateKeyT comparable, PObj commonapi.DcpModelObject] func(types.NamespacedName, StateKeyT, PObj)

// ObjectStateMap is a specialized map that stores state of Kubernetes objects.
// It is optimized for mixed use from controller reconciliation loops
// and event handlers/worker gorooutines running outside of reconciliation loops.

type ObjectStateMap[StateKeyT comparable, OS any, POS PInMemoryObjectState[OS], PObj commonapi.DcpModelObject] struct {
	inner       *maps.DualKeyMap[types.NamespacedName, StateKeyT, POS]
	lock        *sync.RWMutex
	deferredOps *maps.DualKeyMap[types.NamespacedName, StateKeyT, []DeferredMapOperation[StateKeyT, PObj]]
}

func NewObjectStateMap[StateKeyT comparable, OS any, POS PInMemoryObjectState[OS], PObj commonapi.DcpModelObject]() *ObjectStateMap[StateKeyT, OS, POS, PObj] {
	return &ObjectStateMap[StateKeyT, OS, POS, PObj]{
		inner:       maps.NewDualKeyMap[types.NamespacedName, StateKeyT, POS](),
		lock:        new(sync.RWMutex),
		deferredOps: maps.NewDualKeyMap[types.NamespacedName, StateKeyT, []DeferredMapOperation[StateKeyT, PObj]](),
	}
}

// Returns a clone of the object state for the given namespaced name.
// If the object state is not found, the second return value will be nil.
func (m *ObjectStateMap[StateKeyT, OS, POS, PObj]) BorrowByNamespacedName(namespaceName types.NamespacedName) (StateKeyT, POS) {
	m.lock.RLock()
	defer m.lock.RUnlock()

	k2, os, found := m.inner.FindByFirstKey(namespaceName)
	if !found {
		return *new(StateKeyT), nil
	}

	return k2, os.Clone()
}

// Returns a clone of the object state for the given a state key.
// If the object state is not found, the second return value will be nil.
func (m *ObjectStateMap[StateKeyT, OS, POS, PObj]) BorrowByStateKey(stateKey StateKeyT) (types.NamespacedName, POS) {
	m.lock.RLock()
	defer m.lock.RUnlock()

	name, os, found := m.inner.FindBySecondKey(stateKey)
	if !found {
		return types.NamespacedName{}, nil
	}

	return name, os.Clone()
}

// Stores the object state for the given namespaced name and state key,
// unconditionally overwriting any existing state.
func (m *ObjectStateMap[StateKeyT, OS, POS, PObj]) Store(namespaceName types.NamespacedName, k2 StateKeyT, pos POS) {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.inner.Store(namespaceName, k2, pos)
}

// StoreIfStateKeyUnclaimed stores object state unless another object already owns the state key.
// It returns the current owner and whether the state was stored.
func (m *ObjectStateMap[StateKeyT, OS, POS, PObj]) StoreIfStateKeyUnclaimed(namespaceName types.NamespacedName, stateKey StateKeyT, pos POS) (types.NamespacedName, bool) {
	m.lock.Lock()
	defer m.lock.Unlock()

	currentOwner, _, found := m.inner.FindBySecondKey(stateKey)
	if found && currentOwner != namespaceName {
		return currentOwner, false
	}

	m.inner.Store(namespaceName, stateKey, pos)
	return namespaceName, true
}

// Updates the object state for the given namespaced name and state key.
// The operation fails (returning false) if the object state is not found using either key,
// or if no changes have been made to the object (UpdateFrom() returned false).
func (m *ObjectStateMap[StateKeyT, OS, POS, PObj]) Update(namespaceName types.NamespacedName, stateKey StateKeyT, pos POS) bool {
	m.lock.Lock()
	defer m.lock.Unlock()

	currentStateKey, current, found := m.inner.FindByFirstKey(namespaceName)
	if !found || currentStateKey != stateKey {
		return false
	}

	updated := POS(current.Clone())
	if madeChanges := updated.UpdateFrom(pos); !madeChanges {
		return false
	}

	m.inner.Store(namespaceName, stateKey, updated)
	return true
}

// UpdateByNamespacedName() is like Update(), but applies the update under whichever state key the
// object state is currently stored with. Use it when the caller has no state key of its own to
// assert; callers that must detect a state key change should use Update() instead.
func (m *ObjectStateMap[StateKeyT, OS, POS, PObj]) UpdateByNamespacedName(namespaceName types.NamespacedName, pos POS) bool {
	m.lock.Lock()
	defer m.lock.Unlock()

	currentStateKey, current, found := m.inner.FindByFirstKey(namespaceName)
	if !found {
		return false
	}

	updated := POS(current.Clone())
	if madeChanges := updated.UpdateFrom(pos); !madeChanges {
		return false
	}

	m.inner.Store(namespaceName, currentStateKey, updated)
	return true
}

// UpdateChangingStateKey() is like Update(), with the additional effect of changing the state key
// the object state is stored under.
func (m *ObjectStateMap[StateKeyT, OS, POS, PObj]) UpdateChangingStateKey(namespaceName types.NamespacedName, oldStateKey StateKeyT, newStateKey StateKeyT, pos POS) bool {
	m.lock.Lock()
	defer m.lock.Unlock()

	currentStateKey, current, found := m.inner.FindByFirstKey(namespaceName)
	if !found || currentStateKey != oldStateKey {
		return false
	}

	updated := POS(current.Clone())
	if madeChanges := updated.UpdateFrom(pos); !madeChanges {
		return false
	}

	m.inner.DeleteByFirstKey(namespaceName)
	m.inner.Store(namespaceName, newStateKey, updated)
	return true
}

// UpdateChangingStateKeyIfUnclaimed is like UpdateChangingStateKey, but does not replace another
// object's ownership of the new state key. It returns the current owner and whether the update was
// applied.
func (m *ObjectStateMap[StateKeyT, OS, POS, PObj]) UpdateChangingStateKeyIfUnclaimed(
	namespaceName types.NamespacedName,
	oldStateKey StateKeyT,
	newStateKey StateKeyT,
	pos POS,
) (types.NamespacedName, bool) {
	m.lock.Lock()
	defer m.lock.Unlock()

	currentStateKey, current, found := m.inner.FindByFirstKey(namespaceName)
	if !found || currentStateKey != oldStateKey {
		return types.NamespacedName{}, false
	}

	currentOwner, _, claimed := m.inner.FindBySecondKey(newStateKey)
	if claimed && currentOwner != namespaceName {
		return currentOwner, false
	}

	updated := POS(current.Clone())
	madeChanges := updated.UpdateFrom(pos)
	if !madeChanges && newStateKey == oldStateKey {
		return namespaceName, false
	}

	m.inner.DeleteByFirstKey(namespaceName)
	m.inner.Store(namespaceName, newStateKey, updated)
	return namespaceName, true
}

// DeleteByNamespacedName() deletes the object state and queued deferred operations for the given namespaced name.
func (m *ObjectStateMap[StateKeyT, OS, POS, PObj]) DeleteByNamespacedName(namespaceName types.NamespacedName) {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.inner.DeleteByFirstKey(namespaceName)
	m.deferredOps.DeleteByFirstKey(namespaceName)
}

// QueueDeferredOp() queues a deferred operation to be run later (by calling RunDeferredOps()).
// The operation fails (returning false) if the object state is not found using the given namespaced name.
func (m *ObjectStateMap[StateKeyT, OS, POS, PObj]) QueueDeferredOp(namespaceName types.NamespacedName, op DeferredMapOperation[StateKeyT, PObj]) bool {
	m.lock.Lock()
	defer m.lock.Unlock()

	stateKey, _, found := m.inner.FindByFirstKey(namespaceName)
	if !found {
		return false
	}

	return m.queueDeferredOpLocked(namespaceName, stateKey, op)
}

// QueueDeferredOpForStateKey() queues a deferred operation to be run later, but only if the
// object state is still stored with the expected state key.
func (m *ObjectStateMap[StateKeyT, OS, POS, PObj]) QueueDeferredOpForStateKey(
	namespaceName types.NamespacedName,
	stateKey StateKeyT,
	op DeferredMapOperation[StateKeyT, PObj],
) bool {
	m.lock.Lock()
	defer m.lock.Unlock()

	return m.queueDeferredOpLocked(namespaceName, stateKey, op)
}

func (m *ObjectStateMap[StateKeyT, OS, POS, PObj]) queueDeferredOpLocked(
	namespaceName types.NamespacedName,
	stateKey StateKeyT,
	op DeferredMapOperation[StateKeyT, PObj],
) bool {
	currentStateKey, _, found := m.inner.FindByFirstKey(namespaceName)
	if !found || currentStateKey != stateKey {
		return false
	}

	deferredStateKey, ops, opsFound := m.deferredOps.FindByFirstKey(namespaceName)
	if !opsFound || deferredStateKey != stateKey {
		ops = nil
	}

	ops = append(ops, op)
	m.deferredOps.Store(namespaceName, stateKey, ops)
	return true
}

// RunDeferredOps() runs all deferred operations for the given namespaced name.
func (m *ObjectStateMap[StateKeyT, OS, POS, PObj]) RunDeferredOps(namespaceName types.NamespacedName, obj PObj) {
	m.lock.Lock()

	stateKey, ops, found := m.deferredOps.FindByFirstKey(namespaceName)
	if !found || len(ops) == 0 {
		m.lock.Unlock()
		return // Nothing to do
	} else {
		m.deferredOps.DeleteByFirstKey(namespaceName)

		// By releasing the lock here, we allow other operations to call any of the public methods as necessary.
		m.lock.Unlock()
	}

	for _, op := range ops {
		op(namespaceName, stateKey, obj)
	}
}

// Clear() removes all object states from the map.
func (m *ObjectStateMap[StateKeyT, OS, POS, PObj]) Clear() {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.inner.Clear()
	m.deferredOps.Clear()
}

// Range() iterates over all object states in the map and calls the given function for each one.
// Every instance presented to the function is a clone of the object state.
func (m *ObjectStateMap[StateKeyT, OS, POS, PObj]) Range(f func(types.NamespacedName, StateKeyT, POS) bool) {
	m.lock.RLock()
	type mapEntry struct {
		n   types.NamespacedName
		sk  StateKeyT
		pos POS
	}
	var entries []mapEntry
	m.inner.Range(func(n types.NamespacedName, sk StateKeyT, pos POS) bool {
		entries = append(entries, mapEntry{n, sk, pos.Clone()})
		return true
	})
	m.lock.RUnlock()

	// Now we can iterate over the entries without holding the lock,
	// effectively iterating over a snapshot of the map.
	for _, entry := range entries {
		if !f(entry.n, entry.sk, entry.pos) {
			break
		}
	}
}
