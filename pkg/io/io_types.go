package io

import (
	"errors"
	"io"
)

type Syncer interface {
	Sync() error
}

type WriteSyncerCloser interface {
	io.WriteCloser
	Syncer
}

var (
	ErrClosedWriter = errors.New("writer is closed")
	ErrClosedReader = errors.New("reader is closed")
)
