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

func (m *Map[Key, Value]) Load(key Key) (value Value, ok bool) {
	anyValue, ok := m.syncMap().Load(key)
	if !ok {
		return zero[Value](), false
	} else {
		return zeroIfNil[Value](anyValue), true
	}
}

func (m *Map[Key, Value]) Delete(key Key) {
	m.syncMap().Delete(key)
}

func (m *Map[Key, Value]) Range(f func(key Key, value Value) bool) {
	m.syncMap().Range(func(key, value any) bool {
		return f(key.(Key), zeroIfNil[Value](value))
	})
}

func (m *Map[Key, Value]) LoadOrStore(key Key, value Value) (Value, bool) {
	actual, loaded := m.syncMap().LoadOrStore(key, value)
	return zeroIfNil[Value](actual), loaded
}

func (m *Map[Key, Value]) LoadOrStoreNew(key Key, valueFactory func() Value) (Value, bool) {
	// Naive implementation, but it will do for our purposes
	val, found := m.Load(key)
	if found {
		return val, true
	}
	return m.LoadOrStore(key, valueFactory())
}

func (m *Map[Key, Value]) LoadAndDelete(key Key) (Value, bool) {
	anyValue, ok := m.syncMap().LoadAndDelete(key)
	if !ok {
		return zero[Value](), false
	} else {
		return zeroIfNil[Value](anyValue), true
	}
}

func zeroIfNil[T any](v any) T {
	if v == nil {
		return zero[T]()
	} else {
		return v.(T)
	}
}
