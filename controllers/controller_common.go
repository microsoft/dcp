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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

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
	log logr.Logger,
) (ctrl.Result, error) {

	var update PCT
	var err error
	kind := obj.GetObjectKind().GroupVersionKind().Kind

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
			if errors.IsConflict(err) {
				// Error is expected optimistic concurrency check error, simply requeue
				log.V(1).Info(fmt.Sprintf("%s status update failed due to conflict", kind))
				return ctrl.Result{RequeueAfter: conflictRequeueDelay}, nil
			} else {
				log.Error(err, fmt.Sprintf("%s status update failed", kind))
				return ctrl.Result{}, err
			}
		} else {
			log.V(1).Info(fmt.Sprintf("%s status update succeeded", kind))
		}

	case (change & (metadataChanged | specChanged)) != 0:
		update = obj.DeepCopy()
		err = client.Patch(ctx, update, patch)
		if err != nil {
			if errors.IsConflict(err) {
				// Error is expected optimistic concurrency check error, simply requeue
				log.V(1).Info(fmt.Sprintf("%s object update failed due to conflict", kind))
				return ctrl.Result{RequeueAfter: conflictRequeueDelay}, nil
			} else {
				log.Error(err, fmt.Sprintf("%s object update failed", kind))
				return ctrl.Result{}, err
			}
		} else {
			log.V(1).Info(fmt.Sprintf("%s object update succeeded", kind))
		}
	}

	if (change & additionalReconciliationNeeded) != 0 {
		log.V(1).Info(fmt.Sprintf("scheduling additional reconciliation for %s...", kind))
		return ctrl.Result{RequeueAfter: additionalReconciliationDelay}, nil
	} else {
		return ctrl.Result{}, nil
	}
}
