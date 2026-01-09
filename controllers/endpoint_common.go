/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/


package controllers

import (
	"context"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/slices"
	"github.com/microsoft/dcp/pkg/syncmap"
)

// The cache of endpoints created for a given workload and service combination.
var workloadEndpointCache *syncmap.Map[ServiceWorkloadEndpointKey, []apiv1.Endpoint] = &syncmap.Map[ServiceWorkloadEndpointKey, []apiv1.Endpoint]{}

type ServiceWorkloadEndpointKey struct {
	commonapi.NamespacedNameWithKind
	ServiceName string
}

type EndpointOwner[EndpointCreationContext any] interface {
	ctrl_client.Client

	// Creates (but does not persist) new Endpoint objects for the given Service
	// exposed by the workload object (owner parameter). The Service is represented by the serviceProducer parameter.
	// Additional context data needed for Endpoint creation is passed in the ecc parameter.
	// Existing Endpoints for the same workload and service combination are passed via the existingEndpoints parameter.
	// The function should ONLY create new Endpoints that are "missing", i.e. the existing Endpoint is not sufficient.
	createEndpoints(
		ctx context.Context,
		owner ctrl_client.Object,
		serviceProducer commonapi.ServiceProducer,
		existingEndpoints []*apiv1.Endpoint,
		ecc EndpointCreationContext,
		log logr.Logger,
	) ([]*apiv1.Endpoint, error)

	// Validates that the existing Endpoint objects correctly represent the Service exposed by the workload object.
	// Returns two lists: the first list contains valid existing Endpoints that can be kept as-is,
	// the second list contains invalid existing Endpoints that should be deleted.
	validateExistingEndpoints(
		ctx context.Context,
		owner ctrl_client.Object,
		serviceProducer commonapi.ServiceProducer,
		endpoints []*apiv1.Endpoint,
		ecc EndpointCreationContext,
		log logr.Logger,
	) ([]*apiv1.Endpoint, []*apiv1.Endpoint, error)
}

func SetupEndpointIndexWithManager(mgr ctrl.Manager) error {
	return mgr.GetFieldIndexer().IndexField(context.Background(), &apiv1.Endpoint{}, commonapi.WorkloadOwnerKey, func(rawObj ctrl_client.Object) []string {
		endpoint := rawObj.(*apiv1.Endpoint)
		return slices.Map[string](endpoint.OwnerReferences, func(ref metav1.OwnerReference) string {
			return string(ref.UID)
		})
	})
}

// Attempts to create Endpoint objects for all Services exposed by the given workload object (owner parameter).
// If reservedServicePorts is not nil, it is used to determine the port to use for each Service.
// The ecc parameter contains additional context data needed for Endpoint creation,
// which is passed to the EndpointOwner implementation when Endpoints are created.
// The function is generally goroutine-safe, i.e. it can be called concurrently for different workload objects,
// but it should NOT be called concurrently for the same workload object.
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

	var el apiv1.EndpointList
	if err = r.List(ctx, &el, ctrl_client.InNamespace(owner.GetNamespace()), ctrl_client.MatchingFields{commonapi.WorkloadOwnerKey: string(owner.GetUID())}); err != nil {
		log.Error(err, "Failed to list child Endpoint objects", "Workload", owner.NamespacedName().String())
		return
	}
	childEndpoints := toEndpointPointers(el.Items)

	for _, serviceProducer := range serviceProducers {
		// Check if we have already created an Endpoint for this workload.
		sweKey := ServiceWorkloadEndpointKey{
			NamespacedNameWithKind: commonapi.GetNamespacedNameWithKind(owner),
			ServiceName:            serviceProducer.ServiceName,
		}
		existingEndpoints := slices.Select(childEndpoints, func(e *apiv1.Endpoint) bool {
			return e.Spec.ServiceName == serviceProducer.ServiceName && e.Spec.ServiceNamespace == serviceProducer.ServiceNamespace
		})
		var cachedEndpoints []*apiv1.Endpoint
		usingCache := false
		if cached, found := workloadEndpointCache.Load(sweKey); found {
			cachedEndpoints = toEndpointPointers(cached)

			// Our cache (if filled) takes precedence because it is most up-to-date.
			diff := slices.DiffFunc(cachedEndpoints, existingEndpoints, func(a, b *apiv1.Endpoint) bool {
				return a.Spec == b.Spec
			})
			diff2 := slices.DiffFunc(existingEndpoints, cachedEndpoints, func(a, b *apiv1.Endpoint) bool {
				return a.Spec == b.Spec
			})
			usingCache = len(diff) > 0 || len(diff2) > 0
			if usingCache {
				existingEndpoints = cachedEndpoints
			} else {
				// Kubernetes client cache is up to date, we can release ours
				workloadEndpointCache.Delete(sweKey)
			}
		}
		invalidEndpoints := []*apiv1.Endpoint{}

		svcLog := log.WithValues(
			"ServiceName", serviceProducer.ServiceName,
			"Workload", owner.NamespacedName().String(),
		)

		if len(existingEndpoints) > 0 {
			var verr error
			existingEndpoints, invalidEndpoints, verr = r.validateExistingEndpoints(ctx, owner, serviceProducer, existingEndpoints, ecc, log)
			if verr != nil {
				svcLog.Error(verr, "Could not validate existing Endpoint object(s)")
				continue
			}
		}

		if len(invalidEndpoints) > 0 {
			for _, endpoint := range invalidEndpoints {
				deleteErr := r.Delete(ctx, endpoint, ctrl_client.PropagationPolicy(metav1.DeletePropagationBackground))
				if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
					svcLog.Error(deleteErr, "Could not delete Endpoint object",
						"Endpoint", endpoint.NamespacedName().String(),
					)
					existingEndpoints = append(existingEndpoints, endpoint)
				}
			}
		}

		if reservedServicePorts != nil {
			if port, portFound := reservedServicePorts[serviceProducer.ServiceNamespacedName()]; portFound {
				serviceProducer.Port = port
			}
		}
		var newEndpoints []*apiv1.Endpoint
		newEndpoints, err = r.createEndpoints(ctx, owner, serviceProducer, existingEndpoints, ecc, svcLog)

		if err != nil {
			svcLog.Error(err, "Could not create Endpoint object(s)")
			continue
		}

		if len(newEndpoints) == 0 {
			svcLog.V(1).Info("Endpoint(s) already exist for this workload and Service combination")
			continue
		}

		var createdEndpoints []*apiv1.Endpoint
		for _, endpoint := range newEndpoints {
			if err = ctrl.SetControllerReference(owner, endpoint, r.Scheme()); err != nil {
				svcLog.Error(err, "Failed to set owner for Endpoint")
				continue // Try to persist other endpoints
			}

			if err = r.Create(ctx, endpoint); err != nil {
				// Do not log an error if resource cleanup has started.
				if !apiv1.ResourceCreationProhibited.Load() {
					svcLog.Error(err, "Could not persist Endpoint object")
				}
				continue
			}

			createdEndpoints = append(createdEndpoints, endpoint)
		}

		if len(createdEndpoints) > 0 {
			svcLog.V(1).Info("New Endpoint(s) created",
				"Endpoints", logger.FriendlyStringSlice(slices.Map[string](createdEndpoints, (*apiv1.Endpoint).String)),
			)
		}

		if usingCache || len(createdEndpoints) > 0 {
			workloadEndpointCache.Store(sweKey, toEndpointValues(append(existingEndpoints, createdEndpoints...)))
		}
	}
}

// Removes all Endpoint objects associated with the given workload object (owner parameter).
func removeEndpointsForWorkload(ctx context.Context, r ctrl_client.Client, owner commonapi.DcpModelObject, log logr.Logger) {
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

func toEndpointPointers(evs []apiv1.Endpoint) []*apiv1.Endpoint {
	return slices.Map[*apiv1.Endpoint](evs, func(e apiv1.Endpoint) *apiv1.Endpoint { return &e })
}

func toEndpointValues(eps []*apiv1.Endpoint) []apiv1.Endpoint {
	return slices.Map[apiv1.Endpoint](eps, func(e *apiv1.Endpoint) apiv1.Endpoint { return *e })
}
