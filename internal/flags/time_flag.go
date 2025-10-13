// Copyright (c) Microsoft Corporation. All rights reserved.

package flags

import (
	"time"

	"github.com/spf13/pflag"
)

type TimeFlag struct {
	value  *time.Time
	format string
}

func NewTimeFlag(target *time.Time, format string) *TimeFlag {
	return &TimeFlag{
		value:  target,
		format: format,
	}
}

// Set implements pflag.Value.
func (t *TimeFlag) Set(timeStr string) error {
	if timeStr == "" {
		return nil
	}

	parsedTime, err := time.Parse(t.format, timeStr)
	if err != nil {
		return err
	}

	t.value = &parsedTime
	return nil
}

// String implements pflag.Value.
func (t *TimeFlag) String() string {
	if t == nil || t.value == nil {
		return ""
	}

	return t.value.Format(t.format)
}

// Type implements pflag.Value.
func (t *TimeFlag) Type() string {
	return "time"
}

var _ pflag.Value = &TimeFlag{}
