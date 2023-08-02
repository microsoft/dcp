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

type reconcileTrigger[ReconcileInput any] func(reconcileTriggerInput[ReconcileInput]) error

type reconcilerDebounceRunner[ReconcileInput any] func(reconcileTriggerInput[ReconcileInput]) (any, error)

type objectDebounce[ReconcileInput any] resiliency.DebounceLast[reconcileTriggerInput[ReconcileInput], any, reconcilerDebounceRunner[ReconcileInput]]

func (od *objectDebounce[ReconcileInput]) run(ctx context.Context, input reconcileTriggerInput[ReconcileInput]) (any, error) {
	// Not exactly sure why Go cannot do this cast automatically, but whatever...
	rawDebounce := (resiliency.DebounceLast[reconcileTriggerInput[ReconcileInput], any, reconcilerDebounceRunner[ReconcileInput]](*od))
	return rawDebounce.Run(ctx, input)
}

type reconcilerDebouncer[ReconcileInput any] struct {
	debounceMap   *syncmap.Map[types.NamespacedName, objectDebounce[ReconcileInput]]
	debounceDelay time.Duration
}

func newReconcilerDebouncer[ReconcileInput any](debounceDelay time.Duration) *reconcilerDebouncer[ReconcileInput] {
	return &reconcilerDebouncer[ReconcileInput]{
		debounceMap:   &syncmap.Map[types.NamespacedName, objectDebounce[ReconcileInput]]{},
		debounceDelay: debounceDelay,
	}
}

func objectDebounceFactory[ReconcileInput any](trigger reconcileTrigger[ReconcileInput], debounceDelay time.Duration) objectDebounce[ReconcileInput] {
	debounce := resiliency.NewDebounceLast[reconcileTriggerInput[ReconcileInput], any, reconcilerDebounceRunner[ReconcileInput]](
		func(input reconcileTriggerInput[ReconcileInput]) (any, error) {
			return nil, trigger(input)
		},
		debounceDelay,
	)

	return objectDebounce[ReconcileInput](debounce)
}

// Call OnReconcile when an object is being reconciled. This prevents the inner debouncer map from growing indefinitely.
// This method is safe to call regardless whether the name was ever seen.
func (rd *reconcilerDebouncer[ReconcileInput]) OnReconcile(name types.NamespacedName) {
	rd.debounceMap.Delete(name)
}

// Tries to trigger reconciliation for an object identified by name, after appropriate debouncing.
func (rd *reconcilerDebouncer[ReconcileInput]) ReconciliationNeeded(name types.NamespacedName, ri ReconcileInput, trigger reconcileTrigger[ReconcileInput]) error {
	debounce, _ := rd.debounceMap.LoadOrStoreNew(name, func() objectDebounce[ReconcileInput] {
		return objectDebounceFactory[ReconcileInput](trigger, rd.debounceDelay)
	})

	input := reconcileTriggerInput[ReconcileInput]{
		target: name,
		input:  ri,
	}

	_, err := debounce.run(context.Background(), input)
	return err
}
