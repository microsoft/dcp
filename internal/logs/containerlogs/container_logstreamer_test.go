/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containerlogs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/logs"
)

func TestShouldStopContainerLogStreamImmediately(t *testing.T) {
	now := time.Date(2026, time.May, 5, 20, 0, 0, 0, time.UTC)

	tests := []struct {
		name            string
		follow          bool
		state           apiv1.ContainerState
		finishTimestamp metav1.MicroTime
		want            bool
	}{
		{
			name:            "non-follow request stops immediately",
			follow:          false,
			state:           apiv1.ContainerStateRunning,
			finishTimestamp: metav1.MicroTime{},
			want:            true,
		},
		{
			name:            "done container with old finish timestamp stops immediately",
			follow:          true,
			state:           apiv1.ContainerStateExited,
			finishTimestamp: metav1.NewMicroTime(now.Add(-logs.FollowStreamCancellationDelay - time.Second)),
			want:            true,
		},
		{
			name:            "done container with recent finish timestamp uses delayed stop",
			follow:          true,
			state:           apiv1.ContainerStateExited,
			finishTimestamp: metav1.NewMicroTime(now.Add(-logs.FollowStreamCancellationDelay + time.Second)),
			want:            false,
		},
		{
			name:            "done container with zero finish timestamp uses delayed stop",
			follow:          true,
			state:           apiv1.ContainerStateExited,
			finishTimestamp: metav1.MicroTime{},
			want:            false,
		},
		{
			name:            "done container with future finish timestamp uses delayed stop",
			follow:          true,
			state:           apiv1.ContainerStateExited,
			finishTimestamp: metav1.NewMicroTime(now.Add(time.Second)),
			want:            false,
		},
		{
			name:            "running container does not stop immediately",
			follow:          true,
			state:           apiv1.ContainerStateRunning,
			finishTimestamp: metav1.NewMicroTime(now.Add(-logs.FollowStreamCancellationDelay - time.Second)),
			want:            false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctr := &apiv1.Container{
				Status: apiv1.ContainerStatus{
					State:           tt.state,
					FinishTimestamp: tt.finishTimestamp,
				},
			}

			require.Equal(t, tt.want, shouldStopContainerLogStreamImmediately(tt.follow, ctr, now))
		})
	}
}
