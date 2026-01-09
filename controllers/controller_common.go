// Copyright (c) Microsoft Corporation. All rights reserved.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	apivalidation "k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	ctrl_config "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

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

	conflictRequeueDelay        = 100 * time.Millisecond
	reconciliationDebounceDelay = 500 * time.Millisecond
	reconciliationMaxDelay      = 5 * time.Second

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
)

type durationAndJitter struct {
	time.Duration
	Jitter time.Duration
}

var (
	// Maps additionalReconciliationDelay values to actual time.Duration values for delay and jitter.
	delayDurations = map[AdditionalReconciliationDelay]durationAndJitter{
		StandardDelay: {Duration: 2 * time.Second, Jitter: 500 * time.Millisecond},
		LongDelay:     {Duration: 5 * time.Second, Jitter: 2 * time.Second},
		TestDelay:     {Duration: 200 * time.Millisecond, Jitter: 1 * time.Millisecond},
		NoDelay:       {Duration: 0 * time.Second, Jitter: 0 * time.Millisecond},
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

// Sets "target" timestamp to "source" timestamp if "target" is before "source"
// by more than 2 microseconds, or if "target" is not known (zero value) and "source" is known.
// Returns true if the target timestamp was updated.
func setTimestampIfBeforeOrUnknown(source metav1.MicroTime, target *metav1.MicroTime) bool {
	if source.IsZero() {
		return false
	}

	if target.IsZero() || target.Add(timestampEpsilon).Before(source.Time) {
		*target = source
		return true
	} else {
		return false
	}
}

// Sets "target" timestamp to "source" timestamp if "target" is after "source"
// by more than 2 microseconds, or if "target" is not known (zero value) and "source" is known.
// Returns true if the target timestamp was updated.
func setTimestampIfAfterOrUnknown(source metav1.MicroTime, target *metav1.MicroTime) bool {
	if source.IsZero() {
		return false
	}

	if target.IsZero() || source.Add(timestampEpsilon).Before(target.Time) {
		*target = source
		return true
	} else {
		return false
	}
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
