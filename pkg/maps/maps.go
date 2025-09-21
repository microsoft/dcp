package maps

import (
	"fmt"
	"iter"
	stdlib_maps "maps"
)

type MapFunc[K comparable, V any, T any] interface {
	~func(K, V) T | ~func(V) T
}

// Transforms a map[K]V into a map[K]T using given mapping function.
// Keys are preserved, but values are replaced with the result of the mapping
func Map[K comparable, V any, T any, MF MapFunc[K, V, T], MM ~map[K]V](m MM, mapping MF) map[K]T {
	if m == nil {
		return nil
	}

	res := make(map[K]T, len(m))
	if len(m) == 0 {
		return res
	}

	f := func(k K, v V) T {
		switch tf := (any)(mapping).(type) {
		case func(K, V) T:
			return tf(k, v)
		case func(V) T:
			return tf(v)
		default:
			panic(fmt.Sprintf("Map cannot understand function type %T", mapping))
		}
	}

	for k, v := range m {
		res[k] = f(k, v)
	}

	return res
}

// Maps a map to a slice. Note that iteration order over a map is not defined,
// so make no assumptions about the order of the items in the resulting slice.
func MapToSlice[T any, K comparable, V any, MF MapFunc[K, V, T], MM ~map[K]V](m MM, mapping MF) []T {
	if len(m) == 0 {
		return nil
	}

	f := func(k K, v V) T {
		switch tf := (any)(mapping).(type) {
		case func(K, V) T:
			return tf(k, v)
		case func(V) T:
			return tf(v)
		default:
			panic(fmt.Sprintf("Map cannot understand function type %T", mapping))
		}
	}

	res := make([]T, len(m))
	i := 0
	for k, v := range m {
		res[i] = f(k, v)
		i++
	}

	return res
}

type KeyValFunc[T any, K comparable, V any] interface {
	~func(T) (K, V)
}

// Maps a slice to a map. The slice is processed in order, and entries produced by later elements
// may override entries produced by earlier elements (if keys are equal).
// So in general case it should NOT be assumed that the resulting map will contain as many elements
// as the original slice.
// SliceToMap(nil,..) returns an empty map, not nil. This is to make the code using this function less error-prone.
// Nil slices are quite usable (e.g. appending to nil slice works fine). Not so with nil maps.
func SliceToMap[T any, K comparable, V any, KVF KeyValFunc[T, K, V], S ~[]T](s S, f KVF) map[K]V {
	res := make(map[K]V, len(s))
	if len(s) == 0 {
		return res
	}

	for _, t := range s {
		k, v := f(t)
		res[k] = v
	}

	return res
}

type SelectFunc[K comparable, V any] interface {
	~func(K) bool | ~func(K, V) bool
}

func Select[K comparable, V any, SF SelectFunc[K, V], MM ~map[K]V](m MM, selector SF) MM {
	f := func(k K, v V) bool {
		switch tf := (any)(selector).(type) {
		case func(K) bool:
			return tf(k)
		case func(K, V) bool:
			return tf(k, v)
		default:
			panic(fmt.Sprintf("Select cannot understand function type %T", selector))
		}
	}

	res := make(map[K]V)
	for k, v := range m {
		if f(k, v) {
			res[k] = v
		}
	}
	return res
}

func Keys[K comparable, V any, M ~map[K]V](m M) []K {
	if len(m) == 0 {
		return nil
	}

	res := make([]K, len(m))
	i := 0
	for k := range m {
		res[i] = k
		i++
	}
	return res
}

func Values[K comparable, V any, M ~map[K]V](m M) []V {
	if len(m) == 0 {
		return nil
	}

	res := make([]V, len(m))
	i := 0
	for _, v := range m {
		res[i] = v
		i++
	}
	return res
}

// Takes a "map of maps", extracts all the values from the inner maps,
// and then flattens them into a single slice.
func FlattenValues[KO comparable, KI comparable, V any, M ~map[KO]map[KI]V](m M) []V {
	if len(m) == 0 {
		return nil
	}

	// Calculate the total number of elements in all inner maps
	total := 0
	for _, innerMap := range m {
		total += len(innerMap)
	}

	// Preallocate the slice with the calculated capacity
	res := make([]V, 0, total)
	for _, innerMap := range m {
		for _, v := range innerMap {
			res = append(res, v)
		}
	}
	return res
}

func Apply[K comparable, V any, M ~map[K]V](m1 M, m2 M) M {
	if len(m1) == 0 {
		return m2
	}

	retval := stdlib_maps.Clone(m1)
	for k, v := range m2 {
		retval[k] = v
	}
	return retval
}

// Adds key-value pairs from seq to m. If a key in seq already exists in m, its value will be overwritten.
// Returns true if the map was modified, otherwise false.
func Insert[K comparable, V comparable, M ~map[K]V](m M, seq iter.Seq2[K, V]) bool {
	modified := false

	for k, v := range seq {
		if HasExactValue(m, k, v) {
			continue
		} else {
			m[k] = v
			modified = true
		}
	}

	return modified
}

func HasExactValue[K comparable, V comparable, M ~map[K]V](m M, key K, expected V) bool {
	if len(m) == 0 {
		return false
	}

	v, found := m[key]
	if !found {
		return false
	}

	return v == expected
}

func TryGetValidValue[K comparable, V any, M ~map[K]V](m M, key K, validate func(V) bool) (V, bool) {
	if len(m) == 0 {
		return *new(V), false
	}

	v, found := m[key]
	if !found {
		return *new(V), false
	}

	if !validate(v) {
		return *new(V), false
	}

	return v, true
}
