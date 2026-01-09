/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package pointers

// Checks if the values pointed to by two pointers are equal. If either pointer is nil, returns true if both are nil.
func EqualValue[T comparable, PT *T](p1 PT, p2 PT) bool {
	if p1 == nil || p2 == nil {
		return p1 == p2
	}
	return *p1 == *p2
}

// Checks if the values pointed to by two pointers are equal using a custom equality function.
// If either pointer is nil, returns true if both are nil.
// The function will be called only if both pointers are non-nil.
func EqualValueFunc[T any, PT *T](p1 PT, p2 PT, equal func(PT, PT) bool) bool {
	if p1 == nil || p2 == nil {
		return p1 == p2
	}

	return equal(p1, p2)
}

func GetValueOrDefault[T any, PT *T](p PT, defaultValue T) T {
	if p == nil {
		return defaultValue
	}
	return *p
}

// Sets the value pointed to by a pointer to the given value, allocating new memory if the pointer is nil.
func SetValue[T any, PT *T](pp *PT, val T) {
	if pp == nil {
		panic("nil pointer passed as target for pointers.SetValue()")
	}

	if *pp == nil {
		*pp = new(T)
	}

	**pp = val
}

// Sets the value pointed to by a pointer to the value pointed to by another pointer, allocating new memory if the pointer is nil.
func SetValueFrom[T any, PT *T](pp *PT, pVal PT) {
	if pp == nil {
		panic("nil pointer passed as target for pointers.SetValue()")
	}

	if pVal == nil {
		*pp = nil
		return
	}

	if *pp == nil {
		*pp = new(T)
	}

	**pp = *pVal
}

// Returns true if the boolean pointer has value and the value is true.
func TrueValue[T ~bool, PT *T](p PT) bool {
	return bool(GetValueOrDefault(p, false))
}

// Returns true if the boolean pointer has no value OR the value is false.
func NotTrue[T ~bool, PT *T](p PT) bool {
	if p == nil {
		return true
	}
	return !bool(*p)
}

// Sets the value of the pointer to the given value, allocating new memory if the pointer is nil.
func Make[T any, PT *T](pp *PT, val T) {
	if pp == nil {
		panic("nil pointer passed as target for pointers.Make()")
	}

	if *pp == nil {
		*pp = new(T)
	}

	**pp = val
}

// Creates a new pointer from the given pointer, pointing to the same value as the original pointer.
// Returns nil if the input pointer is nil.
func Duplicate[T any, PT *T](p PT) PT {
	if p == nil {
		return nil
	}

	newP := new(T)
	*newP = *p
	return newP
}
