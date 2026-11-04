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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/internal/templating"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/osutil"
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

func logicalPortResourceName(owner ctrl_client.Object, name string) string {
	return "logical-" + string(owner.GetUID()) + "-" + name
}

func prepareLogicalPorts(
	ctx context.Context, apiClient ctrl_client.Client, reader ctrl_client.Reader, owner commonapi.DcpModelObject,
	requested []logicalPortRequest, previous []logicalPortData, log logr.Logger,
) ([]logicalPortData, error) {
	prepared := make([]logicalPortData, 0, len(requested))
	for _, request := range requested {
		service := &apiv2.LogicalService{}
		serviceName := commonapi.AsNamespacedName(request.binding.ServiceRef, owner.GetNamespace())
		if request.binding.Service != nil {
			serviceName.Name = owner.GetName() + "-" + request.binding.Service.Name
			service.ObjectMeta = metav1.ObjectMeta{Name: serviceName.Name, Namespace: owner.GetNamespace()}
			service.Spec = request.binding.Service.LogicalServiceSpec
			createErr := apiClient.Create(ctx, service)
			if createErr != nil {
				if getErr := reader.Get(ctx, serviceName, service); getErr != nil {
					return prepared, fmt.Errorf("create port %q service and verify outcome: %w", request.binding.Name, errors.Join(createErr, getErr))
				}
			}
			if !service.Spec.Equal(request.binding.Service.LogicalServiceSpec) {
				return prepared, fmt.Errorf("service %q configuration does not match the template", serviceName)
			}
		} else if getErr := reader.Get(ctx, serviceName, service); getErr != nil {
			return prepared, fmt.Errorf("get port %q service: %w", request.binding.Name, getErr)
		}
		if !service.DeletionTimestamp.IsZero() {
			return prepared, fmt.Errorf("port %q service is terminating", request.binding.Name)
		}
		port := logicalPortData{ServingPort: templating.ServingPort{Name: request.binding.Name, Address: request.address, Port: request.port},
			serviceRef: serviceName.Name, protocol: service.Spec.EffectiveProtocol()}
		if port.Address == "" {
			port.Address = networking.Localhost
		}
		if port.Port == 0 {
			for _, allocated := range previous {
				if allocated.Name == port.Name {
					port.Port = allocated.Port
					break
				}
			}
			if port.Port == 0 {
				allocatedPort, allocationErr := networking.GetFreePort(port.protocol, port.Address, log)
				if allocationErr != nil {
					return prepared, fmt.Errorf("allocate logical port %q: %w", port.Name, allocationErr)
				}
				port.Port = allocatedPort
			}
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
	for portIndex, port := range ports {
		endpoint := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: logicalPortResourceName(owner, port.Name), Namespace: owner.GetNamespace()}}
		getErr := reader.Get(ctx, endpoint.NamespacedName(), endpoint)
		if getErr != nil && !apierrors.IsNotFound(getErr) {
			return fmt.Errorf("get port %q endpoint: %w", port.Name, getErr)
		}
		if getErr == nil && !metav1.IsControlledBy(endpoint, owner) {
			return fmt.Errorf("port %q endpoint has a different owner", port.Name)
		}
		if portIndex >= len(addresses) || len(addresses[portIndex]) == 0 {
			if getErr == nil {
				uid := endpoint.UID
				deleteErr := apiClient.Delete(ctx, endpoint, ctrl_client.Preconditions{UID: &uid})
				if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
					return fmt.Errorf("delete port %q endpoint: %w", port.Name, deleteErr)
				}
			}
			continue
		}
		desired := apiv2.ServiceEndpointSpec{ServiceRef: port.serviceRef, Addresses: addresses[portIndex]}
		if apierrors.IsNotFound(getErr) {
			endpoint.Spec = desired
			if ownerErr := ctrl.SetControllerReference(owner, endpoint, apiClient.Scheme()); ownerErr != nil {
				return fmt.Errorf("set port %q endpoint owner: %w", port.Name, ownerErr)
			}
			if createErr := apiClient.Create(ctx, endpoint); createErr != nil {
				return fmt.Errorf("create port %q endpoint: %w", port.Name, createErr)
			}
		} else if !reflect.DeepEqual(endpoint.Spec, desired) {
			endpoint.Spec = desired
			if updateErr := apiClient.Update(ctx, endpoint); updateErr != nil {
				return fmt.Errorf("update port %q endpoint: %w", port.Name, updateErr)
			}
		}
	}
	return nil
}

func cleanupLogicalEndpoints(
	ctx context.Context, apiClient ctrl_client.Client, reader ctrl_client.Reader, owner commonapi.DcpModelObject,
	bindings []apiv2.LogicalServiceBinding,
) (bool, error) {
	complete := true
	for _, binding := range bindings {
		name := logicalPortResourceName(owner, binding.Name)
		endpoint := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: owner.GetNamespace()}}
		getErr := reader.Get(ctx, endpoint.NamespacedName(), endpoint)
		if apierrors.IsNotFound(getErr) {
			continue
		}
		if getErr != nil {
			return false, fmt.Errorf("get port %q endpoint: %w", binding.Name, getErr)
		}
		if !metav1.IsControlledBy(endpoint, owner) {
			continue
		}
		complete = false
		if endpoint.DeletionTimestamp.IsZero() {
			uid := endpoint.UID
			deleteErr := apiClient.Delete(ctx, endpoint, ctrl_client.Preconditions{UID: &uid})
			if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
				return false, fmt.Errorf("delete port %q endpoint: %w", binding.Name, deleteErr)
			}
		}
	}
	return complete, nil
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

func (r *LogicalContainerReconciler) prepareLogicalContainerValues(
	ctx context.Context, container *apiv2.LogicalContainer, data *logicalContainerData, log logr.Logger,
) bool {
	if data.runConfig != nil || container.Spec.Stop {
		return true
	}
	requests := make([]logicalPortRequest, len(container.Spec.Ports))
	for portIndex, port := range container.Spec.Ports {
		requests[portIndex] = logicalPortRequest{binding: port.LogicalServiceBinding, address: port.Address, port: port.ContainerPort}
	}
	ports, prepareErr := prepareLogicalPorts(ctx, r.Client, r.NoCacheClient, container, requests, data.ports, log)
	data.ports = ports
	if prepareErr != nil {
		log.Error(prepareErr, "Failed to prepare logical container ports")
		data.reason, data.message = "PortPreparationPending", prepareErr.Error()
		if !apierrors.IsNotFound(prepareErr) {
			data.reconcileErr = prepareErr
		}
		return false
	}
	config := container.Spec.Container.DeepCopy()
	if config != nil {
		arguments, environment, evaluationErr := templating.EvaluateV2Values(ctx, r.NoCacheClient, container, servingPorts(ports), config.Command, config.Env)
		if evaluationErr != nil {
			log.Error(evaluationErr, "Failed to evaluate logical container values")
			data.reason, data.message = "TemplateEvaluationFailed", evaluationErr.Error()
			if !templating.IsTransientTemplateError(evaluationErr) {
				data.state = logicalContainerStatePreparationFailed
			}
			return false
		}
		config.Command, config.Env = arguments, environment
	}
	data.runConfig = config
	return true
}

func (r *LogicalProcessReconciler) prepareLogicalProcessValues(
	ctx context.Context, resource *apiv2.LogicalProcess, data *logicalProcessData, log logr.Logger,
) bool {
	if data.runConfig != nil || resource.Spec.Stop {
		return true
	}
	requests := make([]logicalPortRequest, len(resource.Spec.Ports))
	for portIndex, port := range resource.Spec.Ports {
		requests[portIndex] = logicalPortRequest{binding: port.LogicalServiceBinding, address: port.Address, port: port.Port}
	}
	ports, prepareErr := prepareLogicalPorts(ctx, r.Client, r.NoCacheClient, resource, requests, data.ports, log)
	data.ports = ports
	if prepareErr != nil {
		log.Error(prepareErr, "Failed to prepare logical process ports")
		data.reason, data.message = "PortPreparationPending", prepareErr.Error()
		if !apierrors.IsNotFound(prepareErr) {
			data.reconcileErr = prepareErr
		}
		return false
	}
	config := resource.Spec.Process.DeepCopy()
	if config != nil {
		arguments, environment, evaluationErr := templating.EvaluateV2Values(ctx, r.NoCacheClient, resource, servingPorts(ports), config.Args, config.Env)
		if evaluationErr != nil {
			log.Error(evaluationErr, "Failed to evaluate logical process values")
			data.reason, data.message = "TemplateEvaluationFailed", evaluationErr.Error()
			if !templating.IsTransientTemplateError(evaluationErr) {
				data.state = logicalProcessStatePrepareFailed
			}
			return false
		}
		config.Args, config.Env = arguments, environment
	}
	data.runConfig = config
	return true
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
		data.reason, data.message = "PortPreparationPending", prepareErr.Error()
		if !apierrors.IsNotFound(prepareErr) {
			data.reconcileErr = prepareErr
		}
		return false
	}
	resolvedArgs, resolvedEnv, evaluationErr := templating.EvaluateV2Values(ctx, r.NoCacheClient, resource,
		servingPorts(ports), arguments, environment)
	if evaluationErr != nil {
		log.Error(evaluationErr, "Failed to evaluate Project launch values")
		data.reason, data.message = "TemplateEvaluationFailed", evaluationErr.Error()
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

func projectPortBindings(resource *apiv2.Project) []apiv2.LogicalServiceBinding {
	var bindings []apiv2.LogicalServiceBinding
	if resource.Spec.Process != nil {
		for _, port := range resource.Spec.Process.Ports {
			bindings = append(bindings, port.LogicalServiceBinding)
		}
	} else if resource.Spec.Container != nil {
		for _, port := range resource.Spec.Container.Ports {
			bindings = append(bindings, port.LogicalServiceBinding)
		}
	}
	return bindings
}

func (r *ProjectReconciler) withdrawProjectEndpoints(
	ctx context.Context, resource *apiv2.Project, data *projectData, log logr.Logger,
) objectChange {
	// A prepared context is not an application endpoint; target publication follows DAP launch.
	endpointErr := syncLogicalEndpoints(ctx, r.Client, r.NoCacheClient, resource, data.ports, nil)
	if endpointErr != nil {
		log.Error(endpointErr, "Failed to synchronize Project service endpoints")
		data.retry("EndpointUpdateFailed", endpointErr)
		return additionalReconciliationNeeded
	}
	return noChange
}
