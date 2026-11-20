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
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/proxy"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/syncmap"
)

const (
	logicalServiceFinalizer     = apiv2.GroupName + "/logicalservice-reconciler"
	logicalEndpointServiceIndex = ".spec.serviceRef"
)

type logicalServiceData struct {
	uid                   types.UID
	proxies               []proxyInstanceData
	appliedConfig         *proxy.ProxyConfig
	attempts              uint32
	startupFailureMessage string
	selectedUID           types.UID
	selectedName          string
	selectedAddresses     []apiv2.ServiceAddress
	started               bool
	failureReason         apiv2.ConditionReason
	failureMessage        string
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
		}), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&apiv2.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequests(&apiv2.LogicalServiceList{})), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Named(name).Complete(r)
}

func (r *LogicalServiceReconciler) stop(name types.NamespacedName, log logr.Logger) {
	if data, found := r.services.LoadAndDelete(name); found {
		stopProxies(data.proxies, log)
	}
}

func (r *LogicalServiceReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(request)
	if ctx.Err() != nil {
		return ctrl.Result{}, nil
	}
	service := &apiv2.LogicalService{}
	getErr := reader.Get(ctx, request.NamespacedName, service)
	if apierrors.IsNotFound(getErr) {
		r.stop(request.NamespacedName, log)
		getNotFoundCounter.Add(ctx, 1)
		return ctrl.Result{}, nil
	}
	if getErr != nil {
		log.Error(getErr, "Failed to get LogicalService")
		getFailedCounter.Add(ctx, 1)
		return ctrl.Result{}, getErr
	}
	getSucceededCounter.Add(ctx, 1)
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
		ready, namespaceReason, namespaceErr := checkNamespaceReadiness(ctx, r.Client, service.Namespace)
		if !ready {
			phase, reason := apiv2.LogicalServicePhasePending, apiv2.LogicalServiceReasonStarting
			message := namespaceReadinessMessage(service.Namespace, namespaceReason)
			delay := StandardDelay
			if namespaceErr != nil {
				log.Error(namespaceErr, "Failed to check service namespace")
				message, delay = namespaceErr.Error(), LongDelay
			}
			change := setValue(&service.Status.Phase, phase)
			change |= setCondition(&service.Status.Conditions, apiv2.ConditionReady, service.Generation,
				metav1.ConditionFalse, reason, message)
			return r.SaveChangesWithDelay(ctx, service, patch, change, delay, nil, log)
		}
		data = &logicalServiceData{uid: service.UID}
		if service.Spec.AddressAllocationMode == apiv2.ServiceAddressAllocationModeProxyless {
			data.selectedName = service.Status.ProxylessEndpointRef
			data.selectedAddresses = slices.Clone(service.Status.EffectiveAddresses)
		}
		r.services.Store(service.NamespacedName(), data)
	}
	status := apiv2.LogicalServiceStatus{Phase: apiv2.LogicalServicePhasePending, Conditions: slices.Clone(service.Status.Conditions)}
	reason, message := apiv2.LogicalServiceReasonStarting, ""
	requeue := false
	available := false
	switch {
	case len(data.proxies) == 0 && data.attempts >= MaxServiceStartAttempts:
		status.Phase = apiv2.LogicalServicePhaseFailed
		reason, message = apiv2.LogicalServiceReasonFailedToStart, data.startupFailureMessage
	case len(data.proxies) > 0 && !slices.ContainsFunc(data.proxies, func(listener proxyInstanceData) bool {
		return listener.proxy.State() == proxy.ProxyStateRunning
	}):
		status.Phase = apiv2.LogicalServicePhaseFailed
		reason, message = apiv2.LogicalServiceReasonMissing, "Service listeners have stopped; recreate the service to allocate new listeners."
	default:
		var endpoints apiv2.ServiceEndpointList
		listErr := r.List(ctx, &endpoints, ctrl_client.InNamespace(service.Namespace), ctrl_client.MatchingFields{
			logicalEndpointServiceIndex: service.Name,
		})
		if listErr != nil {
			log.Error(listErr, "Failed to list service endpoints")
			message, requeue = listErr.Error(), true
			if service.Spec.AddressAllocationMode == apiv2.ServiceAddressAllocationModeProxyless {
				status.EffectiveAddresses = slices.Clone(data.selectedAddresses)
				status.ProxylessEndpointRef = data.selectedName
			} else {
				status.EffectiveAddresses = logicalServiceProxyAddresses(data.proxies)
			}
			if len(status.EffectiveAddresses) > 0 {
				status.Phase, reason = apiv2.LogicalServicePhaseRunning, apiv2.LogicalServiceReasonAllocated
			}
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
				data.selectedUID, data.selectedName = endpoint.UID, endpoint.Name
				data.selectedAddresses = slices.Clone(endpoint.Spec.Addresses)
				status.EffectiveAddresses = slices.Clone(data.selectedAddresses)
				status.ProxylessEndpointRef, status.Phase = data.selectedName, apiv2.LogicalServicePhaseRunning
				reason, message = apiv2.LogicalServiceReasonReady, "The selected endpoint's addresses are available."
				data.started, available = true, true
			} else {
				data.selectedUID, data.selectedName, data.selectedAddresses = "", "", nil
				reason, message = apiv2.LogicalServiceReasonStarting, "The service has no available endpoints."
			}
			break
		}
		proxyErr := r.configureProxies(service, data, endpoints.Items, log)
		status.EffectiveAddresses = logicalServiceProxyAddresses(data.proxies)
		if len(status.EffectiveAddresses) > 0 {
			data.started = true
		}
		if proxyErr != nil {
			log.Error(proxyErr, "Failed to configure logical service listeners")
			if len(data.proxies) == 0 {
				data.startupFailureMessage = proxyErr.Error()
			}
			reason, message, requeue = apiv2.LogicalServiceReasonStarting, proxyErr.Error(), true
			if data.started {
				status.Phase, reason = apiv2.LogicalServicePhaseRunning, apiv2.LogicalServiceReasonAllocated
			}
			if len(status.EffectiveAddresses) == 0 && (len(data.proxies) > 0 || data.attempts >= MaxServiceStartAttempts) {
				status.Phase, requeue = apiv2.LogicalServicePhaseFailed, false
				reason = apiv2.LogicalServiceReasonFailedToStart
				if data.started {
					reason = apiv2.LogicalServiceReasonMissing
				}
			}
			break
		}
		if len(status.EffectiveAddresses) < len(data.proxies) {
			status.Phase = apiv2.LogicalServicePhaseFailed
			reason, message = apiv2.LogicalServiceReasonMissing, "Service listeners have stopped; recreate the service to allocate new listeners."
			break
		}
		data.started = true
		status.Phase, reason, message = apiv2.LogicalServicePhaseRunning, apiv2.LogicalServiceReasonAllocated,
			"Listeners are running; the service has no available endpoints."
		if len(endpoints.Items) > 0 {
			available, reason, message = true, apiv2.LogicalServiceReasonReady, "Listeners and endpoints are available."
		}
	}
	if data.failureReason != "" {
		status.Phase, reason, message = apiv2.LogicalServicePhaseFailed, data.failureReason, data.failureMessage
	} else if status.Phase == apiv2.LogicalServicePhaseFailed {
		data.failureReason, data.failureMessage = reason, message
	}
	conditionStatus := metav1.ConditionFalse
	if available {
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
	delay := StandardDelay
	if requeue || (len(data.proxies) > 0 && len(status.EffectiveAddresses) > 0) {
		delay = LongDelay
	}
	return r.SaveChangesWithDelay(ctx, service, patch, change, delay, nil, log)
}

func logicalServiceProxyAddresses(listeners []proxyInstanceData) []apiv2.ServiceAddress {
	var addresses []apiv2.ServiceAddress
	for _, listener := range listeners {
		if listener.proxy.State() == proxy.ProxyStateRunning {
			addresses = append(addresses, apiv2.ServiceAddress{
				Address: listener.proxy.EffectiveAddress(), Port: listener.proxy.EffectivePort(),
			})
		}
	}
	return addresses
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
