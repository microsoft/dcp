package slices

import (
	"fmt"
	"sync"

	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

func Contains[T comparable](ss []T, s T) bool {
	return Index(ss, s) != -1
}

func Index[T comparable](ss []T, s T) int {
	for i, b := range ss {
		if b == s {
			return i
		}
	}
	return -1
}

// This is "naive" algorithm that works well for short sequences (less than 5 elements),
// which is our main use case.
// If a need arises, we could switch this to Knuth-Morris-Pratt algorithm.
func SeqIndex[T comparable](ss []T, seq []T) int {
	if len(ss) == 0 || len(seq) == 0 || len(seq) > len(ss) {
		return -1
	}

	for i := 0; i <= len(ss)-len(seq); i++ {
		match := true

		for j := 0; j < len(seq); j++ {
			if ss[i+j] != seq[j] {
				match = false
				break
			}
		}

		if match {
			return i
		}
	}

	return -1
}

func StartsWith[T comparable](ss []T, prefix []T) bool {
	return SeqIndex(ss, prefix) == 0
}

type MapFunc[T any, R any] interface {
	~func(int, T) R | ~func(T) R | ~func(int, *T) R | ~func(*T) R
}

func Map[T any, R any, MF MapFunc[T, R]](ss []T, mapping MF) []R {
	if len(ss) == 0 {
		return nil
	}
	f := func(i int, s T) R {
		switch tf := (any)(mapping).(type) {
		case func(int, T) R:
			return tf(i, s)
		case func(T) R:
			return tf(s)
		case func(int, *T) R:
			return tf(i, &s)
		case func(*T) R:
			return tf(&s)
		default:
			panic(fmt.Sprintf("Map cannot understand function type %T", mapping))
		}
	}

	res := make([]R, len(ss))
	for i, s := range ss {
		res[i] = f(i, s)
	}
	return res
}

const MaxConcurrency = uint16(0)

// MapConcurrent will call the mapping functions concurrently, up to specified concurrency level.
// If concurrency == 0 (MaxConcurrency), every function will be called using separate goroutine.
func MapConcurrent[T any, R any, MF MapFunc[T, R]](ss []T, mapping MF, concurrency uint16) []R {
	if len(ss) == 0 {
		return nil
	}

	f := func(i int, s T) R {
		switch tf := (any)(mapping).(type) {
		case func(int, T) R:
			return tf(i, s)
		case func(T) R:
			return tf(s)
		case func(int, *T) R:
			return tf(i, &s)
		case func(*T) R:
			return tf(&s)
		default:
			panic(fmt.Sprintf("MapConcurrent cannot understand function type %T", mapping))
		}
	}

	var res syncmap.Map[int, R]
	var wg sync.WaitGroup
	wg.Add(len(ss))

	if concurrency == 0 {
		for i, s := range ss {
			go func(i int, s T) {
				r := f(i, s)
				res.Store(i, r)
				wg.Done()
			}(i, s)
		}
	} else {
		sem := make(chan struct{}, concurrency)
		for i, s := range ss {
			sem <- struct{}{} // Will block if attempting to start more goroutines than concurrency level (semaphore semantics).
			go func(i int, s T) {
				r := f(i, s)
				res.Store(i, r)
				<-sem
				wg.Done()
			}(i, s)
		}
	}

	wg.Wait()

	retval := make([]R, len(ss))
	res.Range(func(i int, r R) bool {
		retval[i] = r
		return true
	})
	return retval
}

type SelectFunc[T any] interface {
	~func(int, T) bool | ~func(T) bool | ~func(int, *T) bool | ~func(*T) bool
}

func Select[T any, SF SelectFunc[T]](ss []T, selector SF) []T {
	return accumulateIf[T, SF, []T](ss, selector, func(ss []T, el T) []T {
		return append(ss, el)
	})
}

func LenIf[T any, SF SelectFunc[T]](ss []T, selector SF) int {
	return accumulateIf[T, SF, int](ss, selector, func(currentCount int, _ T) int {
		return currentCount + 1
	})
}

func NonEmpty[E any, T ~string | []E](ss []T) []T {
	return Select(ss, func(e *T) bool { return len(*e) > 0 })
}

func All[T any, SF SelectFunc[T]](ss []T, selector SF) bool {
	return LenIf(ss, selector) == len(ss)
}

func Any[T any, SF SelectFunc[T]](ss []T, selector SF) bool {
	sf := func(i int, s T) bool {
		switch tsf := (any)(selector).(type) {
		case func(int, T) bool:
			return tsf(i, s)
		case func(T) bool:
			return tsf(s)
		case func(int, *T) bool:
			return tsf(i, &s)
		case func(*T) bool:
			return tsf(&s)
		default:
			panic(fmt.Sprintf("accumulateIf cannot understand selector function type %T", selector))
		}
	}

	if len(ss) == 0 {
		return false
	}

	for i, s := range ss {
		if sf(i, s) {
			return true
		}
	}
	return false
}

type accumulatorFunc[T any, R any] interface {
	~func(R, T) R | ~func(R, *T) R
}

func accumulateIf[T any, SF SelectFunc[T], R any, AF accumulatorFunc[T, R]](ss []T, selector SF, accumulator AF) R {
	sf := func(i int, s T) bool {
		switch tsf := (any)(selector).(type) {
		case func(int, T) bool:
			return tsf(i, s)
		case func(T) bool:
			return tsf(s)
		case func(int, *T) bool:
			return tsf(i, &s)
		case func(*T) bool:
			return tsf(&s)
		default:
			panic(fmt.Sprintf("accumulateIf cannot understand selector function type %T", selector))
		}
	}

	af := func(current R, el T) R {
		switch taf := (any)(accumulator).(type) {
		case func(R, T) R:
			return taf(current, el)
		case func(R, *T) R:
			return taf(current, &el)
		default:
			panic(fmt.Sprintf("accumulateIf cannot understand accumulator function type %T", accumulator))
		}
	}

	res := *new(R)
	if len(ss) == 0 {
		return res
	}

	for i, s := range ss {
		if sf(i, s) {
			res = af(res, s)
		}
	}
	return res
}
