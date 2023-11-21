package maps

import (
	"sync"
)

// A dual-key map, is a variation of the map data structure where the key is a two-part tuple (k1, k2).
// The key parts form a one-to-one relationship, i.e. either key uniquely identifies the other key,
// and the data stored in the map.

type dualKeyMapEntry[K1 comparable, K2 comparable, V any] struct {
	k1  K1
	k2  K2
	val V
}

type DualKeyMap[K1 comparable, K2 comparable, V any] struct {
	firstMap  map[K1]*dualKeyMapEntry[K1, K2, V]
	secondMap map[K2]*dualKeyMapEntry[K1, K2, V]
}

func NewDualKeyMap[K1 comparable, K2 comparable, V any]() *DualKeyMap[K1, K2, V] {
	return &DualKeyMap[K1, K2, V]{
		firstMap:  make(map[K1]*dualKeyMapEntry[K1, K2, V]),
		secondMap: make(map[K2]*dualKeyMapEntry[K1, K2, V]),
	}
}

func (m *DualKeyMap[K1, K2, V]) Store(k1 K1, k2 K2, val V) {
	entry := dualKeyMapEntry[K1, K2, V]{k1, k2, val}
	m.firstMap[k1] = &entry
	m.secondMap[k2] = &entry
}

// Update() is like Store(), except it fails if the entry is not already present (for both keys).
func (m *DualKeyMap[K1, K2, V]) Update(k1 K1, k2 K2, val V) bool {
	_, found := m.firstMap[k1]
	if !found {
		return false
	}
	_, found = m.secondMap[k2]
	if !found {
		return false
	}
	m.Store(k1, k2, val)
	return true
}

// UpdateChangingFirstKey() is like Update(), except it will make the entry available using a new first key value.
func (m *DualKeyMap[K1, K2, V]) UpdateChangingFirstKey(k1 K1, newk1 K1, k2 K2, val V) bool {
	_, found := m.firstMap[k1]
	if !found {
		return false
	}
	_, found = m.secondMap[k2]
	if !found {
		return false
	}

	delete(m.firstMap, k1)
	m.Store(newk1, k2, val)
	return true
}

// UpdateChangingSecondKey() is like Update(), execpt it will make the entry available using a new second key value.
func (m *DualKeyMap[K1, K2, V]) UpdateChangingSecondKey(k1 K1, k2 K2, newk2 K2, val V) bool {
	_, found := m.firstMap[k1]
	if !found {
		return false
	}
	_, found = m.secondMap[k2]
	if !found {
		return false
	}

	delete(m.secondMap, k2)
	m.Store(k1, newk2, val)
	return true
}

func (m *DualKeyMap[K1, K2, V]) FindByFirstKey(k1 K1) (K2, V, bool) {
	entry, found := m.firstMap[k1]
	if found {
		return entry.k2, entry.val, true
	} else {
		return *new(K2), *new(V), false
	}
}

func (m *DualKeyMap[K1, K2, V]) FindBySecondKey(k2 K2) (K1, V, bool) {
	entry, found := m.secondMap[k2]
	if found {
		return entry.k1, entry.val, true
	} else {
		return *new(K1), *new(V), false
	}
}

func (m *DualKeyMap[K1, K2, V]) DeleteByFirstKey(k1 K1) {
	entry, found := m.firstMap[k1]
	if found {
		delete(m.firstMap, k1)
		delete(m.secondMap, entry.k2)
	}
}

func (m *DualKeyMap[K1, K2, V]) DeleteBySecondKey(k2 K2) {
	entry, found := m.secondMap[k2]
	if found {
		delete(m.firstMap, entry.k1)
		delete(m.secondMap, k2)
	}
}

type DeferredMapOperation[K1 comparable, K2 comparable, V any] func(*DualKeyMap[K1, K2, V])

// The SynchronizedDualKeyMap is a variation of DualKeyMap that uses an read/write mutex to make it goroutine-safe.
// It is a naive implementation, but sufficient for our purposes.
// SynchronizedDualKeyMap also adds the concept of "deferred operations", which are operations that are queued
// and associated with specific key pair. The operations (if any exist) can be run explicitly via the RunDeferredOps() method.
type SynchronizedDualKeyMap[K1 comparable, K2 comparable, V any] struct {
	inner       *DualKeyMap[K1, K2, V]
	lock        *sync.RWMutex
	deferredOps *DualKeyMap[K1, K2, []DeferredMapOperation[K1, K2, V]]
}

func NewSynchronizedDualKeyMap[K1 comparable, K2 comparable, V any]() *SynchronizedDualKeyMap[K1, K2, V] {
	return &SynchronizedDualKeyMap[K1, K2, V]{
		inner:       NewDualKeyMap[K1, K2, V](),
		lock:        new(sync.RWMutex),
		deferredOps: NewDualKeyMap[K1, K2, []DeferredMapOperation[K1, K2, V]](),
	}
}

func (m *SynchronizedDualKeyMap[K1, K2, V]) Store(k1 K1, k2 K2, val V) {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.inner.Store(k1, k2, val)
}

func (m *SynchronizedDualKeyMap[K1, K2, V]) UpdateChangingFirstKey(k1 K1, newk1 K1, k2 K2, val V) bool {
	m.lock.Lock()
	defer m.lock.Unlock()

	return m.inner.UpdateChangingFirstKey(k1, newk1, k2, val)
}

func (m *SynchronizedDualKeyMap[K1, K2, V]) UpdateChangingSecondKey(k1 K1, k2 K2, newk2 K2, val V) bool {
	m.lock.Lock()
	defer m.lock.Unlock()

	return m.inner.UpdateChangingSecondKey(k1, k2, newk2, val)
}

func (m *SynchronizedDualKeyMap[K1, K2, V]) Update(k1 K1, k2 K2, val V) bool {
	m.lock.Lock()
	defer m.lock.Unlock()

	return m.inner.Update(k1, k2, val)
}

func (m *SynchronizedDualKeyMap[K1, K2, V]) FindByFirstKey(k1 K1) (K2, V, bool) {
	m.lock.RLock()
	defer m.lock.RUnlock()

	return m.inner.FindByFirstKey(k1)
}

func (m *SynchronizedDualKeyMap[K1, K2, V]) FindBySecondKey(k2 K2) (K1, V, bool) {
	m.lock.RLock()
	defer m.lock.RUnlock()

	return m.inner.FindBySecondKey(k2)
}

func (m *SynchronizedDualKeyMap[K1, K2, V]) DeleteByFirstKey(k1 K1) {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.inner.DeleteByFirstKey(k1)
}

func (m *SynchronizedDualKeyMap[K1, K2, V]) DeleteBySecondKey(k2 K2) {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.inner.DeleteBySecondKey(k2)
}

func (m *SynchronizedDualKeyMap[K1, K2, V]) QueueDeferredOp(k1 K1, op DeferredMapOperation[K1, K2, V]) bool {
	m.lock.Lock()
	defer m.lock.Unlock()

	k2, _, found := m.inner.FindByFirstKey(k1)
	if !found {
		return false
	}

	_, ops, found := m.deferredOps.FindByFirstKey(k1)
	if !found {
		ops = nil
	}

	ops = append(ops, op)
	m.deferredOps.Store(k1, k2, ops)

	return true
}

func (m *SynchronizedDualKeyMap[K1, K2, V]) RunDeferredOps(k1 K1) {
	m.lock.Lock()
	defer m.lock.Unlock()

	_, ops, found := m.deferredOps.FindByFirstKey(k1)
	if !found || len(ops) == 0 {
		return // Nothing to do
	}

	for _, op := range ops {
		op(m.inner)
	}

	// We have run these ops, so clear the queue.
	m.deferredOps.DeleteByFirstKey(k1)
}
