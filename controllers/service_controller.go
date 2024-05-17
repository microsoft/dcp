// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/go-logr/logr"
	"github.com/smallnest/chanx"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap/zapcore"
	"golang.org/x/net/nettest"
	apimachinery_errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	ctrl_event "sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	ctrl_source "sigs.k8s.io/controller-runtime/pkg/source"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/internal/proxy"
	"github.com/microsoft/usvc-apiserver/internal/telemetry"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

type ProxyHandlingOptionValue string

const (
	// Used by tests to disable starting the proxy (proxies are created, but not started)
	ServiceReconcilerProxyHandling = ControllerContextOption("ServiceReconcilerProxyHandling")
	DoNotStartProxies              = ProxyHandlingOptionValue("do-not-start-proxies")
)

type proxyInstanceData struct {
	proxy     *proxy.Proxy
	stopProxy context.CancelFunc
}

type ServiceReconciler struct {
	ctrl_client.Client
	Log                 logr.Logger
	reconciliationSeqNo uint32
	ProcessExecutor     process.Executor
	ProxyConfigDir      string
	proxyData           *syncmap.Map[types.NamespacedName, []proxyInstanceData]

	// Channel used to trigger reconciliation function when underlying run status changes.
	notifyProxyRunChanged *chanx.UnboundedChan[ctrl_event.GenericEvent]

	lifetimeCtx context.Context

	tracer trace.Tracer
}

var (
	serviceFinalizer string = fmt.Sprintf("%s/service-reconciler", apiv1.GroupVersion.Group)
)

func NewServiceReconciler(lifetimeCtx context.Context, client ctrl_client.Client, log logr.Logger, processExecutor process.Executor) *ServiceReconciler {
	r := ServiceReconciler{
		Client:                client,
		ProcessExecutor:       processExecutor,
		ProxyConfigDir:        filepath.Join(os.TempDir(), "usvc-servicecontroller-serviceconfig"),
		proxyData:             &syncmap.Map[types.NamespacedName, []proxyInstanceData]{},
		notifyProxyRunChanged: chanx.NewUnboundedChan[ctrl_event.GenericEvent](lifetimeCtx, 1),
		lifetimeCtx:           lifetimeCtx,
		tracer:                telemetry.GetTelemetrySystem().TracerProvider.Tracer("service-controller"),
		Log:                   log,
	}
	return &r
}

func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &apiv1.Endpoint{}, ".metadata.serviceNamespace", func(rawObj ctrl_client.Object) []string {
		endpoint := rawObj.(*apiv1.Endpoint)
		return []string{endpoint.Spec.ServiceNamespace}
	}); err != nil {
		r.Log.Error(err, "failed to create serviceNamespace index for Endpoint")
		return err
	}

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &apiv1.Endpoint{}, ".metadata.serviceName", func(rawObj ctrl_client.Object) []string {
		endpoint := rawObj.(*apiv1.Endpoint)
		return []string{endpoint.Spec.ServiceName}
	}); err != nil {
		r.Log.Error(err, "failed to create serviceName index for Endpoint")
		return err
	}

	src := ctrl_source.Channel(r.notifyProxyRunChanged.Out, &handler.EnqueueRequestForObject{})
	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.Service{}).
		Watches(&apiv1.Endpoint{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForEndpoint), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(src).
		Complete(r)
}

func (r *ServiceReconciler) requestReconcileForEndpoint(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	endpoint := obj.(*apiv1.Endpoint)
	serviceNamespaceName := types.NamespacedName{
		Namespace: endpoint.Spec.ServiceNamespace,
		Name:      endpoint.Spec.ServiceName,
	}

	r.Log.V(1).Info("endpoint updated, requesting service reconciliation", "Endpoint", endpoint, "ServiceName", serviceNamespaceName)
	return []reconcile.Request{
		{
			NamespacedName: serviceNamespaceName,
		},
	}
}

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("ServiceName", req.NamespacedName).WithValues("Reconciliation", atomic.AddUint32(&r.reconciliationSeqNo, 1))

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	svc := apiv1.Service{}
	if err := r.Get(ctx, req.NamespacedName, &svc); err != nil {
		if apimachinery_errors.IsNotFound(err) {
			log.V(1).Info("the Service object does not exist yet or was deleted")
			r.stopAllProxies(req.NamespacedName, log)
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to Get() the Service object")
			getFailedCounter.Add(ctx, 1)
			return ctrl.Result{}, err
		}
	} else {
		getSucceededCounter.Add(ctx, 1)
	}

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(svc.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if svc.DeletionTimestamp != nil && !svc.DeletionTimestamp.IsZero() {
		log.V(1).Info("Service object is being deleted")
		r.stopAllProxies(svc.NamespacedName(), log)
		change = deleteFinalizer(&svc, serviceFinalizer, log)
		serviceCounters(ctx, &svc, -1) // Service is being deleted
	} else {
		change = ensureFinalizer(&svc, serviceFinalizer, log)
		// If we added a finalizer, we'll do the additional reconciliation next call
		if change == noChange {
			change |= r.ensureServiceEffectiveAddressAndPort(ctx, &svc, log)
		} else {
			serviceCounters(ctx, &svc, 1) // Service was just created
		}
	}

	result, err := saveChanges(r.Client, ctx, &svc, patch, change, nil, log)
	return result, err
}

func (r *ServiceReconciler) stopAllProxies(svcName types.NamespacedName, log logr.Logger) {
	if proxyData, ok := r.proxyData.LoadAndDelete(svcName); ok && len(proxyData) > 0 {
		log.V(1).Info("stopping all proxies...")
		for _, data := range proxyData {
			data.stopProxy()
		}
	}
}

func (r *ServiceReconciler) ensureServiceEffectiveAddressAndPort(ctx context.Context, svc *apiv1.Service, log logr.Logger) objectChange {
	oldPid := svc.Status.ProxyProcessPid
	oldState := svc.Status.State
	oldEffectiveAddress := svc.Status.EffectiveAddress
	oldEffectivePort := svc.Status.EffectivePort
	oldEndpointNamespacedName := types.NamespacedName{
		Namespace: svc.Status.ProxylessEndpointNamespace,
		Name:      svc.Status.ProxylessEndpointName,
	}

	change := noChange

	serviceEndpoints, err := r.getServiceEndpoints(ctx, svc, log)
	if err != nil {
		return additionalReconciliationNeeded
	}

	if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeProxyless {
		// If using Proxyless allocation mode
		if len(serviceEndpoints.Items) == 0 {
			// No Endpoints are available. Empty out the proxyless Endpoint namespace and name, and effective address and port.
			svc.Status.State = apiv1.ServiceStateNotReady
			svc.Status.ProxylessEndpointNamespace = ""
			svc.Status.ProxylessEndpointName = ""
			svc.Status.EffectiveAddress = ""
			svc.Status.EffectivePort = 0
		} else {
			// At least one Endpoint exists.
			svc.Status.State = apiv1.ServiceStateReady

			// If an Endpoint was previously chosen, we need to ensure it is still valid, and if not choose another.
			if svc.Status.ProxylessEndpointName != "" {
				// Ensure the previously chosen Endpoint still exists
				endpointStillExists := slices.Any(serviceEndpoints.Items, func(endpoint apiv1.Endpoint) bool {
					return endpoint.ObjectMeta.Namespace == svc.Status.ProxylessEndpointNamespace && endpoint.ObjectMeta.Name == svc.Status.ProxylessEndpointName
				})

				if !endpointStillExists {
					svc.Status.ProxylessEndpointNamespace = ""
					svc.Status.ProxylessEndpointName = ""
					svc.Status.EffectiveAddress = ""
					svc.Status.EffectivePort = 0
				}
			}

			if svc.Status.ProxylessEndpointNamespace == "" && svc.Status.ProxylessEndpointName == "" {
				// No proxyless Endpoint has been chosen yet (or the chosen one no longer exists), so we need to choose one
				svc.Status.ProxylessEndpointNamespace = serviceEndpoints.Items[0].ObjectMeta.Namespace
				svc.Status.ProxylessEndpointName = serviceEndpoints.Items[0].ObjectMeta.Name
				svc.Status.EffectiveAddress = serviceEndpoints.Items[0].Spec.Address
				svc.Status.EffectivePort = serviceEndpoints.Items[0].Spec.Port
			}
		}
	} else {
		// Else using a regular allocation mode which will use a proxy

		svc.Status.State = apiv1.ServiceStateNotReady

		err = r.startProxyIfNeeded(ctx, svc, log)
		if err != nil {
			log.Error(err, "could not start the proxy")
			return noChange
		} else {
			serviceProxyData, found := r.proxyData.Load(svc.NamespacedName())
			if !found {
				// Should never happen if startProxyIfNeeded() succeeded
				log.Error(errors.New("proxy data not found"), "could not configure the proxy")
				change |= additionalReconciliationNeeded
			} else {
				if len(serviceEndpoints.Items) > 0 {
					svc.Status.State = apiv1.ServiceStateReady
				}

				config := proxy.ProxyConfig{
					Endpoints: []proxy.Endpoint{},
				}

				for _, endpoint := range serviceEndpoints.Items {
					config.Endpoints = append(config.Endpoints, proxy.Endpoint{
						Address: endpoint.Spec.Address,
						Port:    endpoint.Spec.Port,
					})
				}

				for _, proxyInstanceData := range serviceProxyData {
					err = proxyInstanceData.proxy.Configure(config)
					if err != nil {
						log.Error(err, "could not configure the proxy")
					}
				}
			}
		}
	}

	if svc.Status.ProxyProcessPid != oldPid {
		if svc.Status.ProxyProcessPid != apiv1.UnknownPID {
			log.V(1).Info(fmt.Sprintf("proxy process has been started for service %s (PID %d)", svc.NamespacedName(), *svc.Status.ProxyProcessPid))
		}
		change |= statusChanged
	}

	if svc.Status.State != oldState {
		// If the log level is info, we'll only log when the service becomes ready
		logLevel, loggerErr := logger.GetDebugLogLevel()
		if loggerErr == nil && logLevel == zapcore.DebugLevel {
			log.V(1).Info(fmt.Sprintf("service %s is now in state %s", svc.NamespacedName(), svc.Status.State))
		} else if svc.Status.State == apiv1.ServiceStateReady {
			log.Info(fmt.Sprintf("service %s is now in state %s", svc.NamespacedName(), svc.Status.State))
		}

		change |= statusChanged
	}

	if svc.Spec.AddressAllocationMode != apiv1.AddressAllocationModeProxyless && (svc.Status.EffectiveAddress != oldEffectiveAddress || svc.Status.EffectivePort != oldEffectivePort) {
		log.V(1).Info(fmt.Sprintf("service %s is now running on %s:%d", svc.NamespacedName(), svc.Status.EffectiveAddress, svc.Status.EffectivePort))
		change |= statusChanged
	}

	if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeProxyless && (svc.Status.ProxylessEndpointNamespace != oldEndpointNamespacedName.Namespace || svc.Status.ProxylessEndpointName != oldEndpointNamespacedName.Name) {
		if svc.Status.EffectiveAddress != "" || svc.Status.EffectivePort != 0 {
			log.V(1).Info(fmt.Sprintf("proxyless service %s is now running on %s:%d", svc.NamespacedName(), svc.Status.EffectiveAddress, svc.Status.EffectivePort))
		}
		change |= statusChanged
	}

	return change
}

func (r *ServiceReconciler) getServiceEndpoints(ctx context.Context, svc *apiv1.Service, log logr.Logger) (apiv1.EndpointList, error) {
	var serviceEndpoints apiv1.EndpointList
	if err := r.List(ctx, &serviceEndpoints, ctrl_client.MatchingFields{".metadata.serviceNamespace": svc.ObjectMeta.Namespace}, ctrl_client.MatchingFields{".metadata.serviceName": svc.ObjectMeta.Name}); err != nil {
		log.Error(err, "could not get associated endpoints")
		return apiv1.EndpointList{}, fmt.Errorf("could not get associated endpoints: %w", err)
	} else {
		return serviceEndpoints, nil
	}
}

// startProxyIfNeeded starts a proxy process if needed for the given service.
// It returns the error if any.
func (r *ServiceReconciler) startProxyIfNeeded(ctx context.Context, svc *apiv1.Service, log logr.Logger) error {
	requestedServiceAddress, err := getRequestedServiceAddress(svc)
	if err != nil {
		return err
	}

	serviceProxyData, found := r.proxyData.Load(svc.NamespacedName())
	if found {
		svc.Status.EffectiveAddress, svc.Status.EffectivePort = r.getEffectiveAddressAndPort(serviceProxyData, requestedServiceAddress)
		return nil
	}

	// Reset the overall status for the service
	svc.Status.ProxyProcessPid = apiv1.UnknownPID
	svc.Status.EffectiveAddress = ""
	svc.Status.EffectivePort = 0

	proxies, portAllocationErr := r.getProxyData(svc, requestedServiceAddress, log)

	stopAllProxies := func() {
		for _, proxyInstanceData := range proxies {
			proxyInstanceData.stopProxy()
		}
	}

	if portAllocationErr != nil {
		stopAllProxies()
		return fmt.Errorf("cound not create the proxy for the service: %w", portAllocationErr)
	}

	if !r.noProxyStartOption() {
		for _, proxyInstanceData := range proxies {
			err = proxyInstanceData.proxy.Start()
			if err != nil {
				stopAllProxies()
				return fmt.Errorf("cound not start the proxy for the service: %w", err)
			}
		}
	}

	svc.Status.EffectiveAddress, svc.Status.EffectivePort = r.getEffectiveAddressAndPort(proxies, requestedServiceAddress)
	log.V(1).Info("service proxy started",
		"EffectiveAddress", svc.Status.EffectiveAddress,
		"EffectivePort", svc.Status.EffectivePort,
	)

	r.proxyData.Store(svc.NamespacedName(), proxies)

	return nil
}

// Allocates (but does not start) proxy instances for the given service.
func (r *ServiceReconciler) getProxyData(svc *apiv1.Service, requestedServiceAddress string, log logr.Logger) ([]proxyInstanceData, error) {
	proxies := []proxyInstanceData{}
	var getProxyPort func(proxyAddress string) (int32, error)
	if svc.Spec.Port == 0 {
		getProxyPort = func(proxyAddress string) (int32, error) {
			return networking.GetFreePort(svc.Spec.Protocol, proxyAddress)
		}
	} else {
		getProxyPort = func(_ string) (int32, error) {
			return svc.Spec.Port, nil
		}
	}

	// We do not want to use the passed-in logger for the proxy because it has reconciliation-specific data
	// which does not make sense in the context of the proxy.
	// The resulting log name will be "dcpctrl.ServiceReconciler.Proxy".
	proxyLog := r.Log.WithName("Proxy").WithValues("Service", svc.NamespacedName())

	var portAllocationErr error = nil

	if requestedServiceAddress == "localhost" {
		// Bind to all applicable IPs (IPv4 and IPv6) for the proxy address

		ips, err := networking.LookupIP(requestedServiceAddress)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("could not obtain IP address(es) for 'localhost': %w", err)
		}

		// We will try to use the same port for all addresses if possible.
		const invalidPort int32 = 0
		var lastPort int32 = invalidPort
		usingSamePort := true

		for _, ip := range ips {
			proxyInstanceAddress := networking.IpToString(ip)

			if lastPort == invalidPort {
				lastPort, err = getProxyPort(proxyInstanceAddress)
				if err != nil {
					lastPort = invalidPort
					portAllocationErr = errors.Join(portAllocationErr, err)
					continue
				}
			}

			var proxyPort int32

			if usingSamePort {
				proxyPort = lastPort
				err = networking.CheckPortAvailable(svc.Spec.Protocol, proxyInstanceAddress, proxyPort)
				if err != nil {
					usingSamePort = false
					log.Info("could not use the same port for all addresses associated with 'localhost', service will be reachable only using specific IP address", "AttemptedCommonPort", lastPort)
				}
			}

			if !usingSamePort {
				proxyPort, err = getProxyPort(proxyInstanceAddress)
				if err != nil {
					portAllocationErr = errors.Join(portAllocationErr, err)
					continue
				}
			}

			proxyCtx, cancelFunc := context.WithCancel(r.lifetimeCtx)
			proxies = append(proxies, proxyInstanceData{
				proxy:     proxy.NewProxy(svc.Spec.Protocol, proxyInstanceAddress, proxyPort, proxyCtx, proxyLog),
				stopProxy: cancelFunc,
			})
		}
	} else {
		// Bind to just the proxy address

		proxyPort, err := getProxyPort(requestedServiceAddress)
		if err != nil {
			portAllocationErr = err
		} else {
			proxyCtx, cancelFunc := context.WithCancel(r.lifetimeCtx)
			proxies = append(proxies, proxyInstanceData{
				proxy:     proxy.NewProxy(svc.Spec.Protocol, requestedServiceAddress, proxyPort, proxyCtx, proxyLog),
				stopProxy: cancelFunc,
			})
		}
	}

	return proxies, portAllocationErr
}

func (r *ServiceReconciler) noProxyStartOption() bool {
	if v := r.lifetimeCtx.Value(ServiceReconcilerProxyHandling); v != nil {
		if ph, ok := v.(ProxyHandlingOptionValue); ok && ph == DoNotStartProxies {
			return true
		}
	}

	return false
}

func (r *ServiceReconciler) getEffectiveAddressAndPort(proxies []proxyInstanceData, requestedServiceAddress string) (string, int32) {
	if len(proxies) == 0 {
		return "", 0
	}

	var getProxyAddress func(*proxy.Proxy) string
	var getProxyPort func(*proxy.Proxy) int32
	var isEligibleProxy func(*proxy.Proxy) bool

	if r.noProxyStartOption() {
		// This happens only when the reconciler is running under test harness (integration tests).
		// Since the proxy is not actually started, we are going to report the proxy listen address/port
		// (from proxy initialization data) as the effective address/port.
		getProxyAddress = func(p *proxy.Proxy) string { return p.ListenAddress }
		getProxyPort = func(p *proxy.Proxy) int32 { return p.ListenPort }
		isEligibleProxy = func(p *proxy.Proxy) bool { return true }
	} else {
		getProxyAddress = func(p *proxy.Proxy) string { return p.EffectiveAddress }
		getProxyPort = func(p *proxy.Proxy) int32 { return p.EffectivePort }
		isEligibleProxy = func(p *proxy.Proxy) bool { return p.State() == proxy.ProxyStateRunning }
	}

	eligibleProxies := slices.Select(proxies, func(pd proxyInstanceData) bool {
		return isEligibleProxy(pd.proxy)
	})
	if len(eligibleProxies) == 0 {
		return "", 0
	}

	// We might bind to multiple addresses if the address specified by the service spec is "localhost".
	// But we can (and should) still report the effective address as "localhost"
	// if the port used by all addresses is the same.
	portsInUse := make(map[int32]bool)
	for _, pd := range eligibleProxies {
		portsInUse[getProxyPort(pd.proxy)] = true
	}
	if requestedServiceAddress == "localhost" && len(portsInUse) == 1 {
		return "localhost", getProxyPort(eligibleProxies[0].proxy)
	}

	// We give preference to IPv4 because it is the safer default
	// (e.g. host.docker.internal resolves to IPv4 on dual-stack machines).
	eligibleIPv4Proxies := slices.Select(eligibleProxies, func(pd proxyInstanceData) bool {
		return networking.IsIPv4(getProxyAddress(pd.proxy))
	})

	var sourceProxy *proxy.Proxy
	if len(eligibleIPv4Proxies) > 0 {
		sourceProxy = eligibleIPv4Proxies[0].proxy
	} else {
		sourceProxy = eligibleProxies[0].proxy
	}

	return getProxyAddress(sourceProxy), getProxyPort(sourceProxy)
}

func getRequestedServiceAddress(svc *apiv1.Service) (string, error) {
	requestedServiceAddress := svc.Spec.Address

	if requestedServiceAddress == "" {
		if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeLocalhost || svc.Spec.AddressAllocationMode == "" {
			requestedServiceAddress = "localhost"
		} else if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeIPv4ZeroOne {
			requestedServiceAddress = "127.0.0.1"
		} else if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeIPv4Loopback {
			// Use the standard LookupIP implementation because we want to specifically limit to IPv4 addresses
			ips, err := net.LookupIP("localhost")
			if err != nil {
				return "", fmt.Errorf("could not obtain IP address(es) for 'localhost': %w", err)
			}

			// Only select valid IPv4 addresses
			ips = slices.Select(ips, func(ip net.IP) bool {
				return ip.To4() != nil && nettest.SupportsIPv4()
			})

			if len(ips) == 0 {
				return "", fmt.Errorf("could not obtain valid IP address(es) for 'localhost': %w", err)
			}
			requestedServiceAddress = networking.IpToString(ips[rand.Intn(len(ips))])
		} else if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeIPv6ZeroOne {
			requestedServiceAddress = "[::1]"
		} else {
			return "", fmt.Errorf("unsupported address allocation mode: %s", svc.Spec.AddressAllocationMode)
		}
	}

	return requestedServiceAddress, nil
}
