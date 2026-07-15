/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/commonapi"
)

func TestGetWorkloadIDFromFlagsUsesEnvironment(t *testing.T) {
	cmd := &cobra.Command{}
	AddWorkloadIDFlag(cmd)
	t.Setenv(DCPWorkloadIDEnvVar, " workload-a ")

	workloadID, workloadIDErr := GetWorkloadIDFromFlags(cmd.Flags())

	require.NoError(t, workloadIDErr)
	require.Equal(t, commonapi.WorkloadID("workload-a"), workloadID)
}

func TestGetWorkloadIDFromFlagsPrefersFlag(t *testing.T) {
	cmd := &cobra.Command{}
	AddWorkloadIDFlag(cmd)
	t.Setenv(DCPWorkloadIDEnvVar, "workload-env")
	require.NoError(t, cmd.Flags().Set(WorkloadIDFlagName, " workload-flag "))

	workloadID, workloadIDErr := GetWorkloadIDFromFlags(cmd.Flags())

	require.NoError(t, workloadIDErr)
	require.Equal(t, commonapi.WorkloadID("workload-flag"), workloadID)
}

func TestGetWorkloadIDFromFlagsEmptyFlagOverridesEnvironment(t *testing.T) {
	cmd := &cobra.Command{}
	AddWorkloadIDFlag(cmd)
	t.Setenv(DCPWorkloadIDEnvVar, "workload-env")
	require.NoError(t, cmd.Flags().Set(WorkloadIDFlagName, " "))

	workloadID, workloadIDErr := GetWorkloadIDFromFlags(cmd.Flags())

	require.NoError(t, workloadIDErr)
	require.Empty(t, workloadID)
}

func TestGetWorkloadIDFromFlagsRejectsTooLongEnvironmentValue(t *testing.T) {
	cmd := &cobra.Command{}
	AddWorkloadIDFlag(cmd)
	t.Setenv(DCPWorkloadIDEnvVar, strings.Repeat("a", commonapi.MaxWorkloadIDLength+1))

	_, workloadIDErr := GetWorkloadIDFromFlags(cmd.Flags())

	require.ErrorContains(t, workloadIDErr, "invalid DCP_WORKLOAD_ID")
	require.ErrorContains(t, workloadIDErr, "workload ID cannot be longer than")
}

func TestGetWorkloadIDFromFlagsRejectsTooLongFlagValue(t *testing.T) {
	cmd := &cobra.Command{}
	AddWorkloadIDFlag(cmd)
	t.Setenv(DCPWorkloadIDEnvVar, "workload-env")
	require.NoError(t, cmd.Flags().Set(WorkloadIDFlagName, strings.Repeat("a", commonapi.MaxWorkloadIDLength+1)))

	_, workloadIDErr := GetWorkloadIDFromFlags(cmd.Flags())

	require.ErrorContains(t, workloadIDErr, "invalid workload ID flag")
	require.ErrorContains(t, workloadIDErr, "workload ID cannot be longer than")
}

func TestGetWorkloadIDInvocationFlagOnlyWhenFlagSet(t *testing.T) {
	cmd := &cobra.Command{}
	AddWorkloadIDFlag(cmd)

	workloadIDFlag, hasWorkloadIDFlag, workloadIDFlagErr := GetWorkloadIDInvocationFlag(cmd.Flags())
	require.NoError(t, workloadIDFlagErr)
	require.False(t, hasWorkloadIDFlag)
	require.Empty(t, workloadIDFlag)

	require.NoError(t, cmd.Flags().Set(WorkloadIDFlagName, " workload-flag "))

	workloadIDFlag, hasWorkloadIDFlag, workloadIDFlagErr = GetWorkloadIDInvocationFlag(cmd.Flags())
	require.NoError(t, workloadIDFlagErr)
	require.True(t, hasWorkloadIDFlag)
	require.Equal(t, []string{GetWorkloadIDFlag(), "workload-flag"}, workloadIDFlag)
}

func TestGetWorkloadIDInvocationFlagRejectsTooLongFlagValue(t *testing.T) {
	cmd := &cobra.Command{}
	AddWorkloadIDFlag(cmd)
	require.NoError(t, cmd.Flags().Set(WorkloadIDFlagName, strings.Repeat("a", commonapi.MaxWorkloadIDLength+1)))

	_, _, workloadIDFlagErr := GetWorkloadIDInvocationFlag(cmd.Flags())

	require.ErrorContains(t, workloadIDFlagErr, "invalid workload ID flag")
	require.ErrorContains(t, workloadIDFlagErr, "workload ID cannot be longer than")
}
