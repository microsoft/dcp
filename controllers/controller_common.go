/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"hash/fnv"
	mathrand "math/rand"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	apivalidation "k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	ctrl_config "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_slices "github.com/microsoft/dcp/pkg/slices"
)

type objectChange int

const (
	noChange                       objectChange = 0
	statusChanged                  objectChange = 0x1
	metadataChanged                objectChange = 0x2
	specChanged                    objectChange = 0x4
	additionalReconciliationNeeded objectChange = 0x8

	conflictRequeueDelay              = 100 * time.Millisecond
	reconciliationDebounceDelay       = 500 * time.Millisecond
	reconciliationMaxDelay            = 5 * time.Second
	resourceLeaseRevalidationInterval = 30 * time.Second

	PersistentLabel              = "com.microsoft.developer.usvc-dev.persistent"
	CreatorProcessIdLabel        = "com.microsoft.developer.usvc-dev.creatorProcessId"
	CreatorProcessStartTimeLabel = "com.microsoft.developer.usvc-dev.creatorProcessStartTime"
	ContainerIdLabel             = "com.microsoft.developer.usvc-dev.containerId"

	MaxConcurrentReconciles = 6

	numPostfixBytes = 6
)

type AdditionalReconciliationDelay int

const (
	StandardDelay AdditionalReconciliationDelay = 0 // Zero value means standard delay
	NoDelay       AdditionalReconciliationDelay = 1
	LongDelay     AdditionalReconciliationDelay = 2
	TestDelay     AdditionalReconciliationDelay = 3
	// MonitoringDelay paces periodic polling of resources that are in a steady state,
	// where reconciliation only guards against runtime events that were missed or never delivered.
	MonitoringDelay AdditionalReconciliationDelay = 4
)

type durationAndJitter struct {
	time.Duration
	Jitter time.Duration
}

var (
	// Maps additionalReconciliationDelay values to actual time.Duration values for delay and jitter.
	delayDurations = map[AdditionalReconciliationDelay]durationAndJitter{
		StandardDelay:   {Duration: 2 * time.Second, Jitter: 500 * time.Millisecond},
		LongDelay:       {Duration: 5 * time.Second, Jitter: 2 * time.Second},
		MonitoringDelay: {Duration: 30 * time.Second, Jitter: 5 * time.Second},
		TestDelay:       {Duration: 200 * time.Millisecond, Jitter: 1 * time.Millisecond},
		NoDelay:         {Duration: 0 * time.Second, Jitter: 0 * time.Millisecond},
	}
)

var (
	// Base32 encoder used to generate unique postfixes for Executable replicas.
	randomNameEncoder = base32.HexEncoding.WithPadding(base32.NoPadding)
)

func delayDuration(delay AdditionalReconciliationDelay) time.Duration {
	if delay == NoDelay {
		return 0
	}
	dnj, found := delayDurations[delay]
	if !found {
		dnj = delayDurations[StandardDelay] // Should never happen, but just in case...
	}
	retval := dnj.Duration + time.Duration(mathrand.Int63n(int64(dnj.Jitter)))
	return retval
}

func ensureFinalizer(obj metav1.Object, finalizer string, log logr.Logger) objectChange {
	finalizers := obj.GetFinalizers()
	if usvc_slices.Contains(finalizers, finalizer) {
		return noChange
	}

	finalizers = append(finalizers, finalizer)
	obj.SetFinalizers(finalizers)
	log.V(1).Info("Added finalizer", "Finalizer", finalizer)
	return metadataChanged
}

func deleteFinalizer(obj metav1.Object, finalizer string, log logr.Logger) objectChange {
	finalizers := obj.GetFinalizers()
	i := usvc_slices.Index(finalizers, finalizer)
	if i == -1 {
		return noChange
	}

	finalizers = append(finalizers[:i], finalizers[i+1:]...)
	obj.SetFinalizers(finalizers)
	log.V(1).Info("Removed finalizer", "Finalizer", finalizer)
	return metadataChanged
}

func ensureNamespace(
	ctx context.Context,
	client ctrl_client.Client,
	namespaceName string,
	applyPending func(string) objectChange,
	applyFailed func(string) objectChange,
	log logr.Logger,
) (bool, objectChange) {
	namespace := apiv2.Namespace{}
	getErr := client.Get(ctx, types.NamespacedName{Name: namespaceName}, &namespace)
	if apierrors.IsNotFound(getErr) {
		return false, applyPending(fmt.Sprintf("Namespace %q does not exist.", namespaceName))
	}
	if getErr != nil {
		log.Error(getErr, "Failed to get namespace", "Namespace", namespaceName)
		return false, applyFailed(fmt.Sprintf("Failed to get namespace: %v", getErr))
	}

	if namespace.DeletionTimestamp != nil && !namespace.DeletionTimestamp.IsZero() {
		return false, applyPending(fmt.Sprintf("Namespace %q is terminating.", namespaceName))
	}
	if !usvc_slices.Contains(namespace.Finalizers, namespaceFinalizer) {
		return false, applyPending(fmt.Sprintf("Namespace %q is not ready.", namespaceName))
	}
	if namespace.Status.Phase != apiv2.NamespacePhaseActive {
		return false, applyPending(fmt.Sprintf("Namespace %q is not active.", namespaceName))
	}

	return true, noChange
}

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

// Returns a short version of the ID (the first 12 characters). Intended for use
// with container resource IDs, which are usually long and not very human-readable.
// The short ID is used in logs and other places where a shorter identifier is more convenient.
func GetShortId(id string) string {
	if len(id) > 12 {
		return id[:12]
	}

	return id
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
		Controller: ctrl_config.Controller{
			MaxConcurrentReconciles: MaxConcurrentReconciles,
		},
	}
}

type ReconcilerType interface {
}

type PReconcilerType[RT ReconcilerType] interface {
	*RT
	ctrl_client.Client
}
type KubernetesObjectStateType interface {
	~string
}

// A function invoked from the reconciliation loop when an object reaches a particular state.
// The responsibility of the state initializer is threefold:
// 1. Set the object's to the desired state (usually by modifying its Status).
// 2. Update the in-memory data structures that track the object's state (data owned by the reconciler).
// 3. Make necessary changes to the real-world resources that the object represents.
// NOTE: the initializer MUST return noChange if no changes were made to the object, in order to avoid infinite reconciliation loops
type stateInitializerFunc[
	O commonapi.ObjectStruct, PO commonapi.PObjectWithStatusStruct[O],
	R ReconcilerType, PR PReconcilerType[R],
	OS KubernetesObjectStateType,
	IMOS any, PIMOS PInMemoryObjectState[IMOS],
] func(
	context.Context, /* context for the reconciliation operation */
	PR, /* reconciler instance */
	PO, /* Kubernetes object to be reconciled */
	OS, /* The desired state of the object. Useful if the same state initializer is used for multiple states */
	PIMOS, /* The in-memory state of the object (additional data about the object stored in controller's ObjectStateMap). */
	logr.Logger,
) objectChange

func getStateInitializer[
	O commonapi.ObjectStruct, PO commonapi.PObjectWithStatusStruct[O],
	R ReconcilerType, PR PReconcilerType[R],
	OS KubernetesObjectStateType,
	IMOS any, PIMOS PInMemoryObjectState[IMOS],
](
	m map[OS]stateInitializerFunc[O, PO, R, PR, OS, IMOS, PIMOS],
	state OS,
	log logr.Logger,
) stateInitializerFunc[O, PO, R, PR, OS, IMOS, PIMOS] {
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

// MicroTime it is subject to rounding errors when it is serialized, deserialized, and initialized from time.Time.
// We consider a timestamp to be "different" from another one if it is off by more than 2 microseconds.
const timestampEpsilon = 2 * time.Microsecond

// Sets "target" timestamp to "source" timestamp if "target" is before "source" by more than
// 2 microseconds, or if "target" is not known (zero value) and "source" is known.
func setTimestampIfBeforeOrUnknown(target *metav1.MicroTime, source metav1.MicroTime) objectChange {
	if source.IsZero() {
		return noChange
	}

	if target.IsZero() || target.Add(timestampEpsilon).Before(source.Time) {
		*target = source
		return statusChanged
	}
	return noChange
}

// Sets "target" timestamp to "source" timestamp if "target" is after "source" by more than
// 2 microseconds, or if "target" is not known (zero value) and "source" is known.
func setTimestampIfAfterOrUnknown(target *metav1.MicroTime, source metav1.MicroTime) objectChange {
	if source.IsZero() {
		return noChange
	}

	if target.IsZero() || source.Add(timestampEpsilon).Before(target.Time) {
		*target = source
		return statusChanged
	}
	return noChange
}

func trySetTimestampIfAfterOrUnknown(target *metav1.MicroTime, source metav1.MicroTime) bool {
	return setTimestampIfAfterOrUnknown(target, source) != noChange
}

// Sets "target" timestamp to "source" timestamp if it is different by more than 2 microseconds.
func setTimestamp(target *metav1.MicroTime, source metav1.MicroTime) objectChange {
	if target.Add(timestampEpsilon).Before(source.Time) || source.Add(timestampEpsilon).Before(target.Time) {
		*target = source
		return statusChanged
	}
	return noChange
}

func setValue[T comparable](target *T, value T) objectChange {
	if *target == value {
		return noChange
	}
	*target = value
	return statusChanged
}

func setReadyCondition(
	conditions *[]metav1.Condition,
	generation int64,
	status metav1.ConditionStatus,
	reason string,
	message string,
) objectChange {
	return setCondition(conditions, apiv2.ConditionReady, generation, status, reason, message)
}

// Records the condition, reporting noChange when the condition already holds the same values
// so that repeated reconciliation of an unchanged state does not produce status writes.
func setCondition(
	conditions *[]metav1.Condition,
	conditionType string,
	generation int64,
	status metav1.ConditionStatus,
	reason string,
	message string,
) objectChange {
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	}
	if existingCondition := apimeta.FindStatusCondition(*conditions, condition.Type); existingCondition != nil &&
		existingCondition.Status == condition.Status &&
		existingCondition.Reason == condition.Reason &&
		existingCondition.Message == condition.Message &&
		existingCondition.ObservedGeneration == condition.ObservedGeneration {
		return noChange
	}

	apimeta.SetStatusCondition(conditions, condition)
	return statusChanged
}

// Computes a valid Kubernetes label value from an arbitrary string.
// If the passed string is a valid label value, it is returned unchanged.
// Otherwise, a hash of the string is computed and the returned value is the hash in hexadecimal form,
// prefixed with "x-".
func MakeValidLabelValue(s string) string {
	if errs := apivalidation.IsValidLabelValue(s); len(errs) == 0 {
		return s
	}

	fnvHash := fnv.New128()
	fnvHash.Write([]byte(s))
	return fmt.Sprintf("x-%x", fnvHash.Sum(nil))
}
