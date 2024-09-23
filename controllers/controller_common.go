// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/microsoft/usvc-apiserver/internal/telemetry"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

type objectChange int

const (
	noChange                       objectChange = 0
	statusChanged                  objectChange = 0x1
	metadataChanged                objectChange = 0x2
	specChanged                    objectChange = 0x4
	additionalReconciliationNeeded objectChange = 0x8

	additionalReconciliationDelay = 2 * time.Second
	conflictRequeueDelay          = 100 * time.Millisecond
	reconciliationDebounceDelay   = 500 * time.Millisecond
	reconciliationMaxDelay        = 5 * time.Second

	// If the health probe result is different by timestamp only, we do not write it to the status
	// unless the existing result is older than this value.
	// This helps avoid updating the status very frequently if an object has health probes with tight intervals.
	maxStaleHealthProbeResultAge = 15 * time.Second
)

func ensureFinalizer(obj metav1.Object, finalizer string, log logr.Logger) objectChange {
	finalizers := obj.GetFinalizers()
	if slices.Contains(finalizers, finalizer) {
		return noChange
	}

	finalizers = append(finalizers, finalizer)
	obj.SetFinalizers(finalizers)
	log.V(1).Info("added finalizer", "Finalizer", finalizer)
	return metadataChanged
}

func deleteFinalizer(obj metav1.Object, finalizer string, log logr.Logger) objectChange {
	finalizers := obj.GetFinalizers()
	i := slices.Index(finalizers, finalizer)
	if i == -1 {
		return noChange
	}

	finalizers = append(finalizers[:i], finalizers[i+1:]...)
	obj.SetFinalizers(finalizers)
	log.V(1).Info("removed finalizer", "Finalizer", finalizer)
	return metadataChanged
}

const (
	numPostfixBytes = 6
)

var (
	// Base32 encoder used to generate unique postfixes for Executable replicas.
	randomNameEncoder = base32.HexEncoding.WithPadding(base32.NoPadding)
)

// Returns a name made probabilistically unique by appending a random postfix,
// together with the used random postfix and an error, if any.
func MakeUniqueName(prefix string) (string, string, error) {
	postfixBytes := make([]byte, numPostfixBytes)

	if read, err := rand.Read(postfixBytes); err != nil {
		return "", "", err
	} else if read != numPostfixBytes {
		return "", "", fmt.Errorf("could not generate %d bytes of randomness", numPostfixBytes)
	}

	postfix := strings.ToLower(randomNameEncoder.EncodeToString(postfixBytes))
	uniqueName := fmt.Sprintf("%s-%s", prefix, postfix)
	return uniqueName, postfix, nil
}

type ObjectStruct interface {
}

type DeepCopyable[T ObjectStruct] interface {
	DeepCopy() *T
}

type PObjectStruct[T ObjectStruct] interface {
	*T
	ctrl_client.Object
	apiserver_resource.Object
}

type PCopyableObjectStruct[T ObjectStruct] interface {
	DeepCopyable[T]
	PObjectStruct[T]
}

func saveChanges[T ObjectStruct, PCT PCopyableObjectStruct[T]](
	client ctrl_client.Client,
	ctx context.Context,
	obj PCT,
	patch ctrl_client.Patch,
	change objectChange,
	onSuccessfulSave func(),
	log logr.Logger,
) (ctrl.Result, error) {
	return saveChangesWithCustomReconciliationDelay[T, PCT](
		client,
		ctx,
		obj,
		patch,
		change,
		additionalReconciliationDelay,
		onSuccessfulSave,
		log,
	)
}

func saveChangesWithCustomReconciliationDelay[T ObjectStruct, PCT PCopyableObjectStruct[T]](
	client ctrl_client.Client,
	parentCtx context.Context,
	obj PCT,
	patch ctrl_client.Patch,
	change objectChange,
	customReconciliationDelay time.Duration,
	onSuccessfulSave func(),
	log logr.Logger,
) (ctrl.Result, error) {
	return telemetry.CallWithTelemetryOnErrorOnly(telemetry.GetTracer("controller-common"), "saveChanges", parentCtx, func(ctx context.Context) (ctrl.Result, error) {
		var update PCT
		var err error
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		afterGoodSave := func() {
			if onSuccessfulSave != nil {
				onSuccessfulSave()
			}
		}

		// Apply one update per reconciliation function invocation,
		// to avoid observing "partially updated" objects during subsequent reconciliations.
		switch {
		case change == noChange:
			log.V(1).Info(fmt.Sprintf("no changes detected for %s object, continue monitoring...", kind))
			return ctrl.Result{}, nil

		case (change & statusChanged) != 0:
			update = obj.DeepCopy()
			err = client.Status().Patch(ctx, update, patch)
			if err != nil {
				if errors.IsNotFound(err) {
					log.V(1).Info(fmt.Sprintf("%s status update failed as it was removed", kind))
					return ctrl.Result{}, nil
				} else if errors.IsConflict(err) {
					// Error is expected optimistic concurrency check error, simply requeue
					log.V(1).Info(fmt.Sprintf("%s status update failed due to conflict", kind))
					return ctrl.Result{RequeueAfter: conflictRequeueDelay}, nil
				} else {
					log.Error(err, fmt.Sprintf("%s status update failed", kind))
					saveFailedCounter.Add(ctx, 1)
					return ctrl.Result{}, err
				}
			} else {
				log.V(1).Info(fmt.Sprintf("%s status update succeeded", kind))
				afterGoodSave()
				statusSaveCounter.Add(ctx, 1)
			}

		case (change & (metadataChanged | specChanged)) != 0:
			update = obj.DeepCopy()
			err = client.Patch(ctx, update, patch)
			if err != nil {
				if errors.IsNotFound(err) {
					log.V(1).Info(fmt.Sprintf("%s object update failed as it was removed", kind))
					return ctrl.Result{}, nil
				} else if errors.IsConflict(err) {
					// Error is expected optimistic concurrency check error, simply requeue
					log.V(1).Info(fmt.Sprintf("%s object update failed due to conflict", kind))
					return ctrl.Result{RequeueAfter: conflictRequeueDelay}, nil
				} else {
					log.Error(err, fmt.Sprintf("%s object update failed", kind))
					saveFailedCounter.Add(ctx, 1)
					return ctrl.Result{}, err
				}
			} else {
				log.V(1).Info(fmt.Sprintf("%s object update succeeded", kind))
				afterGoodSave()
				metadataOrSpecSaveCounter.Add(ctx, 1)
			}
		}

		if (change & additionalReconciliationNeeded) != 0 {
			log.V(1).Info(fmt.Sprintf("scheduling additional reconciliation for %s...", kind))
			return ctrl.Result{RequeueAfter: customReconciliationDelay}, nil
		} else {
			return ctrl.Result{}, nil
		}
	})
}

type dcpModelObject interface {
	apiserver_resource.Object
	ctrl_client.Object
	NamespacedName() types.NamespacedName
}

type ControllerContextOption string

func NewControllerManagerOptions(lifetimeCtx context.Context, scheme *apiruntime.Scheme, log logr.Logger) ctrl.Options {
	return ctrl.Options{
		Scheme:         scheme,
		LeaderElection: false,
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		Logger:      log.WithName("ControllerManager"),
		BaseContext: func() context.Context { return lifetimeCtx },
	}
}

type ReconcilerType interface {
}

type PReconcilerType[RT ReconcilerType] interface {
	*RT
	ctrl_client.Client
}
type ObjectStateType interface {
	~string
}

type PObjectWithStatusStruct[T ObjectStruct] interface {
	PObjectStruct[T]
	apiserver_resource.ObjectWithStatusSubResource
}

// A function invoked from the reconciliation loop when an object reaches a particular state.
// The responsibilty of the state initializer is threefold:
// 1. Set the object's to the desired state (usually by modifying its Status).
// 2. Update the in-memory data structures that track the object's state (data owned by the reconciler).
// 3. Make necessary changes to the real-world resources that the object represents.
// NOTE: the initializer MUST return noChange if no changes were made to the object, in order to avoid infinite reconciliation loops
type stateInitializerFunc[
	O ObjectStruct, PO PObjectWithStatusStruct[O],
	R ReconcilerType, PR PReconcilerType[R],
	OS ObjectStateType,
] func(context.Context, PR, PO, OS, logr.Logger) objectChange

func getStateInitializer[
	O ObjectStruct, PO PObjectWithStatusStruct[O],
	R ReconcilerType, PR PReconcilerType[R],
	OS ObjectStateType,
](m map[OS]stateInitializerFunc[O, PO, R, PR, OS], state OS, log logr.Logger) stateInitializerFunc[O, PO, R, PR, OS] {
	handler, found := m[state]
	if found {
		return handler
	}

	log.Error(fmt.Errorf("could not find a handler for current object state, will use empty state handler instead"), "", "ObjectState", state)
	handler, found = m[""]
	if found {
		return handler
	}

	panic("the state handler map has no empty state handler")
}
