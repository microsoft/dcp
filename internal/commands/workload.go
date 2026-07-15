/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/microsoft/dcp/pkg/commonapi"
)

const (
	DCPWorkloadIDEnvVar = "DCP_WORKLOAD_ID"
	WorkloadIDFlagName  = "workload-id"
)

func AddWorkloadIDFlag(cmd *cobra.Command) {
	cmd.Flags().String(
		WorkloadIDFlagName,
		"",
		fmt.Sprintf("Associates newly-created persistent Containers, Executables, and ContainerNetworks with the specified workload ID. Overrides %s. The value must be no longer than %d bytes.", DCPWorkloadIDEnvVar, commonapi.MaxWorkloadIDLength),
	)
}

func GetWorkloadIDFlag() string {
	return "--" + WorkloadIDFlagName
}

func GetWorkloadIDFromFlags(flags *pflag.FlagSet) (commonapi.WorkloadID, error) {
	if flags.Changed(WorkloadIDFlagName) {
		workloadID, workloadIDErr := flags.GetString(WorkloadIDFlagName)
		if workloadIDErr != nil {
			return "", fmt.Errorf("could not get workload ID flag: %w", workloadIDErr)
		}
		return normalizeWorkloadID(workloadID, "workload ID flag")
	}

	return normalizeWorkloadID(os.Getenv(DCPWorkloadIDEnvVar), DCPWorkloadIDEnvVar)
}

func GetWorkloadIDInvocationFlag(flags *pflag.FlagSet) ([]string, bool, error) {
	if !flags.Changed(WorkloadIDFlagName) {
		return nil, false, nil
	}

	workloadID, workloadIDErr := flags.GetString(WorkloadIDFlagName)
	if workloadIDErr != nil {
		return nil, false, fmt.Errorf("could not get workload ID flag: %w", workloadIDErr)
	}
	normalizedWorkloadID, validationErr := normalizeWorkloadID(workloadID, "workload ID flag")
	if validationErr != nil {
		return nil, false, validationErr
	}
	return []string{GetWorkloadIDFlag(), string(normalizedWorkloadID)}, true, nil
}

func normalizeWorkloadID(workloadID, source string) (commonapi.WorkloadID, error) {
	normalizedWorkloadID := commonapi.NormalizeWorkloadID(workloadID)
	if validationErr := normalizedWorkloadID.Validate(); validationErr != nil {
		return "", fmt.Errorf("invalid %s: %w", source, validationErr)
	}
	return normalizedWorkloadID, nil
}
