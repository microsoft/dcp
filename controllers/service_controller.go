// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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
)

type ServiceReconciler struct {
	ctrl_client.Client
	Log            logr.Logger
	ProxyConfigDir string
}

var (
	serviceFinalizer string = fmt.Sprintf("%s/service-reconciler", apiv1.GroupVersion.Group)
)

func NewServiceReconciler(client ctrl_client.Client, log logr.Logger) *ServiceReconciler {
	r := ServiceReconciler{
		Client:         client,
		Log:            log,
		ProxyConfigDir: filepath.Join(os.TempDir(), "usvc-servicecontroller-serviceconfig"),
	}
	return &r
}

func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &apiv1.Endpoint{}, ".metadata.serviceController", func(rawObj ctrl_client.Object) []string {
		endpoint := rawObj.(*apiv1.Endpoint)
		return []string{endpoint.Spec.Service}
	}); err != nil {
		r.Log.Error(err, "failed to create index for Endpoint")
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.Service{}).
		Watches(&apiv1.Endpoint{}, handler.EnqueueRequestsFromMapFunc(requestReconcileForEndpoint), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Complete(r)
}

func requestReconcileForEndpoint(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	endpoint := obj.(*apiv1.Endpoint)
	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Namespace: endpoint.Namespace,
				Name:      endpoint.Spec.Service,
			},
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
	err := r.Get(ctx, req.NamespacedName, &svc)

	if err != nil {
		if apimachinery_errors.IsNotFound(err) {
			log.Info("the Service object does not exist yet or was deleted")
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to Get() the Service object")
			return ctrl.Result{}, err
		}
	}

	var change objectChange
	patch := ctrl_client.MergeFrom(svc.DeepCopy())

	if svc.DeletionTimestamp != nil && !svc.DeletionTimestamp.IsZero() {
		log.Info("Service object is being deleted")
		err := r.deleteService(ctx, svc, log)
		if err != nil {
			// deleteService() logged the error already
			change = additionalReconciliationNeeded
		} else {
			change = deleteFinalizer(&svc, serviceFinalizer)
		}
	} else {
		change = ensureFinalizer(&svc, serviceFinalizer)
		change |= r.ensureService(ctx, svc, log)
	}

	if (change & (metadataChanged | specChanged)) != 0 {
		err = r.Patch(ctx, &svc, patch)
		if err != nil {
			log.Error(err, "Service object update failed")
			return ctrl.Result{}, err
		} else {
			log.Info("Service object update succeeded")
		}
	}

	if (change & additionalReconciliationNeeded) != 0 {
		return ctrl.Result{RequeueAfter: additionalReconciliationDelay}, nil
	} else {
		return ctrl.Result{}, nil
	}
}

func (r *ServiceReconciler) deleteService(ctx context.Context, svc apiv1.Service, log logr.Logger) error {
	if err := r.stopProxyIfNeeded(ctx); err != nil {
		log.Error(err, "could not start the proxy")
		return err
	}

	if err := r.deleteServiceConfigFile(svc.ObjectMeta.Name); err != nil {
		log.Error(err, "could not delete the service config file")
		return err
	}

	return nil
}

func (r *ServiceReconciler) ensureService(ctx context.Context, svc apiv1.Service, log logr.Logger) objectChange {
	serviceName := strings.TrimSpace(svc.ObjectMeta.Name)
	if serviceName == "" {
		log.Error(fmt.Errorf("specified service name is empty"), "")

		// Hopefully someone will notice the error and update the Spec.
		// Once the Spec is changed, another reconciliation will kick in automatically.
		return noChange
	}

	if err := r.startProxyIfNeeded(ctx); err != nil {
		log.Error(err, "could not start the proxy")
		return additionalReconciliationNeeded
	}

	var serviceEndpoints apiv1.EndpointList
	if err := r.List(ctx, &serviceEndpoints, ctrl_client.InNamespace(svc.Namespace), ctrl_client.MatchingFields{".metadata.serviceController": serviceName}); err != nil {
		log.Error(err, "could not get associated endpoints")
		return additionalReconciliationNeeded
	}

	if err := r.ensureServiceConfigFile(serviceName, &serviceEndpoints); err != nil {
		log.Error(err, "could not write service config file")
		return additionalReconciliationNeeded
	}

	log.Info("service created")
	return noChange
}

func (r *ServiceReconciler) startProxyIfNeeded(ctx context.Context) error {
	return nil
}

func (r *ServiceReconciler) stopProxyIfNeeded(ctx context.Context) error {
	return nil
}

func (r *ServiceReconciler) getServiceConfigFilePath(serviceName string) string {
	return filepath.Join(r.ProxyConfigDir, fmt.Sprintf("%s.yaml", serviceName))
}

func (r *ServiceReconciler) ensureServiceConfigFile(serviceName string, endpoints *apiv1.EndpointList) error {
	svcConfigFilePath := r.getServiceConfigFilePath(serviceName)

	if err := ensureDir(filepath.Dir(svcConfigFilePath)); err != nil {
		return err
	}

	return writeObjectYamlToFile(svcConfigFilePath, struct{ foo string }{foo: "bar"})
}

func (r *ServiceReconciler) deleteServiceConfigFile(name string) error {
	configFilePath := r.getServiceConfigFilePath(name)

	// Remove the config file
	if err := os.Remove(configFilePath); errors.Is(err, fs.ErrNotExist) {
		// No problem, we want it to not exist
		return nil
	} else if err != nil {
		return err
	}

	// If the directory is now empty, remove it
	if isConfigDirEmpty, err := isEmptyDir(r.ProxyConfigDir); err != nil {
		return err
	} else if isConfigDirEmpty {
		if err := os.Remove(r.ProxyConfigDir); errors.Is(err, fs.ErrNotExist) {
			// No problem, we want it to not exist
			return nil
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
		return os.MkdirAll(dir, 0755)
	}

	return nil
}

func writeObjectYamlToFile(fileName string, data interface{}) error {
	yamlContent, err := yaml.Marshal(data)
	if err != nil {
		return err
	}

	return os.WriteFile(fileName, yamlContent, 0644)
}
