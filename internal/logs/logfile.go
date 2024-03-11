// Copyright (c) Microsoft Corporation. All rights reserved.

package logs

import (
	"io"
	"sync/atomic"
)

type Syncer interface {
	Sync() error
}

type WriteSyncerCloser interface {
	io.WriteCloser
	Syncer
}

type logfile struct {
	inner  WriteSyncerCloser
	closed *atomic.Bool
	id     string
}

func newLogFile(inner WriteSyncerCloser, id string) *logfile {
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

var _ io.WriteCloser = (*logfile)(nil)
