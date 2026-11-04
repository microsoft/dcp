/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2resourcelogs

import (
	"bytes"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/logstream"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that Project waits for runtime and image capture, rejects process startup sources,
// and completes empty output when stopped or failed before physical capture exists.
func TestProjectLogAvailabilityAndSources(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	streamer := NewLogStreamer(logr.Discard())
	defer func() { require.NoError(t, streamer.Dispose()) }()
	for _, container := range []bool{false, true} {
		resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", UID: "project-uid"}}
		if container {
			resource.Spec.Container = &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: "image"}}}
		} else {
			resource.Spec.Process = &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "program"}}
		}
		for _, source := range []string{"stdout", "stderr", "startup_stdout", "startup_stderr"} {
			opts := &apiv2.ResourceLogOptions{Source: source, Follow: true}
			preflightErr := streamer.Preflight(resource, opts)
			if !container && (source == "startup_stdout" || source == "startup_stderr") {
				require.True(t, apierrors.IsBadRequest(preflightErr))
				continue
			}
			require.NoError(t, preflightErr)
			var output bytes.Buffer
			resource.Status.Phase = apiv2.ProjectPhasePending
			status, _, streamErr := streamer.StreamLogs(ctx, &output, resource, opts, logr.Discard())
			require.NoError(t, streamErr)
			require.Equal(t, logstream.ResourceStreamStatusNotReady, status)
			for _, phase := range []apiv2.ProjectPhase{apiv2.ProjectPhaseStopped, apiv2.ProjectPhaseFailed} {
				resource.Status.Phase = phase
				status, _, streamErr = streamer.StreamLogs(ctx, &output, resource, opts, logr.Discard())
				require.NoError(t, streamErr)
				require.Equal(t, logstream.ResourceStreamStatusDone, status)
				require.Empty(t, output.String())
			}
		}
	}
}
