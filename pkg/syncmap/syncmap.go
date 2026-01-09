/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package syncmap is a generic wrapper over standard library sync.Map

package syncmap

import "sync"

func zero[T any]() T {
	return *new(T)
}

type Map[Key comparable, Value any] sync.Map

func (m *Map[Key, Value]) syncMap() *sync.Map {
	return (*sync.Map)(m)
}

func (m *Map[Key, Value]) Store(key Key, value Value) {
	m.syncMap().Store(key, value)
}

// Returns the value stored in the map (if found), and a boolean indicating whether the value was found.
func (m *Map[Key, Value]) Load(key Key) (Value, bool) {
	anyValue, found := m.syncMap().Load(key)
	if !found {
		return zero[Value](), false
	} else {
		return zeroIfNil[Value](anyValue), true
	}
}

// Deletes the value for the passed key.
// If the key has no corresponding value, the map is unchanged.
func (m *Map[Key, Value]) Delete(key Key) {
	m.syncMap().Delete(key)
}

// Calls passed function foreach key-value pair in the map.
// If the function returns false, the iteration stops.
func (m *Map[Key, Value]) Range(f func(key Key, value Value) bool) {
	m.syncMap().Range(func(key, value any) bool {
		return f(key.(Key), zeroIfNil[Value](value))
	})
}

// Loads and returns the value for for the passed key.
// If the key has no corresponding value, the newValue is stored and returned.
// The returned boolean is true if the value was already in the map, and false if the new value was stored.
func (m *Map[Key, Value]) LoadOrStore(key Key, newValue Value) (Value, bool) {
	actual, found := m.syncMap().LoadOrStore(key, newValue)
	return zeroIfNil[Value](actual), found
}

// Loads and returns the value for for the passed key.
// If the key has no corresponding value, a new value is created using the passed valueFactory, then stored and returned.
// The returned boolean is true if the value was already in the map, and false if the new value was stored.
//
// Note: in high-contention conditions the value factory function might be called even if the key is already in the map.
// If this happens, the existing value is returned and the new value is discarded.
func (m *Map[Key, Value]) LoadOrStoreNew(key Key, valueFactory func() Value) (Value, bool) {
	// Naive implementation, but it will do for our purposes
	val, found := m.Load(key)
	if found {
		return val, true
	}
	return m.LoadOrStore(key, valueFactory())
}

// Loads and deletes the value for the passed key.
// If the key has no corresponding value, the map is unchanged and the returned boolean is false.
func (m *Map[Key, Value]) LoadAndDelete(key Key) (Value, bool) {
	anyValue, found := m.syncMap().LoadAndDelete(key)
	if !found {
		return zero[Value](), false
	} else {
		return zeroIfNil[Value](anyValue), true
	}
}

// Returns true if the map is empty.
// Note that this is point-in-time check, and the map might be modified immediately after this method returns.
func (m *Map[Key, Value]) Empty() bool {
	empty := true
	m.syncMap().Range(func(_, _ any) bool {
		empty = false
		return false
	})
	return empty
}

func zeroIfNil[T any](v any) T {
	if v == nil {
		return zero[T]()
	} else {
		return v.(T)
	}
}
