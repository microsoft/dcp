// Copyright (c) Microsoft Corporation. All rights reserved.

package logger

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func IntPtrValToString[T int32 | int64, PT *T](p PT) string {
	if p == nil {
		return "(null)"
	}
	return fmt.Sprintf("%d", *p)
}

func IsPtrValDifferent[T int32 | int64, PT *T](p1 PT, p2 PT) bool {
	if p1 == nil || p2 == nil {
		return p1 != p2
	}
	return *p1 != *p2
}

func FriendlyTimestamp(ts metav1.Time) string {
	if ts.IsZero() {
		return "(zero)"
	} else {
		return ts.Format(time.StampMilli)
	}
}

func FriendlyString(s string) string {
	if s == "" {
		return "(empty)"
	} else {
		return s
	}
}
