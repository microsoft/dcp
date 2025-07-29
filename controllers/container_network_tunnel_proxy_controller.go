// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/go-logr/logr"
	apimachinery_errors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

const (
	tunnelProxyContainerNameProperty = ".spec.containerNetworkName"
)

var (
	tunnelProxyFinalizer string = fmt.Sprintf("%s/tunnel-proxy-reconciler", apiv1.GroupVersion.Group)
)

type ContainerNetworkTunnelProxyReconciler struct {
	ctrl_client.Client
	Log                 logr.Logger
	reconciliationSeqNo uint32
}

func NewContainerNetworkTunnelProxyReconciler(client ctrl_client.Client, log logr.Logger) *ContainerNetworkTunnelProxyReconciler {
	return &ContainerNetworkTunnelProxyReconciler{
		Client: client,
		Log:    log,
	}
}

func (r *ContainerNetworkTunnelProxyReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	// Setup a client-side index to allow quickly finding all ContainerNetworkTunnelProxies referencing a specific ContainerNetwork.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &apiv1.ContainerNetworkTunnelProxy{}, tunnelProxyContainerNameProperty, func(rawObj ctrl_client.Object) []string {
		cntp := rawObj.(*apiv1.ContainerNetworkTunnelProxy)
		if cntp.Spec.ContainerNetworkName == "" {
			return nil
		} else {
			return []string{cntp.Spec.ContainerNetworkName}
		}
	}); err != nil {
		r.Log.Error(err, "Failed to create index for ContainerNetworkTunnelProxy.Spec.ContainerNetworkName field")
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv1.ContainerNetworkTunnelProxy{}).
		Watches(&apiv1.ContainerNetwork{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForContainerNetworkTunnelProxy), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Named(name).
		Complete(r)
}

func (r *ContainerNetworkTunnelProxyReconciler) requestReconcileForContainerNetworkTunnelProxy(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	network := obj.(*apiv1.ContainerNetwork)

	// Find all ContainerNetworkTunnelProxies that reference this ContainerNetwork
	var tunnelProxies apiv1.ContainerNetworkTunnelProxyList
	listOpts := []ctrl_client.ListOption{
		ctrl_client.MatchingFields{tunnelProxyContainerNameProperty: network.Name},
	}

	if err := r.List(ctx, &tunnelProxies, listOpts...); err != nil {
		r.Log.Error(err, "Failed to list ContainerNetworkTunnelProxies for ContainerNetwork", "ContainerNetwork", network.Name)
		return nil
	}

	requests := make([]reconcile.Request, len(tunnelProxies.Items))
	for i, tunnelProxy := range tunnelProxies.Items {
		requests[i] = reconcile.Request{
			NamespacedName: tunnelProxy.NamespacedName(),
		}
	}

	if len(requests) > 0 {
		r.Log.V(1).Info("Enqueuing ContainerNetworkTunnelProxy reconciliation requests due to ContainerNetwork change",
			"ContainerNetwork", network.Name,
			"AffectedTunnelProxies", slices.Map[reconcile.Request, string](
				requests, func(req reconcile.Request) string { return req.NamespacedName.String() },
			),
		)
	}

	return requests
}

func (r *ContainerNetworkTunnelProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues(
		"ContainerNetworkTunnelProxy", req.NamespacedName.String(),
		"Reconciliation", atomic.AddUint32(&r.reconciliationSeqNo, 1),
	)

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	tproxy := apiv1.ContainerNetworkTunnelProxy{}
	err := r.Get(ctx, req.NamespacedName, &tproxy)

	if err != nil {
		if apimachinery_errors.IsNotFound(err) {
			log.V(1).Info("ContainerNetworkTunnelProxy object was not found")
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "Failed to Get() the ContainerNetworkTunnelProxy object")
			getFailedCounter.Add(ctx, 1)
			return ctrl.Result{}, err
		}
	} else {
		getSucceededCounter.Add(ctx, 1)
	}

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(tproxy.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if tproxy.DeletionTimestamp != nil && !tproxy.DeletionTimestamp.IsZero() {
		log.Info("ContainerNetworkTunnelProxy object is being deleted")
		r.releaseProxyResources(ctx, &tproxy, log)
		change = deleteFinalizer(&tproxy, tunnelProxyFinalizer, log)
	} else {
		change = ensureFinalizer(&tproxy, tunnelProxyFinalizer, log)
		if change == noChange {
			change = r.manageTunnelProxy(ctx, &tproxy, log)
		}
	}

	result, err := saveChanges(r.Client, ctx, &tproxy, patch, change, nil, log)
	return result, err
}

func (r *ContainerNetworkTunnelProxyReconciler) releaseProxyResources(_ context.Context, _ *apiv1.ContainerNetworkTunnelProxy, log logr.Logger) {
	// TODO: for now, we just log the cleanup. Later phases will implement actual cleanup.
	log.V(1).Info("Cleaning up ContainerNetworkTunnelProxy resources")
}

func (r *ContainerNetworkTunnelProxyReconciler) manageTunnelProxy(ctx context.Context, tunnelProxy *apiv1.ContainerNetworkTunnelProxy, log logr.Logger) objectChange {
	// TODO: this method needs to be re-worked using state machine approach

	containerNetworkName := commonapi.AsNamespacedName(tunnelProxy.Spec.ContainerNetworkName, tunnelProxy.Namespace)
	containerNetwork := apiv1.ContainerNetwork{}
	tryAgain := false
	err := r.Get(ctx, containerNetworkName, &containerNetwork)

	switch {
	case apimachinery_errors.IsNotFound(err):
		tryAgain = true
		log.V(1).Info("Referenced ContainerNetwork not found", "ContainerNetwork", containerNetworkName.String())

	case err != nil:
		tryAgain = true
		log.Error(err, "Failed to get referenced ContainerNetwork", "ContainerNetwork", containerNetworkName.String())

	case containerNetwork.Status.State != apiv1.ContainerNetworkStateRunning:
		tryAgain = true
		log.V(1).Info("Referenced ContainerNetwork is not in Running state",
			"ContainerNetwork", containerNetworkName.String(),
			"NetworkState", containerNetwork.Status.State)
	}

	if tryAgain {
		change := r.setTunnelProxyState(tunnelProxy, apiv1.ContainerNetworkTunnelProxyStatePending)
		return change | additionalReconciliationNeeded
	}

	// ContainerNetwork is running, transition to Running state for now
	return r.setTunnelProxyState(tunnelProxy, apiv1.ContainerNetworkTunnelProxyStateRunning)
}

func (r *ContainerNetworkTunnelProxyReconciler) setTunnelProxyState(tproxy *apiv1.ContainerNetworkTunnelProxy, state apiv1.ContainerNetworkTunnelProxyState) objectChange {
	change := noChange

	if tproxy.Status.State != state {
		tproxy.Status.State = state
		change = statusChanged
	}

	return change
}
