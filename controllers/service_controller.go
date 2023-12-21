// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"

	"github.com/go-logr/logr"
	"github.com/smallnest/chanx"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap/zapcore"
	"gopkg.in/yaml.v3"
	apimachinery_errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	"github.com/microsoft/usvc-apiserver/internal/dcp/dcppaths"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/internal/osutil"
	"github.com/microsoft/usvc-apiserver/internal/telemetry"
	ourio "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

type ProxyProcessStatus int32

const (
	ProxyProcessStatusRunning ProxyProcessStatus = 0
	ProxyProcessStatusExited  ProxyProcessStatus = 1
)

type serviceProxyData struct {
	Status  ProxyProcessStatus
	Address string
	Port    int32
}

type ServiceReconciler struct {
	ctrl_client.Client
	Log                 logr.Logger
	reconciliationSeqNo uint32
	ProcessExecutor     process.Executor
	ProxyConfigDir      string
	proxyData           *maps.SynchronizedDualKeyMap[types.NamespacedName, process.Pid_t, serviceProxyData]

	// Channel used to trigger reconciliation function when underlying run status changes.
	notifyProxyRunChanged *chanx.UnboundedChan[ctrl_event.GenericEvent]

	// Debouncer used to schedule reconciliations. Extra data carried is the finished PID.
	debouncer *reconcilerDebouncer[process.Pid_t]

	tracer trace.Tracer
}

var (
	serviceFinalizer string = fmt.Sprintf("%s/service-reconciler", apiv1.GroupVersion.Group)
)

func NewServiceReconciler(lifetimeCtx context.Context, client ctrl_client.Client, log logr.Logger, processExecutor process.Executor) *ServiceReconciler {
	r := ServiceReconciler{
		Client:                client,
		Log:                   log,
		ProcessExecutor:       processExecutor,
		ProxyConfigDir:        filepath.Join(os.TempDir(), "usvc-servicecontroller-serviceconfig"),
		proxyData:             maps.NewSynchronizedDualKeyMap[types.NamespacedName, process.Pid_t, serviceProxyData](),
		notifyProxyRunChanged: chanx.NewUnboundedChan[ctrl_event.GenericEvent](lifetimeCtx, 1),
		debouncer:             newReconcilerDebouncer[process.Pid_t](reconciliationDebounceDelay),
		tracer:                telemetry.GetTelemetrySystem().TracerProvider.Tracer("service-controller"),
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

	src := ctrl_source.Channel{
		Source: r.notifyProxyRunChanged.Out,
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.Service{}).
		Watches(&apiv1.Endpoint{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForEndpoint), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(&src, &handler.EnqueueRequestForObject{}).
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

	select {
	case _, isOpen := <-ctx.Done():
		if !isOpen {
			log.Info("Request context expired, nothing to do...")
			return ctrl.Result{}, nil
		}
	default: // not done, proceed
	}

	svc := apiv1.Service{}
	if err := r.Get(ctx, req.NamespacedName, &svc); err != nil {
		if apimachinery_errors.IsNotFound(err) {
			log.Info("the Service object does not exist yet or was deleted")
			r.proxyData.DeleteByFirstKey(req.NamespacedName)
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to Get() the Service object")
			return ctrl.Result{}, err
		}
	}

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(svc.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	r.proxyData.RunDeferredOps(req.NamespacedName)

	if svc.DeletionTimestamp != nil && !svc.DeletionTimestamp.IsZero() {
		log.Info("Service object is being deleted")

		_ = r.deleteService(ctx, &svc, log) // Best effort. Errors will be logged by deleteService().
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

func (r *ServiceReconciler) deleteService(ctx context.Context, svc *apiv1.Service, log logr.Logger) error {
	r.proxyData.DeleteByFirstKey(svc.NamespacedName())

	if err := r.stopProxyIfNeeded(ctx, svc); err != nil {
		log.Error(err, "could not stop the proxy")
		return err
	}

	err := r.deleteServiceConfigFile(svc)
	if err != nil {
		log.Error(err, "could not delete the service config file")
		return err
	}

	return nil
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
			if svc.Status.ProxylessEndpointNamespace != "" && svc.Status.ProxylessEndpointName != "" {
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

			if svc.Status.ProxylessEndpointNamespace == "" || svc.Status.ProxylessEndpointName == "" {
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

		if proxyCfgFile, err := r.ensureServiceConfigFile(svc, &serviceEndpoints); err != nil {
			log.Error(err, "could not write service config file")
			return additionalReconciliationNeeded
		} else {
			if svc.Status.ProxyConfigFile != proxyCfgFile {
				log.V(1).Info("service proxy config file created", "file", proxyCfgFile)
				svc.Status.ProxyConfigFile = proxyCfgFile
				change |= statusChanged
			}
		}

		err := r.startProxyIfNeeded(ctx, svc, log)
		if err != nil {
			log.Error(err, "could not start the proxy")
			change |= additionalReconciliationNeeded
		} else {
			_, data, found := r.proxyData.FindByFirstKey(svc.NamespacedName())
			if found && len(serviceEndpoints.Items) > 0 && data.Status == ProxyProcessStatusRunning {
				svc.Status.State = apiv1.ServiceStateReady
			}
		}
	}

	if svc.Status.ProxyProcessPid != oldPid {
		if svc.Status.ProxyProcessPid != apiv1.UnknownPID {
			log.Info(fmt.Sprintf("proxy process has been started for service %s (PID %d)", svc.NamespacedName(), *svc.Status.ProxyProcessPid))
		}
		change |= statusChanged
	}

	if svc.Status.State != oldState {
		log.Info(fmt.Sprintf("service %s is now in state %s", svc.NamespacedName(), svc.Status.State))
		change |= statusChanged
	}

	if svc.Status.EffectiveAddress != oldEffectiveAddress || svc.Status.EffectivePort != oldEffectivePort {
		log.Info(fmt.Sprintf("service %s is now running on %s:%d", svc.NamespacedName(), svc.Status.EffectiveAddress, svc.Status.EffectivePort))
		change |= statusChanged
	}

	if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeProxyless && (svc.Status.ProxylessEndpointNamespace != oldEndpointNamespacedName.Namespace || svc.Status.ProxylessEndpointName != oldEndpointNamespacedName.Name) {
		if svc.Status.EffectiveAddress != "" || svc.Status.EffectivePort != 0 {
			log.Info(fmt.Sprintf("service %s is now running on %s:%d", svc.NamespacedName(), svc.Status.EffectiveAddress, svc.Status.EffectivePort))
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
	_, proxyData, found := r.proxyData.FindByFirstKey(svc.NamespacedName())

	if found && proxyData.Status == ProxyProcessStatusRunning {
		// Make sure the status reflects the real world
		svc.Status.EffectivePort = proxyData.Port
		svc.Status.EffectiveAddress = proxyData.Address
		return nil
	}

	// Need to start a new proxy process but there might be some cleanup required
	// from previous run.
	if !found && svc.Status.ProxyProcessPid != apiv1.UnknownPID {
		pidFromStatus, err := process.Int64ToPidT(*svc.Status.ProxyProcessPid)
		if err == nil {
			log.Info("attempting to stop the untracked proxy process referenced by service status", "PID", pidFromStatus)
			_ = r.ProcessExecutor.StopProcess(pidFromStatus)
		}
	}

	// Reset the overall status for the service
	r.proxyData.DeleteByFirstKey(svc.NamespacedName())
	svc.Status.ProxyProcessPid = apiv1.UnknownPID
	svc.Status.EffectiveAddress = ""
	svc.Status.EffectivePort = 0

	proxyAddress := svc.Spec.Address
	if proxyAddress == "" {
		if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeLocalhost || svc.Spec.AddressAllocationMode == "" {
			proxyAddress = "localhost"
		} else if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeIPv4ZeroOne {
			proxyAddress = "127.0.0.1"
		} else if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeIPv4Loopback {
			proxyAddress = fmt.Sprintf("127.%d.%d.%d", rand.Intn(254)+1, rand.Intn(254)+1, rand.Intn(254)+1)
		} else if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeIPv6ZeroOne {
			proxyAddress = "[::1]"
		} else {
			return fmt.Errorf("unsupported address allocation mode: %s", svc.Spec.AddressAllocationMode)
		}
	}

	var err error
	proxyPort := svc.Spec.Port
	if proxyPort == 0 {
		// There is a chance that by the time the proxy starts, the port will no longer be free,
		// but this is relatively low. If that happens, the proxy will immediately shut down.
		// TODO: mitigate that by retrying with a different port
		proxyPort, err = networking.GetFreePort(svc.Spec.Protocol, proxyAddress)
		if err != nil {
			return err
		}
	}

	var proxyPortString string
	if svc.Spec.Protocol == apiv1.UDP {
		proxyPortString = fmt.Sprintf("%d/udp", proxyPort)
	} else {
		proxyPortString = fmt.Sprintf("%d", proxyPort)
	}

	args := []string{}
	var newArg string

	if proxyAddress == "localhost" {
		// Bind to all applicable IPs (IPv4 and IPv6) for the proxy address
		ips, err := net.LookupIP(proxyAddress)
		if err != nil {
			return fmt.Errorf("could not obtain IP address(es) for %s: %w", proxyAddress, err)
		}
		if len(ips) == 0 {
			return fmt.Errorf("could not obtain IP address(es) for %s", proxyAddress)
		}

		for _, ip := range ips {
			if ip4 := ip.To4(); len(ip4) == net.IPv4len {
				newArg = fmt.Sprintf("--entryPoints.web.address=%s:%s", ip4.String(), proxyPortString)
			} else if ip6 := ip.To16(); len(ip6) == net.IPv6len {
				newArg = fmt.Sprintf("--entryPoints.webipv6.address=[%s]:%s", ip6.String(), proxyPortString)
			} else {
				// Fallback to just the IP address as a raw string
				newArg = fmt.Sprintf("--entryPoints.web.address=%s:%s", ip.String(), proxyPortString)
			}
			args = append(args, newArg)
		}
	} else {
		// Bind to just the proxy address
		if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeIPv6ZeroOne {
			newArg = fmt.Sprintf("--entryPoints.webipv6.address=%s:%s", proxyAddress, proxyPortString)
		} else {
			newArg = fmt.Sprintf("--entryPoints.web.address=%s:%s", proxyAddress, proxyPortString)
		}
		args = append(args, newArg)
	}

	args = append(args,
		fmt.Sprintf("--providers.file.filename=%s", svc.Status.ProxyConfigFile),
		"--providers.file.watch=true",
	)

	// Enable Traefik's DEBUG level logging if DCP itself is doing DEBUG logging
	if logLevel, err := logger.GetDebugLogLevel(); err == nil && logLevel <= zapcore.DebugLevel {
		if logFolder, err := logger.EnsureDetailedLogsFolder(); err == nil {
			logFilePath := filepath.Join(logFolder, fmt.Sprintf("%s.log", svc.ObjectMeta.Name))
			args = append(args,
				fmt.Sprintf("--log.filePath=%s", logFilePath),
				"--log.level=DEBUG",
				"--log.format=common",
			)
		}
	}

	binDir, err := dcppaths.GetDcpBinDir()
	if err != nil {
		return err
	}
	var proxyExecutable string
	if runtime.GOOS == "windows" {
		proxyExecutable = filepath.Join(binDir, "traefik.exe")
	} else {
		proxyExecutable = filepath.Join(binDir, "traefik")
	}

	cmd := exec.CommandContext(ctx,
		proxyExecutable,
		args...,
	)

	pid, startWaitForProcessExit, err := r.ProcessExecutor.StartProcess(ctx, cmd, r)
	if err != nil {
		return err
	}

	if svc.Status.ProxyProcessPid == apiv1.UnknownPID {
		svc.Status.ProxyProcessPid = new(int64)
	}
	*svc.Status.ProxyProcessPid = int64(pid)

	r.proxyData.Store(svc.NamespacedName(), pid, serviceProxyData{
		Status:  ProxyProcessStatusRunning,
		Address: proxyAddress,
		Port:    proxyPort,
	})
	svc.Status.EffectiveAddress = proxyAddress
	svc.Status.EffectivePort = proxyPort
	startWaitForProcessExit()
	r.Log.Info(fmt.Sprintf("proxy process with PID %d started for service %s", pid, svc.NamespacedName().String()))

	return nil
}

func (r *ServiceReconciler) OnProcessExited(pid process.Pid_t, exitCode int32, err error) {
	namespacedName, _, found := r.proxyData.FindBySecondKey(pid)

	if !found {
		return // Not a process we care about
	}

	r.Log.Info(fmt.Sprintf("proxy process for service %s exited with code %d", namespacedName, exitCode))

	r.proxyData.QueueDeferredOp(
		namespacedName,
		func(proxyData *maps.DualKeyMap[types.NamespacedName, process.Pid_t, serviceProxyData]) {
			_, data, found := proxyData.FindByFirstKey(namespacedName)
			if !found {
				return // Service object may have been deleted already
			}

			data.Status = ProxyProcessStatusExited
			_ = proxyData.Update(namespacedName, pid, data)
		},
	)

	_ = r.debouncer.ReconciliationNeeded(namespacedName, pid, func(rti reconcileTriggerInput[process.Pid_t]) error {
		r.scheduleServiceReconciliation(rti.target, rti.input)
		return nil
	})
}

func (r *ServiceReconciler) scheduleServiceReconciliation(target types.NamespacedName, finishedPid process.Pid_t) {
	event := ctrl_event.GenericEvent{
		Object: &apiv1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      target.Name,
				Namespace: target.Namespace,
			},
		},
	}

	r.notifyProxyRunChanged.In <- event
}

func (r *ServiceReconciler) stopProxyIfNeeded(ctx context.Context, svc *apiv1.Service) error {
	if svc.Status.ProxyProcessPid == apiv1.UnknownPID {
		return nil
	}

	proxyProcessId, err := process.Int64ToPidT(*svc.Status.ProxyProcessPid)
	if err != nil {
		return err
	}
	if err := r.ProcessExecutor.StopProcess(proxyProcessId); err != nil {
		return err
	}

	return nil
}

func (r *ServiceReconciler) getServiceConfigFilePath(svc *apiv1.Service) string {
	return filepath.Join(r.ProxyConfigDir, fmt.Sprintf("%s-%s.yaml", svc.ObjectMeta.Name, svc.ObjectMeta.UID))
}

func (r *ServiceReconciler) ensureServiceConfigFile(svc *apiv1.Service, endpoints *apiv1.EndpointList) (string, error) {
	svcConfigFilePath := r.getServiceConfigFilePath(svc)

	if err := ensureDir(filepath.Dir(svcConfigFilePath)); err != nil {
		return svcConfigFilePath, err
	}

	var proxyConfig interface{}
	if svc.Spec.Protocol == apiv1.UDP {
		proxyConfig = NewUdpProxyConfig(svc, endpoints)
	} else {
		proxyConfig = NewTcpProxyConfig(svc, endpoints)
	}

	return svcConfigFilePath, writeObjectYamlToFile(svcConfigFilePath, proxyConfig)
}

func (r *ServiceReconciler) deleteServiceConfigFile(svc *apiv1.Service) error {
	configFilePath := r.getServiceConfigFilePath(svc)

	// Remove the config file
	if err := os.Remove(configFilePath); errors.Is(err, fs.ErrNotExist) {
		// No problem, we want it to not exist
	} else if err != nil {
		return err
	}

	// If the directory is now empty, remove it
	if isConfigDirEmpty, err := isEmptyDir(r.ProxyConfigDir); err != nil {
		return err
	} else if isConfigDirEmpty {
		if err := os.Remove(r.ProxyConfigDir); errors.Is(err, fs.ErrNotExist) {
			// No problem, we want it to not exist
		} else if err != nil {
			return err
		}
	}

	return nil
}

func isEmptyDir(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()

	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	} else {
		return false, err
	}
}

func ensureDir(dir string) error {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return os.MkdirAll(dir, osutil.PermissionOnlyOwnerReadWriteSetCurrent)
	}

	return nil
}

func writeObjectYamlToFile(fileName string, data interface{}) error {
	yamlContent, err := yaml.Marshal(data)
	if err != nil {
		return err
	}

	return ourio.WriteFile(fileName, yamlContent, osutil.PermissionOwnerReadWriteOthersRead)
}
