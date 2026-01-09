/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commonapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
)

// UnexpectedObjectStateError can be used to provide additional context when an object is not in the expected state.
// In is meant to be used from within isInState() function passed to waitObjectAssumesState().
// Unlike any other error, it won't stop the wait when returned from isInState(),
// but if the wait times out and the object is still not in desired state,
// the test will be terminated with the UnexpectedObjectStateError serving as the terminating error.
type UnexpectedObjectStateError struct {
	errText string
}

func (e *UnexpectedObjectStateError) Error() string { return e.errText }

var _ error = (*UnexpectedObjectStateError)(nil)

const pollImmediately = true // Don't wait before polling for the first time
const waitPollInterval = 200 * time.Millisecond

func WaitObjectAssumesState[T ObjectStruct, PT PObjectStruct[T]](
	ctx context.Context,
	apiServerClient ctrl_client.Client,
	name types.NamespacedName,
	isInState func(*T) (bool, error),
) (*T, error) {
	var updatedObject *T = new(T)
	var unexpectedStateErr *UnexpectedObjectStateError

	hasExpectedState := func(ctx context.Context) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}

		err := apiServerClient.Get(ctx, name, PT(updatedObject))
		if ctrl_client.IgnoreNotFound(err) != nil {
			return false, fmt.Errorf("unable to fetch the object '%s' from API server: %w", name.String(), err)
		} else if err != nil {
			return false, nil
		}

		ok, stateCheckErr := isInState(updatedObject)
		if errors.As(stateCheckErr, &unexpectedStateErr) {
			return ok, nil // Unexpected state error does not stop the wait
		} else {
			return ok, stateCheckErr
		}
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, hasExpectedState)
	if err != nil {
		objJSON := "<not available>"
		if updatedObject != nil {
			jsonBytes, jsonErr := json.Marshal(updatedObject)
			if jsonErr == nil {
				objJSON = string(jsonBytes)
			}
		}

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// If the state check function returns errUnexpectedObjectState, use it.
			if unexpectedStateErr != nil {
				return nil, fmt.Errorf("waiting for object '%s' to assume desired state failed: %w. Last retrieved object was: %s", name.String(), unexpectedStateErr, objJSON)
			}
		}
		return nil, fmt.Errorf("waiting for object '%s' to assume desired state failed: %w. Last retrieved object was: %s", name.String(), err, objJSON)
	}

	return updatedObject, nil
}
