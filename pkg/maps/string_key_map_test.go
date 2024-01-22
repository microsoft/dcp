package maps

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/require"
)

func TestStringKeyMapCaseSensitive(t *testing.T) {
	skm := NewStringKeyMap[int](StringMapModeCaseSensitive)

	require.Equal(t, 0, skm.Len())
	_, found := skm.Get("anything")
	require.False(t, found) // There should be no panic, but nothing should be found either.

	// For case-sensitive map it should not matter whether "Set" or "Override" is used.
	skm.Set("foo", 1)
	skm.Override("Foo", 2)
	skm.Set("bar", 3)
	skm.Override("Bar", 4)

	require.Equal(t, 4, skm.Len())
	i, found := skm.Get("foo")
	require.True(t, found)
	require.Equal(t, 1, i)
	i, found = skm.Get("Foo")
	require.True(t, found)
	require.Equal(t, 2, i)
	i, found = skm.Get("bar")
	require.True(t, found)
	require.Equal(t, 3, i)
	i, found = skm.Get("Bar")
	require.True(t, found)
	require.Equal(t, 4, i)
	_, found = skm.Get("not-there")
	require.False(t, found)

	skm.Set("foo", 11)
	skm.Override("Foo", 12)

	require.Equal(t, 4, skm.Len())
	i, found = skm.Get("foo")
	require.True(t, found)
	require.Equal(t, 11, i)
	i, found = skm.Get("Foo")
	require.True(t, found)
	require.Equal(t, 12, i)

	skm.Delete("foo")
	skm.Delete("Bar")
	skm.Delete("not-there")

	require.Equal(t, 2, skm.Len())
	_, found = skm.Get("foo")
	require.False(t, found)
	_, found = skm.Get("Foo")
	require.True(t, found)
	_, found = skm.Get("bar")
	require.True(t, found)
	_, found = skm.Get("Bar")
	require.False(t, found)
}

func TestStringKeyMapCaseInsensitive(t *testing.T) {
	skm := NewStringKeyMap[int](StringMapModeCaseInsensitive)

	require.Equal(t, 0, skm.Len())
	_, found := skm.Get("anything")
	require.False(t, found) // There should be no panic, but nothing should be found either.

	// When using Set() to set the value, the initial key should be preserved.
	skm.Set("foo", 1)
	skm.Set("Foo", 2)
	skm.Set("bar", 3)
	skm.Set("Bar", 4)

	// Note--the same value should be returned regardless of the case of the key.
	require.Equal(t, 2, skm.Len())
	i, found := skm.Get("foo")
	require.True(t, found)
	require.Equal(t, 2, i)
	i, found = skm.Get("Foo")
	require.True(t, found)
	require.Equal(t, 2, i)
	i, found = skm.Get("bar")
	require.True(t, found)
	require.Equal(t, 4, i)
	i, found = skm.Get("Bar")
	require.True(t, found)
	require.Equal(t, 4, i)
	_, found = skm.Get("not-there")
	require.False(t, found)
	require.True(t, cmp.Equal(map[string]int{
		"foo": 2,
		"bar": 4,
	}, skm.Data(), cmpopts.EquateEmpty()))

	// When using Override() to set the value, the key should be overwritten too.
	skm.Override("Foo", 11)
	skm.Override("Bar", 12)

	require.Equal(t, 2, skm.Len())
	i, found = skm.Get("foo")
	require.True(t, found)
	require.Equal(t, 11, i)
	i, found = skm.Get("Foo")
	require.True(t, found)
	require.Equal(t, 11, i)
	i, found = skm.Get("bar")
	require.True(t, found)
	require.Equal(t, 12, i)
	i, found = skm.Get("Bar")
	require.True(t, found)
	require.Equal(t, 12, i)
	_, found = skm.Get("not-there")
	require.False(t, found)
	require.True(t, cmp.Equal(map[string]int{
		"Foo": 11,
		"Bar": 12,
	}, skm.Data(), cmpopts.EquateEmpty()))

	// Add a couple of more key-value pairs and test that Delete() matches keys in a case-insensitive manner.
	skm.Set("Alpha", 14)
	skm.Set("Bravo", 15)
	skm.Delete("Foo")
	skm.Delete("alpha")

	require.Equal(t, 2, skm.Len())
	require.True(t, cmp.Equal(map[string]int{
		"Bravo": 15,
		"Bar":   12,
	}, skm.Data(), cmpopts.EquateEmpty()))
}

func TestStringKeyApply(t *testing.T) {
	mcs := NewStringKeyMap[int](StringMapModeCaseSensitive)
	mcs.Apply(nil) // No panic
	require.Equal(t, 0, mcs.Len())

	mcs.Apply(map[string]int{})
	require.Equal(t, 0, mcs.Len())

	mcs.Apply(map[string]int{
		"alpha": 1,
		"bravo": 2,
	})
	require.Equal(t, 2, mcs.Len())
	require.True(t, cmp.Equal(map[string]int{
		"alpha": 1,
		"bravo": 2,
	}, mcs.Data(), cmpopts.EquateEmpty()))

	mcs.Apply(map[string]int{
		"Alpha": 3,
		"bravo": 4,
	})
	require.Equal(t, 3, mcs.Len())
	require.True(t, cmp.Equal(map[string]int{
		"alpha": 1,
		"Alpha": 3,
		"bravo": 4,
	}, mcs.Data(), cmpopts.EquateEmpty()))

	mci := NewStringKeyMap[int](StringMapModeCaseInsensitive)
	mci.Apply(nil) // No panic
	require.Equal(t, 0, mci.Len())

	mci.Apply(map[string]int{})
	require.Equal(t, 0, mci.Len())

	mci.Apply(map[string]int{
		"alpha":   1,
		"bravo":   2,
		"charlie": 3,
	})
	require.Equal(t, 3, mci.Len())
	require.True(t, cmp.Equal(map[string]int{
		"alpha":   1,
		"bravo":   2,
		"charlie": 3,
	}, mci.Data(), cmpopts.EquateEmpty()))

	mci.Apply(map[string]int{
		"Alpha": 3,
		"bravo": 4,
	})
	require.Equal(t, 3, mci.Len())
	require.True(t, cmp.Equal(map[string]int{
		"Alpha":   3,
		"bravo":   4,
		"charlie": 3,
	}, mci.Data(), cmpopts.EquateEmpty()))
}
