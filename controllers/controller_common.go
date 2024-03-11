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
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	numPostfixBytes = 4
)

var (
	// Base32 encoder used to generate unique postfixes for Executable replicas.
	randomNameEncoder = base32.HexEncoding.WithPadding(base32.NoPadding)
)

func MakeUniqueName(prefix string) (string, error) {
	postfixBytes := make([]byte, numPostfixBytes)

	if read, err := rand.Read(postfixBytes); err != nil {
		return "", err
	} else if read != numPostfixBytes {
		return "", fmt.Errorf("could not generate %d bytes of randomness", numPostfixBytes)
	}

	return fmt.Sprintf("%s-%s", prefix, strings.ToLower(randomNameEncoder.EncodeToString(postfixBytes))), nil
}

type ObjectStruct interface {
}

type DeepCopyable[T ObjectStruct] interface {
	DeepCopy() *T
}

type PObjectStruct[T ObjectStruct] interface {
	*T
	ctrl_client.Object
}

type PCopyableObjectStruct[T ObjectStruct] interface {
	DeepCopyable[T]
	PObjectStruct[T]
}

type NamespacedNameWithKind struct {
	types.NamespacedName
	Kind schema.GroupVersionKind
}

type NamespacedNameWithKindAndUid struct {
	NamespacedNameWithKind
	Uid types.UID
}

func GetNamespacedNameWithKind(obj ctrl_client.Object) NamespacedNameWithKind {
	return NamespacedNameWithKind{
		NamespacedName: types.NamespacedName{
			Namespace: obj.GetNamespace(),
			Name:      obj.GetName(),
		},
		Kind: obj.GetObjectKind().GroupVersionKind(),
	}
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
