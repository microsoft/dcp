// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	apimachinery_errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

const (
	workloadOwnerKey          = ".metadata.owner"
	serviceProducerAnnotation = "service-producer"
)

var (
	endpointFinalizer string = fmt.Sprintf("%s/endpoint-reconciler", apiv1.GroupVersion.Group)
)

type ContainerOrExecutable interface {
	apiv1.Container | apiv1.Executable
}

type EndpointReconcilerForKind[T ContainerOrExecutable, PT PObjectStruct[T], DT DeepCopyableObject[T, PT]] struct {
	innerReconciler *EndpointReconciler
}

func (erk *EndpointReconcilerForKind[T, PT, DT]) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return reconcileEndpointsForObject[T, PT, DT](erk.innerReconciler, ctx, req)
}

type ServiceWorkloadEndpointKey struct {
	NamespacedNameWithKind
	ServiceName string
}

type EndpointReconciler struct {
	ctrl_client.Client
	Log logr.Logger

	// Local cache of freshly created workload endpoints.
	// This is used to avoid re-creating the same endpoint multiple times.
	// Because there can only be one Endpoint per workload/service combination,
	// we only need to know whenther that combination exists or not.
	workloadEndpoints syncmap.Map[ServiceWorkloadEndpointKey, bool]
}

func NewEndpointReconciler(client ctrl_client.Client, log logr.Logger) *EndpointReconciler {
	r := EndpointReconciler{
		Client:            client,
		Log:               log,
		workloadEndpoints: syncmap.Map[ServiceWorkloadEndpointKey, bool]{},
	}
	return &r
}

func (r *EndpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &apiv1.Endpoint{}, workloadOwnerKey, func(rawObj ctrl_client.Object) []string {
		endpoint := rawObj.(*apiv1.Endpoint)
		return slices.Map[metav1.OwnerReference, string](endpoint.OwnerReferences, func(ref metav1.OwnerReference) string {
			return string(ref.UID)
		})
	}); err != nil {
		r.Log.Error(err, "failed to create owner index for Endpoint")
		return err
	}

	reconcilerForContainers := EndpointReconcilerForKind[apiv1.Container, *apiv1.Container, *apiv1.Container]{
		innerReconciler: r,
	}
	err := ctrl.NewControllerManagedBy(mgr).
		Named("ContainerEndpointReconciler").
		Watches(&apiv1.Container{}, handler.EnqueueRequestsFromMapFunc(requestReconcile)).
		Complete(&reconcilerForContainers)

	if err != nil {
		return err
	}

	reconcilerForExecutables := EndpointReconcilerForKind[apiv1.Executable, *apiv1.Executable, *apiv1.Executable]{
		innerReconciler: r,
	}
	err = ctrl.NewControllerManagedBy(mgr).
		Named("ExecutableEndpointReconciler").
		Watches(&apiv1.Executable{}, handler.EnqueueRequestsFromMapFunc(requestReconcile)).
		Complete(&reconcilerForExecutables)

	if err != nil {
		return err
	}

	return nil
}

func requestReconcile(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Namespace: obj.GetNamespace(),
				Name:      obj.GetName(),
			},
		},
	}
}

func reconcileEndpointsForObject[T ContainerOrExecutable, PT PObjectStruct[T], DT DeepCopyableObject[T, PT]](
	r *EndpointReconciler,
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	workload := *new(T)
	workloadPtr := DT(&workload)
	kind := workloadPtr.GetObjectKind().GroupVersionKind().Kind

	log := r.Log.WithValues("WorkloadName", req.NamespacedName, "WorkloadKind", kind)

	select {
	case _, isOpen := <-ctx.Done():
		if !isOpen {
			log.Info("Request context expired, nothing to do...")
			return ctrl.Result{}, nil
		}
	default: // not done, proceed
	}

	if err := r.Get(ctx, req.NamespacedName, workloadPtr); err != nil {
		if apimachinery_errors.IsNotFound(err) {
			log.Info("the workload object does not exist yet or was deleted")
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to Get() the workload object")
			return ctrl.Result{}, err
		}
	}

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(workloadPtr.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if workloadPtr.GetDeletionTimestamp() != nil && !workloadPtr.GetDeletionTimestamp().IsZero() {
		r.removeEndpointsForWorkload(ctx, workloadPtr, log)
		change = deleteFinalizer(workloadPtr, endpointFinalizer)
	} else {
		change = ensureFinalizer(workloadPtr, endpointFinalizer)
		if change == noChange {
			var childEndpoints apiv1.EndpointList
			if err := r.List(ctx, &childEndpoints, ctrl_client.InNamespace(req.Namespace), ctrl_client.MatchingFields{workloadOwnerKey: string(workloadPtr.GetUID())}); err != nil {
				log.Error(err, "failed to list child Endpoint objects")
				return ctrl.Result{}, err
			}
			ensureEndpointForWorkload(r, ctx, workloadPtr, childEndpoints, log)
		}
	}

	var update PT

	switch {
	case (change & statusChanged) != 0:
		update = workloadPtr.DeepCopy()
		err := r.Status().Patch(ctx, update, patch)
		if err != nil {
			log.Error(err, "workload status update failed")
			return ctrl.Result{}, err
		} else {
			log.V(1).Info("workload status update succeeded")
		}
	case (change & metadataChanged) != 0:
		update = workloadPtr.DeepCopy()
		err := r.Patch(ctx, update, patch)
		if err != nil {
			log.Error(err, "workload metadata update failed")
			return ctrl.Result{}, err
		} else {
			log.V(1).Info("workload metadata update succeeded")
		}
	}

	if (change & additionalReconciliationNeeded) != 0 {
		return ctrl.Result{RequeueAfter: additionalReconciliationDelay}, nil
	} else {
		return ctrl.Result{}, nil
	}

}

func ensureEndpointForWorkload(
	r *EndpointReconciler,
	ctx context.Context,
	owner ctrl_client.Object,
	childEndpoints apiv1.EndpointList,
	log logr.Logger,
) {
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

	for _, serviceProducer := range serviceProducers {
		// Check if we have already created an Endpoint for this workload.
		sweKey := ServiceWorkloadEndpointKey{
			NamespacedNameWithKind: NamespacedNameWithKind{
				NamespacedName: types.NamespacedName{
					Namespace: owner.GetNamespace(),
					Name:      owner.GetName(),
				},
				Kind: owner.GetObjectKind().GroupVersionKind().Kind,
			},
			ServiceName: serviceProducer.ServiceName,
		}
		hasEndpoints := slices.Any(childEndpoints.Items, func(e apiv1.Endpoint) bool {
			return e.Spec.ServiceName == serviceProducer.ServiceName
		})
		if hasEndpoints {
			log.V(1).Info("Endpoint already exists for this workload and service combination", "ServiceName", serviceProducer.ServiceName)

			// Client has caught up and has the info about the service endpoint workload, we can clear the local cache.
			r.workloadEndpoints.Delete(sweKey)

			continue
		}
		_, found := r.workloadEndpoints.Load(sweKey)
		if found {
			log.V(1).Info("Endpoint was just created for this workload and service combination", "ServiceName", serviceProducer.ServiceName)
			continue
		}

		var endpoint *apiv1.Endpoint
		switch sweKey.Kind {
		case "Container":
			endpoint, err = r.createEndpointForContainer(ctx, (owner).(*apiv1.Container), serviceProducer, log)
		case "Executable":
			endpoint, err = r.createEndpointForExecutable(ctx, (owner).(*apiv1.Executable), serviceProducer, log)
		default:
			// Should never happen.
			panic(fmt.Errorf("don't know how to create endpont for kind: '%s'", sweKey.Kind))
		}

		if err != nil {
			log.Error(err, "could not create Endpoint object")
			continue
		}

		// CONSIDER: this is not necessary if we just Watch(Executable), but if we ever decide to to
		// For(Executable).Owns(Endpoint) and For(Container).Owns(Endpoint), then setting the controller reference
		// will trigger our reconcile loop if any of the Endpoints change.
		/*
			if err := ctrl.SetControllerReference(owner, endpoint, r.Scheme()); err != nil {
				log.Error(err, "failed to set owner for endpoint")
				return metadataChanged
			}
		*/

		if err := r.Create(ctx, endpoint); err != nil {
			log.Error(err, "could not persist Endpoint object")
		}

		r.workloadEndpoints.Store(sweKey, true)
	}
}

func (r *EndpointReconciler) createEndpointForContainer(
	ctx context.Context,
	ctr *apiv1.Container,
	serviceProducer ServiceProducer,
	log logr.Logger,
) (*apiv1.Endpoint, error) {
	endpointName, err := MakeUniqueName(ctr.GetName())
	if err != nil {
		log.Error(err, "could not generate unique name for Endpoint object")
		return nil, err
	}

	if serviceProducer.Address != "" {
		log.Error(fmt.Errorf("address cannot be specified for Container objects"), serviceProducerIsInvalid)
		return nil, err
	}

	// TODO: validate port according to descirption in ServiceProducer struct below

	// Otherwise, create a new Endpoint object.
	endpoint := &apiv1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      endpointName,
			Namespace: ctr.Namespace,
		},
		Spec: apiv1.EndpointSpec{
			ServiceNamespace: ctr.Namespace,
			ServiceName:      serviceProducer.ServiceName,
			Address:          "", // TODO: find address to use
			Port:             0,  // TODO: find port to use
		},
	}

	return endpoint, nil
}

func (r *EndpointReconciler) createEndpointForExecutable(
	ctx context.Context,
	exe *apiv1.Executable,
	serviceProducer ServiceProducer,
	log logr.Logger,
) (*apiv1.Endpoint, error) {
	endpointName, err := MakeUniqueName(exe.GetName())
	if err != nil {
		log.Error(err, "could not generate unique name for Endpoint object")
		return nil, err
	}

	address := serviceProducer.Address
	if address == "" {
		address = "localhost"
	}

	// Otherwise, create a new Endpoint object.
	endpoint := &apiv1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      endpointName,
			Namespace: exe.Namespace,
		},
		Spec: apiv1.EndpointSpec{
			ServiceNamespace: exe.Namespace,
			ServiceName:      serviceProducer.ServiceName,
			Address:          address,
			Port:             serviceProducer.Port,
		},
	}

	return endpoint, nil
}

func (r *EndpointReconciler) removeEndpointsForWorkload(ctx context.Context, owner ctrl_client.Object, log logr.Logger) objectChange {
	err := r.DeleteAllOf(ctx, &apiv1.Endpoint{}, ctrl_client.InNamespace(owner.GetNamespace()), ctrl_client.MatchingFields{workloadOwnerKey: string(owner.GetUID())})

	if err != nil {
		log.Error(err, "could not delete Endpoint object")
	}

	// TODO: remove corresponding entries from r.workloadEndpoints, if any

	return noChange
}

func parseServiceProducerAnnotation(annotation string) ([]ServiceProducer, error) {
	retval := make([]ServiceProducer, 0)
	err := json.Unmarshal([]byte(annotation), &retval)

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
