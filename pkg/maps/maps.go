package maps

import (
	"fmt"
	stdlib_maps "maps"
)

type MapFunc[K comparable, V any, T any] interface {
	~func(K, V) T | ~func(V) T
}

// Transforms a map[K]V into a map[K]T using given mapping function.
// Keys are preserved, but values are replaced with the result of the mapping
func Map[K comparable, V any, T any, MF MapFunc[K, V, T]](m map[K]V, mapping MF) map[K]T {
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

	res := make(map[K]T, len(m))
	for k, v := range m {
		res[k] = f(k, v)
	}
	return res
}

// Maps a map to a slice. Note that iteration order over a map is not defined,
// so make no assumptions about the order of the items in the resulting slice.
func MapToSlice[K comparable, V any, T any, MF MapFunc[K, V, T]](m map[K]V, mapping MF) []T {
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

// Maps a slice to a map. The slice is processed in order, and entries produced by later elements
// may override entries produced by earlier elements (if keys are equal).
// So in general case it should NOT be assumed that the resulting map will contain as many elements
// as the original slice.
type KeyValFunc[T any, K comparable, V any] interface {
	~func(T) (K, V)
}

func SliceToMap[T any, K comparable, V any, KVF KeyValFunc[T, K, V]](s []T, f KVF) map[K]V {
	if len(s) == 0 {
		return nil
	}

	res := make(map[K]V, len(s))
	for _, t := range s {
		k, v := f(t)
		res[k] = v
	}
	return res
}

type SelectFunc[K comparable, V any] interface {
	~func(K) bool | ~func(K, V) bool
}

func Select[K comparable, V any, SF SelectFunc[K, V]](m map[K]V, selector SF) map[K]V {
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

func Keys[K comparable, V any](m map[K]V) []K {
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

func Values[K comparable, V any](m map[K]V) []V {
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

func Apply[K comparable, V any](m1 map[K]V, m2 map[K]V) map[K]V {
	if len(m1) == 0 {
		return m2
	}

	retval := stdlib_maps.Clone(m1)
	for k, v := range m2 {
		retval[k] = v
	}
	return retval
}
