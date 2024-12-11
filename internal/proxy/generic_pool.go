// Copyright (c) Microsoft Corporation. All rights reserved.

package proxy

import (
	"sync"
)

type genericPool[T any] struct {
	inner   *sync.Pool
	newItem func() T
}

func newGenericPool[T any](newItem func() T) *genericPool[T] {
	inner := &sync.Pool{
		New: func() any {
			return newItem()
		},
	}
	return &genericPool[T]{
		inner:   inner,
		newItem: newItem,
	}
}

func (gp *genericPool[T]) Get() T {
	if v := gp.inner.Get(); v != nil {
		return v.(T)
	}
	return gp.newItem()
}

func (gp *genericPool[T]) Put(item T) {
	gp.inner.Put(item)
}
