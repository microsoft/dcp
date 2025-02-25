package containers

import (
	"errors"
	"fmt"
	"time"
)

const (
	ContainerLogsHttpPath string = "/apis/usvc-dev.developer.microsoft.com/v1/containers/%s/log"
)

var (
	ErrNotFound          = errors.New("object not found")
	ErrAlreadyExists     = errors.New("object already exists")
	ErrCouldNotAllocate  = errors.New("object could not allocate required resources")
	ErrRuntimeNotHealthy = errors.New("runtime is not healthy")
	ErrObjectInUse       = errors.New("object is in use")
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
