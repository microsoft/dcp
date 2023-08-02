package maps

import "sync"

// A dual-key map, is a variation of the map data structure where the key is a two-part tuple (k1, k2).
// The key parts form a one-to-one relationship, i.e. either key uniquely identifies the other key,
// and the data stored in the map.

type firstKeyEntry[K2 comparable, V any] struct {
	k2  K2
	val V
}

type secondKeyEntry[K1 comparable, V any] struct {
	k1  K1
	val V
}

type DualKeyMap[K1 comparable, K2 comparable, V any] struct {
	firstMap  map[K1]firstKeyEntry[K2, V]
	secondMap map[K2]secondKeyEntry[K1, V]
}

func NewDualKeyMap[K1 comparable, K2 comparable, V any]() *DualKeyMap[K1, K2, V] {
	return &DualKeyMap[K1, K2, V]{
		firstMap:  make(map[K1]firstKeyEntry[K2, V]),
		secondMap: make(map[K2]secondKeyEntry[K1, V]),
	}
}

func (m *DualKeyMap[K1, K2, V]) Store(k1 K1, k2 K2, val V) {
	m.firstMap[k1] = firstKeyEntry[K2, V]{k2, val}
	m.secondMap[k2] = secondKeyEntry[K1, V]{k1, val}
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

// The SynchronizedDualKeyMap is a variation of DualKeyMap that uses an read/write mutex to make it goroutine-safe.
// It is a naive implementation, but sufficient for our purposes.
type SynchronizedDualKeyMap[K1 comparable, K2 comparable, V any] struct {
	inner *DualKeyMap[K1, K2, V]
	lock  *sync.RWMutex
}

func NewSynchronizedDualKeyMap[K1 comparable, K2 comparable, V any]() *SynchronizedDualKeyMap[K1, K2, V] {
	return &SynchronizedDualKeyMap[K1, K2, V]{
		inner: NewDualKeyMap[K1, K2, V](),
		lock:  new(sync.RWMutex),
	}
}

func (m *SynchronizedDualKeyMap[K1, K2, V]) Store(k1 K1, k2 K2, val V) {
	m.lock.Lock()
	defer m.lock.Unlock()
	m.inner.Store(k1, k2, val)
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
