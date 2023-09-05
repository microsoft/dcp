// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/go-logr/logr"
	"gopkg.in/yaml.v3"
	apimachinery_errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/dcp/dcppaths"
	"github.com/microsoft/usvc-apiserver/internal/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/networking"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

type ServiceReconciler struct {
	ctrl_client.Client
	Log             logr.Logger
	ProcessExecutor process.Executor
	ProxyConfigDir  string
}

var (
	serviceFinalizer string = fmt.Sprintf("%s/service-reconciler", apiv1.GroupVersion.Group)
)

func NewServiceReconciler(client ctrl_client.Client, log logr.Logger, processExecutor process.Executor) *ServiceReconciler {
	r := ServiceReconciler{
		Client:          client,
		Log:             log,
		ProcessExecutor: processExecutor,
		ProxyConfigDir:  filepath.Join(os.TempDir(), "usvc-servicecontroller-serviceconfig"),
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

	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.Service{}).
		Watches(&apiv1.Endpoint{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForEndpoint), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Complete(r)
}

func (r *ServiceReconciler) requestReconcileForEndpoint(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	endpoint := obj.(*apiv1.Endpoint)
	serviceNamespaceName := types.NamespacedName{
		Namespace: endpoint.Spec.ServiceNamespace,
		Name:      endpoint.Spec.ServiceName,
	}

	r.Log.V(1).Info("endpoint updated, requesting service reconciliation", "endpoint", endpoint, "serviceName", serviceNamespaceName)
	return []reconcile.Request{
		{
			NamespacedName: serviceNamespaceName,
		},
	}
}

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("ServiceName", req.NamespacedName)

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
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to Get() the Service object")
			return ctrl.Result{}, err
		}
	}

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(svc.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if svc.DeletionTimestamp != nil && !svc.DeletionTimestamp.IsZero() {
		log.Info("Service object is being deleted")
		_ = r.deleteService(ctx, &svc, log) // Best effort. Errors will be logged by deleteService().
		change = deleteFinalizer(&svc, serviceFinalizer)
		// Removing the finalizer will unblock the deletion of the ExecutableReplicaSet object.
		// Status update will fail, because the object will no longer be there, so suppress it.
		change &= ^statusChanged
	} else {
		change = ensureFinalizer(&svc, serviceFinalizer)
		// If we added a finalizer, we'll do the additional reconciliation next call
		if change == noChange {
			change |= r.ensureServiceProxyStarted(ctx, &svc, log)
		}
	}

	result, err := saveChanges(r.Client, ctx, &svc, patch, change, log)
	return result, err
}

func (r *ServiceReconciler) deleteService(ctx context.Context, svc *apiv1.Service, log logr.Logger) error {
	if err := r.stopProxyIfNeeded(ctx, svc); err != nil {
		log.Error(err, "could not stop the proxy")
		return err
	}

	err := r.deleteServiceConfigFile(svc.ObjectMeta.Name)
	if err != nil {
		log.Error(err, "could not delete the service config file")
		return err
	}

	return nil
}

func (r *ServiceReconciler) ensureServiceProxyStarted(ctx context.Context, svc *apiv1.Service, log logr.Logger) objectChange {
	serviceNamespace := svc.ObjectMeta.Namespace
	serviceName := svc.ObjectMeta.Name
	oldPid := svc.Status.ProxyProcessPid
	oldState := svc.Status.State

	var serviceEndpoints apiv1.EndpointList
	if err := r.List(ctx, &serviceEndpoints, ctrl_client.MatchingFields{".metadata.serviceNamespace": serviceNamespace}, ctrl_client.MatchingFields{".metadata.serviceName": serviceName}); err != nil {
		log.Error(err, "could not get associated endpoints")
		return additionalReconciliationNeeded
	}

	change := noChange

	if len(serviceEndpoints.Items) == 0 {
		svc.Status.State = apiv1.ServiceStateNotReady
	} else {
		svc.Status.State = apiv1.ServiceStateReady
	}

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

	if err := r.startProxyIfNeeded(ctx, svc); err != nil {
		log.Error(err, "could not start the proxy")
		change |= additionalReconciliationNeeded
	} else if svc.Status.ProxyProcessPid != oldPid || svc.Status.State != oldState {
		change |= statusChanged
	}

	if svc.Status.ProxyProcessPid != oldPid {
		log.Info(fmt.Sprintf("proxy process has been started for service %s", serviceName))
	}

	if svc.Status.State != oldState {
		log.Info(fmt.Sprintf("service %s is now in state %s", serviceName, svc.Status.State))
	}

	return change
}

func (r *ServiceReconciler) startProxyIfNeeded(ctx context.Context, svc *apiv1.Service) error {
	if svc.Status.ProxyProcessPid != 0 {
		return nil
	}

	proxyAddress := svc.Spec.Address
	if proxyAddress == "" {
		if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeIPv4ZeroOne || svc.Spec.AddressAllocationMode == "" {
			proxyAddress = "127.0.0.1"
		} else if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeIPv4Loopback {
			proxyAddress = fmt.Sprintf("127.%d.%d.%d", rand.Intn(254)+1, rand.Intn(254)+1, rand.Intn(254)+1)
		} else if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeIPv6ZeroOne {
			proxyAddress = "[::1]"
		} else {
			return fmt.Errorf("unsupported address allocation mode: %s", svc.Spec.AddressAllocationMode)
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

	cmd := exec.CommandContext(ctx,
		proxyExecutable,
		fmt.Sprintf("--providers.file.filename=%s", svc.Status.ProxyConfigFile),
		"--providers.file.watch=true",
		"--log.level=INFO",
		"--log.format=common",
		fmt.Sprintf("--entryPoints.web.address=%s:%s", proxyAddress, proxyPortString),
	)

	if pid, startWaitForProcessExit, err := r.ProcessExecutor.StartProcess(ctx, cmd, nil); err != nil {
		return err
	} else {
		svc.Status.ProxyProcessPid = pid
		startWaitForProcessExit()
	}

	svc.Status.EffectiveAddress = proxyAddress
	svc.Status.EffectivePort = proxyPort

	return nil
}

func (r *ServiceReconciler) stopProxyIfNeeded(ctx context.Context, svc *apiv1.Service) error {
	if svc.Status.ProxyProcessPid == 0 {
		return nil
	}

	if err := r.ProcessExecutor.StopProcess(svc.Status.ProxyProcessPid); err != nil {
		return err
	}

	return nil
}

func (r *ServiceReconciler) getServiceConfigFilePath(serviceName string) string {
	return filepath.Join(r.ProxyConfigDir, fmt.Sprintf("%s.yaml", serviceName))
}

func (r *ServiceReconciler) ensureServiceConfigFile(svc *apiv1.Service, endpoints *apiv1.EndpointList) (string, error) {
	serviceName := svc.ObjectMeta.Name
	svcConfigFilePath := r.getServiceConfigFilePath(serviceName)

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

func (r *ServiceReconciler) deleteServiceConfigFile(name string) error {
	configFilePath := r.getServiceConfigFilePath(name)

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
		return os.MkdirAll(dir, osutil.PermissionFileOwnerAll)
	}

	return nil
}

func writeObjectYamlToFile(fileName string, data interface{}) error {
	yamlContent, err := yaml.Marshal(data)
	if err != nil {
		return err
	}

	return os.WriteFile(fileName, yamlContent, osutil.PermissionFile)
}
