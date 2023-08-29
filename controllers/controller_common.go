// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	reconciliationDebounceDelay   = 500 * time.Millisecond
)

func ensureFinalizer(obj metav1.Object, finalizer string) objectChange {
	finalizers := obj.GetFinalizers()
	if slices.Contains(finalizers, finalizer) {
		return noChange
	}

	finalizers = append(finalizers, finalizer)
	obj.SetFinalizers(finalizers)
	return metadataChanged
}

func deleteFinalizer(obj metav1.Object, finalizer string) objectChange {
	finalizers := obj.GetFinalizers()
	i := slices.Index(finalizers, finalizer)
	if i == -1 {
		return noChange
	}

	finalizers = append(finalizers[:i], finalizers[i+1:]...)
	obj.SetFinalizers(finalizers)
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

type PObjectStruct[T any] interface {
	*T
	ctrl_client.Object
}

type DeepCopyableObject[T any, PT PObjectStruct[T]] interface {
	*T
	ctrl_client.Object

	// We need to express that "pointer to T has a DeepCopy method that returns a pointer to T"
	// Unfortunately it is not possible in Go to refer in the generic type definition to the type that is being defined
	// So we need to settle for this kind of gymnastics and say that DeepCopy() returns PObjectStruct[T], and not DeepCopyableObbject[T]
	// This is not really what we want, but sufficient for the code we need in controllers.
	DeepCopy() PT
}
