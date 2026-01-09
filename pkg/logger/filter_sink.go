/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logger

import (
	"sync/atomic"

	"github.com/go-logr/logr"
)

type filterSink struct {
	active    *atomic.Bool
	maxLife   uint32
	life      *atomic.Uint32
	filter    string
	innerSink logr.LogSink
}

func newFilterSink(filter string, maxLife uint32, innerSink logr.LogSink) *filterSink {
	if maxLife == 0 {
		panic("maxLife must be greater than 0")
	}

	fs := filterSink{
		active:    &atomic.Bool{},
		maxLife:   maxLife,
		life:      &atomic.Uint32{},
		filter:    filter,
		innerSink: innerSink,
	}

	fs.active.Store(true)
	return &fs
}

func (fs *filterSink) Init(info logr.RuntimeInfo) {
	fs.innerSink.Init(info)
}

func (fs *filterSink) Enabled(level int) bool {
	return fs.innerSink.Enabled(level)
}

func (fs *filterSink) Info(level int, msg string, keysAndValues ...any) {
	fs.innerSink.Info(level, msg, keysAndValues...)
}

func (fs *filterSink) Error(err error, msg string, keysAndValues ...any) {
	active := fs.active.Load()
	if !active {
		fs.innerSink.Error(err, msg, keysAndValues...)
		return
	}

	if msg == fs.filter {
		fs.active.Store(false)
		return
	}

	life := fs.life.Add(1)
	if life >= fs.maxLife {
		fs.active.Store(false)
	}

	fs.innerSink.Error(err, msg, keysAndValues...)
}

func (fs *filterSink) WithValues(keysAndValues ...any) logr.LogSink {
	return &filterSink{
		active:    fs.active,
		maxLife:   fs.maxLife,
		life:      fs.life,
		filter:    fs.filter,
		innerSink: fs.innerSink.WithValues(keysAndValues...),
	}
}

func (fs *filterSink) WithName(name string) logr.LogSink {
	return &filterSink{
		active:    fs.active,
		maxLife:   fs.maxLife,
		life:      fs.life,
		filter:    fs.filter,
		innerSink: fs.innerSink.WithName(name),
	}
}

var _ logr.LogSink = (*filterSink)(nil)
