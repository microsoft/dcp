package output

import (
	"fmt"
	"strings"
)

type OmitSliceBrackets[T any] []T

func (ss OmitSliceBrackets[T]) GoString() string {
	var sb strings.Builder
	for i, e := range ss {
		if i > 0 {
			sb.WriteString(" ")
		}
		fmt.Fprintf(&sb, "%v", e)
	}
	return sb.String()
}
