/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// LogicalServiceBinding references a service or ensures that a matching session service exists.
// Exactly one of serviceRef or service must be set.
// +k8s:openapi-gen=true
type LogicalServiceBinding struct {
	// Name identifies this logical port within the workload and in serving templates.
	Name string `json:"name"`

	// ServiceRef identifies a LogicalService using <name> or <namespace>/<name>.
	// Cross-namespace references are not supported. The referenced service is not owned by the workload.
	ServiceRef string `json:"serviceRef,omitempty"`

	// Service creates a named session-mode LogicalService if absent, or references it if its configuration matches.
	// The service has an independent lifetime and is not deleted with the workload.
	Service *LogicalServiceTemplate `json:"service,omitempty"`
}

// LogicalServiceTemplate describes a named session service to create if absent or reuse when its configuration matches.
// +k8s:openapi-gen=true
type LogicalServiceTemplate struct {
	LogicalServiceConfig `json:",inline"`

	// Name forms the service resource name as <parent-name>-<name>.
	Name string `json:"name"`
}

func (binding LogicalServiceBinding) validate(namespace string, parentName string, bindingPath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}
	for _, validationMessage := range validation.IsDNS1123Label(binding.Name) {
		errorList = append(errorList, field.Invalid(bindingPath.Child("name"), binding.Name, validationMessage))
	}
	if binding.ServiceRef == "" && binding.Service == nil {
		errorList = append(errorList, field.Required(bindingPath, "exactly one of serviceRef or service must be set"))
	}
	if binding.ServiceRef != "" && binding.Service != nil {
		errorList = append(errorList, field.Forbidden(bindingPath.Child("service"), "service cannot be set when serviceRef is set"))
	}
	if binding.ServiceRef != "" {
		errorList = append(errorList, validateSameNamespaceResourceReference(binding.ServiceRef, namespace, bindingPath.Child("serviceRef"))...)
	}
	if binding.Service != nil {
		for _, validationMessage := range validation.IsDNS1123Label(binding.Service.Name) {
			errorList = append(errorList, field.Invalid(bindingPath.Child("service", "name"), binding.Service.Name, validationMessage))
		}
		for _, validationMessage := range validation.IsDNS1123Subdomain(parentName + "-" + binding.Service.Name) {
			errorList = append(errorList, field.Invalid(bindingPath.Child("service", "name"), binding.Service.Name, "generated service name: "+validationMessage))
		}
		errorList = append(errorList, binding.Service.Validate(bindingPath.Child("service"))...)
	}
	return errorList
}

// LogicalContainerPort publishes one container port for a service-backed endpoint.
// The referenced or templated service determines the transport protocol.
// +k8s:openapi-gen=true
type LogicalContainerPort struct {
	LogicalServiceBinding `json:",inline"`

	// ContainerPort is the container port to publish.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	ContainerPort int32 `json:"containerPort"`

	// HostPort is the backend's host port. Zero selects an available port.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	HostPort int32 `json:"hostPort,omitempty"`

	// HostIP is the host address on which the container runtime publishes the backend port.
	HostIP string `json:"hostIP,omitempty"`

	// Address is the listening address inside the container, defaulting to localhost.
	Address string `json:"address,omitempty"`
}

// ToContainerPort produces the physical port configuration using the selected service's protocol.
func (port LogicalContainerPort) ToContainerPort(protocol commonapi.PortProtocol) ContainerPort {
	return ContainerPort{
		ContainerPort: port.ContainerPort, HostPort: port.HostPort, HostIP: port.HostIP, Protocol: protocol,
	}
}

func validateLogicalContainerPorts(ports []LogicalContainerPort, namespace string, parentName string, portsPath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}
	physicalPorts := make([]ContainerPort, len(ports))
	names := make(map[string]struct{}, len(ports))
	serviceNames := make(map[string]struct{}, len(ports))
	for portIndex, port := range ports {
		physicalPorts[portIndex] = port.ToContainerPort("")
		errorList = append(errorList, port.LogicalServiceBinding.validate(namespace, parentName, portsPath.Index(portIndex))...)
		if port.Service != nil {
			if _, found := serviceNames[port.Service.Name]; found {
				errorList = append(errorList, field.Duplicate(portsPath.Index(portIndex).Child("service", "name"), port.Service.Name))
			}
			serviceNames[port.Service.Name] = struct{}{}
		}
		if _, found := names[port.Name]; found {
			errorList = append(errorList, field.Duplicate(portsPath.Index(portIndex).Child("name"), port.Name))
		}
		names[port.Name] = struct{}{}
	}
	return append(errorList, ValidateContainerPorts(physicalPorts, portsPath)...)
}

// LogicalProcessPort registers one listening process port as a service-backed endpoint.
// The referenced or templated service determines the transport protocol.
// +k8s:openapi-gen=true
type LogicalProcessPort struct {
	LogicalServiceBinding `json:",inline"`

	// Address is the process's listening IP address or hostname, defaulting to localhost.
	Address string `json:"address,omitempty"`

	// Port is the port on which the process listens. Zero allocates a port for portForServing templates
	// in session and persistent modes; existing mode requires an explicit nonzero port.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

func validateLogicalProcessPorts(ports []LogicalProcessPort, namespace string, parentName string, portsPath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}
	names := make(map[string]struct{}, len(ports))
	serviceNames := make(map[string]struct{}, len(ports))
	for portIndex, port := range ports {
		portPath := portsPath.Index(portIndex)
		errorList = append(errorList, port.LogicalServiceBinding.validate(namespace, parentName, portPath)...)
		if port.Service != nil {
			if _, found := serviceNames[port.Service.Name]; found {
				errorList = append(errorList, field.Duplicate(portPath.Child("service", "name"), port.Service.Name))
			}
			serviceNames[port.Service.Name] = struct{}{}
		}
		if port.Port < 0 || port.Port > 65535 {
			errorList = append(errorList, field.Invalid(portPath.Child("port"), port.Port, "port must be between 0 and 65535"))
		}
		if _, found := names[port.Name]; found {
			errorList = append(errorList, field.Duplicate(portPath.Child("name"), port.Name))
		}
		names[port.Name] = struct{}{}
	}
	return errorList
}

// LogicalPortStatus describes a logical port's allocated serving address and selected service.
// +k8s:openapi-gen=true
type LogicalPortStatus struct {
	// Name is the logical port name.
	Name string `json:"name"`
	// ServiceRef identifies the same-namespace LogicalService selected for the port.
	ServiceRef string `json:"serviceRef"`
	// Address is the workload's listening address, not the service's consumer address.
	Address string `json:"address"`
	// Port is the workload's listening port, not the service's consumer port.
	Port int32 `json:"port"`
}
