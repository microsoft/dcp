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
func EqualValueFunc[T any, PT *T](p1 PT, p2 PT, equal func(T, T) bool) bool {
	if p1 == nil || p2 == nil {
		return p1 == p2
	}

	return equal(*p1, *p2)
}
