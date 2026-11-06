/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/internal/templating"
	"github.com/microsoft/dcp/pkg/commonapi"
)

var (
	errLogicalServiceDependencyFailed = errors.New("service dependency failed")
	errLogicalValuePreparationFailed  = errors.New("logical value preparation cannot complete")
)

type logicalPortRequest struct {
	binding apiv2.LogicalServiceBinding
	address string
	port    int32
}

type logicalPortData struct {
	templating.ServingPort
	serviceRef string
	protocol   commonapi.PortProtocol
}

type logicalPortSelection struct {
	allocatedPort int32
	protocol      commonapi.PortProtocol
}

func logicalPortForSelection(owner ctrl_client.Object, request logicalPortRequest, selection logicalPortSelection) logicalPortData {
	serviceRef := commonapi.AsNamespacedName(request.binding.ServiceRef, owner.GetNamespace()).Name
	if request.binding.Service != nil {
		serviceRef = owner.GetName() + "-" + request.binding.Service.Name
	}
	address := request.address
	if address == "" {
		address = networking.Localhost
	}
	port := request.port
	if port == 0 {
		port = selection.allocatedPort
	}
	return logicalPortData{ServingPort: templating.ServingPort{Name: request.binding.Name, Address: address, Port: port},
		serviceRef: serviceRef, protocol: selection.protocol}
}

func logicalPortSelections(ports []logicalPortData, requests []logicalPortRequest) []logicalPortSelection {
	var selected []logicalPortSelection
	for portIndex, port := range ports {
		selection := logicalPortSelection{protocol: port.protocol}
		if requests[portIndex].port == 0 {
			selection.allocatedPort = port.Port
		}
		selected = append(selected, selection)
	}
	return selected
}

func (data *logicalContainerData) portData(container *apiv2.LogicalContainer) []logicalPortData {
	var ports []logicalPortData
	for portIndex, selected := range data.ports {
		declared := container.Spec.Ports[portIndex]
		ports = append(ports, logicalPortForSelection(container, logicalPortRequest{
			binding: declared.LogicalServiceBinding, address: declared.Address, port: declared.ContainerPort,
		}, selected))
	}
	return ports
}

func (data *logicalProcessData) portData(resource *apiv2.LogicalProcess) []logicalPortData {
	var ports []logicalPortData
	for portIndex, selected := range data.ports {
		declared := resource.Spec.Ports[portIndex]
		ports = append(ports, logicalPortForSelection(resource, logicalPortRequest{
			binding: declared.LogicalServiceBinding, address: declared.Address, port: declared.Port,
		}, selected))
	}
	return ports
}

func logicalPortResourceName(owner ctrl_client.Object, name string) string {
	return "logical-" + string(owner.GetUID()) + "-" + name
}

func prepareLogicalPorts(
	ctx context.Context, apiClient ctrl_client.Client, reader ctrl_client.Reader, owner commonapi.DcpModelObject,
	requested []logicalPortRequest, log logr.Logger,
) ([]logicalPortData, error) {
	prepared := make([]logicalPortData, 0, len(requested))
	for _, request := range requested {
		port := logicalPortForSelection(owner, request, logicalPortSelection{})
		service := &apiv2.LogicalService{}
		serviceName := commonapi.AsNamespacedName(port.serviceRef, owner.GetNamespace())
		if request.binding.Service != nil {
			service.ObjectMeta = metav1.ObjectMeta{Name: serviceName.Name, Namespace: owner.GetNamespace()}
			service.Spec = request.binding.Service.LogicalServiceSpec
			createErr := apiClient.Create(ctx, service)
			if createErr != nil {
				if getErr := reader.Get(ctx, serviceName, service); getErr != nil {
					return prepared, fmt.Errorf("create port %q service and verify outcome: %w", request.binding.Name, errors.Join(createErr, getErr))
				}
			}
		} else if getErr := reader.Get(ctx, serviceName, service); getErr != nil {
			return prepared, fmt.Errorf("get port %q service: %w", request.binding.Name, getErr)
		}
		if !service.DeletionTimestamp.IsZero() {
			return prepared, fmt.Errorf("%w: port %q service %q is terminating", errLogicalServiceDependencyFailed, request.binding.Name, serviceName)
		}
		if service.Status.Phase == apiv2.LogicalServicePhaseFailed {
			message := fmt.Sprintf("port %q service %q failed", request.binding.Name, serviceName)
			if condition := apimeta.FindStatusCondition(service.Status.Conditions, string(apiv2.ConditionReady)); condition != nil {
				message += fmt.Sprintf(": %s: %s", condition.Reason, condition.Message)
			}
			return prepared, fmt.Errorf("%w: %s", errLogicalServiceDependencyFailed, message)
		}
		if request.binding.Service != nil && !service.Spec.Equal(request.binding.Service.LogicalServiceSpec) {
			return prepared, fmt.Errorf("service %q configuration does not match the template", serviceName)
		}
		port.protocol = service.Spec.EffectiveProtocol()
		if port.Port == 0 {
			allocatedPort, allocationErr := networking.GetFreePort(port.protocol, port.Address, log)
			if allocationErr != nil {
				return prepared, fmt.Errorf("allocate logical port %q: %w", port.Name, allocationErr)
			}
			port.Port = allocatedPort
		}
		prepared = append(prepared, port)
	}
	return prepared, nil
}

func servingPorts(ports []logicalPortData) []templating.ServingPort {
	result := make([]templating.ServingPort, len(ports))
	for portIndex, port := range ports {
		result[portIndex] = port.ServingPort
	}
	return result
}

func projectLogicalPorts(ports []logicalPortData, destination *[]apiv2.LogicalPortStatus) objectChange {
	var projected []apiv2.LogicalPortStatus
	for _, port := range ports {
		projected = append(projected, apiv2.LogicalPortStatus{Name: port.Name, ServiceRef: port.serviceRef, Address: port.Address, Port: port.Port})
	}
	if reflect.DeepEqual(projected, *destination) {
		return noChange
	}
	*destination = projected
	return statusChanged
}

func syncLogicalEndpoints(
	ctx context.Context, apiClient ctrl_client.Client, reader ctrl_client.Reader, owner commonapi.DcpModelObject,
	ports []logicalPortData, addresses [][]apiv2.ServiceAddress,
) error {
	desired := make(map[string]apiv2.ServiceEndpointSpec, len(ports))
	for portIndex, port := range ports {
		if portIndex >= len(addresses) || len(addresses[portIndex]) == 0 {
			continue
		}
		desired[logicalPortResourceName(owner, port.Name)] = apiv2.ServiceEndpointSpec{
			ServiceRef: port.serviceRef, Addresses: addresses[portIndex],
		}
	}
	var current apiv2.ServiceEndpointList
	if listErr := reader.List(ctx, &current, ctrl_client.InNamespace(owner.GetNamespace())); listErr != nil {
		return fmt.Errorf("list logical workload endpoints: %w", listErr)
	}
	var syncErr error
	for endpointIndex := range current.Items {
		endpoint := &current.Items[endpointIndex]
		spec, wanted := desired[endpoint.Name]
		if !metav1.IsControlledBy(endpoint, owner) {
			if wanted {
				delete(desired, endpoint.Name)
				syncErr = errors.Join(syncErr, fmt.Errorf("endpoint %q has a different owner", endpoint.NamespacedName()))
			}
			continue
		}
		delete(desired, endpoint.Name)
		if wanted && endpoint.DeletionTimestamp.IsZero() && reflect.DeepEqual(endpoint.Spec, spec) {
			continue
		}
		if endpoint.DeletionTimestamp.IsZero() {
			uid := endpoint.UID
			deleteErr := apiClient.Delete(ctx, endpoint, ctrl_client.Preconditions{UID: &uid})
			if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
				syncErr = errors.Join(syncErr, fmt.Errorf("delete endpoint %q: %w", endpoint.NamespacedName(), deleteErr))
				continue
			}
		}
		if wanted {
			syncErr = errors.Join(syncErr, fmt.Errorf("endpoint %q is awaiting replacement after deletion", endpoint.NamespacedName()))
		}
	}
	for _, port := range ports {
		name := logicalPortResourceName(owner, port.Name)
		spec, missing := desired[name]
		if !missing {
			continue
		}
		endpoint := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: owner.GetNamespace()}, Spec: spec}
		if ownerErr := ctrl.SetControllerReference(owner, endpoint, apiClient.Scheme()); ownerErr != nil {
			syncErr = errors.Join(syncErr, fmt.Errorf("set port %q endpoint owner: %w", port.Name, ownerErr))
			continue
		}
		if createErr := apiClient.Create(ctx, endpoint); createErr != nil {
			syncErr = errors.Join(syncErr, fmt.Errorf("create port %q endpoint: %w", port.Name, createErr))
		}
	}
	return syncErr
}

func cleanupLogicalEndpoints(
	ctx context.Context, apiClient ctrl_client.Client, reader ctrl_client.Reader, owner commonapi.DcpModelObject,
) (bool, error) {
	var current apiv2.ServiceEndpointList
	if listErr := reader.List(ctx, &current, ctrl_client.InNamespace(owner.GetNamespace())); listErr != nil {
		return false, fmt.Errorf("list logical workload endpoints before cleanup: %w", listErr)
	}
	complete := true
	var cleanupErr error
	for endpointIndex := range current.Items {
		endpoint := &current.Items[endpointIndex]
		if !metav1.IsControlledBy(endpoint, owner) {
			continue
		}
		complete = false
		if endpoint.DeletionTimestamp.IsZero() {
			uid := endpoint.UID
			deleteErr := apiClient.Delete(ctx, endpoint, ctrl_client.Preconditions{UID: &uid})
			if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete endpoint %q: %w", endpoint.NamespacedName(), deleteErr))
			}
		}
	}
	return complete, cleanupErr
}

func containerEndpointAddresses(
	ports []apiv2.LogicalContainerPort, resolved []logicalPortData, mappings []apiv2.PhysicalContainerPortMapping,
) [][]apiv2.ServiceAddress {
	addresses := make([][]apiv2.ServiceAddress, len(resolved))
	for portIndex, port := range resolved {
		for _, mapping := range mappings {
			if mapping.ContainerPort == ports[portIndex].ContainerPort && mapping.Protocol == port.protocol {
				address := mapping.HostIP
				switch address {
				case "", networking.IPv4AllInterfaceAddress:
					address = networking.IPv4LocalhostDefaultAddress
				case networking.IPv6AllInterfaceAddress, "::":
					address = networking.IPv6LocalhostDefaultAddress
				}
				if networking.IsValidPort(int(mapping.HostPort)) {
					addresses[portIndex] = append(addresses[portIndex], apiv2.ServiceAddress{Address: networking.ToStandaloneAddress(address), Port: mapping.HostPort})
				}

			}
		}
		slices.SortFunc(addresses[portIndex], func(left, right apiv2.ServiceAddress) int {
			if left.Address < right.Address {
				return -1
			}
			if left.Address > right.Address {
				return 1
			}
			return int(left.Port - right.Port)
		})
		addresses[portIndex] = slices.Compact(addresses[portIndex])
	}
	return addresses
}

func processEndpointAddresses(ports []logicalPortData) [][]apiv2.ServiceAddress {
	addresses := make([][]apiv2.ServiceAddress, len(ports))
	for portIndex, port := range ports {
		address := port.Address
		switch address {
		case networking.IPv4AllInterfaceAddress:
			address = networking.IPv4LocalhostDefaultAddress
		case networking.IPv6AllInterfaceAddress, "::":
			address = networking.IPv6LocalhostDefaultAddress
		}
		addresses[portIndex] = []apiv2.ServiceAddress{{Address: networking.ToStandaloneAddress(address), Port: port.Port}}
	}
	return addresses
}

func (r *ProjectReconciler) prepareProjectValues(
	ctx context.Context, resource *apiv2.Project, data *projectData, log logr.Logger,
) bool {
	if data.processConfig != nil || data.containerConfig != nil || resource.Spec.Stop {
		return true
	}
	var requests []logicalPortRequest
	var arguments []string
	var environment []commonapi.EnvVar
	if resource.Spec.Process != nil {
		for _, port := range resource.Spec.Process.Ports {
			requests = append(requests, logicalPortRequest{binding: port.LogicalServiceBinding, address: port.Address, port: port.Port})
		}
		arguments = resource.Spec.Process.Args
		if resource.Spec.Process.AmbientEnvironment.EffectiveBehavior() == apiv2.EnvironmentBehaviorInherit {
			for _, entry := range os.Environ() {
				name, value, found := strings.Cut(entry, "=")
				if found {
					environment = append(environment, commonapi.EnvVar{Name: name, Value: value})
				}
			}
		}
		environment = append(environment, resource.Spec.Process.Env...)
	} else {
		for _, port := range resource.Spec.Container.Ports {
			requests = append(requests, logicalPortRequest{binding: port.LogicalServiceBinding, address: port.Address, port: port.ContainerPort})
		}
		arguments, environment = resource.Spec.Container.Command, resource.Spec.Container.Env
	}
	ports, prepareErr := prepareLogicalPorts(ctx, r.Client, r.NoCacheClient, resource, requests, data.ports, log)
	data.ports = ports
	if prepareErr != nil {
		log.Error(prepareErr, "Failed to prepare Project ports")
		data.reason, data.message = apiv2.LogicalResourceReasonPortPreparationPending, prepareErr.Error()
		if !apierrors.IsNotFound(prepareErr) {
			data.retry(apiv2.LogicalResourceReasonPortPreparationPending, prepareErr)
		}
		return false
	}
	if resource.Spec.Container != nil && resource.Spec.DebugAdapter != nil {
		adapterPort := resource.Spec.DebugAdapter.GetContainerTCPPort()
		for index, selected := range ports {
			if adapterPort != 0 && selected.protocol == commonapi.PortProtocolTCP && resource.Spec.Container.Ports[index].ContainerPort == adapterPort {
				data.state, data.reason, data.message = projectStateFailed, apiv2.ProjectReasonAdapterPortConflict, "The adapter port conflicts with a declared TCP application port."
				log.Error(fmt.Errorf("%s", data.message), "Failed to prepare Project adapter port")
				return false
			}
		}
	}
	resolvedArgs, resolvedEnv, evaluationErr := templating.EvaluateV2Values(ctx, r.NoCacheClient, resource,
		servingPorts(ports), arguments, environment, resource.Spec.Process != nil && osutil.IsWindows())
	if evaluationErr != nil {
		log.Error(evaluationErr, "Failed to evaluate Project launch values")
		data.reason, data.message = apiv2.LogicalResourceReasonTemplateEvaluationFailed, evaluationErr.Error()
		if !templating.IsTransientTemplateError(evaluationErr) {
			data.state = projectStateFailed
		}
		return false
	}
	resolvedEnv = slices.DeleteFunc(resolvedEnv, func(variable commonapi.EnvVar) bool {
		name := variable.Name
		if osutil.IsWindows() {
			name = strings.ToUpper(name)
		}
		for _, prefix := range suppressVarPrefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	})
	if resource.Spec.Process != nil {
		data.processConfig = resource.Spec.Process.LogicalProcessConfig.DeepCopy()
		data.processConfig.Args, data.processConfig.Env = resolvedArgs, resolvedEnv
		data.processConfig.AmbientEnvironment.Behavior = apiv2.EnvironmentBehaviorDoNotInherit
	} else {
		data.containerConfig = resource.Spec.Container.LogicalContainerConfig.DeepCopy()
		data.containerConfig.Command, data.containerConfig.Env = resolvedArgs, resolvedEnv
	}
	return true
}

func (r *ProjectReconciler) withdrawProjectEndpoints(
	ctx context.Context, resource *apiv2.Project, data *projectData, log logr.Logger,
) objectChange {
	// A prepared context is not an application endpoint; target publication follows DAP launch.
	endpointErr := syncLogicalEndpoints(ctx, r.Client, r.NoCacheClient, resource, data.ports, nil)
	if endpointErr != nil {
		log.Error(endpointErr, "Failed to synchronize Project service endpoints")
		data.retry(apiv2.ProjectReasonEndpointUpdateFailed, endpointErr)
		return additionalReconciliationNeeded
	}
	return noChange
}
