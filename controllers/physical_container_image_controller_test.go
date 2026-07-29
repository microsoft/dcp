/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"testing"

	"github.com/cenkalti/backoff/v4"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

func TestPhysicalContainerImageOperationFailedTerminally(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		phase    apiv2.PhysicalContainerImagePhase
		reason   string
		expected bool
	}{
		{
			name:     "pull failure is terminal",
			phase:    apiv2.PhysicalContainerImagePhaseFailed,
			reason:   apiv2.PhysicalContainerImageReasonPullFailed,
			expected: true,
		},
		{
			name:     "build failure is terminal",
			phase:    apiv2.PhysicalContainerImagePhaseFailed,
			reason:   apiv2.PhysicalContainerImageReasonBuildFailed,
			expected: true,
		},
		{
			// A transient reconciliation failure (for example, a namespace read error)
			// must stay retryable, otherwise the image is stranded permanently.
			name:     "reconciliation failure stays retryable",
			phase:    apiv2.PhysicalContainerImagePhaseFailed,
			reason:   apiv2.PhysicalContainerImageReasonReconciliationFailed,
			expected: false,
		},
		{
			name:     "pending image is not terminal",
			phase:    apiv2.PhysicalContainerImagePhasePending,
			reason:   apiv2.PhysicalContainerImageReasonPulling,
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			image := &apiv2.PhysicalContainerImage{
				Status: apiv2.PhysicalContainerImageStatus{
					Phase: tc.phase,
					Conditions: []metav1.Condition{
						{
							Type:   apiv2.ConditionReady,
							Status: metav1.ConditionFalse,
							Reason: tc.reason,
						},
					},
				},
			}

			require.Equal(t, tc.expected, physicalContainerImageOperationFailedTerminally(image))
		})
	}
}

func TestPhysicalContainerImageOperationFailedTerminallyWithoutReadyCondition(t *testing.T) {
	t.Parallel()

	image := &apiv2.PhysicalContainerImage{
		Status: apiv2.PhysicalContainerImageStatus{
			Phase: apiv2.PhysicalContainerImagePhaseFailed,
		},
	}

	require.False(t, physicalContainerImageOperationFailedTerminally(image))
}

func TestImagePullBackoffHonorsRetryLimit(t *testing.T) {
	t.Parallel()

	disabled := int32(0)
	explicit := int32(1)

	testCases := []struct {
		name            string
		retryLimit      *int32
		expectedRetries int
	}{
		{
			name:            "default is used when unset",
			retryLimit:      nil,
			expectedRetries: int(defaultImagePullRetryLimit),
		},
		{
			name:            "zero disables retries",
			retryLimit:      &disabled,
			expectedRetries: 0,
		},
		{
			name:            "explicit limit is honored",
			retryLimit:      &explicit,
			expectedRetries: 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			image := &apiv2.PhysicalContainerImage{
				Spec: apiv2.PhysicalContainerImageSpec{
					Image:          "some-image",
					PullRetryLimit: tc.retryLimit,
				},
			}

			policy := imagePullBackoff(image)
			retries := 0
			for retries <= int(defaultImagePullRetryLimit)+2 {
				if policy.NextBackOff() == backoff.Stop {
					break
				}
				retries++
			}

			require.Equal(t, tc.expectedRetries, retries)
		})
	}
}

// ObjectStateMap.Update() rebuilds the stored state from a clone of the current value and then
// applies UpdateFrom(), so the cancellation function must survive both or a queued pull/build
// becomes impossible to cancel once its first result is recorded.
func TestPhysicalContainerImageDataPreservesCancelOperation(t *testing.T) {
	t.Parallel()

	cancelled := false
	data := &physicalContainerImageData{
		conditionReason: apiv2.PhysicalContainerImageReasonPulling,
		cancelOperation: func() { cancelled = true },
	}

	stored := data.Clone()
	require.NotNil(t, stored.cancelOperation)

	// A result reported by the queued operation carries no cancellation function.
	require.True(t, stored.UpdateFrom(&physicalContainerImageData{
		conditionReason: apiv2.PhysicalContainerImageReasonPulled,
		imageID:         "image-id",
	}))
	require.NotNil(t, stored.cancelOperation)

	stored.cancelOperation()
	require.True(t, cancelled)
}

func TestPhysicalContainerImageDataOperationInProgress(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		conditionReason string
		expected        bool
	}{
		{apiv2.PhysicalContainerImageReasonPulling, true},
		{apiv2.PhysicalContainerImageReasonBuilding, true},
		{apiv2.PhysicalContainerImageReasonPulled, false},
		{apiv2.PhysicalContainerImageReasonBuilt, false},
		{apiv2.PhysicalContainerImageReasonPullFailed, false},
		{apiv2.PhysicalContainerImageReasonBuildFailed, false},
		{"", false},
	}

	for _, tc := range testCases {
		t.Run(tc.conditionReason, func(t *testing.T) {
			data := &physicalContainerImageData{conditionReason: tc.conditionReason}
			require.Equal(t, tc.expected, data.operationInProgress())
		})
	}
}
