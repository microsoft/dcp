/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/syncmap"
)

// ReconcilerDebouncer helps debounce calls that trigger reconciliation. Useful for processing external events
// that require reconciliation for an object, and that might come in "batches" of quick succession.

type reconcileTrigger func(types.NamespacedName)

type objectDebounce struct {
	debounce *resiliency.DebounceLastAction
}

func (od *objectDebounce) run(ctx context.Context) {
	od.debounce.Run(ctx)
}

type reconcilerDebouncer struct {
	debounceMap   *syncmap.Map[types.NamespacedName, *objectDebounce]
	debounceDelay time.Duration
	maxDelay      time.Duration
}

func newReconcilerDebouncer() *reconcilerDebouncer {
	return &reconcilerDebouncer{
		debounceMap:   &syncmap.Map[types.NamespacedName, *objectDebounce]{},
		debounceDelay: reconciliationDebounceDelay,
		maxDelay:      reconciliationMaxDelay,
	}
}

func objectDebounceFactory(trigger reconcileTrigger, name types.NamespacedName, debounceDelay, maxDelay time.Duration) *objectDebounce {
	return &objectDebounce{
		debounce: resiliency.NewDebounceLastAction(func() {
			trigger(name)
		}, debounceDelay, maxDelay),
	}
}

// Call OnReconcile when an object is being reconciled. This prevents the inner debouncer map from growing indefinitely.
// This method is safe to call regardless whether the name was ever seen.
func (rd *reconcilerDebouncer) OnReconcile(name types.NamespacedName) {
	rd.debounceMap.Delete(name)
}

// Tries to trigger reconciliation for an object identified by name, after appropriate debouncing.
func (rd *reconcilerDebouncer) ReconciliationNeeded(ctx context.Context, name types.NamespacedName, trigger reconcileTrigger) {
	debounce, _ := rd.debounceMap.LoadOrStoreNew(name, func() *objectDebounce {
		return objectDebounceFactory(trigger, name, rd.debounceDelay, rd.maxDelay)
	})

	debounce.run(ctx)
}
