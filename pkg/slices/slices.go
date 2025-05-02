package slices

import (
	"cmp"
	"fmt"
	stdslices "slices"
	"sync"

	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

func Contains[T comparable, S ~[]T](ss S, s T) bool {
	return Index(ss, s) != -1
}

func Index[T comparable, S ~[]T](ss S, s T) int {
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
func SeqIndex[T comparable, S ~[]T](ss S, seq S) int {
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

func StartsWith[T comparable, S ~[]T](ss S, prefix S) bool {
	return SeqIndex(ss, prefix) == 0
}

type MapFunc[T any, R any] interface {
	~func(int, T) R | ~func(T) R | ~func(int, *T) R | ~func(*T) R
}

func Map[T any, R any, MF MapFunc[T, R], S ~[]T](ss S, mapping MF) []R {
	if len(ss) == 0 {
		return nil
	}
	f := func(i int, s *T) R {
		switch tf := (any)(mapping).(type) {
		case func(int, T) R:
			return tf(i, *s)
		case func(T) R:
			return tf(*s)
		case func(int, *T) R:
			return tf(i, s)
		case func(*T) R:
			return tf(s)
		default:
			panic(fmt.Sprintf("Map cannot understand function type %T", mapping))
		}
	}

	res := make([]R, len(ss))
	for i, s := range ss {
		res[i] = f(i, &s)
	}
	return res
}

const MaxConcurrency = uint16(0)

// MapConcurrent will call the mapping functions concurrently, up to specified concurrency level.
// If concurrency == 0 (MaxConcurrency), every function will be called using separate goroutine.
func MapConcurrent[T any, R any, MF MapFunc[T, R], S ~[]T](ss S, mapping MF, concurrency uint16) []R {
	if len(ss) == 0 {
		return nil
	}

	f := func(i int, s *T) R {
		switch tf := (any)(mapping).(type) {
		case func(int, T) R:
			return tf(i, *s)
		case func(T) R:
			return tf(*s)
		case func(int, *T) R:
			return tf(i, s)
		case func(*T) R:
			return tf(s)
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
				defer wg.Done()
				r := f(i, &s)
				res.Store(i, r)
			}(i, s)
		}
	} else {
		sem := make(chan struct{}, concurrency)
		for i, s := range ss {
			sem <- struct{}{} // Will block if attempting to start more goroutines than concurrency level (semaphore semantics).

			go func(i int, s T) {
				defer func() {
					<-sem
					wg.Done()
				}()
				r := f(i, &s)
				res.Store(i, r)
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

func Select[T any, SF SelectFunc[T], S ~[]T](ss S, selector SF) S {
	return AccumulateIf[T, S](ss, selector, func(ss S, el T) S {
		return append(ss, el)
	})
}

func LenIf[T any, SF SelectFunc[T], S ~[]T](ss S, selector SF) int {
	return AccumulateIf[T, int](ss, selector, func(currentCount int, _ T) int {
		return currentCount + 1
	})
}

func NonEmpty[E any, T ~string | ~[]E, S ~[]T](ss S) S {
	return Select(ss, func(e *T) bool { return len(*e) > 0 })
}

func All[T any, SF SelectFunc[T], S ~[]T](ss S, selector SF) bool {
	return LenIf(ss, selector) == len(ss)
}

// IndexFunc returns the index of the first element that matches the selector function.
// The name is not great, but it matches the Go standard library's `slices.IndexFunc` function.
func IndexFunc[T any, SF SelectFunc[T], S ~[]T](ss S, selector SF) int {
	sf := func(i int, s *T) bool {
		switch tsf := (any)(selector).(type) {
		case func(int, T) bool:
			return tsf(i, *s)
		case func(T) bool:
			return tsf(*s)
		case func(int, *T) bool:
			return tsf(i, s)
		case func(*T) bool:
			return tsf(s)
		default:
			panic(fmt.Sprintf("IndexFunc cannot understand selector function type %T", selector))
		}
	}

	if len(ss) == 0 {
		return -1
	}

	for i, s := range ss {
		if sf(i, &s) {
			return i
		}
	}
	return -1
}

func Any[T any, SF SelectFunc[T], S ~[]T](ss S, selector SF) bool {
	return IndexFunc(ss, selector) != -1
}

type AccumulatorFunc[T any, R any] interface {
	~func(R, T) R | ~func(R, *T) R
}

func Accumulate[T any, R any, AF AccumulatorFunc[T, R], S ~[]T](ss S, accumulator AF) R {
	return AccumulateIf[T, R](ss, func(_ T) bool { return true }, accumulator)
}

func AccumulateIf[T any, R any, SF SelectFunc[T], AF AccumulatorFunc[T, R], S ~[]T](ss S, selector SF, accumulator AF) R {
	sf := func(i int, s *T) bool {
		switch tsf := (any)(selector).(type) {
		case func(int, T) bool:
			return tsf(i, *s)
		case func(T) bool:
			return tsf(*s)
		case func(int, *T) bool:
			return tsf(i, s)
		case func(*T) bool:
			return tsf(s)
		default:
			panic(fmt.Sprintf("AccumulateIf cannot understand selector function type %T", selector))
		}
	}

	af := func(current R, el *T) R {
		switch taf := (any)(accumulator).(type) {
		case func(R, T) R:
			return taf(current, *el)
		case func(R, *T) R:
			return taf(current, el)
		default:
			panic(fmt.Sprintf("AccumulateIf cannot understand accumulator function type %T", accumulator))
		}
	}

	res := *new(R)
	if len(ss) == 0 {
		return res
	}

	for i, s := range ss {
		if sf(i, &s) {
			res = af(res, &s)
		}
	}
	return res
}

// Computes set difference between two slices, returning
// a slice of elements that are in `a` but not in `b`,
// and a slice of elements that are in `b` but not in `a`.
func Diff[T cmp.Ordered, S []T](a, b S) (S, S) {
	if len(a) == 0 {
		return nil, b
	}
	if len(b) == 0 {
		return a, nil
	}

	ac := stdslices.Clone(a)
	stdslices.Sort(ac)
	bc := stdslices.Clone(b)
	stdslices.Sort(bc)

	var aMinusB, bMinusA S
	var i, j int

	for i < len(ac) && j < len(bc) {
		switch cmp.Compare(ac[i], bc[j]) {
		case -1:
			aMinusB = append(aMinusB, ac[i])
			i++
		case 0:
			i++
			j++
		case 1:
			bMinusA = append(bMinusA, bc[j])
			j++
		}
	}

	for ; i < len(ac); i++ {
		aMinusB = append(aMinusB, ac[i])
	}

	for ; j < len(bc); j++ {
		bMinusA = append(bMinusA, bc[j])
	}

	return aMinusB, bMinusA
}

type EqualFunc[T any] interface {
	~func(T, T) bool | ~func(*T, *T) bool
}

// Returns a set difference between two slices, using custom equality function.
func DiffFunc[T any, EF EqualFunc[T], S ~[]T](a, b S, equalFunc EF) S {
	if len(a) == 0 {
		return nil
	}
	if len(b) == 0 {
		return a
	}

	eq := func(first *T, second *T) bool {
		switch teq := (any)(equalFunc).(type) {
		case func(T, T) bool:
			return teq(*first, *second)
		case func(*T, *T) bool:
			return teq(first, second)
		default:
			panic(fmt.Sprintf("DiffFunc cannot understand equality function type %T", equalFunc))
		}
	}

	var diff S

	for _, el := range a {
		found := false
		for _, bel := range b {
			if eq(&el, &bel) {
				found = true
				break
			}
		}
		if !found {
			diff = append(diff, el)
		}
	}

	return diff
}

type KeyFunc[T any, K comparable] interface {
	~func(T) K | ~func(*T) K
}

// Groups elements of a slice by a key, using a custom function to compute the key for each slice element.
func GroupBy[S ~[]T, K comparable, KF KeyFunc[T, K], T any](ss S, keyFunc KF) map[K]S {
	if len(ss) == 0 {
		return nil
	}

	kf := func(s *T) K {
		switch tkf := (any)(keyFunc).(type) {
		case func(T) K:
			return tkf(*s)
		case func(*T) K:
			return tkf(s)
		default:
			panic(fmt.Sprintf("GroupBy cannot understand key function type %T", keyFunc))
		}
	}

	groups := make(map[K]S)
	for _, s := range ss {
		key := kf(&s)
		groups[key] = append(groups[key], s)
	}

	return groups
}

// Flatten takes a list of lists and puts all elements into a single list.
func Flatten[T any, S ~[]T, SS ~[]S](ss SS) S {
	if len(ss) == 0 {
		return nil
	}

	var res S
	for _, s := range ss {
		res = append(res, s...)
	}
	return res
}

// Unique returns a slice with unique elements from the input slice.
func Unique[T comparable, S ~[]T](s S) S {
	if len(s) == 0 {
		return nil
	}

	seen := make(map[T]struct{})
	var res S
	for _, i := range s {
		if _, found := seen[i]; !found {
			seen[i] = struct{}{}
			res = append(res, i)
		}
	}
	return res
}
