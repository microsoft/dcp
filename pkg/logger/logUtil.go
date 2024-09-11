// Copyright (c) Microsoft Corporation. All rights reserved.

package logger

import (
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func IntPtrValToString[T int32 | int64, PT *T](p PT) string {
	if p == nil {
		return "(null)"
	}
	return fmt.Sprintf("%d", *p)
}

func FriendlyTimestamp(ts time.Time) string {
	if ts.IsZero() {
		return "(zero)"
	} else {
		return ts.Format(time.StampMilli)
	}
}

func FriendlyMetav1Timestamp(ts metav1.MicroTime) string {
	return FriendlyTimestamp(ts.Time)
}

func FriendlyString(s string) string {
	if s == "" {
		return "(empty)"
	} else {
		return s
	}
}

func FriendlyErrorString(err error) string {
	if err == nil {
		return "(none)"
	}
	return err.Error()
}

func FriendlyStringMap(m map[string]string) string {
	var b strings.Builder
	var sep string = ""
	b.WriteString("{")
	for k, v := range m {
		b.WriteString(sep)
		b.WriteString(fmt.Sprintf("'%s': '%s'", FriendlyString(k), FriendlyString(v)))
		sep = ", "
	}
	b.WriteString("}")
	return b.String()
}
