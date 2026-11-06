/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/logs"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

type staleEndpointCreateClient struct {
	ctrl_client.Client
	created atomic.Bool
	stale   atomic.Pointer[apiv2.ServiceEndpoint]
}

func (apiClient *staleEndpointCreateClient) Create(ctx context.Context, object ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	endpoint, isEndpoint := object.(*apiv2.ServiceEndpoint)
	if !isEndpoint || !apiClient.created.CompareAndSwap(false, true) {
		return apiClient.Client.Create(ctx, object, options...)
	}
	stale := endpoint.DeepCopy()
	stale.Spec.Addresses[0].Port++
	stale.Finalizers = append(stale.Finalizers, "tests.dcp.microsoft.com/hold")
	createErr := apiClient.Client.Create(ctx, stale, options...)
	if createErr == nil {
		apiClient.stale.Store(stale.DeepCopy())
	}
	return createErr
}

// Verifies that logical workload endpoint synchronization replaces stale immutable bindings,
// waits for deletion finalizers, and publishes the desired bindings under a new endpoint UID.
func TestV2LogicalEndpointReplacesChangedBindings(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			var decorated *staleEndpointCreateClient
			server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, logicalServicePreparationControllers, t.Name(), t.TempDir(),
				TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
					decorated = &staleEndpointCreateClient{Client: apiClient}
					return decorated
				}})
			require.NoError(t, startupErr)
			apiClient := server.Client
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-replace-binding-" + kind}}
			require.NoError(t, apiClient.Create(ctx, namespace))
			service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: namespace.Name},
				Spec: apiv2.LogicalServiceSpec{AddressAllocationMode: apiv2.ServiceAddressAllocationModeProxyless}}
			require.NoError(t, apiClient.Create(ctx, service))
			binding := apiv2.LogicalServiceBinding{Name: "http", ServiceRef: service.Name}
			if kind == "process" {
				resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: namespace.Name},
					Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{ExecutablePath: namespace.Name},
						Ports: []apiv2.LogicalProcessPort{{LogicalServiceBinding: binding, Address: "127.0.0.1", Port: 8080}}}}
				require.NoError(t, apiClient.Create(ctx, resource))
			} else {
				resource := &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: namespace.Name},
					Spec: apiv2.LogicalContainerSpec{Container: &apiv2.LogicalContainerConfig{
						Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name},
					}, Ports: []apiv2.LogicalContainerPort{{LogicalServiceBinding: binding, ContainerPort: 8080, HostPort: 18080}}}}
				require.NoError(t, apiClient.Create(ctx, resource))
			}
			var stale *apiv2.ServiceEndpoint
			waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(_ *apiv2.LogicalService) (bool, error) {
				stale = decorated.stale.Load()
				if stale == nil {
					return false, nil
				}
				current := &apiv2.ServiceEndpoint{}
				if getErr := apiClient.Get(ctx, stale.NamespacedName(), current); getErr != nil {
					return false, getErr
				}
				return !current.DeletionTimestamp.IsZero(), nil
			})
			current := &apiv2.ServiceEndpoint{}
			require.NoError(t, apiClient.Get(ctx, stale.NamespacedName(), current))
			require.Equal(t, stale.UID, current.UID)
			require.Equal(t, stale.Spec, current.Spec)
			current.Finalizers = nil
			require.NoError(t, apiClient.Update(ctx, current))
			ready := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(currentService *apiv2.LogicalService) (bool, error) {
				return currentService.Status.Phase == apiv2.LogicalServicePhaseReady && len(currentService.Status.EffectiveAddresses) == 1 &&
					currentService.Status.EffectiveAddresses[0].Port == stale.Spec.Addresses[0].Port-1, nil
			})
			replacement := &apiv2.ServiceEndpoint{}
			require.NoError(t, apiClient.Get(ctx, stale.NamespacedName(), replacement))
			require.NotEqual(t, stale.UID, replacement.UID)
			require.Equal(t, stale.OwnerReferences, replacement.OwnerReferences)
			require.Equal(t, stale.Spec.ServiceRef, replacement.Spec.ServiceRef)
			require.Equal(t, ready.Status.EffectiveAddresses, replacement.Spec.Addresses)
			require.Empty(t, replacement.Finalizers)
			require.NoError(t, apiClient.Delete(ctx, namespace))
			ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, namespace)
		})
	}
}

const logicalServicePreparationControllers = LogicalServiceController | LogicalProcessController |
	PhysicalProcessController | LogicalContainerController | PhysicalContainerController | PhysicalContainerImageController

type uncertainServiceCreateClient struct {
	ctrl_client.Client
	created atomic.Bool
}

func (client *uncertainServiceCreateClient) Create(ctx context.Context, object ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	createErr := client.Client.Create(ctx, object, options...)
	if _, service := object.(*apiv2.LogicalService); service && createErr == nil && client.created.CompareAndSwap(false, true) {
		return errors.New("service create response lost")
	}
	return createErr
}

type gatedServiceConfirmationReader struct {
	ctrl_client.Reader
	createClient *uncertainServiceCreateClient
	allow        atomic.Bool
	failures     atomic.Int32
}

func (reader *gatedServiceConfirmationReader) Get(
	ctx context.Context, key ctrl_client.ObjectKey, object ctrl_client.Object, options ...ctrl_client.GetOption,
) error {
	if _, service := object.(*apiv2.LogicalService); service && reader.createClient.created.Load() && !reader.allow.Load() {
		reader.failures.Add(1)
		return errors.New("service confirmation unavailable")
	}
	return reader.Reader.Get(ctx, key, object, options...)
}

// Verifies that logical workload preparation waits after an ambiguous templated-service create
// and failed uncached confirmation, then resumes through the existing lifecycle using evaluated values.
func TestV2LogicalServicePreparationRecoversOnLaterReconciliation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			var createClient *uncertainServiceCreateClient
			var confirmationReader *gatedServiceConfirmationReader
			serverInfo, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, logicalServicePreparationControllers, t.Name(), t.TempDir(),
				TestEnvironmentOptions{
					DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
						createClient = &uncertainServiceCreateClient{Client: apiClient}
						return createClient
					},
					DecorateAPIReader: func(reader ctrl_client.Reader) ctrl_client.Reader {
						confirmationReader = &gatedServiceConfirmationReader{Reader: reader, createClient: createClient}
						return confirmationReader
					},
				})
			require.NoError(t, startupErr)
			apiClient := serverInfo.Client
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "service-preparation"}}
			require.NoError(t, apiClient.Create(ctx, namespace))
			waitObjectAssumesStateEx(t, ctx, apiClient, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
				return current.Status.Phase == apiv2.NamespacePhaseActive, nil
			})
			name := types.NamespacedName{Namespace: namespace.Name, Name: "workload"}
			binding := apiv2.LogicalServiceBinding{Name: "http", Service: &apiv2.LogicalServiceTemplate{
				Name: "management", LogicalServiceSpec: apiv2.LogicalServiceSpec{Address: "127.0.0.1"},
			}}
			arguments := []string{`--listen={{portForServing "http"}}`, `--front={{portFor "workload-management"}}`}
			environment := []commonapi.EnvVar{{Name: "FRONT", Value: `{{addressFor "workload-management"}}:{{portFor "workload-management"}}`}}
			if kind == "process" {
				process := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
					Spec: apiv2.LogicalProcessSpec{
						Ports: []apiv2.LogicalProcessPort{{LogicalServiceBinding: binding, Address: "127.0.0.1"}},
						Process: &apiv2.LogicalProcessConfig{
							ExecutablePath: "service-preparation-process", Args: arguments, Env: environment,
						},
					}}
				require.NoError(t, apiClient.Create(ctx, process))
				waitObjectAssumesStateEx(t, ctx, apiClient, name, func(current *apiv2.LogicalProcess) (bool, error) {
					return confirmationReader.failures.Load() > 0 && current.Status.PhysicalProcessRef == "" &&
						len(current.Status.Conditions) > 0 && current.Status.Conditions[0].Reason == string(apiv2.LogicalResourceReasonPortPreparationPending), nil
				})
			} else {
				container := &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
					Spec: apiv2.LogicalContainerSpec{
						Ports: []apiv2.LogicalContainerPort{{LogicalServiceBinding: binding, ContainerPort: 8080}},
						Container: &apiv2.LogicalContainerConfig{
							Image: &apiv2.PhysicalContainerImageConfig{Image: "service-preparation-image"}, Command: arguments, Env: environment,
						},
					}}
				require.NoError(t, apiClient.Create(ctx, container))
				waitObjectAssumesStateEx(t, ctx, apiClient, name, func(current *apiv2.LogicalContainer) (bool, error) {
					return confirmationReader.failures.Load() > 0 && current.Status.PhysicalContainerRef == "" &&
						len(current.Status.Conditions) > 0 && current.Status.Conditions[0].Reason == string(apiv2.LogicalResourceReasonPortPreparationPending), nil
				})
			}
			var services apiv2.LogicalServiceList
			require.NoError(t, apiClient.List(ctx, &services, ctrl_client.InNamespace(namespace.Name)))
			require.Len(t, services.Items, 1)
			require.Equal(t, "workload-management", services.Items[0].Name)
			require.Empty(t, services.Items[0].OwnerReferences)
			confirmationReader.allow.Store(true)
			service := waitObjectAssumesStateEx(t, ctx, apiClient, services.Items[0].NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
				return len(current.Status.EffectiveAddresses) == 1, nil
			})
			front := service.Status.EffectiveAddresses[0]
			expectedEnv := []commonapi.EnvVar{{Name: "FRONT", Value: fmt.Sprintf("%s:%d", front.Address, front.Port)}}
			if kind == "process" {
				running := waitObjectAssumesStateEx(t, ctx, apiClient, name, func(current *apiv2.LogicalProcess) (bool, error) {
					return current.Status.Phase == apiv2.LogicalProcessPhaseRunning, nil
				})
				require.Len(t, running.Status.Ports, 1)
				listeningPort := running.Status.Ports[0].Port
				require.Positive(t, listeningPort)
				physical := &apiv2.PhysicalProcess{}
				require.NoError(t, apiClient.Get(ctx, types.NamespacedName{Namespace: name.Namespace, Name: running.Status.PhysicalProcessRef}, physical))
				require.Equal(t, []string{fmt.Sprintf("--listen=%d", listeningPort), fmt.Sprintf("--front=%d", front.Port)}, physical.Spec.Process.Args)
				require.Equal(t, expectedEnv, physical.Spec.Process.Env)
				evaluated := running.Spec.DeepCopy()
				evaluated.Process.Args, evaluated.Process.Env = physical.Spec.Process.Args, physical.Spec.Process.Env
				expectedKey, _, keyErr := evaluated.GetLifecycleKey()
				require.NoError(t, keyErr)
				require.Equal(t, expectedKey, running.Status.LifecycleKey)
				require.Equal(t, arguments, running.Spec.Process.Args)
			} else {
				running := waitObjectAssumesStateEx(t, ctx, apiClient, name, func(current *apiv2.LogicalContainer) (bool, error) {
					return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
				})
				physical := &apiv2.PhysicalContainer{}
				require.NoError(t, apiClient.Get(ctx, types.NamespacedName{Namespace: name.Namespace, Name: running.Status.PhysicalContainerRef}, physical))
				require.Equal(t, []string{"--listen=8080", fmt.Sprintf("--front=%d", front.Port)}, physical.Spec.Container.Command)
				require.Equal(t, expectedEnv, physical.Spec.Container.Env)
				evaluated := running.Spec.DeepCopy()
				evaluated.Container.Command, evaluated.Container.Env = physical.Spec.Container.Command, physical.Spec.Container.Env
				expectedKey, _, keyErr := evaluated.GetLifecycleKey(physical.Spec.Container.Ports)
				require.NoError(t, keyErr)
				require.Equal(t, expectedKey, running.Status.LifecycleKey)
				require.Equal(t, arguments, running.Spec.Container.Command)
			}
			waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
				return current.Status.Phase == apiv2.LogicalServicePhaseReady, nil
			})
			require.NoError(t, apiClient.Delete(ctx, namespace))
			ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, apiClient, namespace)
		})
	}
}

// Verifies that template services survive workload deletion and are reused by replacement instances,
// including pre-existing services with another owner and equivalent defaults, while endpoints remain instance-owned.
func TestV2LogicalServiceTemplateSurvivesWorkloadReplacement(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
		for _, source := range []string{"created", "existing"} {
			t.Run(kind+"/"+source, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
				defer cancel()
				namespace := createActiveV2Namespace(t, ctx, "v2-service-reuse-"+kind+"-"+source)
				name := types.NamespacedName{Name: "workload", Namespace: namespace.Name}
				serviceName := types.NamespacedName{Name: "workload-management", Namespace: namespace.Name}
				if source == "existing" {
					controller := true
					service := &apiv2.LogicalService{
						ObjectMeta: metav1.ObjectMeta{Name: serviceName.Name, Namespace: serviceName.Namespace,
							Annotations: map[string]string{"preserve": "existing-service"},
							OwnerReferences: []metav1.OwnerReference{{APIVersion: apiv2.GroupVersion.String(), Kind: "Namespace",
								Name: namespace.Name, UID: namespace.UID, Controller: &controller}},
						},
						Spec: apiv2.LogicalServiceSpec{
							Protocol: commonapi.PortProtocolTCP, Address: "127.0.0.1",
							AddressAllocationMode: apiv2.ServiceAddressAllocationModeLocalhost,
						},
					}
					require.NoError(t, client.Create(ctx, service))
				}
				var serviceUID, parentUID, endpointUID types.UID
				var retainedAddresses []apiv2.ServiceAddress
				var retainedSpec *apiv2.LogicalServiceSpec
				for instance := range 3 {
					binding := apiv2.LogicalServiceBinding{Name: "http", Service: &apiv2.LogicalServiceTemplate{
						Name: "management", LogicalServiceSpec: apiv2.LogicalServiceSpec{Address: "127.0.0.1"},
					}}
					if instance > 0 {
						binding.Service.Protocol = commonapi.PortProtocolTCP
						binding.Service.AddressAllocationMode = apiv2.ServiceAddressAllocationModeLocalhost
					}
					if instance == 2 {
						binding.Service.Port = 65000
					}
					var resource ctrl_client.Object
					if kind == "process" {
						process := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
							Spec: apiv2.LogicalProcessSpec{
								Process: &apiv2.LogicalProcessConfig{ExecutablePath: "service-reuse-process"},
								Ports:   []apiv2.LogicalProcessPort{{LogicalServiceBinding: binding, Address: "127.0.0.1", Port: 8080}},
							}}
						require.NoError(t, client.Create(ctx, process))
						resource = waitObjectAssumesState(t, ctx, name, func(current *apiv2.LogicalProcess) (bool, error) {
							if instance == 2 {
								return current.Status.PhysicalProcessRef == "" && len(current.Status.Conditions) > 0 &&
									strings.Contains(current.Status.Conditions[0].Message, "does not match the template"), nil
							}
							return current.Status.Phase == apiv2.LogicalProcessPhaseRunning, nil
						})
					} else {
						container := &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
							Spec: apiv2.LogicalContainerSpec{
								Container: &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: "service-reuse-image"}},
								Ports:     []apiv2.LogicalContainerPort{{LogicalServiceBinding: binding, ContainerPort: 8080}},
							}}
						require.NoError(t, client.Create(ctx, container))
						resource = waitObjectAssumesState(t, ctx, name, func(current *apiv2.LogicalContainer) (bool, error) {
							if instance == 2 {
								return current.Status.PhysicalContainerRef == "" && len(current.Status.Conditions) > 0 &&
									strings.Contains(current.Status.Conditions[0].Message, "does not match the template"), nil
							}
							return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
						})
					}
					require.NotEqual(t, parentUID, resource.GetUID())
					parentUID = resource.GetUID()
					if instance < 2 {
						service := waitObjectAssumesState(t, ctx, serviceName, func(current *apiv2.LogicalService) (bool, error) {
							return current.Status.Phase == apiv2.LogicalServicePhaseReady, nil
						})
						if instance == 0 {
							serviceUID, retainedAddresses, retainedSpec = service.UID, service.Status.EffectiveAddresses, service.Spec.DeepCopy()
						}
						require.Equal(t, serviceUID, service.UID)
						require.Equal(t, retainedAddresses, service.Status.EffectiveAddresses)
						require.Equal(t, *retainedSpec, service.Spec)
						var endpoints apiv2.ServiceEndpointList
						require.NoError(t, client.List(ctx, &endpoints, ctrl_client.InNamespace(namespace.Name)))
						require.Len(t, endpoints.Items, 1)
						require.True(t, metav1.IsControlledBy(&endpoints.Items[0], resource))
						require.NotEqual(t, endpointUID, endpoints.Items[0].UID)
						endpointUID = endpoints.Items[0].UID
					}
					require.NoError(t, client.Delete(ctx, resource))
					switch current := resource.(type) {
					case *apiv2.LogicalProcess:
						ctrl_testutil.WaitObjectDeleted[apiv2.LogicalProcess](t, ctx, client, current)
					case *apiv2.LogicalContainer:
						ctrl_testutil.WaitObjectDeleted[apiv2.LogicalContainer](t, ctx, client, current)
					}
					service := waitObjectAssumesState(t, ctx, serviceName, func(current *apiv2.LogicalService) (bool, error) {
						return current.Status.Phase == apiv2.LogicalServicePhasePending, nil
					})
					require.Equal(t, serviceUID, service.UID)
					require.Equal(t, retainedAddresses, service.Status.EffectiveAddresses)
					require.Equal(t, *retainedSpec, service.Spec)
					if source == "existing" {
						require.Len(t, service.OwnerReferences, 1)
						require.Equal(t, namespace.UID, service.OwnerReferences[0].UID)
						require.Equal(t, "existing-service", service.Annotations["preserve"])
					} else {
						require.Empty(t, service.OwnerReferences)
					}
					var remainingEndpoints apiv2.ServiceEndpointList
					require.NoError(t, client.List(ctx, &remainingEndpoints, ctrl_client.InNamespace(namespace.Name)))
					require.Empty(t, remainingEndpoints.Items)
				}
				require.NoError(t, client.Delete(ctx, namespace))
				ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)
				ctrl_testutil.WaitObjectDeleted[apiv2.LogicalService](t, ctx, client,
					&apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: serviceName.Name, Namespace: serviceName.Namespace}})
			})
		}
	}
}

// Verifies that argument/environment service references gate logical workload startup even
// without declared ports, and that both controllers hash and launch the resolved address/port pair.
func TestV2LogicalValuesWaitForReferencedService(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			namespace := createActiveV2Namespace(t, ctx, "v2-template-reference-"+kind)
			name := types.NamespacedName{Namespace: namespace.Name, Name: "workload"}
			arguments := []string{`{{portFor "dependency" "IPv4"}}`}
			environment := []commonapi.EnvVar{{Name: "HOST", Value: `{{addressFor "dependency" "IPv4"}}`}}
			if kind == "process" {
				resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
					Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{ExecutablePath: "template-reference-process", Args: arguments, Env: environment}}}
				require.NoError(t, client.Create(ctx, resource))
				waitObjectAssumesState(t, ctx, name, func(current *apiv2.LogicalProcess) (bool, error) {
					return current.Status.Phase == apiv2.LogicalProcessPhasePending && len(current.Status.Conditions) > 0 &&
						current.Status.Conditions[0].Reason == string(apiv2.LogicalResourceReasonTemplateEvaluationFailed) && current.Status.PhysicalProcessRef == "", nil
				})
			} else {
				resource := &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
					Spec: apiv2.LogicalContainerSpec{Container: &apiv2.LogicalContainerConfig{
						Image: &apiv2.PhysicalContainerImageConfig{Image: "template-reference-image"}, Command: arguments, Env: environment,
					}}}
				require.NoError(t, client.Create(ctx, resource))
				waitObjectAssumesState(t, ctx, name, func(current *apiv2.LogicalContainer) (bool, error) {
					return current.Status.Phase == apiv2.LogicalContainerPhasePending && len(current.Status.Conditions) > 0 &&
						current.Status.Conditions[0].Reason == string(apiv2.LogicalResourceReasonTemplateEvaluationFailed) && current.Status.PhysicalContainerRef == "", nil
				})
			}

			service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "dependency", Namespace: namespace.Name},
				Spec: apiv2.LogicalServiceSpec{
					AddressAllocationMode: apiv2.ServiceAddressAllocationModeProxyless,
				}}
			require.NoError(t, client.Create(ctx, service))
			endpoint := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "dependency", Namespace: namespace.Name},
				Spec: apiv2.ServiceEndpointSpec{ServiceRef: service.Name,
					Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 5000}, {Address: "::1", Port: 5001}}}}
			require.NoError(t, client.Create(ctx, endpoint))
			if kind == "process" {
				running := waitObjectAssumesState(t, ctx, name, func(current *apiv2.LogicalProcess) (bool, error) {
					return current.Status.Phase == apiv2.LogicalProcessPhaseRunning, nil
				})
				physical := &apiv2.PhysicalProcess{}
				require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: name.Namespace, Name: running.Status.PhysicalProcessRef}, physical))
				require.Equal(t, []string{strconv.Itoa(5000)}, physical.Spec.Process.Args)
				require.Equal(t, []commonapi.EnvVar{{Name: "HOST", Value: "127.0.0.1"}}, physical.Spec.Process.Env)
			} else {
				running := waitObjectAssumesState(t, ctx, name, func(current *apiv2.LogicalContainer) (bool, error) {
					return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
				})
				physical := &apiv2.PhysicalContainer{}
				require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: name.Namespace, Name: running.Status.PhysicalContainerRef}, physical))
				require.Equal(t, []string{strconv.Itoa(5000)}, physical.Spec.Container.Command)
				require.Equal(t, []commonapi.EnvVar{{Name: "HOST", Value: "127.0.0.1"}}, physical.Spec.Container.Env)
			}
			require.NoError(t, client.Delete(ctx, namespace))
			ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)
		})
	}
}

// Verifies that logical argument and environment templates with invalid service references fail
// preparation terminally without creating workload backings or image dependencies.
func TestV2LogicalValuesRejectInvalidServiceReferences(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
		for _, fieldName := range []string{"arguments", "environment"} {
			for _, reference := range []string{"", "INVALID"} {
				t.Run(kind+"/"+fieldName+"/"+reference, func(t *testing.T) {
					t.Parallel()
					ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
					defer cancel()
					referenceName := "empty"
					if reference != "" {
						referenceName = strings.ToLower(reference)
					}
					namespace := createActiveV2Namespace(t, ctx, "v2-invalid-reference-"+kind+"-"+fieldName+"-"+referenceName)
					name := types.NamespacedName{Namespace: namespace.Name, Name: "workload"}
					var arguments []string
					var environment []commonapi.EnvVar
					if fieldName == "arguments" {
						arguments = []string{fmt.Sprintf(`{{portFor %q}}`, reference)}
					} else {
						environment = []commonapi.EnvVar{{Name: "HOST", Value: fmt.Sprintf(`{{addressFor %q}}`, reference)}}
					}
					if kind == "process" {
						resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
							Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{
								ExecutablePath: namespace.Name, Args: arguments, Env: environment,
							}}}
						require.NoError(t, client.Create(ctx, resource))
						failed := waitObjectAssumesState(t, ctx, name, func(current *apiv2.LogicalProcess) (bool, error) {
							return current.Status.Phase == apiv2.LogicalProcessPhaseFailed && len(current.Status.Conditions) > 0 &&
								current.Status.Conditions[0].Reason == string(apiv2.LogicalResourceReasonTemplateEvaluationFailed), nil
						})
						require.Contains(t, failed.Status.Conditions[0].Message, "invalid service reference")
						require.Empty(t, failed.Status.PhysicalProcessRef)
						require.Empty(t, testProcessExecutor.FindAll([]string{namespace.Name}, "", nil))
					} else {
						resource := &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
							Spec: apiv2.LogicalContainerSpec{Container: &apiv2.LogicalContainerConfig{
								Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name}, Command: arguments, Env: environment,
							}}}
						require.NoError(t, client.Create(ctx, resource))
						failed := waitObjectAssumesState(t, ctx, name, func(current *apiv2.LogicalContainer) (bool, error) {
							return current.Status.Phase == apiv2.LogicalContainerPhaseFailed && len(current.Status.Conditions) > 0 &&
								current.Status.Conditions[0].Reason == string(apiv2.LogicalResourceReasonTemplateEvaluationFailed), nil
						})
						require.Contains(t, failed.Status.Conditions[0].Message, "invalid service reference")
						require.Empty(t, failed.Status.PhysicalContainerRef)
					}
					var processes apiv2.PhysicalProcessList
					require.NoError(t, client.List(ctx, &processes, ctrl_client.InNamespace(namespace.Name)))
					require.Empty(t, processes.Items)
					var containersList apiv2.PhysicalContainerList
					require.NoError(t, client.List(ctx, &containersList, ctrl_client.InNamespace(namespace.Name)))
					require.Empty(t, containersList.Items)
					var images apiv2.PhysicalContainerImageList
					require.NoError(t, client.List(ctx, &images, ctrl_client.InNamespace(namespace.Name)))
					require.Empty(t, images.Items)
					require.NoError(t, client.Delete(ctx, namespace))
					ctrl_testutil.WaitObjectDeleted(t, ctx, client, namespace)
				})
			}
		}
	}
}

func createTerminatingPreparationService(t *testing.T, ctx context.Context, namespace string) (*apiv2.LogicalService, func()) {
	t.Helper()
	const hold = "tests.dcp.microsoft.com/preparation-hold"
	service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "dependency", Namespace: namespace, Finalizers: []string{hold}},
		Spec: apiv2.LogicalServiceSpec{AddressAllocationMode: apiv2.ServiceAddressAllocationModeProxyless}}
	require.NoError(t, client.Create(ctx, service))
	require.NoError(t, client.Delete(ctx, service))
	release := sync.OnceFunc(func() {
		cleanupCtx := context.WithoutCancel(ctx)
		require.NoError(t, retryOnConflict(cleanupCtx, service.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalService) error {
			current.Finalizers = slices.DeleteFunc(current.Finalizers, func(finalizer string) bool { return finalizer == hold })
			return client.Update(updateCtx, current)
		}))
		ctrl_testutil.WaitObjectDeleted(t, cleanupCtx, client, service)
	})
	t.Cleanup(release)
	return service, release
}

// Verifies that LogicalProcess stops retained identities only during nonterminal preparation,
// while invalid templates and terminating service dependencies remain terminal after service replacement.
func TestV2LogicalProcessPreparationLifecycle(t *testing.T) {
	t.Parallel()
	for _, fieldName := range []string{"arguments", "environment"} {
		for _, keyKind := range []string{"computed", "explicit"} {
			for _, outcome := range []string{"waiting", "fatal", "terminating", "terminating-port"} {
				if outcome == "terminating-port" && fieldName != "arguments" {
					continue
				}
				t.Run(fieldName+"/"+keyKind+"/"+outcome, func(t *testing.T) {
					t.Parallel()
					ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
					defer cancel()
					namespace := createActiveV2Namespace(t, ctx, "v2-process-template-"+fieldName+"-"+keyKind+"-"+outcome)
					command := namespace.Name + "-command"
					spec := apiv2.LogicalProcessSpec{Mode: apiv2.LogicalProcessModePersistent,
						Process: &apiv2.LogicalProcessConfig{ExecutablePath: command, Args: []string{"original"}}}
					if keyKind == "explicit" {
						spec.LifecycleKey = "retained-key"
					}
					first := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: namespace.Name}, Spec: spec}
					require.NoError(t, client.Create(ctx, first))
					running := waitLogicalProcessPhase(t, ctx, first.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
					pid, pidErr := process.Int64_ToPidT(*running.Status.PID)
					require.NoError(t, pidErr)
					handle := process.NewHandle(pid, running.Status.IdentityTimestamp.Time)
					record, recordErr := testStateStore.GetPersistentProcess(ctx, first.GetLeaseKey())
					require.NoError(t, recordErr)
					t.Cleanup(func() {
						cleanupCtx := context.WithoutCancel(ctx)
						stopErr := testProcessExecutor.StopProcess(cleanupCtx, handle)
						require.True(t, stopErr == nil || process.IsProcessGoneErr(stopErr), "%v", stopErr)
						require.NoError(t, logs.RemoveOutputFiles(cleanupCtx, record.StdOutFile, record.StdErrFile))
						require.NoError(t, testStateStore.DeletePersistentProcess(cleanupCtx, record.ResourceKey))
					})
					require.NoError(t, client.Delete(ctx, first))
					ctrl_testutil.WaitObjectDeleted(t, ctx, client, first)
					require.NoError(t, testProcessExecutor.CheckProcessRunning(handle))
					replacementSpec := spec.DeepCopy()
					if keyKind == "explicit" {
						replacementSpec.LifecycleKey = "different-requested-key"
					}
					value := "{{"
					expectedPhase := apiv2.LogicalProcessPhaseFailed
					expectedReason := apiv2.LogicalResourceReasonTemplateEvaluationFailed
					var dependency *apiv2.LogicalService
					var releaseDependency func()
					if outcome == "waiting" {
						value = `{{portFor "missing"}}`
						expectedPhase = apiv2.LogicalProcessPhasePending
					} else if strings.HasPrefix(outcome, "terminating") {
						dependency, releaseDependency = createTerminatingPreparationService(t, ctx, namespace.Name)
						value = `{{portFor "dependency"}}`
						if outcome == "terminating-port" {
							value = "literal"
							expectedReason = apiv2.LogicalResourceReasonPortPreparationFailed
							replacementSpec.Ports = []apiv2.LogicalProcessPort{{LogicalServiceBinding: apiv2.LogicalServiceBinding{
								Name: "http", ServiceRef: dependency.Name,
							}, Port: 8080}}
						}
					}
					if fieldName == "arguments" {
						replacementSpec.Process.Args = []string{value}
					} else {
						replacementSpec.Process.Env = []commonapi.EnvVar{{Name: "VALUE", Value: value}}
					}
					replacement := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: first.Name, Namespace: namespace.Name}, Spec: *replacementSpec}
					require.NoError(t, client.Create(ctx, replacement))
					prepared := waitObjectAssumesState(t, ctx, replacement.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
						return current.Status.Phase == expectedPhase && len(current.Status.Conditions) > 0 &&
							current.Status.Conditions[0].Reason == string(expectedReason), nil
					})
					require.Empty(t, prepared.Status.PhysicalProcessRef)
					if dependency != nil {
						require.Contains(t, prepared.Status.Conditions[0].Message, "terminating")
						releaseDependency()
						require.NoError(t, client.Create(ctx, &apiv2.LogicalService{
							ObjectMeta: metav1.ObjectMeta{Name: dependency.Name, Namespace: dependency.Namespace}, Spec: dependency.Spec,
						}))
					}
					require.NoError(t, client.Patch(ctx, replacement,
						ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
					if outcome == "waiting" {
						stopped := waitLogicalProcessPhase(t, ctx, replacement.NamespacedName(), apiv2.LogicalProcessPhaseExited)
						require.Equal(t, replacement.Spec.LifecycleKey, stopped.Status.LifecycleKey)
						require.Equal(t, int64(record.PID), *stopped.Status.PID)
						var backings apiv2.PhysicalProcessList
						require.NoError(t, client.List(ctx, &backings, ctrl_client.InNamespace(namespace.Name)))
						require.Len(t, backings.Items, 1)
						physical := &backings.Items[0]
						require.Equal(t, stopped.Status.PhysicalProcessRef, physical.Name)
						require.True(t, physical.Spec.Stop)
						require.Equal(t, stopped.Status.PID, physical.Spec.PID)
						require.True(t, record.IdentityTime.Equal(physical.Spec.IdentityTimestamp.Time))
						require.Nil(t, physical.Spec.Process)
						require.Equal(t, record.StdOutFile, physical.Spec.StdoutFile)
						require.Equal(t, record.StdErrFile, physical.Spec.StderrFile)
						retainedRecord, retainedErr := testStateStore.GetPersistentProcess(ctx, replacement.GetLeaseKey())
						require.NoError(t, retainedErr)
						expectedRecord := *record
						expectedRecord.UpdatedAt = retainedRecord.UpdatedAt
						require.Equal(t, &expectedRecord, retainedRecord)
						require.True(t, process.IsProcessGoneErr(testProcessExecutor.CheckProcessRunning(handle)))
					} else {
						// Force status reprojection to observe reconciliation after Stop rather than a stale failure snapshot.
						require.NoError(t, retryOnConflict(ctx, replacement.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalProcess) error {
							current.Status.Phase = apiv2.LogicalProcessPhaseUnknown
							current.Status.Conditions = nil
							return client.Status().Update(updateCtx, current)
						}))
						terminal := waitObjectAssumesState(t, ctx, replacement.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
							return current.Status.Phase == apiv2.LogicalProcessPhaseFailed && current.Spec.Stop &&
								len(current.Status.Conditions) > 0 && current.Status.Conditions[0].Reason == string(expectedReason), nil
						})
						require.Empty(t, terminal.Status.PhysicalProcessRef)
						var backings apiv2.PhysicalProcessList
						require.NoError(t, client.List(ctx, &backings, ctrl_client.InNamespace(namespace.Name)))
						require.Empty(t, backings.Items)
						require.NoError(t, testProcessExecutor.CheckProcessRunning(handle))
					}
					require.Len(t, testProcessExecutor.FindAll([]string{command}, "", nil), 1)
					require.NoError(t, client.Delete(ctx, namespace))
					ctrl_testutil.WaitObjectDeleted(t, ctx, client, namespace)
				})
			}
		}
	}
}

// Verifies that LogicalContainer honors stop during nonterminal preparation, while invalid
// templates and terminating service dependencies remain terminal after service replacement.
func TestV2LogicalContainerPreparationLifecycle(t *testing.T) {
	t.Parallel()
	for _, fieldName := range []string{"arguments", "environment"} {
		for _, keyKind := range []string{"computed", "explicit"} {
			for _, outcome := range []string{"waiting", "fatal", "terminating", "terminating-port"} {
				if outcome == "terminating-port" && fieldName != "arguments" {
					continue
				}
				t.Run(fieldName+"/"+keyKind+"/"+outcome, func(t *testing.T) {
					t.Parallel()
					ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
					defer cancel()
					namespace := createActiveV2Namespace(t, ctx, "v2-container-template-"+fieldName+"-"+keyKind+"-"+outcome)
					runtimeName := namespace.Name + "-runtime"
					spec := apiv2.LogicalContainerSpec{Mode: apiv2.LogicalContainerModePersistent, ContainerName: runtimeName,
						Container: &apiv2.LogicalContainerConfig{
							Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name}, Command: []string{"original"},
						}}
					if keyKind == "explicit" {
						spec.LifecycleKey = "retained-key"
					}
					first := &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: namespace.Name}, Spec: spec}
					require.NoError(t, client.Create(ctx, first))
					running := waitObjectAssumesState(t, ctx, first.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
						return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
					})
					removeRuntimeContainerOnCleanup(t, running.Status.ContainerID)
					require.NoError(t, client.Delete(ctx, first))
					ctrl_testutil.WaitObjectDeleted(t, ctx, client, first)
					retained, inspectErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{running.Status.ContainerID}})
					require.NoError(t, inspectErr)
					require.Len(t, retained, 1)
					require.Equal(t, containers.ContainerStatusRunning, retained[0].Status)
					replacementSpec := spec.DeepCopy()
					if keyKind == "explicit" {
						replacementSpec.LifecycleKey = "different-requested-key"
					}
					value := "{{"
					expectedPhase := apiv2.LogicalContainerPhaseFailed
					expectedReason := apiv2.LogicalResourceReasonTemplateEvaluationFailed
					var dependency *apiv2.LogicalService
					var releaseDependency func()
					if outcome == "waiting" {
						value = `{{portFor "missing"}}`
						expectedPhase = apiv2.LogicalContainerPhasePending
					} else if strings.HasPrefix(outcome, "terminating") {
						dependency, releaseDependency = createTerminatingPreparationService(t, ctx, namespace.Name)
						value = `{{portFor "dependency"}}`
						if outcome == "terminating-port" {
							value = "literal"
							expectedReason = apiv2.LogicalResourceReasonPortPreparationFailed
							replacementSpec.Ports = []apiv2.LogicalContainerPort{{LogicalServiceBinding: apiv2.LogicalServiceBinding{
								Name: "http", ServiceRef: dependency.Name,
							}, ContainerPort: 8080}}
						}
					}
					if fieldName == "arguments" {
						replacementSpec.Container.Command = []string{value}
					} else {
						replacementSpec.Container.Env = []commonapi.EnvVar{{Name: "VALUE", Value: value}}
					}
					replacement := &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: first.Name, Namespace: namespace.Name}, Spec: *replacementSpec}
					require.NoError(t, client.Create(ctx, replacement))
					prepared := waitObjectAssumesState(t, ctx, replacement.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
						return current.Status.Phase == expectedPhase && len(current.Status.Conditions) > 0 &&
							current.Status.Conditions[0].Reason == string(expectedReason), nil
					})
					require.Empty(t, prepared.Status.PhysicalContainerRef)
					if dependency != nil {
						require.Contains(t, prepared.Status.Conditions[0].Message, "terminating")
						releaseDependency()
						require.NoError(t, client.Create(ctx, &apiv2.LogicalService{
							ObjectMeta: metav1.ObjectMeta{Name: dependency.Name, Namespace: dependency.Namespace}, Spec: dependency.Spec,
						}))
					}
					require.NoError(t, client.Patch(ctx, replacement,
						ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
					expectedRuntimeState := containers.ContainerStatusRunning
					if outcome == "waiting" {
						stopped := waitObjectAssumesState(t, ctx, replacement.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
							return current.Status.Phase == apiv2.LogicalContainerPhaseExited, nil
						})
						require.Equal(t, running.Status.ContainerID, stopped.Status.ContainerID)
						require.Equal(t, replacement.Spec.LifecycleKey, stopped.Status.LifecycleKey)
						expectedRuntimeState = containers.ContainerStatusExited
					} else {
						// Force status reprojection to observe reconciliation after Stop rather than a stale failure snapshot.
						require.NoError(t, retryOnConflict(ctx, replacement.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainer) error {
							current.Status.Phase = apiv2.LogicalContainerPhaseUnknown
							current.Status.Conditions = nil
							return client.Status().Update(updateCtx, current)
						}))
						terminal := waitObjectAssumesState(t, ctx, replacement.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
							return current.Status.Phase == apiv2.LogicalContainerPhaseFailed && current.Spec.Stop &&
								len(current.Status.Conditions) > 0 && current.Status.Conditions[0].Reason == string(expectedReason), nil
						})
						require.Empty(t, terminal.Status.PhysicalContainerRef)
						var backings apiv2.PhysicalContainerList
						require.NoError(t, client.List(ctx, &backings, ctrl_client.InNamespace(namespace.Name)))
						require.Empty(t, backings.Items)
						var images apiv2.PhysicalContainerImageList
						require.NoError(t, client.List(ctx, &images, ctrl_client.InNamespace(namespace.Name)))
						require.Empty(t, images.Items)
					}
					require.Equal(t, 1, containerOrchestrator.CreateContainerCallCount(runtimeName))
					require.Zero(t, containerOrchestrator.RemoveContainerCallCount(runtimeName))
					observed, observeErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{running.Status.ContainerID}})
					require.NoError(t, observeErr)
					require.Len(t, observed, 1)
					require.Equal(t, expectedRuntimeState, observed[0].Status)
					require.NoError(t, client.Delete(ctx, namespace))
					ctrl_testutil.WaitObjectDeleted(t, ctx, client, namespace)
				})
			}
		}
	}
}
