// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

const (
	serviceProducerAnnotation = "service-producer"
	workloadOwnerKey          = ".metadata.owner"
)

var workloadEndpointCache *syncmap.Map[ServiceWorkloadEndpointKey, bool] = &syncmap.Map[ServiceWorkloadEndpointKey, bool]{}

type ServiceWorkloadEndpointKey struct {
	apiv1.NamespacedNameWithKind
	ServiceName string
}

type EndpointOwner interface {
	ctrl_client.Client
	createEndpoint(ctx context.Context, owner ctrl_client.Object, serviceProducer ServiceProducer, log logr.Logger) (*apiv1.Endpoint, error)
}

func SetupEndpointIndexWithManager(mgr ctrl.Manager) error {
	return mgr.GetFieldIndexer().IndexField(context.Background(), &apiv1.Endpoint{}, workloadOwnerKey, func(rawObj ctrl_client.Object) []string {
		endpoint := rawObj.(*apiv1.Endpoint)
		return slices.Map[metav1.OwnerReference, string](endpoint.OwnerReferences, func(ref metav1.OwnerReference) string {
			return string(ref.UID)
		})
	})
}

func ensureEndpointsForWorkload(ctx context.Context, r EndpointOwner, owner dcpModelObject, reservedServicePorts map[types.NamespacedName]int32, log logr.Logger) {
	serviceProducers, err := getServiceProducersForObject(owner, log)
	if err != nil || len(serviceProducers) == 0 {
		return
	}

	var childEndpoints apiv1.EndpointList
	if err = r.List(ctx, &childEndpoints, ctrl_client.InNamespace(owner.GetNamespace()), ctrl_client.MatchingFields{workloadOwnerKey: string(owner.GetUID())}); err != nil {
		log.Error(err, "failed to list child Endpoint objects", "Workload", owner.NamespacedName().String())
	}

	for _, serviceProducer := range serviceProducers {
		// Check if we have already created an Endpoint for this workload.
		sweKey := ServiceWorkloadEndpointKey{
			NamespacedNameWithKind: apiv1.GetNamespacedNameWithKind(owner),
			ServiceName:            serviceProducer.ServiceName,
		}
		hasEndpoint := slices.Any(childEndpoints.Items, func(e apiv1.Endpoint) bool {
			return e.Spec.ServiceName == serviceProducer.ServiceName && e.Spec.ServiceNamespace == serviceProducer.ServiceNamespace
		})

		if hasEndpoint {
			log.V(1).Info("Endpoint already exists for this workload and service combination",
				"ServiceName", serviceProducer.ServiceName,
				"Workload", owner.NamespacedName().String(),
			)

			// Client has caught up and has the info about the service endpoint workload, we can clear the local cache.
			workloadEndpointCache.Delete(sweKey)

			continue
		}
		_, found := workloadEndpointCache.Load(sweKey)
		if found {
			log.V(1).Info("Endpoint was just created for this workload and service combination",
				"ServiceName", serviceProducer.ServiceName,
				"Workload", owner.NamespacedName().String(),
			)
			continue
		}

		if reservedServicePorts != nil {
			if port, portFound := reservedServicePorts[serviceProducer.ServiceNamespacedName()]; portFound {
				serviceProducer.Port = port
			}
		}
		var endpoint *apiv1.Endpoint
		endpoint, err = r.createEndpoint(ctx, owner, serviceProducer, log)

		if err != nil {
			log.Error(err, "could not create Endpoint object",
				"ServiceName", serviceProducer.ServiceName,
				"Workload", owner.NamespacedName().String(),
			)
			continue
		}

		if err = ctrl.SetControllerReference(owner, endpoint, r.Scheme()); err != nil {
			log.Error(err, "failed to set owner for endpoint",
				"ServiceName", serviceProducer.ServiceName,
				"Workload", owner.NamespacedName().String(),
			)
		}

		if err = r.Create(ctx, endpoint); err != nil {
			log.Error(err, "could not persist Endpoint object",
				"ServiceName", serviceProducer.ServiceName,
				"Workload", owner.NamespacedName().String(),
			)
		}

		log.V(1).Info("New Endpoint created", "Endpoint", endpoint, "ServiceName", serviceProducer.ServiceName)

		workloadEndpointCache.Store(sweKey, true)
	}
}

func removeEndpointsForWorkload(r EndpointOwner, ctx context.Context, owner dcpModelObject, log logr.Logger) {
	var childEndpoints apiv1.EndpointList
	if err := r.List(ctx, &childEndpoints, ctrl_client.InNamespace(owner.GetNamespace()), ctrl_client.MatchingFields{workloadOwnerKey: string(owner.GetUID())}); err != nil {
		log.Error(err, "failed to list child Endpoint objects", "Workload", owner.NamespacedName().String())
	}

	for _, endpoint := range childEndpoints.Items {
		err := r.Delete(ctx, &endpoint, ctrl_client.PropagationPolicy(metav1.DeletePropagationBackground))
		if err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "could not delete Endpoint object",
				"Endpoint", endpoint.NamespacedName().String(),
				"Workload", owner.NamespacedName().String(),
			)
		}

		sweKey := ServiceWorkloadEndpointKey{
			NamespacedNameWithKind: apiv1.GetNamespacedNameWithKind(owner),
			ServiceName:            endpoint.Spec.ServiceName,
		}

		workloadEndpointCache.Delete(sweKey)
	}
}

func getServiceProducersForObject(owner dcpModelObject, log logr.Logger) ([]ServiceProducer, error) {
	annotations := owner.GetAnnotations()

	spa, found := annotations[serviceProducerAnnotation]
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

	nn := asNamespacedName(sp.ServiceName, owner.GetNamespace())
	sp.ServiceNamespace = nn.Namespace
	sp.ServiceName = nn.Name
}

func (sp *ServiceProducer) ServiceNamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Namespace: sp.ServiceNamespace,
		Name:      sp.ServiceName,
	}
}

func asNamespacedName(maybeNamespacedName, defaultNamespace string) types.NamespacedName {
	if !strings.Contains(maybeNamespacedName, string(types.Separator)) {
		return types.NamespacedName{Namespace: defaultNamespace, Name: maybeNamespacedName}
	}

	parts := strings.SplitN(maybeNamespacedName, string(types.Separator), 2)
	return types.NamespacedName{
		Namespace: parts[0],
		Name:      parts[1],
	}
}
