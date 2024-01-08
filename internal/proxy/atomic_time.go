// Copyright (c) Microsoft Corporation. All rights reserved.

package proxy

import (
	"sync/atomic"
	"time"
)

type AtomicTime struct {
	unixNano int64
}

func (at *AtomicTime) Load() time.Time {
	return time.Unix(0, atomic.LoadInt64(&at.unixNano))
}

func (at *AtomicTime) Store(t time.Time) {
	if t.IsZero() {
		panic("Cannot store zero time")
	}
	atomic.StoreInt64(&at.unixNano, t.UnixNano())
}

func (at *AtomicTime) TryAdvancingTo(t time.Time) {
	for {
		oldNano := atomic.LoadInt64(&at.unixNano)
		newNano := t.UnixNano()
		if oldNano >= newNano {
			return
		}
		if atomic.CompareAndSwapInt64(&at.unixNano, oldNano, newNano) {
			return
		}
	}
}

func AtomicTimeNow() *AtomicTime {
	var at AtomicTime
	at.Store(time.Now())
	return &at
}
