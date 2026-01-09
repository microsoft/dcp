/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commonapi

import (
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ServiceProducerAnnotation = "service-producer"
)

// DynamicServiceProducer is an object for which the set of produced services can change at run time.
type DynamicServiceProducer interface {
	ServicesProduced() []ServiceProducer
}

func GetServiceProducersForObject(owner DcpModelObject, log logr.Logger) ([]ServiceProducer, error) {
	dsp, isDsp := owner.(DynamicServiceProducer)
	if isDsp {
		return dsp.ServicesProduced(), nil
	}

	annotations := owner.GetAnnotations()

	spa, found := annotations[ServiceProducerAnnotation]
	if !found {
		return nil, nil
	}

	retval := make([]ServiceProducer, 0)
	err := json.Unmarshal([]byte(spa), &retval)

	if err != nil {
		log.Error(err, fmt.Sprintf("%s: the annotation could not be parsed", serviceProducerIsInvalid),
			"AnnotationText", spa,
			"Workload", owner.NamespacedName().String(),
		)
		return nil, err
	}

	for i := range retval {
		sp := retval[i]
		sp.InferServiceNamespace(owner)
		retval[i] = sp
	}

	return retval, nil
}

const serviceProducerIsInvalid = "service-producer annotation is invalid"

type ServiceProducer struct {
	// Name of the service that the workload implements.
	ServiceName string `json:"serviceName"`

	// Namespace of the service that the workload implements.
	// It is optional and defaults to the namespace of the workload.
	ServiceNamespace string `json:"serviceNamespace,omitempty"`

	// Address that should be used (listened on) by the workload to serve the service.
	// It defaults to localhost if not present.
	// For Containers this is the address that the workload should listen on INSIDE the container.
	// This is NOT the address that the Container will be available on the host network;
	// that address is part of the Container spec, specifically it is the HostIP property of the ContainerPort definition(s).
	Address string `json:"address,omitempty"`

	// Port used by the workload to serve the service.
	//
	// For Containers it is mandatory and must match one of the Container ports.
	// We first match on HostPort, and if one is found, we use that port.
	// If no HostPort is found, we match on ContainerPort.
	// If such port is found, we proxy to the corresponding host port.
	//
	// For Executables it is required UNLESS the Executable also expect the port to be injected into it
	// via environment variable and {{- portForServing "<service-name>" -}} template function.
	Port int32 `json:"port,omitempty"`
}

func (sp *ServiceProducer) InferServiceNamespace(owner ctrl_client.Object) {
	if sp.ServiceNamespace != "" {
		return
	}

	nn := AsNamespacedName(sp.ServiceName, owner.GetNamespace())
	sp.ServiceNamespace = nn.Namespace
	sp.ServiceName = nn.Name
}

func (sp *ServiceProducer) ServiceNamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Namespace: sp.ServiceNamespace,
		Name:      sp.ServiceName,
	}
}
