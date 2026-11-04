/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

func serviceProject(name, namespace, root, kind string, arguments []string, environment []commonapi.EnvVar) *apiv2.Project {
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: apiv2.ProjectSpec{RootDirectory: root}}
	binding := apiv2.LogicalServiceBinding{Name: "http", Service: &apiv2.LogicalServiceTemplate{
		Name: "management", LogicalServiceConfig: apiv2.LogicalServiceConfig{Address: "127.0.0.1", Protocol: commonapi.PortProtocolUDP},
	}}
	if kind == "process" {
		resource.Spec.Process = &apiv2.ProjectProcessTemplate{
			LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: namespace + "-command", Args: arguments, Env: environment,
				AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit}},
			Ports: []apiv2.LogicalProcessPort{{LogicalServiceBinding: binding, Address: "127.0.0.1"}},
		}
	} else {
		resource.Spec.Container = &apiv2.ProjectContainerTemplate{
			LogicalContainerConfig: apiv2.LogicalContainerConfig{
				Image: &apiv2.PhysicalContainerImageConfig{Image: namespace + "-image"}, Command: arguments, Env: environment,
			},
			Ports: []apiv2.LogicalContainerPort{{LogicalServiceBinding: binding, ContainerPort: 8080, Address: "127.0.0.1"}},
		}
	}
	return resource
}

// Verifies that Project prepares service-backed inputs without registering an application endpoint,
// preserves the declared values and keeps independently named services across replacement.
func TestV2ProjectServicePortsAndEvaluatedValues(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			server, _, startupErr := StartTestEnvironment(t, ctx, projectTestControllers|LogicalServiceController, t.Name(), t.TempDir())
			require.NoError(t, startupErr)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-services-"+kind)
			arguments := []string{`--listen={{addressForServing "http"}}:{{portForServing "http"}}`,
				`--consumer={{addressFor "project-management"}}:{{portFor "project-management"}}`}
			environment := []commonapi.EnvVar{
				{Name: "VALUE", Value: "{{"}, {Name: "VALUE", Value: `{{portForServing "http"}}`},
				{Name: "FRONT", Value: `{{addressFor "project-management"}}:{{portFor "project-management"}}`},
			}
			resource := serviceProject("project", namespace.Name, t.TempDir(), kind, arguments, environment)
			require.NoError(t, server.Client.Create(ctx, resource))
			running := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
			require.Len(t, running.Status.Ports, 1)
			port := running.Status.Ports[0]
			require.Positive(t, port.Port)
			require.Equal(t, "project-management", port.ServiceRef)
			service := waitObjectAssumesStateEx(t, ctx, server.Client, types.NamespacedName{Namespace: namespace.Name, Name: port.ServiceRef},
				func(current *apiv2.LogicalService) (bool, error) {
					return len(current.Status.EffectiveAddresses) != 0, nil
				})
			require.Empty(t, service.OwnerReferences)
			front := service.Status.EffectiveAddresses[0]
			expectedArgs := []string{fmt.Sprintf("--listen=127.0.0.1:%d", port.Port), fmt.Sprintf("--consumer=%s:%d", front.Address, front.Port)}
			expectedEnv := []commonapi.EnvVar{{Name: "VALUE", Value: fmt.Sprint(port.Port)}, {Name: "FRONT", Value: fmt.Sprintf("%s:%d", front.Address, front.Port)}}
			require.Equal(t, expectedArgs, running.Status.EffectiveArgs)
			require.Equal(t, expectedEnv, running.Status.EffectiveEnv)
			require.True(t, running.Status.LaunchInputsReady)
			var endpoints apiv2.ServiceEndpointList
			require.NoError(t, server.Client.List(ctx, &endpoints, ctrl_client.InNamespace(namespace.Name)))
			require.Empty(t, endpoints.Items)
			if kind == "process" {
				var physicals apiv2.PhysicalProcessList
				require.NoError(t, server.Client.List(ctx, &physicals, ctrl_client.InNamespace(namespace.Name)))
				require.Empty(t, physicals.Items)
				require.Equal(t, arguments, running.Spec.Process.Args)
				require.Equal(t, environment, running.Spec.Process.Env)
			} else {
				physical := &apiv2.PhysicalContainer{}
				require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalContainerRef}, physical))
				require.Equal(t, []string{"-f", "/dev/null"}, physical.Spec.Container.Command)
				require.Equal(t, expectedEnv, physical.Spec.Container.Env)
				require.Equal(t, int32(8080), physical.Spec.Container.Ports[0].ContainerPort)
				require.Equal(t, commonapi.PortProtocolUDP, physical.Spec.Container.Ports[0].Protocol)
				require.Equal(t, arguments, running.Spec.Container.Command)
				require.Equal(t, environment, running.Spec.Container.Env)
			}
			require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseStopped)
			require.NoError(t, server.Client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
			retained := &apiv2.LogicalService{}
			require.NoError(t, server.Client.Get(ctx, service.NamespacedName(), retained))
			require.Equal(t, service.UID, retained.UID)
			replacement := serviceProject("project", namespace.Name, t.TempDir(), kind, arguments, environment)
			require.NoError(t, server.Client.Create(ctx, replacement))
			replacementRunning := waitProjectPhase(t, ctx, server.Client, replacement, apiv2.ProjectPhaseReady)
			require.NotEqual(t, resource.UID, replacement.UID)
			require.Equal(t, service.Name, replacementRunning.Status.Ports[0].ServiceRef)
			require.NoError(t, server.Client.Get(ctx, service.NamespacedName(), retained))
			require.Equal(t, service.UID, retained.UID)
			require.NoError(t, server.Client.Delete(ctx, replacement))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, replacement)
		})
	}
}

// Verifies that a missing service template dependency retries without launching a backing,
// malformed templates fail terminally, and deletion can clean either preparation outcome.
func TestV2ProjectTemplatePreparationFailures(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
		for _, malformed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/malformed=%t", kind, malformed), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
				t.Cleanup(cancel)
				server, _, startupErr := StartTestEnvironment(t, ctx, projectTestControllers, t.Name(), t.TempDir())
				require.NoError(t, startupErr)
				namespace := createV2ExecNamespace(t, ctx, server.Client, fmt.Sprintf("v2-project-template-%s-%t", kind, malformed))
				arguments := []string{`{{portFor "absent"}}`}
				if malformed {
					arguments = []string{"{{"}
				}
				resource := serviceProject("project", namespace.Name, t.TempDir(), kind, arguments, nil)
				if kind == "process" {
					resource.Spec.Process.Ports = nil
				} else {
					resource.Spec.Container.Ports = nil
				}
				require.NoError(t, server.Client.Create(ctx, resource))
				expected := apiv2.ProjectPhasePending
				if malformed {
					expected = apiv2.ProjectPhaseFailed
				}
				waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
					return current.Status.Phase == expected && len(current.Status.Conditions) != 0 &&
						current.Status.Conditions[0].Reason == "TemplateEvaluationFailed", nil
				})
				if !malformed {
					service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "absent", Namespace: namespace.Name},
						Spec: apiv2.LogicalServiceSpec{Mode: apiv2.LogicalServiceModeExisting, Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 43210}}}}
					require.NoError(t, server.Client.Create(ctx, service))
					service.Status.EffectiveAddresses = service.Spec.Addresses
					require.NoError(t, server.Client.Status().Update(ctx, service))
					waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
				}
				require.NoError(t, server.Client.Delete(ctx, resource))
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
			})
		}
	}
}

type projectEvaluatedCreateClient struct {
	ctrl_client.Client
	created chan ctrl_client.Object
	release chan struct{}
	lost    atomic.Bool
}

func (apiClient *projectEvaluatedCreateClient) Create(ctx context.Context, object ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	createErr := apiClient.Client.Create(ctx, object, options...)
	if createErr != nil {
		return createErr
	}
	owner := metav1.GetControllerOf(object)
	_, process := object.(*apiv2.PhysicalProcess)
	_, container := object.(*apiv2.PhysicalContainer)
	if owner != nil && owner.Kind == "Project" && (process || container) && apiClient.lost.CompareAndSwap(false, true) {
		apiClient.created <- object.DeepCopyObject().(ctrl_client.Object)
		select {
		case <-apiClient.release:
		case <-ctx.Done():
		}
		return errors.New("evaluated backing create response lost")
	}
	return nil
}

// Verifies that a committed physical create pins evaluated launch values even if the source
// service changes before a lost response is recovered, without evaluating or launching again.
func TestV2ProjectPinsEvaluatedSnapshotAcrossLostCreate(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"container"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			var decorated *projectEvaluatedCreateClient
			server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
				TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
					decorated = &projectEvaluatedCreateClient{Client: apiClient, created: make(chan ctrl_client.Object, 1), release: make(chan struct{})}
					return decorated
				}})
			require.NoError(t, startupErr)
			release := sync.OnceFunc(func() { close(decorated.release) })
			t.Cleanup(release)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-snapshot-"+kind)
			service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: namespace.Name},
				Spec: apiv2.LogicalServiceSpec{Mode: apiv2.LogicalServiceModeExisting, Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 43210}}}}
			require.NoError(t, server.Client.Create(ctx, service))
			service.Status.EffectiveAddresses = service.Spec.Addresses
			require.NoError(t, server.Client.Status().Update(ctx, service))
			resource := serviceProject("project", namespace.Name, t.TempDir(), kind, []string{`{{portFor "source"}}`}, nil)
			if kind == "process" {
				resource.Spec.Process.Ports = nil
			} else {
				resource.Spec.Container.Ports = nil
			}
			require.NoError(t, server.Client.Create(ctx, resource))
			var backing ctrl_client.Object
			select {
			case backing = <-decorated.created:
			case <-ctx.Done():
				t.Fatal("Project did not declare evaluated execution")
			}
			service.Status.EffectiveAddresses[0].Port = 54321
			require.NoError(t, server.Client.Status().Update(ctx, service))
			release()
			running := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
			require.Equal(t, backing.GetUID(), running.Status.RuntimeLogs.UID)
			if kind == "process" {
				physical := &apiv2.PhysicalProcess{}
				require.NoError(t, server.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(backing), physical))
				require.Equal(t, []string{"43210"}, physical.Spec.Process.Args)
				require.Len(t, environment.TestProcessExecutor.FindAll([]string{resource.Spec.Process.ExecutablePath}, "", nil), 1)
			} else {
				physical := &apiv2.PhysicalContainer{}
				require.NoError(t, server.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(backing), physical))
				require.Equal(t, []string{"43210"}, running.Status.EffectiveArgs)
				require.Equal(t, []string{"-f", "/dev/null"}, physical.Spec.Container.Command)
				simulated := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
				require.Equal(t, 1, simulated.CreateContainerCallCount(physical.Spec.Container.ContainerName))
			}
			require.NoError(t, server.Client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
		})
	}
}
