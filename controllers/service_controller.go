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
	"time"

	"github.com/go-logr/logr"
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
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

type proxyInstanceData struct {
	proxy     proxy.Proxy
	stopProxy context.CancelFunc
}

type serviceData struct {
	proxies       []proxyInstanceData
	startAttempts uint32
}

// Stores ServiceReconciler dependencies and configuration that often varies
// between normal execution and testing.
type ServiceReconcilerConfig struct {
	ProcessExecutor               process.Executor
	CreateProxy                   proxy.ProxyFactory
	AdditionalReconciliationDelay time.Duration
}

type ServiceReconciler struct {
	ctrl_client.Client
	Log                           logr.Logger
	reconciliationSeqNo           uint32
	ProcessExecutor               process.Executor
	ProxyConfigDir                string
	serviceInfo                   *syncmap.Map[types.NamespacedName, *serviceData]
	createProxy                   proxy.ProxyFactory
	additionalReconciliationDelay time.Duration

	// Channel used to trigger reconciliation function when underlying run status changes.
	notifyProxyRunChanged *concurrency.UnboundedChan[ctrl_event.GenericEvent]

	lifetimeCtx context.Context

	tracer trace.Tracer
}

const (
	// With the default additional reconciliation delay of 2 seconds, this results in approximately 60 seconds
	// of the ServiceReconciler attempting to start the service (proxies) before giving up.
	// A typical failure is associated with port conflict, which may be transient.
	MaxServiceStartAttempts = 30
)

var (
	serviceFinalizer string = fmt.Sprintf("%s/service-reconciler", apiv1.GroupVersion.Group)
)

func NewServiceReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	log logr.Logger,
	config ServiceReconcilerConfig,
) *ServiceReconciler {
	if config.ProcessExecutor == nil {
		panic("ProcessExecutor is required")
	}

	r := ServiceReconciler{
		Client:                client,
		ProcessExecutor:       config.ProcessExecutor,
		ProxyConfigDir:        filepath.Join(usvc_io.DcpTempDir(), "usvc-servicecontroller-serviceconfig"),
		serviceInfo:           &syncmap.Map[types.NamespacedName, *serviceData]{},
		notifyProxyRunChanged: concurrency.NewUnboundedChan[ctrl_event.GenericEvent](lifetimeCtx),
		lifetimeCtx:           lifetimeCtx,
		tracer:                telemetry.GetTelemetrySystem().TracerProvider.Tracer("service-controller"),
		Log:                   log,
	}

	if config.CreateProxy == nil {
		r.createProxy = proxy.NewRuntimeProxy
	} else {
		r.createProxy = config.CreateProxy
	}

	if config.AdditionalReconciliationDelay != 0 {
		r.additionalReconciliationDelay = config.AdditionalReconciliationDelay
	} else {
		r.additionalReconciliationDelay = defaultAdditionalReconciliationDelay
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
			r.stopService(req.NamespacedName, log)
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
		r.stopService(svc.NamespacedName(), log)
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

	result, err := saveChangesWithCustomReconciliationDelay(
		r.Client,
		ctx,
		&svc,
		patch,
		change,
		r.additionalReconciliationDelay,
		nil,
		log,
	)
	return result, err
}

func (r *ServiceReconciler) stopService(svcName types.NamespacedName, log logr.Logger) {
	serviceData, found := r.serviceInfo.LoadAndDelete(svcName)
	if !found || len(serviceData.proxies) == 0 {
		return
	}
	stopProxies(serviceData.proxies, log)
}

func (r *ServiceReconciler) ensureServiceEffectiveAddressAndPort(ctx context.Context, svc *apiv1.Service, log logr.Logger) objectChange {
	oldState := svc.Status.State
	oldEffectiveAddress := svc.Status.EffectiveAddress
	oldEffectivePort := svc.Status.EffectivePort
	oldEndpointNamespacedName := types.NamespacedName{
		Namespace: svc.Status.ProxylessEndpointNamespace,
		Name:      svc.Status.ProxylessEndpointName,
	}

	change := noChange

	serviceEndpoints, serviceEndpointsErr := r.getServiceEndpoints(ctx, svc, log)
	if serviceEndpointsErr != nil {
		// Service endpoint retrieval is just a read from K8s API server, if it fails, we will retry indefinitely,
		// it does not count as a service (proxy) startup attempt.
		return additionalReconciliationNeeded
	}

	if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeProxyless {
		// Using Proxyless allocation mode
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
		// Using regular allocation mode with proxies

		svc.Status.State = apiv1.ServiceStateNotReady

		psd, proxyStartErr := r.startProxyIfNeeded(ctx, svc, log)
		if proxyStartErr != nil {
			if psd.startAttempts >= MaxServiceStartAttempts {
				log.Error(proxyStartErr, "could not start the proxy")
				if oldState != apiv1.ServiceStateNotReady {
					return statusChanged
				} else {
					return noChange
				}
			} else {
				log.V(1).Info("could not start the proxy, will retry", "Attempt", psd.startAttempts, "Error", proxyStartErr.Error())
				return additionalReconciliationNeeded
			}
		}

		if len(serviceEndpoints.Items) > 0 {
			myPid := int64(os.Getpid())
			pointers.SetValue(&svc.Status.ProxyProcessPid, &myPid)
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

		for _, proxyInstanceData := range psd.proxies {
			configErr := proxyInstanceData.proxy.Configure(config)
			if configErr != nil {
				log.Error(configErr, "could not configure the proxy")
			}
		}
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
		log.V(1).Info(fmt.Sprintf("service %s is now running on %s",
			svc.NamespacedName(),
			networking.AddressAndPort(svc.Status.EffectiveAddress, svc.Status.EffectivePort)),
		)
		change |= statusChanged
	}

	if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeProxyless && (svc.Status.ProxylessEndpointNamespace != oldEndpointNamespacedName.Namespace || svc.Status.ProxylessEndpointName != oldEndpointNamespacedName.Name) {
		if svc.Status.EffectiveAddress != "" || svc.Status.EffectivePort != 0 {
			log.V(1).Info(fmt.Sprintf("proxyless service %s is now running on %s",
				svc.NamespacedName(),
				networking.AddressAndPort(svc.Status.EffectiveAddress, svc.Status.EffectivePort)),
			)
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

// startProxyIfNeeded() starts all necessary proxies for the given service.
// No matter if an error occurs, a *serviceData is returned that is used for storing proxy information
// and count startup attempts for the service. The service data is stored in ServiceReconciler.serviceInfo
// before the method returns.
func (r *ServiceReconciler) startProxyIfNeeded(_ context.Context, svc *apiv1.Service, log logr.Logger) (*serviceData, error) {
	psd, found := r.serviceInfo.Load(svc.NamespacedName())
	if !found {
		psd = &serviceData{}
	}

	requestedServiceAddress, requestedAddressErr := getRequestedServiceAddress(svc)

	if found && requestedAddressErr == nil && len(psd.proxies) > 0 {
		svc.Status.EffectiveAddress, svc.Status.EffectivePort = r.getEffectiveAddressAndPort(psd.proxies, requestedServiceAddress)
		return psd, nil
	}

	// Reset the overall status for the service
	svc.Status.ProxyProcessPid = apiv1.UnknownPID
	svc.Status.EffectiveAddress = ""
	svc.Status.EffectivePort = 0
	stopProxies(psd.proxies, log)
	psd.proxies = nil
	psd.startAttempts += 1
	defer r.serviceInfo.Store(svc.NamespacedName(), psd)

	if requestedAddressErr != nil {
		return psd, requestedAddressErr
	}

	proxies, portAllocationErr := r.getProxyData(svc, requestedServiceAddress, log)
	if portAllocationErr != nil {
		stopProxies(proxies, log)
		return psd, fmt.Errorf("could not create the proxy for the service: port allocation failed: %w", portAllocationErr)
	}

	for _, proxyInstanceData := range proxies {
		startErr := proxyInstanceData.proxy.Start()
		if startErr != nil {
			stopProxies(proxies, log)
			return psd, fmt.Errorf("could not start the proxy for the service: %w", startErr)
		} else {
			log.V(1).Info("service proxy started",
				"ProxyListenAddress", proxyInstanceData.proxy.ListenAddress(),
				"ProxyListenPort", proxyInstanceData.proxy.ListenPort(),
				"ProxyEffectiveAddress", proxyInstanceData.proxy.EffectiveAddress(),
				"ProxyEffectivePort", proxyInstanceData.proxy.EffectivePort(),
			)
		}
	}

	svc.Status.EffectiveAddress, svc.Status.EffectivePort = r.getEffectiveAddressAndPort(proxies, requestedServiceAddress)
	log.V(1).Info("service serving traffic",
		"EffectiveAddress", svc.Status.EffectiveAddress,
		"EffectivePort", svc.Status.EffectivePort,
	)

	psd.proxies = proxies
	return psd, nil
}

// Allocates (but does not start) proxy instances for the given service.
func (r *ServiceReconciler) getProxyData(svc *apiv1.Service, requestedServiceAddress string, log logr.Logger) ([]proxyInstanceData, error) {
	proxies := []proxyInstanceData{}
	var getProxyPort func(proxyAddress string) (int32, error)
	if svc.Spec.Port == 0 {
		getProxyPort = func(proxyAddress string) (int32, error) {
			return networking.GetFreePort(svc.Spec.Protocol, proxyAddress, log)
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

	ips, err := networking.GetPreferredHostIps(requestedServiceAddress)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("there is no available network interface for requested service address: %s", requestedServiceAddress)
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
			err = networking.CheckPortAvailable(svc.Spec.Protocol, proxyInstanceAddress, proxyPort, log)
			if err != nil {
				usingSamePort = false
				log.Info("could not use the same port for all addresses associated with requested service address, service will be reachable only using specific IP address",
					"AttemptedCommonPort", lastPort,
					"RequestedServiceAddress", requestedServiceAddress,
				)
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
			proxy:     r.createProxy(svc.Spec.Protocol, proxyInstanceAddress, proxyPort, proxyCtx, proxyLog),
			stopProxy: cancelFunc,
		})
	}

	return proxies, portAllocationErr
}

func (r *ServiceReconciler) getEffectiveAddressAndPort(proxies []proxyInstanceData, requestedServiceAddress string) (string, int32) {
	if len(proxies) == 0 {
		return "", 0
	}

	eligibleProxies := slices.Select(proxies, func(pd proxyInstanceData) bool {
		return pd.proxy.State() == proxy.ProxyStateRunning
	})
	if len(eligibleProxies) == 0 {
		return "", 0
	}

	// We might bind to multiple addresses if the address specified by the service spec
	// is one of the pseudo-addresses (e.g. "localhost", "0.0.0.0.", or "[::]").

	// For "localhost" we can (and should) still report the effective address as "localhost"
	// if the port used by all addresses is the same.
	if requestedServiceAddress == networking.Localhost {
		portsInUse := make(map[int32]bool)
		for _, pd := range eligibleProxies {
			portsInUse[pd.proxy.EffectivePort()] = true
		}
		if len(portsInUse) == 1 {
			return networking.Localhost, eligibleProxies[0].proxy.EffectivePort()
		}
	}

	var isPreferredAddress func(string) bool
	switch networking.GetIpVersionPreference() {
	case networking.IpVersionPreference4:
		isPreferredAddress = networking.IsIPv4
	case networking.IpVersionPreference6:
		isPreferredAddress = networking.IsIPv6
	default:
		// We give preference to IPv4 because it is the safer default
		// (e.g. host.docker.internal resolves to IPv4 on dual-stack machines).
		isPreferredAddress = networking.IsIPv4
	}
	preferredProxies := slices.Select(eligibleProxies, func(pd proxyInstanceData) bool {
		return isPreferredAddress(pd.proxy.EffectiveAddress())
	})

	var sourceProxy proxy.Proxy
	if len(preferredProxies) > 0 {
		sourceProxy = preferredProxies[0].proxy
	} else {
		sourceProxy = eligibleProxies[0].proxy
	}

	return sourceProxy.EffectiveAddress(), sourceProxy.EffectivePort()
}

func getRequestedServiceAddress(svc *apiv1.Service) (string, error) {
	requestedServiceAddress := svc.Spec.Address

	if requestedServiceAddress == "" {
		if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeLocalhost || svc.Spec.AddressAllocationMode == "" {
			requestedServiceAddress = networking.Localhost
		} else if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeIPv4ZeroOne {
			requestedServiceAddress = networking.IPv4LocalhostDefaultAddress
		} else if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeIPv4Loopback {
			// Use the standard LookupIP implementation because we want to specifically limit to IPv4 addresses
			ips, err := net.LookupIP(networking.Localhost)
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
			requestedServiceAddress = networking.IPv6LocalhostDefaultAddress
		} else {
			return "", fmt.Errorf("unsupported address allocation mode: %s", svc.Spec.AddressAllocationMode)
		}
	}

	return requestedServiceAddress, nil
}

func stopProxies(proxies []proxyInstanceData, log logr.Logger) {
	if len(proxies) > 0 {
		log.V(1).Info("stopping all proxies...")
		for _, proxyInstanceData := range proxies {
			proxyInstanceData.stopProxy()
		}
	}
}
