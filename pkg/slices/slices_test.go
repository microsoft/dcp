package slices

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIndex(t *testing.T) {
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
	// Mapping empty slice should return empty result
	var s []string
	result := Map[string, string](s, func(s string) string { return s })
	require.Empty(t, result)

	// Map with matching output type, mapping includes index
	s = []string{"a", "b", "c"}
	result = Map[string, string](s, func(i int, s string) string {
		return strings.ToUpper(s)
	})
	expected := []string{"A", "B", "C"}
	require.Equal(t, expected, result)

	// Map with matching output type, mapping on value only
	require.Equal(t, []string{"FISH"}, Map[string, string]([]string{"fish"}, strings.ToUpper))

	// Map to different type, mapping includes index
	require.Equal(t, []int{0, 1, 2}, Map[string, int](s, func(i int, s *string) int {
		return i
	}))

	// Mapping to same type, mapping operates on pointer to the value
	require.Equal(t, expected, Map[string, string](s, func(i int, s *string) string {
		return strings.ToUpper(*s)
	}))
}

var concurrency = []uint16{MaxConcurrency, 1, 2, 4}

func TestMapConcurrent(t *testing.T) {
	for _, c := range concurrency {
		t.Run(fmt.Sprintf("concurrency=%d", c), func(t *testing.T) {
			// Mapping empty slice should return empty result
			var s []string
			result := MapConcurrent[string, string](s, func(s string) string { return s }, c)
			require.Empty(t, result)

			// Map with matching output type, mapping includes index
			s = []string{"a", "b", "c"}
			result = MapConcurrent[string, string](s, func(i int, s string) string {
				return strings.ToUpper(s)
			}, c)
			expected := []string{"A", "B", "C"}
			require.Equal(t, expected, result)

			// Map with matching output type, mapping on value only
			require.Equal(t, []string{"FISH"}, MapConcurrent[string, string]([]string{"fish"}, strings.ToUpper, c))

			// Map to different type, mapping includes index
			require.Equal(t, []int{0, 1, 2}, MapConcurrent[string, int](s, func(i int, s *string) int {
				return i
			}, c))

			// Mapping to same type, mapping operates on pointer to the value
			require.Equal(t, expected, MapConcurrent[string, string](s, func(i int, s *string) string {
				return strings.ToUpper(*s)
			}, c))
		})
	}
}

func TestSelect(t *testing.T) {
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

func TestNonEmpty(t *testing.T) {
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
