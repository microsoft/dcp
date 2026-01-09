/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package ctrlutil

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/slices"
)

const (
	pollImmediately  = true // Don't wait before polling for the first time
	waitPollInterval = 200 * time.Millisecond
)

func WaitObjectDeleted[T commonapi.ObjectStruct, PT commonapi.PObjectStruct[T]](
	t *testing.T,
	ctx context.Context,
	client ctrl_client.Client,
	deleted PT,
) {
	name := ctrl_client.ObjectKeyFromObject(deleted)

	objectNotFound := func(ctx context.Context) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}

		var obj T = *new(T)
		err := client.Get(ctx, name, PT(&obj))
		if err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			} else {
				return false, err
			}
		}
		return false, nil
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, objectNotFound)
	if err != nil {
		t.Fatalf("Object '%s' was not deleted as expected: %v", name.Name, err)
	}
}

func WaitObjectCount[T commonapi.ObjectStruct, LT commonapi.ObjectList, PT commonapi.PObjectStruct[T], PLT commonapi.PObjectList[T, LT, PT]](
	t *testing.T,
	ctx context.Context,
	client ctrl_client.Client,
	expectedCount int,
	contextStr string,
	selector func(obj PT) bool,
) {
	const unknownCount = -1
	currentCount := unknownCount

	hasDesiredCount := func(ctx context.Context) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}

		var pObjList PLT = new(LT)
		err := client.List(ctx, pObjList)
		if err != nil {
			t.Fatalf("Error listing objects (%s): %v", contextStr, err)
			return false, err
		}

		currentCount = slices.LenIf(pObjList.GetItems(), selector)
		return currentCount == expectedCount, nil
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, hasDesiredCount)
	if err != nil {
		t.Fatalf("Waiting for number of objects matching criteria (%s) to reach %d failed. Last count was %d, the error from waiting routine is %v", contextStr, expectedCount, currentCount, err)
	}
}

func WaitObjectExists[T commonapi.ObjectStruct, LT commonapi.ObjectList, PT commonapi.PObjectStruct[T], PLT commonapi.PObjectList[T, LT, PT]](
	t *testing.T,
	ctx context.Context,
	client ctrl_client.Client,
	contextStr string,
	selector func(obj PT) (bool, error),
) PT {
	var updatedObject PT = nil

	existsWithExpectedState := func(ctx context.Context) (bool, error) {
		var pObjList PLT = new(LT)
		err := client.List(ctx, pObjList)
		if err != nil {
			t.Fatalf("Error listing objects (%s): %v", contextStr, err)
			return false, err
		}

		for _, pObj := range pObjList.GetItems() {
			if matches, matchErr := selector(pObj); matchErr != nil {
				t.Fatalf("Error matching object (%s): %v", contextStr, matchErr)
				return false, matchErr
			} else if matches {
				updatedObject = pObj
				return true, nil
			}
		}

		return false, nil
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, existsWithExpectedState)
	if err != nil {
		t.Fatalf("Waiting for object in desired state to appear (%s) failed: %v", contextStr, err)
		return nil // make the compiler happy
	} else {
		return updatedObject
	}
}
