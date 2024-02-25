// Copyright (c) Microsoft Corporation. All rights reserved.

package logs

import (
	"context"
	"io"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

const (
	oldDescriptorScavengeInterval    = 1 * time.Minute
	oldDescriptorNonUseTimeout       = 15 * time.Second
	DefaultDescriptorDisposalTimeout = 2 * time.Second
)

type LogDescriptorSet struct {
	lock        *sync.Mutex
	lifetimeCtx context.Context
	descriptors map[types.UID]*LogDescriptor
	logsFolder  string
}

func NewLogDescriptorSet(lifetimeCtx context.Context, logsFolder string) *LogDescriptorSet {
	lds := &LogDescriptorSet{
		lock:        &sync.Mutex{},
		lifetimeCtx: lifetimeCtx,
		descriptors: make(map[types.UID]*LogDescriptor),
		logsFolder:  logsFolder,
	}

	go lds.scavenger()

	return lds
}

// Acquires a log descriptor for the given resource UID. If the descriptor does not exist, it is created.
// Apart from the log desciptor, the function returns the stdOut and stdErr writers which are used
// for log capturing and log reading.
//
// The returned bool indicates whether the descriptor was created.
// This allows the caller to know whether they should start the log capturing process.
func (lds *LogDescriptorSet) AcquireForResource(
	descriptorCtx context.Context,
	cancel context.CancelFunc,
	resourceName types.NamespacedName,
	resourceUID types.UID,
) (*LogDescriptor, io.Writer, io.Writer, bool, error) {
	lds.lock.Lock()

	ld, found := lds.descriptors[resourceUID]
	if !found || ld.IsDisposed() {
		ld = NewLogDescriptor(descriptorCtx, cancel, resourceName, resourceUID)
		lds.descriptors[resourceUID] = ld
	}

	lds.lock.Unlock()

	stdOut, stdErr, created, err := ld.EnableLogCapturing(lds.logsFolder)
	// If EnableLogCapturing fails, the descriptor is disposed.
	// It will be removed from the set either by the check above
	// (next time someone attempts to watch logs for the same resource)
	// or by the scavenger.

	return ld, stdOut, stdErr, created, err
}

func (lds *LogDescriptorSet) ReleaseForResource(resourceUID types.UID) {
	lds.lock.Lock()
	defer lds.lock.Unlock()

	ld, found := lds.descriptors[resourceUID]
	if !found {
		return
	}
	delete(lds.descriptors, resourceUID)
	go func() {
		// CONSIDER logging errors from descriptor disposal
		_ = ld.Dispose(lds.lifetimeCtx, DefaultDescriptorDisposalTimeout)
	}()
}

func (lds *LogDescriptorSet) scavenger() {
	timer := time.NewTimer(oldDescriptorScavengeInterval)

	for {
		select {

		case <-lds.lifetimeCtx.Done():
			return

		case <-timer.C:
			lds.lock.Lock()

			oldDescriptors := make([]*LogDescriptor, len(lds.descriptors))
			for _, ld := range lds.descriptors {
				consumers, lastUsed := ld.Usage()
				if ld.IsDisposed() || (consumers == 0 && time.Since(lastUsed) > oldDescriptorNonUseTimeout) {
					oldDescriptors = append(oldDescriptors, ld)

				}
			}

			for _, ld := range oldDescriptors {
				delete(lds.descriptors, ld.ResourceUID)
			}

			lds.lock.Unlock()

			// This should be relatively fast since there are no watchers, but still,
			// better to do this not holding the lock.
			for _, ld := range oldDescriptors {
				// CONSIDER logging errors from descriptor disposal
				_ = ld.Dispose(lds.lifetimeCtx, DefaultDescriptorDisposalTimeout)
			}

			timer.Reset(oldDescriptorScavengeInterval)
		}
	}
}
