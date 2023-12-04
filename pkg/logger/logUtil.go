// Copyright (c) Microsoft Corporation. All rights reserved.

package logger

import (
	"fmt"
)

func IntPtrValToString[T int32 | int64, PT *T](p PT) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprintf("%d", *p)
}
