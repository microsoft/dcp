/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package slices

import (
	"fmt"
	stdmaps "maps"
	stdslices "slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIndex(t *testing.T) {
	t.Parallel()

	data := []string{"alpha1", "alpha2", "bravo1", "bravo2"}

	// Index of first element
	result := Index(data, "alpha1")
	require.Equal(t, 0, result)

	// Index of last element
	result = Index(data, "bravo2")
	require.Equal(t, 3, result)

	// Index of something that does not exist in the slice
	result = Index(data, "not there")
	require.Equal(t, -1, result)

	// Empty slice should not contain anything
	result = Index([]string{}, "anything")
	require.Equal(t, -1, result)
	data = nil
	result = Index(data, "")
	require.Equal(t, -1, result)
}

func TestContains(t *testing.T) {
	t.Parallel()

	data := []string{"alpha1", "alpha2", "bravo1", "bravo2"}

	// Contains first element
	result := Contains(data, "alpha1")
	require.True(t, result)

	// Contains last element
	result = Contains(data, "bravo2")
	require.True(t, result)

	// Returns false if asked about element not in the slice
	result = Contains(data, "not there")
	require.False(t, result)

	// Empty slice should not contain anything
	result = Contains([]string{}, "anything")
	require.False(t, result)
	data = nil
	result = Contains(data, "")
	require.False(t, result)
}

func TestSeqIndex(t *testing.T) {
	t.Parallel()

	data := []string{"alpha1", "alpha2", "bravo1", "bravo2"}

	// Empty slice does not start with anything
	result := SeqIndex([]string{}, []string{"anything"})
	require.Equal(t, -1, result)
	result = SeqIndex(nil, []string{"anything"})
	require.Equal(t, -1, result)

	// Empty prefix does not start anything
	result = SeqIndex(data, []string{})
	require.Equal(t, -1, result)
	result = SeqIndex(data, nil)
	require.Equal(t, -1, result)

	// 1-element sequence: beginning, end, middle, nonexistent
	result = SeqIndex(data, []string{"alpha1"})
	require.Equal(t, 0, result)
	result = SeqIndex(data, []string{"bravo1"})
	require.Equal(t, 2, result)
	result = SeqIndex(data, []string{"bravo2"})
	require.Equal(t, 3, result)
	result = SeqIndex(data, []string{"charlie"})
	require.Equal(t, -1, result)

	// 2-element sequence: beginning, end, middle
	result = SeqIndex(data, []string{"alpha1", "alpha2"})
	require.Equal(t, 0, result)
	result = SeqIndex(data, []string{"alpha2", "bravo1"})
	require.Equal(t, 1, result)
	result = SeqIndex(data, []string{"bravo1", "bravo2"})
	require.Equal(t, 2, result)

	// 2-element sequence failure cases: the second element does not exist, exists, but out of place
	result = SeqIndex(data, []string{"alpha1", "alpha3"})
	require.Equal(t, -1, result)
	result = SeqIndex(data, []string{"alpha2", "bravo2"})
	require.Equal(t, -1, result)

	// Full sequence match
	result = SeqIndex(data, []string{"alpha1", "alpha2", "bravo1", "bravo2"})
	require.Equal(t, 0, result)

	// Full sequence no match at the beginning, middle, end
	result = SeqIndex(data, []string{"charlie", "alpha2", "bravo1", "bravo2"})
	require.Equal(t, -1, result)
	result = SeqIndex(data, []string{"alpha1", "charlie", "bravo1", "bravo2"})
	require.Equal(t, -1, result)
	result = SeqIndex(data, []string{"alpha1", "alpha2", "bravo1", "charlie"})
	require.Equal(t, -1, result)

	// Prefix longer than data
	result = SeqIndex(data, []string{"alpha1", "alpha2", "bravo1", "bravo2", "bravo3"})
	require.Equal(t, -1, result)
}

func TestMap(t *testing.T) {
	t.Parallel()

	// Mapping empty slice should return empty result
	var s []string
	result := Map[string](s, func(s string) string { return s })
	require.Empty(t, result)

	// Map with matching output type, mapping includes index
	s = []string{"a", "b", "c"}
	result = Map[string](s, func(i int, s string) string {
		return strings.ToUpper(s)
	})
	expected := []string{"A", "B", "C"}
	require.Equal(t, expected, result)

	// Map with matching output type, mapping on value only
	require.Equal(t, []string{"FISH"}, Map[string]([]string{"fish"}, strings.ToUpper))

	// Map to different type, mapping includes index
	require.Equal(t, []int{0, 1, 2}, Map[int](s, func(i int, s *string) int {
		return i
	}))

	// Mapping to same type, mapping operates on pointer to the value
	require.Equal(t, expected, Map[string](s, func(i int, s *string) string {
		return strings.ToUpper(*s)
	}))
}

var concurrency = []uint16{MaxConcurrency, 1, 2, 4}

func TestMapConcurrent(t *testing.T) {
	t.Parallel()

	for _, c := range concurrency {
		t.Run(fmt.Sprintf("concurrency=%d", c), func(t *testing.T) {
			// Mapping empty slice should return empty result
			var s []string
			result := MapConcurrent[string](s, func(s string) string { return s }, c)
			require.Empty(t, result)

			// Map with matching output type, mapping includes index
			s = []string{"a", "b", "c"}
			result = MapConcurrent[string](s, func(i int, s string) string {
				return strings.ToUpper(s)
			}, c)
			expected := []string{"A", "B", "C"}
			require.Equal(t, expected, result)

			// Map with matching output type, mapping on value only
			require.Equal(t, []string{"FISH"}, MapConcurrent[string]([]string{"fish"}, strings.ToUpper, c))

			// Map to different type, mapping includes index
			require.Equal(t, []int{0, 1, 2}, MapConcurrent[int](s, func(i int, s *string) int {
				return i
			}, c))

			// Mapping to same type, mapping operates on pointer to the value
			require.Equal(t, expected, MapConcurrent[string](s, func(i int, s *string) string {
				return strings.ToUpper(*s)
			}, c))
		})
	}
}

func TestSelect(t *testing.T) {
	t.Parallel()

	// Filtering empty data set should return empty result
	var data []string
	result := Select(data, func(s string) bool { return true })
	require.Empty(t, result)

	data = []string{"alpha1", "alpha2", "bravo1", "bravo2"}
	expected := []string{"alpha1", "alpha2"}

	// Selector includes index
	result = Select(data, func(i int, s string) bool {
		return strings.HasPrefix(s, "alpha")
	})
	require.Equal(t, expected, result)

	// Selector does not include index
	result = Select(data, func(s string) bool {
		return strings.HasPrefix(s, "alpha")
	})
	require.Equal(t, expected, result)

	// Selector includes index and operates on pointers
	result = Select(data, func(i int, s *string) bool {
		return strings.HasPrefix(*s, "alpha")
	})
	require.Equal(t, expected, result)

	// Selector does not include index and operates on pointers
	result = Select(data, func(s *string) bool {
		return strings.HasPrefix(*s, "alpha")
	})
	require.Equal(t, expected, result)

	// Result should be empty if selector does not select anything
	result = Select(data, func(s string) bool {
		return false
	})
	require.Empty(t, result)
}

func TestLenIf(t *testing.T) {
	t.Parallel()

	// Counting empty data set should return empty result
	var data []string
	result := LenIf(data, func(s string) bool { return true })
	require.Equal(t, 0, result)

	data = []string{"alpha1", "alpha2", "bravo1", "bravo2"}

	// Selector includes index
	result = LenIf(data, func(i int, s string) bool {
		return strings.HasPrefix(s, "alpha")
	})
	require.Equal(t, 2, result)

	// Selector does not include index
	result = LenIf(data, func(s string) bool {
		return strings.HasPrefix(s, "alpha")
	})
	require.Equal(t, 2, result)

	// Selector includes index and operates on pointers
	result = LenIf(data, func(i int, s *string) bool {
		return strings.HasPrefix(*s, "alpha")
	})
	require.Equal(t, 2, result)

	// Selector does not include index and operates on pointers
	result = LenIf(data, func(s *string) bool {
		return strings.HasPrefix(*s, "alpha")
	})
	require.Equal(t, 2, result)

	// Count should be zero if selector does not select anything
	result = LenIf(data, func(s string) bool {
		return false
	})
	require.Equal(t, 0, result)
}

func TestAny(t *testing.T) {
	t.Parallel()

	// Any with empty data set should return false
	var data []string
	result := Any(data, func(s string) bool { return true })
	require.False(t, result)

	data = []string{"alpha1", "alpha2", "bravo1", "bravo2"}

	// Selector includes index
	result = Any(data, func(i int, s string) bool {
		return strings.HasPrefix(s, "alpha")
	})
	require.True(t, result)

	// Selector does not include index
	result = Any(data, func(s string) bool {
		return strings.HasPrefix(s, "alpha")
	})
	require.True(t, result)

	// Selector includes index and operates on pointers
	result = Any(data, func(i int, s *string) bool {
		return strings.HasPrefix(*s, "alpha")
	})
	require.True(t, result)

	// Selector does not include index and operates on pointers
	result = Any(data, func(s *string) bool {
		return strings.HasPrefix(*s, "alpha")
	})
	require.True(t, result)

	// Any should be false if selector does not select anything
	result = Any(data, func(s string) bool {
		return false
	})
	require.False(t, result)
}

func TestIndexFunc(t *testing.T) {
	t.Parallel()

	// IndexOf with empty data set should return -1
	var data []string
	result := IndexFunc(data, func(s string) bool { return true })
	require.Equal(t, -1, result)

	data = []string{"alpha1", "alpha2", "bravo1", "bravo2"}

	// Selector includes index
	result = IndexFunc(data, func(i int, s string) bool {
		return s == "alpha1"
	})
	require.Equal(t, 0, result)

	// Selector does not include index
	result = IndexFunc(data, func(s string) bool {
		return s == "alpha2"
	})
	require.Equal(t, 1, result)

	// Selector includes index and operates on pointers
	result = IndexFunc(data, func(i int, s *string) bool {
		return *s == "bravo1"
	})
	require.Equal(t, 2, result)

	// Selector does not include index and operates on pointers
	result = IndexFunc(data, func(s *string) bool {
		return *s == "bravo2"
	})
	require.Equal(t, 3, result)

	// Index should be -1 if selector does not select anything
	result = IndexFunc(data, func(s string) bool {
		return false
	})
	require.Equal(t, -1, result)

}

func TestNonEmpty(t *testing.T) {
	t.Parallel()

	// NonEmpty with slices of strings
	require.Empty(t, NonEmpty[string]([]string{}))
	require.EqualValues(t,
		[]string{"something"},
		NonEmpty[string]([]string{"something", ""}))

	// NonEmpty with slices of slices (using bytes here)
	require.Empty(t, NonEmpty[byte]([][]byte{}))
	require.EqualValues(t,
		[][]byte{[]byte("alpha"), []byte("bravo")},
		NonEmpty[byte]([][]byte{[]byte("alpha"), []byte(""), []byte("bravo"), []byte("")}),
	)
}

func TestDiff(t *testing.T) {
	t.Parallel()

	// Difference between two empty slices should be empty
	aMinusB, bMinusA := Diff([]string{}, []string{})
	require.Empty(t, aMinusB)
	require.Empty(t, bMinusA)

	// Difference between empty and non-empty slice should be empty
	aMinusB, bMinusA = Diff([]string{}, []string{"something"})
	require.Empty(t, aMinusB)
	require.EqualValues(t, []string{"something"}, bMinusA)

	// Difference between non-empty and empty slice should be the non-empty slice
	aMinusB, bMinusA = Diff([]string{"alpha", "bravo"}, []string{})
	require.EqualValues(t, []string{"alpha", "bravo"}, aMinusB)
	require.Empty(t, bMinusA)

	// First slice is a subset of the second slice (difference is empty)
	aMinusB, bMinusA = Diff([]string{"bravo", "alpha"}, []string{"charlie", "alpha", "bravo"})
	require.Empty(t, aMinusB)
	require.EqualValues(t, []string{"charlie"}, bMinusA)

	// First slice is a superset of the second slice
	aMinusB, bMinusA = Diff([]string{"charlie", "alpha", "bravo"}, []string{"bravo", "alpha"})
	require.EqualValues(t, []string{"charlie"}, aMinusB)
	require.Empty(t, bMinusA)

	// First slice and second slice have no elements in common
	aMinusB, bMinusA = Diff([]string{"charlie", "delta"}, []string{"alpha", "bravo"})
	require.EqualValues(t, []string{"charlie", "delta"}, aMinusB)
	require.EqualValues(t, []string{"alpha", "bravo"}, bMinusA)

	// First slice and second slice have some elements in common
	aMinusB, bMinusA = Diff([]string{"charlie", "delta", "alpha"}, []string{"alpha", "bravo"})
	require.EqualValues(t, []string{"charlie", "delta"}, aMinusB)
	require.EqualValues(t, []string{"bravo"}, bMinusA)
}

func TestDiffFunc(t *testing.T) {
	t.Parallel()

	type point2d struct {
		x int
		y int
	}

	samePoint := func(p1, p2 point2d) bool {
		return p1.x == p2.x && p1.y == p2.y
	}

	// Difference between two empty slices should be empty
	require.Empty(t, DiffFunc([]point2d{}, []point2d{}, samePoint))

	// Difference between empty and non-empty slice should be empty
	require.Empty(t, DiffFunc([]point2d{}, []point2d{{1, 2}}, samePoint))

	// Difference between non-empty and empty slice should be the non-empty slice
	require.EqualValues(t, []point2d{{1, 2}, {3, 4}}, DiffFunc([]point2d{{1, 2}, {3, 4}}, []point2d{}, samePoint))

	// First slice is a subset of the second slice (difference is empty)
	require.Empty(t, DiffFunc([]point2d{{1, 2}, {3, 4}}, []point2d{{5, 6}, {3, 4}, {1, 2}}, samePoint))

	// First slice is a superset of the second slice
	require.EqualValues(t, []point2d{{5, 6}}, DiffFunc([]point2d{{5, 6}, {1, 2}, {3, 4}}, []point2d{{3, 4}, {1, 2}}, samePoint))

	// First slice and second slice have no elements in common
	require.EqualValues(t, []point2d{{5, 6}, {7, 8}}, DiffFunc([]point2d{{5, 6}, {7, 8}}, []point2d{{1, 2}, {3, 4}}, samePoint))

	// First slice and second slice have some elements in common
	require.EqualValues(t, []point2d{{5, 6}, {7, 8}}, DiffFunc([]point2d{{5, 6}, {7, 8}, {1, 2}}, []point2d{{1, 2}, {3, 4}}, samePoint))
}

func TestIntersect(t *testing.T) {
	t.Parallel()

	// Intersection of two empty slices should be empty
	result := Intersect([]string{}, []string{})
	require.Empty(t, result)

	// Intersection of empty and non-empty slice should be empty
	result = Intersect([]string{}, []string{"something"})
	require.Empty(t, result)

	// Intersection of non-empty and empty slice should be empty
	result = Intersect([]string{"alpha", "bravo"}, []string{})
	require.Empty(t, result)

	// First slice is a subset of the second slice (intersection is the first slice)
	result = Intersect([]string{"bravo", "alpha"}, []string{"charlie", "alpha", "bravo"})
	require.EqualValues(t, []string{"alpha", "bravo"}, result)

	// First slice is a superset of the second slice (intersection is the second slice)
	result = Intersect([]string{"charlie", "alpha", "bravo"}, []string{"bravo", "alpha"})
	require.EqualValues(t, []string{"alpha", "bravo"}, result)

	// First slice and second slice have no elements in common
	result = Intersect([]string{"charlie", "delta"}, []string{"alpha", "bravo"})
	require.Empty(t, result)

	// First slice and second slice have some elements in common
	result = Intersect([]string{"charlie", "delta", "alpha"}, []string{"alpha", "bravo"})
	require.EqualValues(t, []string{"alpha"}, result)

	// Test with duplicates in input slices
	result = Intersect([]string{"alpha", "alpha", "bravo"}, []string{"alpha", "charlie", "alpha"})
	require.EqualValues(t, []string{"alpha"}, result)

	// Test with integers
	intResult := Intersect([]int{3, 1, 4, 1, 5}, []int{2, 1, 4, 6})
	require.EqualValues(t, []int{1, 4}, intResult)
}

func TestIntersectFunc(t *testing.T) {
	t.Parallel()

	type point2d struct {
		x int
		y int
	}

	samePoint := func(p1, p2 point2d) bool {
		return p1.x == p2.x && p1.y == p2.y
	}

	// Intersection of two empty slices should be empty
	result := IntersectFunc([]point2d{}, []point2d{}, samePoint)
	require.Empty(t, result)

	// Intersection of empty and non-empty slice should be empty
	result = IntersectFunc([]point2d{}, []point2d{{1, 2}}, samePoint)
	require.Empty(t, result)

	// Intersection of non-empty and empty slice should be empty
	result = IntersectFunc([]point2d{{1, 2}, {3, 4}}, []point2d{}, samePoint)
	require.Empty(t, result)

	// First slice is a subset of the second slice (intersection is the first slice)
	result = IntersectFunc([]point2d{{1, 2}, {3, 4}}, []point2d{{5, 6}, {3, 4}, {1, 2}}, samePoint)
	require.EqualValues(t, []point2d{{1, 2}, {3, 4}}, result)

	// First slice is a superset of the second slice (intersection is the second slice elements in first slice order)
	result = IntersectFunc([]point2d{{5, 6}, {1, 2}, {3, 4}}, []point2d{{3, 4}, {1, 2}}, samePoint)
	require.EqualValues(t, []point2d{{1, 2}, {3, 4}}, result)

	// First slice and second slice have no elements in common
	result = IntersectFunc([]point2d{{5, 6}, {7, 8}}, []point2d{{1, 2}, {3, 4}}, samePoint)
	require.Empty(t, result)

	// First slice and second slice have some elements in common
	result = IntersectFunc([]point2d{{5, 6}, {7, 8}, {1, 2}}, []point2d{{1, 2}, {3, 4}}, samePoint)
	require.EqualValues(t, []point2d{{1, 2}}, result)

	// Test with duplicates in input slices
	result = IntersectFunc([]point2d{{1, 2}, {1, 2}, {3, 4}}, []point2d{{1, 2}, {5, 6}, {1, 2}}, samePoint)
	require.EqualValues(t, []point2d{{1, 2}}, result)
}

func TestGroupBy(t *testing.T) {
	t.Parallel()

	type record struct {
		id, val string
	}

	sortedValues := func(m map[string][]record, id string) []string {
		retval := Map[string](m[id], func(r record) string { return r.val })
		stdslices.Sort(retval)
		return retval
	}
	sortedKeys := func(m map[string][]record) []string {
		retval := stdslices.Collect(stdmaps.Keys(m))
		stdslices.Sort(retval)
		return retval
	}

	// Grouping empty slice should return empty result
	require.Empty(t, GroupBy[[]record, string](nil, func(r record) string { return r.id }))
	require.Empty(t, GroupBy[[]record, string]([]record{}, func(r record) string { return r.id }))

	// Group with one key
	data := []record{
		{"a", "alpha1"},
		{"a", "alpha2"},
	}
	result := GroupBy[[]record, string](data, func(r record) string { return r.id })
	require.Len(t, result, 1)
	require.EqualValues(t, []string{"a"}, sortedKeys(result))
	require.EqualValues(t, []string{"alpha1", "alpha2"}, sortedValues(result, "a"))

	// Group with multiple keys
	data = []record{
		{"a", "alpha1"},
		{"b", "bravo1"},
		{"a", "alpha2"},
		{"c", "charlie1"},
		{"b", "bravo2"},
	}
	result = GroupBy[[]record, string](data, func(r record) string { return r.id })
	require.Len(t, result, 3)
	require.EqualValues(t, []string{"a", "b", "c"}, sortedKeys(result))
	require.EqualValues(t, []string{"alpha1", "alpha2"}, sortedValues(result, "a"))
	require.EqualValues(t, []string{"bravo1", "bravo2"}, sortedValues(result, "b"))
	require.EqualValues(t, []string{"charlie1"}, sortedValues(result, "c"))

	// Group with duplicate records
	data = []record{
		{"a", "alpha1"},
		{"b", "bravo1"},
		{"c", "charlie1"},
		{"a", "alpha1"},
		{"c", "charlie1"},
		{"b", "bravo2"},
	}
	result = GroupBy[[]record, string](data, func(r record) string { return r.id })
	require.Len(t, result, 3)
	require.EqualValues(t, []string{"a", "b", "c"}, sortedKeys(result))
	require.EqualValues(t, []string{"alpha1", "alpha1"}, Map[string](result["a"], func(r record) string { return r.val }))
	require.EqualValues(t, []string{"bravo1", "bravo2"}, Map[string](result["b"], func(r record) string { return r.val }))
	require.EqualValues(t, []string{"charlie1", "charlie1"}, Map[string](result["c"], func(r record) string { return r.val }))
}
