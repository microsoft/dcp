/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"context"
	"sync"
)

type v2NamespaceLifecycleState struct {
	activeCreates      int
	activeDeletes      int
	closed             bool
	deleteAccepted     bool
	deletionObserved   bool
	uncertainMutation  v2NamespaceMutationKind
	uncertaintyVersion uint64
	drained            chan struct{}
}

type v2NamespaceLifecycleGate struct {
	lock               sync.Mutex
	namespaces         map[string]*v2NamespaceLifecycleState
	namespaceMutations map[string]*v2NamespaceMutationState
	refreshRequested   chan struct{}
}

func newV2NamespaceLifecycleGate() *v2NamespaceLifecycleGate {
	return &v2NamespaceLifecycleGate{
		namespaces:         map[string]*v2NamespaceLifecycleState{},
		namespaceMutations: map[string]*v2NamespaceMutationState{},
		refreshRequested:   make(chan struct{}, 1),
	}
}

type v2NamespaceMutationKind uint8

const (
	v2NamespaceMutationNone v2NamespaceMutationKind = iota
	v2NamespaceMutationCreate
	v2NamespaceMutationDelete
)

type v2NamespaceMutationOutcome uint8

const (
	v2NamespaceMutationRejected v2NamespaceMutationOutcome = iota
	v2NamespaceMutationAccepted
	v2NamespaceMutationUncertain
)

type v2NamespaceMutationState struct {
	available  chan struct{}
	references int
}

type v2NamespaceMutationLease struct {
	gate      *v2NamespaceLifecycleGate
	namespace string
	state     *v2NamespaceMutationState
	once      sync.Once
}

func (gate *v2NamespaceLifecycleGate) beginNamespaceMutation(
	ctx context.Context,
	namespace string,
) (*v2NamespaceMutationLease, error) {
	gate.lock.Lock()
	state := gate.namespaceMutations[namespace]
	if state == nil {
		state = &v2NamespaceMutationState{available: make(chan struct{}, 1)}
		state.available <- struct{}{}
		gate.namespaceMutations[namespace] = state
	}
	state.references++
	if lifecycleState := gate.namespaces[namespace]; lifecycleState != nil {
		lifecycleState.uncertaintyVersion++
	}
	gate.lock.Unlock()

	select {
	case <-state.available:
		if ctx.Err() != nil {
			state.available <- struct{}{}
			gate.releaseNamespaceMutationReference(namespace, state)
			return nil, ctx.Err()
		}
		return &v2NamespaceMutationLease{
			gate:      gate,
			namespace: namespace,
			state:     state,
		}, nil
	case <-ctx.Done():
		gate.releaseNamespaceMutationReference(namespace, state)
		return nil, ctx.Err()
	}
}

func (gate *v2NamespaceLifecycleGate) releaseNamespaceMutationReference(
	namespace string,
	state *v2NamespaceMutationState,
) {
	gate.lock.Lock()

	refreshNeeded := false
	state.references--
	if state.references == 0 && gate.namespaceMutations[namespace] == state {
		delete(gate.namespaceMutations, namespace)
		lifecycleState := gate.namespaces[namespace]
		refreshNeeded = lifecycleState != nil && lifecycleState.closed
	}
	gate.lock.Unlock()

	if refreshNeeded {
		gate.requestRefresh()
	}
}

func (lease *v2NamespaceMutationLease) complete() {
	lease.once.Do(func() {
		lease.state.available <- struct{}{}
		lease.gate.releaseNamespaceMutationReference(lease.namespace, lease.state)
	})
}

func (gate *v2NamespaceLifecycleGate) beginCreate(namespace string) (func(), bool) {
	gate.lock.Lock()
	state := gate.namespaces[namespace]
	if state != nil && state.closed {
		gate.lock.Unlock()
		return nil, false
	}
	if state == nil {
		state = &v2NamespaceLifecycleState{}
		gate.namespaces[namespace] = state
	}
	state.activeCreates++
	gate.lock.Unlock()

	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() {
			gate.lock.Lock()
			defer gate.lock.Unlock()

			state.activeCreates--
			if state.activeCreates != 0 {
				return
			}
			if state.drained != nil {
				close(state.drained)
				state.drained = nil
			}
			if !state.closed && gate.namespaces[namespace] == state {
				delete(gate.namespaces, namespace)
			}
		})
	}, true
}

type v2NamespaceDeleteLease struct {
	gate      *v2NamespaceLifecycleGate
	namespace string
	state     *v2NamespaceLifecycleState
	once      sync.Once
}

func (gate *v2NamespaceLifecycleGate) beginDelete(ctx context.Context, namespace string) (*v2NamespaceDeleteLease, error) {
	gate.lock.Lock()
	state := gate.namespaces[namespace]
	if state == nil {
		state = &v2NamespaceLifecycleState{}
		gate.namespaces[namespace] = state
	}
	state.activeDeletes++
	state.closed = true
	lease := &v2NamespaceDeleteLease{
		gate:      gate,
		namespace: namespace,
		state:     state,
	}
	if state.activeCreates == 0 {
		gate.lock.Unlock()
		return lease, nil
	}
	if state.drained == nil {
		state.drained = make(chan struct{})
	}
	drained := state.drained
	gate.lock.Unlock()

	select {
	case <-drained:
		return lease, nil
	case <-ctx.Done():
		lease.complete(v2NamespaceMutationRejected)
		return nil, ctx.Err()
	}
}

func (lease *v2NamespaceDeleteLease) complete(outcome v2NamespaceMutationOutcome) {
	lease.once.Do(func() {
		lease.gate.lock.Lock()

		refreshNeeded := false
		switch outcome {
		case v2NamespaceMutationAccepted:
			lease.state.deleteAccepted = true
			lease.state.closed = true
			lease.state.uncertainMutation = v2NamespaceMutationNone
		case v2NamespaceMutationUncertain:
			lease.state.closed = true
			lease.state.uncertainMutation = v2NamespaceMutationDelete
			lease.state.uncertaintyVersion++
			refreshNeeded = true
		}
		lease.state.activeDeletes--
		if lease.gate.removeObservedNamespaceDeletion(lease.namespace, lease.state) {
			lease.gate.lock.Unlock()
			return
		}
		if lease.state.activeDeletes == 0 && !lease.state.deleteAccepted &&
			lease.state.uncertainMutation == v2NamespaceMutationNone {
			lease.state.closed = false
		}
		if !lease.state.closed && lease.state.activeCreates == 0 &&
			lease.gate.namespaces[lease.namespace] == lease.state {
			delete(lease.gate.namespaces, lease.namespace)
		}
		lease.gate.lock.Unlock()

		if refreshNeeded {
			lease.gate.requestRefresh()
		}
	})
}

type v2NamespaceLifecycleSnapshot struct {
	state              *v2NamespaceLifecycleState
	uncertaintyVersion uint64
}

func (gate *v2NamespaceLifecycleGate) closedNamespaceStates() map[string]v2NamespaceLifecycleSnapshot {
	gate.lock.Lock()
	defer gate.lock.Unlock()

	states := make(map[string]v2NamespaceLifecycleSnapshot)
	for namespace, state := range gate.namespaces {
		if state.closed && gate.namespaceMutations[namespace] == nil {
			states[namespace] = v2NamespaceLifecycleSnapshot{
				state:              state,
				uncertaintyVersion: state.uncertaintyVersion,
			}
		}
	}
	return states
}

type v2NamespaceStorageState struct {
	terminating bool
}

func (gate *v2NamespaceLifecycleGate) observeNamespaces(
	namespaces map[string]v2NamespaceStorageState,
	closedStates map[string]v2NamespaceLifecycleSnapshot,
) {
	gate.lock.Lock()
	defer gate.lock.Unlock()

	for namespace, snapshot := range closedStates {
		state := snapshot.state
		if gate.namespaces[namespace] != state || !state.closed ||
			state.uncertaintyVersion != snapshot.uncertaintyVersion ||
			gate.namespaceMutations[namespace] != nil {
			continue
		}
		storageState, found := namespaces[namespace]
		if !found {
			state.deletionObserved = true
			gate.removeObservedNamespaceDeletion(namespace, state)
			continue
		}
		if state.uncertainMutation == v2NamespaceMutationNone || state.activeDeletes != 0 {
			continue
		}

		state.uncertainMutation = v2NamespaceMutationNone
		if storageState.terminating {
			state.deleteAccepted = true
			continue
		}

		state.closed = false
		state.deleteAccepted = false
		state.deletionObserved = false
		if state.activeCreates == 0 && state.activeDeletes == 0 {
			delete(gate.namespaces, namespace)
		}
	}
}

func (gate *v2NamespaceLifecycleGate) markCreateUncertain(namespace string) {
	gate.lock.Lock()
	state := gate.namespaces[namespace]
	if state == nil || !state.closed {
		gate.lock.Unlock()
		return
	}
	state.uncertainMutation = v2NamespaceMutationCreate
	state.uncertaintyVersion++
	gate.lock.Unlock()

	gate.requestRefresh()
}

func (gate *v2NamespaceLifecycleGate) requestRefresh() {
	select {
	case gate.refreshRequested <- struct{}{}:
	default:
	}
}

// removeObservedNamespaceDeletion removes a closed lifecycle state after storage confirms that
// the Namespace is gone and all requests using the state have completed. The gate lock must be held.
func (gate *v2NamespaceLifecycleGate) removeObservedNamespaceDeletion(
	namespace string,
	state *v2NamespaceLifecycleState,
) bool {
	if !state.deletionObserved || state.activeCreates != 0 || state.activeDeletes != 0 ||
		gate.namespaces[namespace] != state {
		return false
	}
	delete(gate.namespaces, namespace)
	return true
}

func (gate *v2NamespaceLifecycleGate) open(namespace string) {
	gate.lock.Lock()
	defer gate.lock.Unlock()

	state := gate.namespaces[namespace]
	if state == nil {
		return
	}
	state.closed = false
	state.deleteAccepted = false
	state.deletionObserved = false
	state.uncertainMutation = v2NamespaceMutationNone
	if state.activeCreates == 0 && state.activeDeletes == 0 {
		delete(gate.namespaces, namespace)
	}
}
