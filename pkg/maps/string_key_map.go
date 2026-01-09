/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package maps

import (
	"strings"

	"golang.org/x/text/cases"
)

// StringKeyMap is a map-like data structure with string keys.
// The keys can be treated in case-sensitive or case-insensitive manner.
// StringKeyMap is not goroutine-safe.
type StringKeyMap[T any] struct {
	// The data, using keys with "desired" casing.
	data map[string]T

	// Map of folded-case keys to "desired" key, used for case-insensitive lookups.
	keys map[string]string

	// Function that canonicalizes keys.
	canonicalize func(string) string
}

type StringMapMode uint32

const (
	StringMapModeCaseSensitive   StringMapMode = 0
	StringMapModeCaseInsensitive StringMapMode = 1
)

func NewStringKeyMap[T any](mode StringMapMode) StringKeyMap[T] {
	skm := StringKeyMap[T]{
		data: make(map[string]T),
		keys: make(map[string]string),
	}

	if mode == StringMapModeCaseInsensitive {
		caser := cases.Fold()
		skm.canonicalize = func(s string) string {
			retval := caser.String(s)
			caser.Reset()
			return retval
		}
	} else {
		skm.canonicalize = func(s string) string { return s }
	}

	return skm
}

// Set sets the value for the given key, preserving desired case of the key if it already exists.
func (skm StringKeyMap[T]) Set(key string, value T) {
	canonicalKey := skm.canonicalize(key)
	if desiredKey, found := skm.keys[canonicalKey]; found {
		skm.data[desiredKey] = value
	} else {
		skm.data[key] = value
		skm.keys[canonicalKey] = key
	}
}

// Gets the data associated with the given key.
func (skm StringKeyMap[T]) Get(key string) (T, bool) {
	canonicalKey := skm.canonicalize(key)
	if desiredKey, found := skm.keys[canonicalKey]; found {
		return skm.data[desiredKey], true
	} else {
		return *new(T), false
	}
}

// Delete deletes data that is associated with the given key.
// It is a no-op if the key does not exist.
func (skm StringKeyMap[T]) Delete(key string) {
	canonicalKey := skm.canonicalize(key)
	if desiredKey, found := skm.keys[canonicalKey]; found {
		delete(skm.data, desiredKey)
		delete(skm.keys, canonicalKey)
	} else {
		delete(skm.data, key)
	}
}

// DeletePrefix deletes all keys that have the given prefix.
func (skm StringKeyMap[T]) DeletePrefix(prefix string) {
	if prefix == "" {
		return // No-op
	}

	canonicalPrefix := skm.canonicalize(prefix)
	for canonicalKey, desiredKey := range skm.keys {
		if strings.HasPrefix(canonicalKey, canonicalPrefix) {
			delete(skm.data, desiredKey)
			delete(skm.keys, canonicalKey)
		}
	}
	for key := range skm.data {
		if strings.HasPrefix(key, prefix) {
			delete(skm.data, key)
		}
	}
}

// Override sets the value for the given key. If the key already exists, it is deleted first (making the passed key the new "desired" key).
func (skm StringKeyMap[T]) Override(key string, value T) {
	skm.Delete(key)
	skm.Set(key, value)
}

// Apply will add all the key-value pairs from the given map to this StringKeyMap.
// If there are existing keys in the StringKeyMap, they will be overridden.
func (skm StringKeyMap[T]) Apply(m map[string]T) {
	for k, v := range m {
		skm.Override(k, v)
	}
}

// Data returns the undelying map, with desired keys.
// It should not be modified, but it can be used for iteration.
func (skm StringKeyMap[T]) Data() map[string]T {
	return skm.data
}

func (skm StringKeyMap[T]) Len() int {
	return len(skm.data)
}
