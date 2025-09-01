// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"fmt"
	mathrand "math/rand"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/metric"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	ctrl_event "sigs.k8s.io/controller-runtime/pkg/event"
	ctrl_handler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrl_source "sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/microsoft/usvc-apiserver/internal/telemetry"
	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

type ReconcilerBase[T commonapi.ObjectStruct, PT commonapi.PCopyableObjectStruct[T]] struct {
	// The regular, cached client
	ctrl_client.Client

	// Non-cached client, to be used if a optimistic concurrency conflict occurs
	NoCacheClient ctrl_client.Reader

	// Base logger for the reconciler
	Log logr.Logger

	// Reconciliation sequence number, incremented on each reconciliation, mostly for logging purposes
	reconciliationSeqNo uint32

	// Debouncer used to schedule reconciliations.
	debouncer *reconcilerDebouncer[struct{}]

	// Channel used to trigger programmatic reconciliations
	notifyRunChanged *concurrency.UnboundedChan[ctrl_event.GenericEvent]

	// Reconciler lifetime context, used to cancel operations during reconciler shutdown
	LifetimeCtx context.Context

	// A map used for exponential backoff of reconciliation requests
	// when optimistic concurrency conflicts occur during object updates
	conflictBackoff *syncmap.Map[types.NamespacedName, *backoff.ExponentialBackOff]

	// The kind of the object being reconciled, for logging purposes
	kind string
}

func NewReconcilerBase[T commonapi.ObjectStruct, PT commonapi.PCopyableObjectStruct[T]](
	client ctrl_client.Client,
	noCacheClient ctrl_client.Reader,
	log logr.Logger,
	lifetimeCtx context.Context,
) *ReconcilerBase[T, PT] {
	return &ReconcilerBase[T, PT]{
		Client:           client,
		NoCacheClient:    noCacheClient,
		Log:              log,
		debouncer:        newReconcilerDebouncer[struct{}](),
		notifyRunChanged: concurrency.NewUnboundedChan[ctrl_event.GenericEvent](lifetimeCtx),
		LifetimeCtx:      lifetimeCtx,
		conflictBackoff:  &syncmap.Map[types.NamespacedName, *backoff.ExponentialBackOff]{},
		kind:             reflect.TypeFor[T]().Name(),
	}
}

// Marks the startup of another reconciliation.
// Returns object reader and a derived log to be used for the current reconciliation.
// The log has unique reconciliation sequence number and the namespaced name of the object being reconciled.
func (rb *ReconcilerBase[T, PT]) StartReconciliation(req ctrl.Request) (ctrl_client.Reader, logr.Logger) {
	rb.debouncer.OnReconcile(req.NamespacedName)

	var reader ctrl_client.Reader = rb.Client
	_, conflictOccurred := rb.conflictBackoff.Load(req.NamespacedName)
	if conflictOccurred {
		reader = rb.NoCacheClient
	}

	log := rb.Log.WithValues(
		rb.kind, req.NamespacedName.String(),
		"Reconciliation", atomic.AddUint32(&rb.reconciliationSeqNo, 1),
	)

	return reader, log
}

// Schedules reconciliation for specific object identified by namespaced name.
func (rb *ReconcilerBase[T, PT]) ScheduleReconciliation(nn types.NamespacedName) {
	rb.debouncer.ReconciliationNeeded(rb.LifetimeCtx, nn, struct{}{}, func(rti reconcileTriggerInput[struct{}]) {
		var obj PT = new(T)
		om := obj.GetObjectMeta()
		om.Name = rti.target.Name
		om.Namespace = rti.target.Namespace
		event := ctrl_event.GenericEvent{
			Object: obj,
		}
		rb.notifyRunChanged.In <- event
	})
}

// Gets a reconciliation event source for triggering reconciliations programmatically.
// The returned source should be passed to WatchesRawSource() controller builder method.
func (rb *ReconcilerBase[T, PT]) GetReconciliationEventSource() ctrl_source.Source {
	src := ctrl_source.Channel(rb.notifyRunChanged.Out, &ctrl_handler.EnqueueRequestForObject{})
	return src
}

// Saves changes to the object and schedules additional reconciliation as appropriate.
// Standard delay is used for additional reconciliation.
// If conflicts occurred during previous save, additional delay will be exponentially increased up to maxConflictDelay.
func (rb *ReconcilerBase[T, PT]) SaveChanges(
	client ctrl_client.Client,
	ctx context.Context,
	obj PT,
	patch ctrl_client.Patch,
	change objectChange,
	onSuccessfulSave func(),
	log logr.Logger,
) (ctrl.Result, error) {
	return rb.SaveChangesWithDelay(client, ctx, obj, patch, change, standardDelay, onSuccessfulSave, log)
}

type additionalReconciliationDelay int

const (
	standardDelay additionalReconciliationDelay = iota
	longDelay
)

// Saves changes to the object and schedules additional reconciliation as appropriate.
// Standard or long delay will be used for additional reconciliation, depending on the flag passed.
// If long delay is requested, it implies that additional reconciliation is needed.
// If conflicts occurred during previous save, reconciliation delay will be exponentially increased up to maxConflictDelay.
func (rb *ReconcilerBase[T, PT]) SaveChangesWithDelay(
	client ctrl_client.Client,
	ctx context.Context,
	obj PT,
	patch ctrl_client.Patch,
	change objectChange,
	delay additionalReconciliationDelay,
	onSuccessfulSave func(),
	log logr.Logger,
) (ctrl.Result, error) {
	return telemetry.CallWithTelemetryOnErrorOnly(telemetry.GetTracer("controller-common"), "saveChanges", ctx, func(ctx context.Context) (ctrl.Result, error) {
		var update PT
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		afterGoodSave := func() {
			if onSuccessfulSave != nil {
				onSuccessfulSave()
			}
		}

		if delay == longDelay {
			change |= additionalReconciliationNeeded
		}

		doUpdate := func(p Patcher, operation string, saveSuccesCounter metric.Int64Counter) (ctrl.Result, error) {
			update = obj.DeepCopy()
			updateErr := p(ctx, update, patch)

			if k8serrors.IsConflict(updateErr) {
				log.V(1).Info(fmt.Sprintf("%s %s failed due to conflict", kind, operation))
				b, _ := rb.conflictBackoff.LoadOrStoreNew(obj.NamespacedName(), conflictRequeueBackoff)
				return ctrl.Result{RequeueAfter: b.NextBackOff()}, nil
			} else {
				rb.conflictBackoff.Delete(obj.NamespacedName())
			}

			if k8serrors.IsNotFound(updateErr) {
				log.V(1).Info(fmt.Sprintf("%s %s failed as the object was removed", kind, operation))
				return ctrl.Result{}, nil
			} else if updateErr != nil {
				log.Error(updateErr, fmt.Sprintf("%s %s failed", kind, operation))
				saveFailedCounter.Add(ctx, 1)
				return ctrl.Result{}, updateErr
			} else {
				log.V(1).Info(fmt.Sprintf("%s %s succeeded", kind, operation))
				afterGoodSave()
				saveSuccesCounter.Add(ctx, 1)
				return ctrl.Result{}, nil
			}
		}

		if change == noChange {
			log.V(1).Info(fmt.Sprintf("no changes detected for %s object, continue monitoring...", kind))
			return ctrl.Result{}, nil
		}

		var res ctrl.Result
		var err error

		// Apply one update per reconciliation function invocation (no simultaneous status and metadata/spec updates),
		// to avoid observing "partially updated" objects during subsequent reconciliations.

		if (change & statusChanged) != 0 {
			res, err = doUpdate(func(ctx context.Context, obj ctrl_client.Object, patch ctrl_client.Patch) error {
				return client.Status().Patch(ctx, obj, patch)
			}, "status update", statusSaveCounter)
		} else if (change & (metadataChanged | specChanged)) != 0 {
			res, err = doUpdate(func(ctx context.Context, obj ctrl_client.Object, patch ctrl_client.Patch) error {
				return client.Patch(ctx, obj, patch)
			}, "update", metadataOrSpecSaveCounter)
		}

		if res.RequeueAfter > 0 {
			// Update logic already determined that additional reconciliation is needed.
			return res, err
		} else if (change & additionalReconciliationNeeded) != 0 {
			log.V(1).Info(fmt.Sprintf("scheduling additional reconciliation for %s...", kind))
			reconciliationDelay := defaultAdditionalReconciliationDelay
			reconciliationJitter := defaultAdditionalReconciliationJitter
			if delay == longDelay {
				reconciliationDelay = minimumLongAdditionalReconciliationDelay
				reconciliationJitter = minimumLongAdditionalReconciliationDelay
			}
			reconciliationDelay += time.Duration(mathrand.Int63n(int64(reconciliationJitter)))
			return ctrl.Result{RequeueAfter: reconciliationDelay}, err
		} else {
			return res, err
		}
	})
}

func conflictRequeueBackoff() *backoff.ExponentialBackOff {
	return backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(conflictRequeueDelay),
		backoff.WithMaxInterval(reconciliationMaxDelay),
		// No elapsed time
	)
}

type Patcher func(ctx context.Context, obj ctrl_client.Object, patch ctrl_client.Patch) error
