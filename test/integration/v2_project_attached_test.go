/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	internaltest "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that Project observes attached targets without taking cleanup ownership,
// preserves them through stop and deletion, and still cleans up a separately launched target.
func TestV2ProjectPreservesAttachedTargets(t *testing.T) {
	t.Parallel()
	for _, ending := range []string{"stop", "delete"} {
		t.Run(ending, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			server, _, startupErr := StartTestEnvironment(t, ctx, ProjectController, t.Name(), t.TempDir())
			require.NoError(t, startupErr)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-attached-"+ending)
			tool, toolErr := internaltest.GetTestToolPath("delay")
			require.NoError(t, toolErr)
			owner := process.NewOSExecutor(logr.Discard())
			t.Cleanup(owner.Dispose)
			attached := []process.ProcessHandle{
				startProjectTarget(t, ctx, owner, tool),
				startProjectTarget(t, ctx, owner, tool),
			}
			launched := startProjectTarget(t, ctx, owner, tool)
			project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(),
					Process:      &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "unused"}},
					DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{tool, "--dap-stdio"}},
				}}
			require.NoError(t, server.Client.Create(ctx, project))
			waitProjectPhase(t, ctx, server.Client, project, apiv2.ProjectPhaseReady)
			connection, connectErr := connectProjectDebug(t, ctx, server.ClientConfig, project)
			require.NoError(t, connectErr)
			defer connection.Close()
			for index, method := range []string{"attach", "attachForSuspendedLaunch"} {
				sendProjectProtocolRequest(t, connection, fmt.Sprintf(
					`{"seq":%d,"type":"request","command":"reportProcess","arguments":{"systemProcessId":%d,"startMethod":%q}}`,
					index+1, attached[index].Pid, method))
				readProjectDebugMessage(t, connection)
				readProjectDebugMessage(t, connection)
			}
			reportProjectTarget(t, connection, launched.Pid)
			observed := waitObjectAssumesStateEx(t, ctx, server.Client, project.NamespacedName(), func(current *apiv2.Project) (bool, error) {
				if current.Status.Debug == nil || len(current.Status.Debug.Sessions) != 1 || len(current.Status.Debug.Sessions[0].Processes) != 3 {
					return false, nil
				}
				for _, target := range current.Status.Debug.Sessions[0].Processes {
					if target.Observation.Phase != apiv2.ProjectDebugProcessRunning {
						return false, nil
					}
				}
				return true, nil
			})
			for _, target := range observed.Status.Debug.Sessions[0].Processes[:2] {
				require.True(t, target.Attached)
				require.Equal(t, apiv2.ProjectDebugProcessCleanupNotOwned, target.Observation.Cleanup)
			}
			require.NoError(t, connection.Close())
			if ending == "stop" {
				require.NoError(t, server.Client.Patch(ctx, project, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
				waitProjectPhase(t, ctx, server.Client, project, apiv2.ProjectPhaseStopped)
			}
			require.NoError(t, server.Client.Delete(ctx, project))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, project)
			for _, handle := range attached {
				require.NoError(t, owner.CheckProcessRunning(handle), "attached targets must survive Project cleanup")
			}
			require.True(t, process.IsProcessGoneErr(owner.CheckProcessRunning(launched)))
		})
	}
}
