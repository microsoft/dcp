/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package statestore

import (
	"fmt"

	"github.com/microsoft/dcp/pkg/commonapi"
)

func normalizePersistentWorkloadID(workloadID commonapi.WorkloadID) (commonapi.WorkloadID, error) {
	normalizedWorkloadID := workloadID.Normalized()
	if validationErr := normalizedWorkloadID.Validate(); validationErr != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidArgument, validationErr)
	}
	return normalizedWorkloadID, nil
}
