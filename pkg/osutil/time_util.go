/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package osutil

import (
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// RFC3339 with millisecond precision, fixed width.
	// This is a template, not a literal example. In particular, "Z" stands for timezone designator,
	// which is either "Z" or "+hh:mm" or "-hh:mm". See below for the regex that defines the actual format.
	RFC3339MiliTimestampFormat = "2006-01-02T15:04:05.000Z07:00"

	RFC33339YearRegex                     = `\d{4}`
	RFC3339MonthRegex                     = `(0[1-9]|1[0-2])`
	RFC3339DayRegex                       = `(0[1-9]|[12][0-9]|3[01])`
	RFC3339HourRegex                      = `([01][0-9]|2[0-3])`
	RFC3339TimestampMinutesOrSecondsRegex = `([0-5][0-9])`
	RFC3339MillisecondRegex               = `\d{3}`
)

var (
	RFC3339TimezoneRegex = fmt.Sprintf(
		`(Z|([+-]?%s:%s))`,
		RFC3339HourRegex,
		RFC3339TimestampMinutesOrSecondsRegex,
	)

	// Matches a timestamp in RFC3339 format with millisecond precision.
	// See https://www.rfc-editor.org/rfc/rfc3339#section-5.6 for more details.
	RFC3339MiliTimestampRegex = fmt.Sprintf(
		`%s-%s-%sT%s:%s:%s.%s%s`,
		RFC33339YearRegex,
		RFC3339MonthRegex,
		RFC3339DayRegex,
		RFC3339HourRegex,
		RFC3339TimestampMinutesOrSecondsRegex,
		RFC3339TimestampMinutesOrSecondsRegex,
		RFC3339MillisecondRegex,
		RFC3339TimezoneRegex,
	)
)

// Ensures two given timestamps are within a given duration of each other.
func Within(a, b time.Time, max time.Duration) bool {
	return a.Sub(b).Abs() <= max
}

// Checks whether two metav1.MicroTime values can be considered equal.
// Due to serialization/deserialization, the values may have different represenation,
// and thus Equal() may return false even if they represent essentially the same time.
func MicroEqual(a, b metav1.MicroTime) bool {
	return Within(a.Time, b.Time, 2*time.Microsecond)
}

// Formats a duration into a human readable string.
// If this proves not enough, consider https://github.com/hako/durafmt
func FormatDuration(duration time.Duration) string {
	days := duration / (24 * time.Hour)
	duration = duration % (24 * time.Hour)
	hours := duration / time.Hour
	duration = duration % time.Hour
	minutes := duration / time.Minute
	duration = duration % time.Minute
	seconds := duration / time.Second
	milliseconds := duration % time.Second / time.Millisecond

	var parts []string

	if days > 0 {
		parts = append(parts, fmt.Sprintf("%d days", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%d hours", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%d minutes", minutes))
	}
	if seconds > 0 || milliseconds > 0 {
		parts = append(parts, fmt.Sprintf("%d.%03d seconds", seconds, milliseconds))
	}

	if len(parts) == 0 {
		return "< 1ms"
	}

	return strings.Join(parts, " ")
}
