/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commonapi

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeWorkloadID(t *testing.T) {
	workloadID := NormalizeWorkloadID(" workload-a ")

	require.Equal(t, WorkloadID("workload-a"), workloadID)
}

func TestWorkloadIDValidateAllowsMaxLength(t *testing.T) {
	workloadID := WorkloadID(strings.Repeat("a", MaxWorkloadIDLength))

	require.NoError(t, workloadID.Validate())
}

func TestWorkloadIDValidateRejectsTooLong(t *testing.T) {
	workloadID := WorkloadID(strings.Repeat("a", MaxWorkloadIDLength+1))

	require.ErrorContains(t, workloadID.Validate(), "workload ID cannot be longer than")
}
