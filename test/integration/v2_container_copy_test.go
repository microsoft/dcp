/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/statestore"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/testutil"
)

type archiveCopyOrchestrator struct {
	containers.ContainerOrchestrator
	failCopy   bool
	failCreate bool
	entered    chan struct{}
	release    chan struct{}
	copies     atomic.Int32
}

// Verifies that PhysicalContainer never deletes a directory supplied incorrectly as a tar source,
// including after copy failure, initial stop, and deletion before preparation.
func TestV2PhysicalContainerCopyCleanupPreservesDirectories(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"copy-failure", "early-stop", "delete"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			server, _, startErr := StartTestEnvironment(t, ctx, PhysicalContainerController|PhysicalContainerImageController,
				t.Name(), t.TempDir())
			require.NoError(t, startErr)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-copy-directory-"+scenario)
			image := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: "image", Namespace: namespace.Name},
				Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}}}
			require.NoError(t, server.Client.Create(ctx, image))
			source := t.TempDir()
			resource := &apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
				Spec: apiv2.PhysicalContainerSpec{Stop: scenario == "early-stop", Container: &apiv2.PhysicalContainerConfig{
					ImageRef: image.Name, ContainerName: namespace.Name,
					CopyFiles: []apiv2.ContainerCopyFileContext{{Source: source, SHA256: strings.Repeat("0", 64), DeleteAfterCopy: true}},
				}}}
			require.NoError(t, server.Client.Create(ctx, resource))
			if scenario == "copy-failure" {
				failed := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
					return current.Status.Phase == apiv2.PhysicalContainerPhaseFailed, nil
				})
				require.Contains(t, failed.Status.Conditions[0].Message, "not a regular file")
			} else if scenario == "early-stop" {
				waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
					for _, condition := range current.Status.Conditions {
						if condition.Reason == string(apiv2.PhysicalContainerReasonStopRequested) {
							return true, nil
						}
					}
					return false, nil
				})
			}
			require.NoError(t, server.Client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
			require.DirExists(t, source)
		})
	}
}

func (orchestrator *archiveCopyOrchestrator) CreateContainer(ctx context.Context, options containers.CreateContainerOptions) (string, error) {
	if orchestrator.failCreate {
		return "", errors.Join(containers.ErrCouldNotAllocate, errors.New("container creation denied"))
	}
	return orchestrator.ContainerOrchestrator.CreateContainer(ctx, options)
}

func (orchestrator *archiveCopyOrchestrator) CopyFiles(ctx context.Context, options containers.CopyFilesOptions) error {
	orchestrator.copies.Add(1)
	if orchestrator.entered != nil {
		close(orchestrator.entered)
		select {
		case <-orchestrator.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if orchestrator.failCopy {
		return errors.New("archive copy denied")
	}
	return orchestrator.ContainerOrchestrator.CopyFiles(ctx, options)
}

// Verifies that PhysicalContainer copies verified file and inline archives before startup,
// deletes opted-in sources on success or terminal failure, and retains caller-owned sources.
func TestV2PhysicalContainerCopyArchives(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"owned", "retained", "inline", "hash-failure", "copy-failure", "create-failure", "failure-retained", "early-stop", "already-gone"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			var recording *archiveCopyOrchestrator
			server, _, startErr := StartTestEnvironmentWithOptions(t, ctx, PhysicalContainerController|PhysicalContainerImageController,
				t.Name(), t.TempDir(), TestEnvironmentOptions{DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
					recording = &archiveCopyOrchestrator{ContainerOrchestrator: orchestrator, failCopy: scenario == "copy-failure", failCreate: scenario == "create-failure"}
					return recording
				}})
			require.NoError(t, startErr)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-copy-"+scenario)
			image := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: "image", Namespace: namespace.Name},
				Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}}}
			require.NoError(t, server.Client.Create(ctx, image))
			writer := usvc_io.NewTarWriter()
			now := time.Now()
			require.NoError(t, writer.WriteFile([]byte("dependency"), "dependency.txt", 0, 0, 0644, now, now, now))
			buffer, finalizeErr := writer.Buffer()
			require.NoError(t, finalizeErr)
			source := filepath.Join(t.TempDir(), "copy.tar")
			require.NoError(t, usvc_io.WriteFile(source, buffer.Bytes(), 0600))
			hash := sha256.Sum256(buffer.Bytes())
			copyContext := apiv2.ContainerCopyFileContext{Source: source, SHA256: fmt.Sprintf("%x", hash), DeleteAfterCopy: scenario != "retained" && scenario != "failure-retained"}
			if scenario == "hash-failure" || scenario == "failure-retained" {
				copyContext.SHA256 = strings.Repeat("0", 64)
			} else if scenario == "already-gone" {
				require.NoError(t, os.Remove(source))
			} else if scenario == "inline" {
				copyContext = apiv2.ContainerCopyFileContext{RawContents: base64.StdEncoding.EncodeToString(buffer.Bytes())}
			}
			resource := &apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
				Spec: apiv2.PhysicalContainerSpec{Stop: scenario == "early-stop", Container: &apiv2.PhysicalContainerConfig{
					ImageRef: image.Name, ContainerName: namespace.Name, CopyFiles: []apiv2.ContainerCopyFileContext{copyContext},
				}}}
			require.NoError(t, server.Client.Create(ctx, resource))
			if scenario == "early-stop" {
				waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(_ *apiv2.PhysicalContainer) (bool, error) {
					_, statErr := os.Stat(source)
					return os.IsNotExist(statErr), nil
				})
				require.Zero(t, recording.copies.Load())
			} else {
				phase := apiv2.PhysicalContainerPhaseRunning
				if strings.HasSuffix(scenario, "-failure") || scenario == "failure-retained" || scenario == "already-gone" {
					phase = apiv2.PhysicalContainerPhaseFailed
				}
				observed := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
					return current.Status.Phase == phase, nil
				})
				if phase == apiv2.PhysicalContainerPhaseRunning {
					require.EqualValues(t, 1, recording.copies.Load())
					simulated := recording.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
					copied, copiedErr := simulated.GetCreatedFiles(observed.Status.ContainerID)
					require.NoError(t, copiedErr)
					require.Len(t, copied, 1)
					require.Equal(t, buffer.Bytes(), copied[0].Tar.Bytes())
				}
			}
			if copyContext.DeleteAfterCopy {
				waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(_ *apiv2.PhysicalContainer) (bool, error) {
					_, statErr := os.Stat(source)
					return os.IsNotExist(statErr), nil
				})
			} else {
				require.FileExists(t, source)
			}
			require.NoError(t, server.Client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
			if !copyContext.DeleteAfterCopy {
				require.FileExists(t, source)
			}
		})
	}
}

// Verifies that deletion settles an outstanding archive copy before removing its source,
// and subsequent best-effort source deletion does not prevent the PhysicalContainer from disappearing.
func TestV2PhysicalContainerDeletionSettlesArchiveCopy(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	release := make(chan struct{})
	entered := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	server, _, startErr := StartTestEnvironmentWithOptions(t, ctx, PhysicalContainerController|PhysicalContainerImageController,
		t.Name(), t.TempDir(), TestEnvironmentOptions{DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
			return &archiveCopyOrchestrator{ContainerOrchestrator: orchestrator, entered: entered, release: release}
		}})
	require.NoError(t, startErr)
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-copy-deleting")
	image := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: "image", Namespace: namespace.Name},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}}}
	require.NoError(t, server.Client.Create(ctx, image))
	source := filepath.Join(t.TempDir(), "copy.tar")
	writer := usvc_io.NewTarWriter()
	now := time.Now()
	require.NoError(t, writer.WriteFile([]byte("data"), "data", 0, 0, 0644, now, now, now))
	buffer, finalizeErr := writer.Buffer()
	require.NoError(t, finalizeErr)
	require.NoError(t, usvc_io.WriteFile(source, buffer.Bytes(), 0600))
	hash := sha256.Sum256(buffer.Bytes())
	resource := &apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec: apiv2.PhysicalContainerSpec{Container: &apiv2.PhysicalContainerConfig{ImageRef: image.Name, ContainerName: namespace.Name,
			CopyFiles: []apiv2.ContainerCopyFileContext{{Source: source, SHA256: fmt.Sprintf("%x", hash), DeleteAfterCopy: true}},
		}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("archive copy did not start")
	}
	require.NoError(t, server.Client.Delete(ctx, resource))
	require.FileExists(t, source)
	unblock()
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
	require.NoFileExists(t, source)
}
