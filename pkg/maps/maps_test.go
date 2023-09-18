package maps

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMap(t *testing.T) {
	// Mapping empty map returns empty result
	var m map[int]string
	result := Map[int, string, string](m, func(i int, s string) string { return s })
	require.Empty(t, result)

	m = map[int]string{
		1: "alpha",
		2: "bravo",
		3: "charlie",
	}
	expected := map[int]string{
		1: "ALPHA",
		2: "BRAVO",
		3: "CHARLIE",
	}
	// Mapping with a function that operates on values only
	require.Equal(t, expected, Map[int, string, string](m, strings.ToUpper))

	// Mapping with a function that operates on keys and values
	expected = map[int]string{
		1: "ALPHA-1",
		2: "BRAVO-2",
		3: "CHARLIE-3",
	}
	actual := Map[int, string, string](m, func(k int, v string) string {
		return fmt.Sprintf("%s-%d", strings.ToUpper(v), k)
	})
	require.Equal(t, expected, actual)
}

func TestMapToSlice(t *testing.T) {
	// Mapping empty map returns empty result
	var m map[int]string
	result := MapToSlice[int, string, string](m, func(i int, s string) string { return s })
	require.Empty(t, result)

	m = map[int]string{
		1: "alpha",
		2: "bravo",
		3: "charlie",
	}
	expected := []string{
		"ALPHA",
		"BRAVO",
		"CHARLIE",
	}
	// Mapping with a function that operates on values only
	result = MapToSlice[int, string, string](m, strings.ToUpper)
	sort.Strings(result)
	require.Equal(t, expected, result)

	// Mapping with a function that operates on keys and values
	expected = []string{
		"ALPHA-1",
		"BRAVO-2",
		"CHARLIE-3",
	}
	actual := MapToSlice[int, string, string](m, func(k int, v string) string {
		return fmt.Sprintf("%s-%d", strings.ToUpper(v), k)
	})
	sort.Strings(actual)
	require.Equal(t, expected, actual)
}

type keyValPair struct {
	key   string
	value string
}

func TestSliceToMap(t *testing.T) {
	// Mapping empty slice returns empty result
	var s []keyValPair = nil
	result := SliceToMap(s, func(kvp keyValPair) (string, string) { return kvp.key, kvp.value })
	require.Empty(t, result)

	s = make([]keyValPair, 0)
	result = SliceToMap(s, func(kvp keyValPair) (string, string) { return kvp.key, kvp.value })
	require.Empty(t, result)

	// Map with all keys unique
	s = []keyValPair{
		{"a", "alpha"},
		{"b", "bravo"},
		{"c", "charlie"},
	}
	expected := map[string]string{
		"a": "alpha",
		"b": "bravo",
		"c": "charlie",
	}
	require.Equal(t, expected, SliceToMap(s, func(kvp keyValPair) (string, string) { return kvp.key, kvp.value }))

	// Map with duplicate keys
	s = []keyValPair{
		{"a", "alpha"},
		{"b", "bravo"},
		{"a", "ALPHA"},
		{"c", "charlie"},
		{"b", "BRAVO"},
	}
	expected = map[string]string{
		"a": "ALPHA",
		"b": "BRAVO",
		"c": "charlie",
	}
	require.Equal(t, expected, SliceToMap(s, func(kvp keyValPair) (string, string) { return kvp.key, kvp.value }))
}

func TestSelect(t *testing.T) {
	// Filtering empty data set should return empty result
	var data map[int]string
	result := Select(data, func(i int) bool { return true })
	require.Empty(t, result)

	data = map[int]string{
		1: "alpha1",
		2: "bravo2",
		3: "charlie3",
		4: "delta4",
	}
	expected := map[int]string{
		1: "alpha1",
		3: "charlie3",
	}

	// Selector includes key and value
	result = Select(data, func(i int, s string) bool {
		return strings.HasPrefix(s, "a") || strings.HasPrefix(s, "c")
	})
	require.Equal(t, expected, result)

	// Selector operates on keys only
	result = Select(data, func(i int) bool {
		return i%2 == 1 // Odd indices
	})
	require.Equal(t, expected, result)
}

func TestApply(t *testing.T) {
	// Applying empty map on top of empty map should result in an empty map
	var m1 = make(map[int]string)
	var m2 = make(map[int]string)

	result := Apply(m1, m2)
	require.Empty(t, result)

	// Apply empty map shold result in no change
	m1 = map[int]string{
		1: "alpha1",
		2: "bravo2",
		3: "charlie3",
		4: "delta4",
	}
	result = Apply(m1, m2)
	require.Equal(t, m1, result)

	// Applying something on top of empty map should result in the something
	result = Apply(m2, m1)
	require.Equal(t, m1, result)

	// Replace some values and add some more
	m2 = map[int]string{
		0: "zulu0",
		2: "oterbravo",
	}
	expected := map[int]string{
		0: "zulu0",
		1: "alpha1",
		2: "oterbravo",
		3: "charlie3",
		4: "delta4",
	}
	result = Apply(m1, m2)
	require.Equal(t, expected, result)
}
