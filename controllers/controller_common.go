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
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
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

func setRuntimeLabel(labels []commonapi.Label, key string, value string) []commonapi.Label {
	for i := range labels {
		if labels[i].Key == key {
			labels[i].Value = value
			return labels
		}
	}
	return append(labels, commonapi.Label{Key: key, Value: value})
}

func physicalResourceCreationLabels(
	labels []commonapi.Label,
	persistent bool,
	resourceUID types.UID,
	log logr.Logger,
) []commonapi.Label {
	result := append([]commonapi.Label{}, labels...)
	result = setRuntimeLabel(result, PersistentLabel, fmt.Sprintf("%t", persistent))
	if resourceUID != "" {
		result = setRuntimeLabel(result, uidLabel, string(resourceUID))
	}

	thisProcess, thisProcessErr := process.This()
	if thisProcessErr != nil {
		log.Error(thisProcessErr, "Could not get current process information; physical resource will not have creator process information")
		return result
	}

	result = setRuntimeLabel(result, CreatorProcessIdLabel, fmt.Sprintf("%d", thisProcess.Pid))
	result = setRuntimeLabel(result, CreatorProcessStartTimeLabel, thisProcess.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat))
	return result
}

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

// checkNamespaceReady reports whether a namespace permits a V2 resource to perform runtime work.
// Deletion paths bypass this check so cleanup can still finish.
func checkNamespaceReady(
	ctx context.Context,
	client ctrl_client.Client,
	namespaceName string,
) (bool, apiv2.ConditionReason, error) {
	namespace := apiv2.Namespace{}
	getErr := client.Get(ctx, types.NamespacedName{Name: namespaceName}, &namespace)
	if apierrors.IsNotFound(getErr) {
		return false, apiv2.PhysicalResourceReasonNamespaceNotFound, nil
	}
	if getErr != nil {
		return false, apiv2.PhysicalResourceReasonNamespaceLookupFailed, getErr
	}

	if namespace.DeletionTimestamp != nil && !namespace.DeletionTimestamp.IsZero() {
		return false, apiv2.PhysicalResourceReasonNamespaceTerminating, nil
	}
	if !usvc_slices.Contains(namespace.Finalizers, namespaceFinalizer) {
		return false, apiv2.PhysicalResourceReasonNamespaceNotReady, nil
	}
	if namespace.Status.Phase != apiv2.NamespacePhaseActive {
		return false, apiv2.PhysicalResourceReasonNamespaceNotActive, nil
	}

	return true, "", nil
}

func namespaceReadinessMessage(namespaceName string, reason apiv2.ConditionReason) string {
	switch reason {
	case apiv2.PhysicalResourceReasonNamespaceNotFound:
		return fmt.Sprintf("Namespace %q does not exist.", namespaceName)
	case apiv2.PhysicalResourceReasonNamespaceTerminating:
		return fmt.Sprintf("Namespace %q is terminating.", namespaceName)
	case apiv2.PhysicalResourceReasonNamespaceNotActive:
		return fmt.Sprintf("Namespace %q is not active.", namespaceName)
	default:
		return fmt.Sprintf("Namespace %q is not ready.", namespaceName)
	}
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

// stateInitializerFunc is invoked when reconciliation handles a particular controller state.
// An initializer is responsible for projecting that state onto the API object, updating any
// controller-owned in-memory data, and applying necessary changes to the represented runtime
// resources. It must return noChange when it does not modify the API object.
type stateInitializerFunc[
	O commonapi.ObjectStruct, PO commonapi.PObjectWithStatusStruct[O],
	R ReconcilerType, PR PReconcilerType[R],
	OS KubernetesObjectStateType,
	IMOS any, PIMOS PInMemoryObjectState[IMOS],
] func(
	ctx context.Context,
	reconciler PR,
	obj PO,
	state OS,
	inMemoryState PIMOS,
	log logr.Logger,
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

// Records the condition, reporting noChange when the condition already holds the same values
// so that repeated reconciliation of an unchanged state does not produce status writes.
func setCondition(
	conditions *[]metav1.Condition,
	conditionType apiv2.ConditionType,
	generation int64,
	status metav1.ConditionStatus,
	reason apiv2.ConditionReason,
	message string,
) objectChange {
	condition := metav1.Condition{
		Type:               string(conditionType),
		Status:             status,
		Reason:             string(reason),
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
