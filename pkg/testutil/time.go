package testutil

import (
	"fmt"
	"strings"
	"time"
)

const (
	RFC3339MiliTimestampRegex = `\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z`
)

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

// Ensures two given timestamps are within a given duration of each other.
func Within(a, b time.Time, max time.Duration) bool {
	return a.Sub(b).Abs() <= max
}
