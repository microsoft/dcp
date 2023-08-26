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
)

const (
	workloadOwnerKey = ".metadata.owner"
)

var (
	endpointFinalizer string = fmt.Sprintf("%s/endpoint-reconciler", apiv1.GroupVersion.Group)
)

type EndpointReconcilerForKind[T any, PT PObjectStruct[T], DT DeepCopyableObject[T, PT]] struct {
	innerReconciler *EndpointReconciler
}

func (erk *EndpointReconcilerForKind[T, PT, DT]) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return ReconcileEndpointsForObject[T, PT, DT](erk.innerReconciler, ctx, req)
}

type EndpointReconciler struct {
	ctrl_client.Client
	Log logr.Logger
}

func NewEndpointReconciler(client ctrl_client.Client, log logr.Logger) *EndpointReconciler {
	r := EndpointReconciler{
		Client: client,
		Log:    log,
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
		WithEventFilter(&CreateDeletePredicate{}).
		Complete(&reconcilerForContainers)

	if err != nil {
		return err
	}

	reconcilerForExecutables := EndpointReconcilerForKind[apiv1.Executable, *apiv1.Executable, *apiv1.Executable]{
		innerReconciler: r,
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("ExecutableEndpointReconciler").
		Watches(&apiv1.Executable{}, handler.EnqueueRequestsFromMapFunc(requestReconcile)).
		WithEventFilter(&CreateDeletePredicate{}).
		Complete(&reconcilerForExecutables)
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

func ReconcileEndpointsForObject[T any, PT PObjectStruct[T], DT DeepCopyableObject[T, PT]](r *EndpointReconciler, ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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
		r.removeEndpointForWorkload(ctx, workloadPtr, log)
		change = deleteFinalizer(workloadPtr, endpointFinalizer)
	} else {
		change = ensureFinalizer(workloadPtr, endpointFinalizer)
		if change == noChange {
			change = r.ensureEndpointForWorkload(ctx, workloadPtr, log)
		}
	}

	var update PT

	switch {
	case change&statusChanged != 0:
		update = workloadPtr.DeepCopy()
		err := r.Status().Patch(ctx, update, patch)
		if err != nil {
			log.Error(err, "workload status update failed")
			return ctrl.Result{}, err
		} else {
			log.V(1).Info("workload status update succeeded")
		}
	case change&metadataChanged != 0:
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

func (r *EndpointReconciler) ensureEndpointForWorkload(ctx context.Context, owner ctrl_client.Object, log logr.Logger) objectChange {
	var serviceProducer ServiceProducer
	var err error

	annotations := owner.GetAnnotations()

	if serviceProducerAnnotation, ok := annotations["service-producer"]; ok {
		if serviceProducer, err = parseServiceProducerAnnotation(serviceProducerAnnotation); err != nil {
			log.Error(err, "could not parse service-producer annotation")
			return noChange
		} else {
			log.V(1).Info("found service-producer annotation", "serviceProducer", serviceProducer)
		}
	} else {
		log.V(1).Info("no service-producer annotation found on Container/Executable object")
		return noChange
	}

	endpointName, err := MakeUniqueName(owner.GetName())
	if err != nil {
		log.Error(err, "could not generate unique name for Endpoint object")
		return noChange
	}

	namespace := owner.GetNamespace()
	var address string
	if serviceProducer.Address != "" {
		address = serviceProducer.Address
	} else {
		address = "localhost"
	}

	// Otherwise, create a new Endpoint object.
	endpoint := &apiv1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:        endpointName,
			Namespace:   namespace,
			Labels:      make(map[string]string),
			Annotations: make(map[string]string),
		},
		Spec: apiv1.EndpointSpec{
			ServiceNamespace: namespace,
			ServiceName:      serviceProducer.ServiceName,
			Address:          address,
			Port:             serviceProducer.Port,
		},
	}

	// Set the Executable/Container as the owner of the Endpoint so that deletion of the owner will
	// delete the Endpoint
	if err := ctrl.SetControllerReference(owner, endpoint, r.Scheme()); err != nil {
		log.Error(err, "failed to set owner for endpoint")
		return noChange
	}

	if err := r.Create(ctx, endpoint); err != nil {
		log.Error(err, "could not create Endpoint object")
		return noChange
	}

	return noChange
}

func (r *EndpointReconciler) removeEndpointForWorkload(ctx context.Context, owner ctrl_client.Object, log logr.Logger) objectChange {
	err := r.DeleteAllOf(ctx, &apiv1.Endpoint{}, ctrl_client.InNamespace(owner.GetNamespace()), ctrl_client.MatchingFields{workloadOwnerKey: string(owner.GetUID())})

	if err != nil {
		log.Error(err, "could not delete Endpoint object")
	}

	return noChange
}

func parseServiceProducerAnnotation(annotation string) (ServiceProducer, error) {
	var serviceProducer ServiceProducer
	err := json.Unmarshal([]byte(annotation), &serviceProducer)

	return serviceProducer, err
}

type ServiceProducer struct {
	ServiceName string `json:"serviceName"`
	Address     string `json:"address,omitempty"`
	Port        int32  `json:"port"`
}
