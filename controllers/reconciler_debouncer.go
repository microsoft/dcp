// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

// ReconcilerDebouncer helps debounce calls that trigger reconciliation. Useful for processing external events
// that require reconciliation for an object, and that might come in "batches" of quick succession.

type reconcileTriggerInput[ReconcileInput any] struct {
	target types.NamespacedName
	input  ReconcileInput
}

type reconcileTrigger[ReconcileInput any] func(reconcileTriggerInput[ReconcileInput])

type objectDebounce[ReconcileInput any] resiliency.DebounceLastAction[reconcileTriggerInput[ReconcileInput]]

func (od *objectDebounce[ReconcileInput]) run(ctx context.Context, input reconcileTriggerInput[ReconcileInput]) {
	debounceRaw := resiliency.DebounceLastAction[reconcileTriggerInput[ReconcileInput]](*od)
	debounceRaw.Run(ctx, input)
}

type reconcilerDebouncer[ReconcileInput any] struct {
	debounceMap   *syncmap.Map[types.NamespacedName, *objectDebounce[ReconcileInput]]
	debounceDelay time.Duration
	maxDelay      time.Duration
}

func newReconcilerDebouncer[ReconcileInput any]() *reconcilerDebouncer[ReconcileInput] {
	return &reconcilerDebouncer[ReconcileInput]{
		debounceMap:   &syncmap.Map[types.NamespacedName, *objectDebounce[ReconcileInput]]{},
		debounceDelay: reconciliationDebounceDelay,
		maxDelay:      reconciliationMaxDelay,
	}
}

func objectDebounceFactory[ReconcileInput any](trigger reconcileTrigger[ReconcileInput], debounceDelay, maxDelay time.Duration) *objectDebounce[ReconcileInput] {
	debounce := resiliency.NewDebounceLastAction(trigger, debounceDelay, maxDelay)
	return (*objectDebounce[ReconcileInput])(debounce)
}

// Call OnReconcile when an object is being reconciled. This prevents the inner debouncer map from growing indefinitely.
// This method is safe to call regardless whether the name was ever seen.
func (rd *reconcilerDebouncer[ReconcileInput]) OnReconcile(name types.NamespacedName) {
	rd.debounceMap.Delete(name)
}

// Tries to trigger reconciliation for an object identified by name, after appropriate debouncing.
func (rd *reconcilerDebouncer[ReconcileInput]) ReconciliationNeeded(ctx context.Context, name types.NamespacedName, ri ReconcileInput, trigger reconcileTrigger[ReconcileInput]) {
	debounce, _ := rd.debounceMap.LoadOrStoreNew(name, func() *objectDebounce[ReconcileInput] {
		return objectDebounceFactory(trigger, rd.debounceDelay, rd.maxDelay)
	})

	input := reconcileTriggerInput[ReconcileInput]{
		target: name,
		input:  ri,
	}

	debounce.run(context.Background(), input)
}
