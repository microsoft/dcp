/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/internal/proxy"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/syncmap"
)

const (
	logicalServiceFinalizer     = apiv2.GroupName + "/logicalservice-reconciler"
	logicalEndpointServiceIndex = ".spec.serviceRef"
)

type logicalServiceData struct {
	uid           types.UID
	proxies       []proxyInstanceData
	appliedConfig *proxy.ProxyConfig
	attempts      uint32
	selectedUID   types.UID
	selectedName  string
}

// LogicalServiceReconciler owns proxy listeners and projects listener or endpoint-selected addresses.
type LogicalServiceReconciler struct {
	*ReconcilerBase[apiv2.LogicalService, *apiv2.LogicalService]
	createProxy proxy.ProxyFactory
	services    *syncmap.Map[types.NamespacedName, *logicalServiceData]
}

func NewLogicalServiceReconciler(
	ctx context.Context, client ctrl_client.Client, reader ctrl_client.Reader, log logr.Logger, createProxy proxy.ProxyFactory,
) *LogicalServiceReconciler {
	if createProxy == nil {
		createProxy = proxy.NewRuntimeProxy
	}
	return &LogicalServiceReconciler{
		ReconcilerBase: NewReconcilerBase[apiv2.LogicalService](client, reader, log, ctx),
		createProxy:    createProxy, services: &syncmap.Map[types.NamespacedName, *logicalServiceData]{},
	}
}

func (r *LogicalServiceReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	indexErr := mgr.GetFieldIndexer().IndexField(r.LifetimeCtx, &apiv2.ServiceEndpoint{}, logicalEndpointServiceIndex, func(object ctrl_client.Object) []string {
		endpoint := object.(*apiv2.ServiceEndpoint)
		return []string{commonapi.AsNamespacedName(endpoint.Spec.ServiceRef, endpoint.Namespace).Name}
	})
	if indexErr != nil {
		return fmt.Errorf("index service endpoint references: %w", indexErr)
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv2.LogicalService{}).
		Watches(&apiv2.ServiceEndpoint{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, object ctrl_client.Object) []reconcile.Request {
			endpoint := object.(*apiv2.ServiceEndpoint)
			return []reconcile.Request{{NamespacedName: commonapi.AsNamespacedName(endpoint.Spec.ServiceRef, endpoint.Namespace)}}
		})).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequests(&apiv2.LogicalServiceList{}))).
		Named(name).Complete(r)
}

func (r *LogicalServiceReconciler) stop(name types.NamespacedName, log logr.Logger) {
	if data, found := r.services.LoadAndDelete(name); found {
		stopProxies(data.proxies, log)
	}
}

func (r *LogicalServiceReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(request)
	service := &apiv2.LogicalService{}
	getErr := reader.Get(ctx, request.NamespacedName, service)
	if apierrors.IsNotFound(getErr) {
		r.stop(request.NamespacedName, log)
		return ctrl.Result{}, nil
	}
	if getErr != nil {
		log.Error(getErr, "Failed to get LogicalService")
		return ctrl.Result{}, getErr
	}
	patch := ctrl_client.MergeFromWithOptions(service.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	if !service.DeletionTimestamp.IsZero() {
		r.stop(service.NamespacedName(), log)
		return r.SaveChanges(ctx, service, patch, deleteFinalizer(service, logicalServiceFinalizer, log), nil, log)
	}
	if change := ensureFinalizer(service, logicalServiceFinalizer, log); change != noChange {
		return r.SaveChanges(ctx, service, patch, change, nil, log)
	}
	data, found := r.services.Load(service.NamespacedName())
	if found && data.uid != service.UID {
		r.stop(service.NamespacedName(), log)
		found = false
	}
	if !found {
		data = &logicalServiceData{uid: service.UID}
		r.services.Store(service.NamespacedName(), data)
	}
	status := apiv2.LogicalServiceStatus{Phase: apiv2.LogicalServicePhasePending, Conditions: slices.Clone(service.Status.Conditions)}
	ready, reason, namespaceErr := checkNamespaceReadiness(ctx, r.Client, service.Namespace)
	message := namespaceReadinessMessage(service.Namespace, reason)
	requeue := false
	switch {
	case !ready:
		if namespaceErr != nil {
			log.Error(namespaceErr, "Failed to check service namespace")
			message, requeue = namespaceErr.Error(), true
		}
	default:
		var endpoints apiv2.ServiceEndpointList
		listErr := r.List(ctx, &endpoints, ctrl_client.InNamespace(service.Namespace), ctrl_client.MatchingFields{
			logicalEndpointServiceIndex: service.Name,
		})
		if listErr != nil {
			log.Error(listErr, "Failed to list service endpoints")
			reason, message, requeue = "EndpointLookupFailed", listErr.Error(), true
			break
		}
		endpoints.Items = slices.DeleteFunc(endpoints.Items, func(endpoint apiv2.ServiceEndpoint) bool { return !endpoint.DeletionTimestamp.IsZero() })
		slices.SortFunc(endpoints.Items, func(left, right apiv2.ServiceEndpoint) int {
			if left.Name < right.Name {
				return -1
			}
			if left.Name > right.Name {
				return 1
			}
			return 0
		})
		if service.Spec.AddressAllocationMode == apiv2.ServiceAddressAllocationModeProxyless {
			selected := slices.IndexFunc(endpoints.Items, func(endpoint apiv2.ServiceEndpoint) bool { return endpoint.UID == data.selectedUID })
			if selected < 0 && len(endpoints.Items) > 0 {
				selected = 0
			}
			if selected >= 0 {
				endpoint := &endpoints.Items[selected]
				data.selectedName, data.selectedUID = endpoint.Name, endpoint.UID
				status.EffectiveAddresses = slices.Clone(endpoint.Spec.Addresses)
				status.ProxylessEndpointRef, status.Phase = endpoint.Name, apiv2.LogicalServicePhaseReady
				reason, message = "EndpointSelected", "The selected endpoint's addresses are available."
			} else {
				data.selectedName, data.selectedUID = "", ""
				reason, message = "WaitingForEndpoints", "The service has no available endpoints."
			}
			break
		}
		proxyErr := r.configureProxies(service, data, endpoints.Items, log)
		for _, listener := range data.proxies {
			if listener.proxy.State() == proxy.ProxyStateRunning {
				status.EffectiveAddresses = append(status.EffectiveAddresses, apiv2.ServiceAddress{
					Address: networking.ToStandaloneAddress(listener.proxy.EffectiveAddress()), Port: listener.proxy.EffectivePort(),
				})
			}
		}
		if proxyErr != nil {
			log.Error(proxyErr, "Failed to configure logical service listeners")
			reason, message, requeue = "ProxyConfigurationFailed", proxyErr.Error(), true
			if len(data.proxies) > 0 {
				status.Phase = apiv2.LogicalServicePhaseUnknown
			}
			if len(status.EffectiveAddresses) == 0 && (len(data.proxies) > 0 || data.attempts >= MaxServiceStartAttempts) {
				status.Phase, requeue = apiv2.LogicalServicePhaseFailed, false
			}
			break
		}
		if len(status.EffectiveAddresses) < len(data.proxies) {
			status.Phase = apiv2.LogicalServicePhaseUnknown
			if len(status.EffectiveAddresses) == 0 {
				status.Phase = apiv2.LogicalServicePhaseFailed
			}
			reason, message = "ProxyListenerUnavailable", "Service listeners have stopped; recreate the service to allocate new listeners."
			break
		}
		reason, message = "WaitingForEndpoints", "Listeners are allocated; the service has no available endpoints."
		if len(endpoints.Items) > 0 {
			status.Phase, reason, message = apiv2.LogicalServicePhaseReady, "ServiceReady", "Listeners and endpoints are available."
		}
	}
	conditionStatus := metav1.ConditionFalse
	if status.Phase == apiv2.LogicalServicePhaseReady {
		conditionStatus = metav1.ConditionTrue
	}
	setCondition(&status.Conditions, apiv2.ConditionReady, service.Generation, conditionStatus, reason, message)
	change := noChange
	if !reflect.DeepEqual(service.Status, status) {
		service.Status = status
		change |= statusChanged
	}
	if requeue {
		change |= additionalReconciliationNeeded
	}
	return r.SaveChangesWithDelay(ctx, service, patch, change, LongDelay, nil, log)
}

func (r *LogicalServiceReconciler) configureProxies(
	service *apiv2.LogicalService, data *logicalServiceData, endpoints []apiv2.ServiceEndpoint, log logr.Logger,
) error {
	if len(data.proxies) == 0 {
		data.appliedConfig = nil
		if data.attempts >= MaxServiceStartAttempts {
			return fmt.Errorf("service listener startup attempts exhausted")
		}
		data.attempts++
		address, addressErr := getRequestedServiceAddress(service.Spec.Address, string(service.Spec.AddressAllocationMode))
		if addressErr != nil {
			return addressErr
		}
		listeners, allocationErr := allocateServiceProxies(r.LifetimeCtx, r.createProxy, service.Spec.EffectiveProtocol(), service.Spec.Port, address,
			r.Log.WithName("Proxy").WithValues("LogicalService", service.NamespacedName()))
		if allocationErr != nil {
			stopProxies(listeners, log)
			return allocationErr
		}
		for _, listener := range listeners {
			if startErr := listener.proxy.Start(); startErr != nil {
				stopProxies(listeners, log)
				return fmt.Errorf("start service listener: %w", startErr)
			}
		}
		data.proxies = listeners
	}
	config := proxy.ProxyConfig{Endpoints: []proxy.Endpoint{}}
	for _, endpoint := range endpoints {
		for _, address := range endpoint.Spec.Addresses {
			config.Endpoints = append(config.Endpoints, proxy.Endpoint{Address: address.Address, Port: address.Port})
		}
	}
	if data.appliedConfig != nil && reflect.DeepEqual(*data.appliedConfig, config) {
		return nil
	}
	data.appliedConfig = nil
	for _, listener := range data.proxies {
		if listener.proxy.State() != proxy.ProxyStateRunning {
			continue
		}
		if configErr := listener.proxy.Configure(config); configErr != nil {
			return fmt.Errorf("configure service listener: %w", configErr)
		}
	}
	applied := config.Clone()
	data.appliedConfig = &applied
	return nil
}
