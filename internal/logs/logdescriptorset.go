// Copyright (c) Microsoft Corporation. All rights reserved.

package logs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/go-logr/logr"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"k8s.io/apimachinery/pkg/types"
)

const (
	oldDescriptorScavengeInterval     = 30 * time.Second
	oldDescriptorNonUseTimeout        = 10 * time.Second
	DefaultDescriptorDisposalTimeout  = 2 * time.Second
	shutdownDescriptorDisposalTimeout = 5 * time.Second
)

// A LogDescriptorSet is a set of log descriptors for specific kind of a resource.
// We have one descriptor set for Containers and another for Executables.
// LogDescriptorSet ensures that there is at most one log descriptor per resource UID,
// and that the descriptors are disposed when there are no clients interested in logs
// from a corresponding resource.
type LogDescriptorSet struct {
	lock        *sync.Mutex
	lifetimeCtx context.Context
	descriptors map[types.UID]*LogDescriptor
	logsFolder  string
	log         logr.Logger
}

func NewLogDescriptorSet(lifetimeCtx context.Context, logsFolder string, log logr.Logger) *LogDescriptorSet {
	lds := &LogDescriptorSet{
		lock:        &sync.Mutex{},
		lifetimeCtx: lifetimeCtx,
		descriptors: make(map[types.UID]*LogDescriptor),
		logsFolder:  logsFolder,
		log:         log,
	}

	go lds.scavenger()

	return lds
}

func (lds *LogDescriptorSet) Dispose() error {
	lds.lock.Lock()
	allDescriptors := maps.Values(lds.descriptors)
	clear(lds.descriptors)
	lds.lock.Unlock()

	ldDisposeErrors := slices.MapConcurrent[*LogDescriptor, error](allDescriptors, func(ld *LogDescriptor) error {
		if ld.IsDisposed() {
			return nil // Dispose() on a disposed descriptor is a no-op, but let's not do it anyway.
		}

		// We are shutting down and the lifetime context is already done, so not using it.
		// Best-effort attempt to do a cleanup.

		disposeErr := ld.Dispose(context.Background(), shutdownDescriptorDisposalTimeout)
		if disposeErr != nil {
			lds.log.Error(disposeErr, "Error disposing log descriptor",
				"ResourceName", ld.ResourceName,
				"ResourceUID", ld.ResourceUID,
			)
		}
		return disposeErr
	}, slices.MaxConcurrency)

	return errors.Join(ldDisposeErrors...)
}

// Acquires a log descriptor for the given resource UID. If the descriptor does not exist, it is created.
// Apart from the log descriptor, the function returns the stdOut and stdErr writers which are used
// for log capturing and log reading.
//
// The returned bool indicates whether the descriptor was created.
// This allows the caller to know whether they should start the log capturing process.
func (lds *LogDescriptorSet) AcquireForResource(
	descriptorCtx context.Context,
	cancel context.CancelFunc,
	resourceName types.NamespacedName,
	resourceUID types.UID,
) (*LogDescriptor, usvc_io.WriteSyncerCloser, usvc_io.WriteSyncerCloser, bool, error) {
	lds.lock.Lock()

	if lds.lifetimeCtx.Err() != nil {
		lds.lock.Unlock()
		return nil, nil, nil, false, lds.lifetimeCtx.Err()
	}

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

	if ld.IsDisposed() {
		return
	}

	go func() {
		disposeErr := ld.Dispose(lds.lifetimeCtx, DefaultDescriptorDisposalTimeout)
		if disposeErr != nil {
			lds.log.Error(disposeErr, "Error disposing log descriptor",
				"ResourceName", ld.ResourceName,
				"ResourceUID", resourceUID,
			)
		}
	}()
}

func (lds *LogDescriptorSet) scavenger() {
	timer := time.NewTimer(oldDescriptorScavengeInterval)
	defer timer.Stop()

	for {
		select {

		case <-lds.lifetimeCtx.Done():
			return

		case <-timer.C:
			lds.lock.Lock()

			var oldDescriptors []*LogDescriptor
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
				disposeErr := ld.Dispose(lds.lifetimeCtx, DefaultDescriptorDisposalTimeout)
				if disposeErr != nil {
					lds.log.Error(disposeErr, "Error disposing log descriptor",
						"ResourceName", ld.ResourceName,
						"ResourceUID", ld.ResourceUID,
					)
				}
			}

			timer.Reset(oldDescriptorScavengeInterval)
		}
	}
}
