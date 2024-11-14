package osutil

import (
	"fmt"
	"strings"
	"time"
)

const (
	RFC3339MiliTimestampFormat = "2006-01-02T15:04:05.000Z07:00" // RFC3339 with millisecond precision, fixed width

	// RFC3339MiliTimestampRegex is a regex that matches a timestamp in RFC3339 format with milliseconds. Explanation:
	// 4 digits for the year
	// 2 digits for the month, restricted to 01-12
	// 2 digits for the day, restricted to 01-31
	// T (time separator)
	// 2 digits for the hour, restricted to 00-23
	// 2 digits for the minute, restricted to 00-59
	// 2 digits for the second, restricted to 00-59
	// Period and 3 digits for the milliseconds portion
	// Z (UTC timezone)
	RFC3339MiliTimestampRegex = `\d{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):([0-5][0-9]):([0-5][0-9])\.\d{3}Z`
)

// Ensures two given timestamps are within a given duration of each other.
func Within(a, b time.Time, max time.Duration) bool {
	return a.Sub(b).Abs() <= max
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

	return strings.Join(parts, " ")
}
