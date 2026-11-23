/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/logs"
	"github.com/microsoft/dcp/internal/logstream"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/resiliency"
)

// Verifies that rejected cleanup after host cancellation does not fall back to inline execution.
func TestOutputCleanupAfterShutdownDoesNotRunInline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	queue := resiliency.NewWorkQueue(ctx, 1)
	var called atomic.Bool
	enqueueOutputCleanup(queue, logr.Discard(), func(context.Context) error {
		called.Store(true)
		return nil
	})
	require.False(t, called.Load())
}

// Verifies that shared capture cleanup closes writers while preserving output when requested,
// and removes captures normally otherwise, independently of the producing resource kind.
func TestCaptureOutputCleanupHonorsPreservation(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		name, setting := "remove", ""
		if preserve {
			name, setting = "preserve", "1"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv(usvc_io.DCP_PRESERVE_EXECUTABLE_LOGS, setting)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			queue := resiliency.NewWorkQueue(ctx, 1)
			uid, uidErr := randdata.MakeRandomString(8)
			require.NoError(t, uidErr)
			resource := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: "cleanup", UID: types.UID(uid)}}
			capture, captureErr := logs.NewCaptureFile(resource, logstream.LogStreamSourceStdout)
			require.NoError(t, captureErr)
			defer func() { require.NoError(t, capture.Close(logs.CaptureFileDisposeOnClose)) }()
			_, writeErr := capture.Writer.Write([]byte("retained capture\n"))
			require.NoError(t, writeErr)
			enqueueCaptureFileCleanup(queue, logr.Discard(), capture)
			done := make(chan struct{})
			require.NoError(t, queue.Enqueue(func(context.Context) { close(done) }))
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			_, afterCloseErr := capture.Writer.Write([]byte("after close"))
			require.Error(t, afterCloseErr)
			output, readErr := os.ReadFile(capture.Path)
			if preserve {
				require.NoError(t, readErr)
				require.Contains(t, string(output), "retained capture\n")
			} else {
				require.True(t, errors.Is(readErr, os.ErrNotExist), "%v", readErr)
			}
		})
	}
}
