// Copyright (c) Microsoft Corporation. All rights reserved.

package logs

import (
	"io"
	"sync/atomic"
)

type notifyWriteCloser struct {
	inner  io.WriteCloser
	closed *atomic.Bool
	id     string
}

func newNotifyWriteCloser(inner io.WriteCloser, id string) *notifyWriteCloser {
	return &notifyWriteCloser{
		inner:  inner,
		closed: &atomic.Bool{},
		id:     id,
	}
}

func (n *notifyWriteCloser) Write(p []byte) (int, error) {
	return n.inner.Write(p)
}

func (n *notifyWriteCloser) Close() error {
	err := n.inner.Close()
	n.closed.Store(true)
	return err
}

func (n *notifyWriteCloser) IsClosed() bool {
	return n.closed.Load()
}

func (n *notifyWriteCloser) Id() string {
	return n.id
}

var _ io.WriteCloser = (*notifyWriteCloser)(nil)
