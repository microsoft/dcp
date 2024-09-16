package containers

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrNotFound         = errors.New("object not found")
	ErrAlreadyExists    = errors.New("object already exists")
	ErrCouldNotAllocate = errors.New("object could not allocate required resources")
)

func (o StreamContainerLogsOptions) Apply(args []string) []string {
	if o.Follow {
		args = append(args, "--follow")
	}
	if o.Timestamps {
		args = append(args, "--timestamps")
	}
	if o.Tail != 0 {
		args = append(args, "--tail", fmt.Sprintf("%d", o.Tail))
	}
	if !o.Since.IsZero() {
		args = append(args, "--since", o.Since.Format(time.RFC3339))
	}
	if !o.Until.IsZero() {
		args = append(args, "--until", o.Until.Format(time.RFC3339))
	}
	return args
}
