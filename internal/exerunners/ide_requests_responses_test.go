/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package exerunners

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/osutil"
)

func TestRunSessionRequestCreation(t *testing.T) {
	t.Parallel()

	// An example of a launch configurations annotation
	ann := []byte(`[
		{
			"type":"project",
			"project_path":"/home/user/code/project/project.csproj",
			"launch_mode":"Debug",
			"launch_profile":"DefaultProfile"
		}
	]`)

	var lconfigs json.RawMessage
	unmarshalErr := json.Unmarshal(ann, &lconfigs)
	require.NoError(t, unmarshalErr)

	req := ideRunSessionRequestV1{
		LaunchConfigurations: lconfigs,
		Env:                  []apiv1.EnvVar{{Name: "key1", Value: "value1"}, {Name: "key2", Value: "value2"}},
		Args:                 []string{"arg1", "arg2"},
	}

	reqBody, marshalErr := json.Marshal(req)
	require.NoError(t, marshalErr)
	require.JSONEq(t, `{
		"launch_configurations": [
			{
				"type":"project",
				"project_path":"/home/user/code/project/project.csproj",
				"launch_mode":"Debug",
				"launch_profile":"DefaultProfile"
			}
		],
		"env": [
			{"name":"key1","value":"value1"},
			{"name":"key2","value":"value2"}
		],
		"args": [
			"arg1",
			"arg2"
		]
	}`, string(reqBody))
}

func TestErrorResponseString(t *testing.T) {
	t.Parallel()

	// Case 1: completely empty error response
	errResp := errorResponse{}
	require.Equal(t, "", errResp.String()) // No panic

	// Case 2: error code/message, but no details
	errResp = errorResponse{
		Error: errorDetail{
			Code:    "BadStuff",
			Message: "Bad stuff explanation",
		},
	}
	require.Equal(t, "BadStuff: Bad stuff explanation", errResp.String())

	// Case 3: error code/message, with details
	errResp = errorResponse{
		Error: errorDetail{
			Code:    "BadStuff",
			Message: "Bad stuff explanation",
			Details: []errorDetail{
				{
					Code:    "MoreBadStuff",
					Message: "Details about bad stuff",
				},
			},
		},
	}

	expected := fmt.Sprintf("BadStuff: Bad stuff explanation%s  MoreBadStuff: Details about bad stuff", osutil.WithNewline(nil))
	require.Equal(t, expected, errResp.String())
}

func TestApiVersionComparison(t *testing.T) {
	t.Parallel()

	// If some version is missing or invalid, assume the current version is "older"
	require.False(t, equalOrNewer("", ""))
	require.False(t, equalOrNewer("2024-04-23", ""))
	require.False(t, equalOrNewer("", "2024-04-23"))
	require.False(t, equalOrNewer("zzzzz", "2024-04-23"))
	require.False(t, equalOrNewer("2024-04-23", "zzzzz"))

	// Valid versions should be compared correctly
	require.True(t, equalOrNewer("2024-04-23", "2024-04-23"))
	require.True(t, equalOrNewer("2024-04-23", "2024-03-03"))
	require.False(t, equalOrNewer("2024-03-03", "2024-04-23"))
}
