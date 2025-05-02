// Copyright (c) Microsoft Corporation. All rights reserved.

package logs

import (
	"sync/atomic"

	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
)

type logfile struct {
	inner  usvc_io.WriteSyncerCloser
	closed *atomic.Bool
	id     string
}

func newLogFile(inner usvc_io.WriteSyncerCloser, id string) *logfile {
	return &logfile{
		inner:  inner,
		closed: &atomic.Bool{},
		id:     id,
	}
}

func (f *logfile) Write(p []byte) (int, error) {
	return f.inner.Write(p)
}

func (f *logfile) Close() error {
	err := f.inner.Close()
	f.closed.Store(true)
	return err
}

func (f *logfile) IsClosed() bool {
	return f.closed.Load()
}

func (f *logfile) Id() string {
	return f.id
}

func (f *logfile) Sync() error {
	return f.inner.Sync()
}

var _ usvc_io.WriteSyncerCloser = (*logfile)(nil)
