package maps

import (
	"fmt"
	"io"
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

// UpdateChangingSecondKey() is like Update(), except it will make the entry available using a new second key value.
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

func (m *DualKeyMap[K1, K2, V]) DebugDump(w io.Writer) {
	fmt.Fprintf(w, "{\n")

	m.Range(func(k1 K1, k2 K2, val V) bool {
		fmt.Fprint(w, " (")
		fmt.Fprintf(w, "%v", k1)
		fmt.Fprint(w, ", ")
		fmt.Fprintf(w, "%v", k2)
		fmt.Fprint(w, "): ")
		fmt.Fprintf(w, "%v,\n", val)
		return true
	})

	fmt.Fprint(w, "}")
}

func (m *DualKeyMap[K1, K2, V]) Range(f func(k1 K1, k2 K2, val V) bool) {
	for k1, entry := range m.firstMap {
		k2 := entry.k2
		val := entry.val

		if !f(k1, k2, val) {
			break
		}
	}
}

func (m *DualKeyMap[K1, K2, V]) Clear() {
	clear(m.firstMap)
	clear(m.secondMap)
}
