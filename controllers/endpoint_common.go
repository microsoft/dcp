// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

var workloadEndpointCache *syncmap.Map[ServiceWorkloadEndpointKey, bool] = &syncmap.Map[ServiceWorkloadEndpointKey, bool]{}

type ServiceWorkloadEndpointKey struct {
	commonapi.NamespacedNameWithKind
	ServiceName string
}

type EndpointOwner[EndpointCreationContext any] interface {
	ctrl_client.Client

	// Creates a new Endpoint object for the given Service exposed by the workload object (owner parameter).
	// The Service is represented by the serviceProducer parameter.
	createEndpoint(ctx context.Context, owner ctrl_client.Object, serviceProducer commonapi.ServiceProducer, ecc EndpointCreationContext, log logr.Logger) (*apiv1.Endpoint, error)

	// Validates that the existing Endpoint object still correctly represents the Service exposed by the workload object (owner parameter).
	// If an error is returned, the Endpoint will be deleted and a new one will be created
	// (via call to createEndpoint function).
	validateExistingEndpoint(ctx context.Context, owner ctrl_client.Object, serviceProducer commonapi.ServiceProducer, ecc EndpointCreationContext, endpoint *apiv1.Endpoint, log logr.Logger) error
}

func SetupEndpointIndexWithManager(mgr ctrl.Manager) error {
	return mgr.GetFieldIndexer().IndexField(context.Background(), &apiv1.Endpoint{}, commonapi.WorkloadOwnerKey, func(rawObj ctrl_client.Object) []string {
		endpoint := rawObj.(*apiv1.Endpoint)
		return slices.Map[metav1.OwnerReference, string](endpoint.OwnerReferences, func(ref metav1.OwnerReference) string {
			return string(ref.UID)
		})
	})
}

func ensureEndpointsForWorkload[EndpointCreationContext any](
	ctx context.Context,
	r EndpointOwner[EndpointCreationContext],
	owner commonapi.DcpModelObject,
	reservedServicePorts map[types.NamespacedName]int32,
	ecc EndpointCreationContext,
	log logr.Logger,
) {
	serviceProducers, err := commonapi.GetServiceProducersForObject(owner, log)
	if err != nil || len(serviceProducers) == 0 {
		return
	}

	var childEndpoints apiv1.EndpointList
	if err = r.List(ctx, &childEndpoints, ctrl_client.InNamespace(owner.GetNamespace()), ctrl_client.MatchingFields{commonapi.WorkloadOwnerKey: string(owner.GetUID())}); err != nil {
		log.Error(err, "Failed to list child Endpoint objects", "Workload", owner.NamespacedName().String())
	}

	for _, serviceProducer := range serviceProducers {
		// Check if we have already created an Endpoint for this workload.
		sweKey := ServiceWorkloadEndpointKey{
			NamespacedNameWithKind: commonapi.GetNamespacedNameWithKind(owner),
			ServiceName:            serviceProducer.ServiceName,
		}
		existingEndpoints := slices.Select(childEndpoints.Items, func(e apiv1.Endpoint) bool {
			return e.Spec.ServiceName == serviceProducer.ServiceName && e.Spec.ServiceNamespace == serviceProducer.ServiceNamespace
		})
		// Initially assume that all existing endpoints are valid.
		allExistingEndpointsAreValid := len(existingEndpoints) > 0

		for _, endpoint := range existingEndpoints {
			err = r.validateExistingEndpoint(ctx, owner, serviceProducer, ecc, &endpoint, log)
			if err != nil {
				allExistingEndpointsAreValid = false
				break
			}
		}

		if allExistingEndpointsAreValid {
			log.V(1).Info("Endpoint(s) already exists for this workload and service combination",
				"ServiceName", serviceProducer.ServiceName,
				"Workload", owner.NamespacedName().String(),
			)

			// Client has caught up and has the info about the service endpoint workload, we can clear the local cache.
			workloadEndpointCache.Delete(sweKey)

			continue
		} else if len(existingEndpoints) > 0 {
			// Delete all existing endpoints and force re-creation of a new one.
			workloadEndpointCache.Delete(sweKey)

			for _, endpoint := range existingEndpoints {
				deleteErr := r.Delete(ctx, &endpoint, ctrl_client.PropagationPolicy(metav1.DeletePropagationBackground))
				if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
					log.Error(deleteErr, "Could not delete Endpoint object",
						"Endpoint", endpoint.NamespacedName().String(),
						"Workload", owner.NamespacedName().String(),
					)
				}
			}
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
		endpoint, err = r.createEndpoint(ctx, owner, serviceProducer, ecc, log)

		if err != nil {
			log.Error(err, "Could not create Endpoint object",
				"ServiceName", serviceProducer.ServiceName,
				"Workload", owner.NamespacedName().String(),
			)
			continue
		}

		if err = ctrl.SetControllerReference(owner, endpoint, r.Scheme()); err != nil {
			log.Error(err, "Failed to set owner for endpoint",
				"ServiceName", serviceProducer.ServiceName,
				"Workload", owner.NamespacedName().String(),
			)
		}

		if err = r.Create(ctx, endpoint); err != nil {
			// Do not log an error if resource cleanup has started.
			if !apiv1.ResourceCreationProhibited.Load() {
				log.Error(err, "Could not persist Endpoint object",
					"ServiceName", serviceProducer.ServiceName,
					"Workload", owner.NamespacedName().String(),
				)
			}
		}

		log.V(1).Info("New Endpoint created", "Endpoint", endpoint, "ServiceName", serviceProducer.ServiceName)

		workloadEndpointCache.Store(sweKey, true)
	}
}

func removeEndpointsForWorkload(r ctrl_client.Client, ctx context.Context, owner commonapi.DcpModelObject, log logr.Logger) {
	var childEndpoints apiv1.EndpointList
	if err := r.List(ctx, &childEndpoints, ctrl_client.InNamespace(owner.GetNamespace()), ctrl_client.MatchingFields{commonapi.WorkloadOwnerKey: string(owner.GetUID())}); err != nil {
		log.Error(err, "Failed to list child Endpoint objects", "Workload", owner.NamespacedName().String())
	}

	for _, endpoint := range childEndpoints.Items {
		err := r.Delete(ctx, &endpoint, ctrl_client.PropagationPolicy(metav1.DeletePropagationBackground))
		if err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "Could not delete Endpoint object",
				"Endpoint", endpoint.NamespacedName().String(),
				"Workload", owner.NamespacedName().String(),
			)
		}

		sweKey := ServiceWorkloadEndpointKey{
			NamespacedNameWithKind: commonapi.GetNamespacedNameWithKind(owner),
			ServiceName:            endpoint.Spec.ServiceName,
		}

		workloadEndpointCache.Delete(sweKey)
	}
}
