// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
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

type ServiceWorkloadEndpointKey struct {
	NamespacedNameWithKind
	ServiceName string
}

type ContainerReconcilerOrExecutableReconciler interface {
	ctrl_client.Client
	getWorkloadEndpointCache() *syncmap.Map[ServiceWorkloadEndpointKey, bool]
}

func ensureEndpointsForWorkload(r ContainerReconcilerOrExecutableReconciler, ctx context.Context, owner ctrl_client.Object, log logr.Logger) {
	var serviceProducers []ServiceProducer
	var err error
	annotations := owner.GetAnnotations()

	spa, found := annotations[serviceProducerAnnotation]
	if !found {
		log.V(1).Info("no service-producer annotation found on Container/Executable object")
		return
	}

	serviceProducers, err = parseServiceProducerAnnotation(spa)
	if err != nil {
		log.Error(err, serviceProducerIsInvalid)
		return
	}

	var childEndpoints apiv1.EndpointList
	if err := r.List(ctx, &childEndpoints, ctrl_client.InNamespace(owner.GetNamespace()), ctrl_client.MatchingFields{workloadOwnerKey: string(owner.GetUID())}); err != nil {
		log.Error(err, "failed to list child Endpoint objects")
	}

	for _, serviceProducer := range serviceProducers {
		// Check if we have already created an Endpoint for this workload.
		sweKey := ServiceWorkloadEndpointKey{
			NamespacedNameWithKind: GetNamespacedNameWithKind(owner),
			ServiceName:            serviceProducer.ServiceName,
		}
		hasEndpoint := slices.Any(childEndpoints.Items, func(e apiv1.Endpoint) bool {
			return e.Spec.ServiceName == serviceProducer.ServiceName
		})

		if hasEndpoint {
			log.V(1).Info("Endpoint already exists for this workload and service combination", "ServiceName", serviceProducer.ServiceName)

			// Client has caught up and has the info about the service endpoint workload, we can clear the local cache.
			r.getWorkloadEndpointCache().Delete(sweKey)

			continue
		}
		_, found := r.getWorkloadEndpointCache().Load(sweKey)
		if found {
			log.V(1).Info("Endpoint was just created for this workload and service combination", "ServiceName", serviceProducer.ServiceName)
			continue
		}

		var endpoint *apiv1.Endpoint
		switch sweKey.Kind {
		case "Container":
			endpoint, err = createEndpointForContainer(owner.(*apiv1.Container), serviceProducer, log)
		case "Executable":
			endpoint, err = createEndpointForExecutable(owner.(*apiv1.Executable), serviceProducer, log)
		default:
			panic(fmt.Errorf("don't know how to create endpont for kind: '%s'", sweKey.Kind))
		}

		if err != nil {
			log.Error(err, "could not create Endpoint object")
			continue
		}

		if err := ctrl.SetControllerReference(owner, endpoint, r.Scheme()); err != nil {
			log.Error(err, "failed to set owner for endpoint")
		}

		if err := r.Create(ctx, endpoint); err != nil {
			log.Error(err, "could not persist Endpoint object")
		}

		r.getWorkloadEndpointCache().Store(sweKey, true)
	}
}

func removeEndpointsForWorkload(r ContainerReconcilerOrExecutableReconciler, ctx context.Context, owner ctrl_client.Object, log logr.Logger) {
	var childEndpoints apiv1.EndpointList
	if err := r.List(ctx, &childEndpoints, ctrl_client.InNamespace(owner.GetNamespace()), ctrl_client.MatchingFields{workloadOwnerKey: string(owner.GetUID())}); err != nil {
		log.Error(err, "failed to list child Endpoint objects")
	}

	for _, endpoint := range childEndpoints.Items {
		if err := r.Delete(ctx, &endpoint); err != nil {
			log.Error(err, "could not delete Endpoint object")
		}

		sweKey := ServiceWorkloadEndpointKey{
			NamespacedNameWithKind: GetNamespacedNameWithKind(owner),
			ServiceName:            endpoint.Spec.ServiceName,
		}

		r.getWorkloadEndpointCache().Delete(sweKey)
	}
}

func parseServiceProducerAnnotation(annotation string) ([]ServiceProducer, error) {
	retval := make([]ServiceProducer, 0)

	// TODO: parse multiple entries when possible
	sp := ServiceProducer{}
	err := json.Unmarshal([]byte(annotation), &sp)
	retval = append(retval, sp)

	return retval, err
}

const serviceProducerIsInvalid = "service-producer annotation is invalid"

type ServiceProducer struct {
	// Name of the service that the workload implements.
	ServiceName string `json:"serviceName"`

	// Address used by the workload to serve the service.
	// In the current implementation it only applies to Executables and defaults to localhost if not present.
	// (Containers use the address specified by their Spec).
	Address string `json:"address,omitempty"`

	// Port used by the workload to serve the service. Mandatory.
	// For Containers it must match one of the Container ports.
	// We first match on HostPort, and if one is found, we use that port.
	// If no HostPort is found, we match on ContainerPort for ports that do not specify a HostPort
	// (the port is auto-allocated by Docker). If such port is found, we proxy to the auto-allocated host port.
	Port int32 `json:"port"`
}
