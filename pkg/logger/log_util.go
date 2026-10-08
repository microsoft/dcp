/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logger

import (
	"errors"
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

func BoolPtrValToString(p *bool) string {
	if p == nil {
		return "(null)"
	}
	return fmt.Sprintf("%t", *p)
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

const maxErrorExamples = 5

type errorSummary struct {
	causes []error
}

func (summary *errorSummary) Error() string {
	return fmt.Sprintf(
		"%d errors occurred (showing first %d): %v",
		len(summary.causes),
		maxErrorExamples,
		errors.Join(summary.causes[:maxErrorExamples]...),
	)
}

func (summary *errorSummary) Unwrap() []error {
	return summary.causes
}

// SummarizeErrors joins non-nil errors, rendering at most five examples for logging.
// All causes remain available through errors.Is and errors.As.
func SummarizeErrors(errs []error) error {
	causes := make([]error, 0, len(errs))
	for _, cause := range errs {
		if cause != nil {
			causes = append(causes, cause)
		}
	}
	if len(causes) <= maxErrorExamples {
		return errors.Join(causes...)
	}
	return &errorSummary{causes: causes}
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

func FriendlyStringSlice(s []string) string {
	var b strings.Builder
	var sep string = ""
	b.WriteString("[")
	for _, v := range s {
		b.WriteString(sep)
		b.WriteString(fmt.Sprintf("'%s'", FriendlyString(v)))
		sep = ", "
	}
	b.WriteString("]")
	return b.String()
}
