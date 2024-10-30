package appmgmt

import (
	"context"
	"os"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/dcpclient"
	"github.com/microsoft/usvc-apiserver/internal/perftrace"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/kubeconfig"
)

func ShutdownApp(ctx context.Context, log logr.Logger) error {
	if len(apiv1.CleanupResources) <= 0 {
		log.Info("No resources to delete")
		return nil
	}

	shutdownCtx, shutdownCtxCancel := context.WithCancel(ctx)
	defer shutdownCtxCancel()

	err := perftrace.CaptureShutdownProfileIfRequested(shutdownCtx, log)
	if err != nil {
		log.Error(err, "failed to capture shutdown profile")
	}

	dcpclient, err := dcpclient.NewClient(shutdownCtx, 5*time.Second)
	if err != nil {
		log.Error(err, "could not get dcpclient")
		return err
	}

	clusterConfig, err := config.GetConfig()
	if err != nil {
		log.Error(err, "could not get config")
		return err
	}

	token, _ := os.LookupEnv(kubeconfig.DCP_SECURE_TOKEN)
	if token != "" {
		// If a token was supplied, use it to authenticate to the API server
		clusterConfig.BearerToken = token
	}

	dynamicClient, err := dynamic.NewForConfig(clusterConfig)
	if err != nil {
		log.Error(err, "could not get client")
		return err
	}

	currentWeight := apiv1.CleanupResources[0].Weight
	batchIndex := 0
	resourceBatches := [][]*apiv1.WeightedResource{}
	for _, resource := range apiv1.CleanupResources {
		// If the context is cancelled, stop the informers
		if ctx.Err() != nil {
			log.Info("Context cancelled, stopping shutdown")
			return ctx.Err()
		}

		if resource.Weight != currentWeight {
			currentWeight = resource.Weight
			batchIndex += 1
		}

		if batchIndex >= len(resourceBatches) {
			resourceBatches = append(resourceBatches, []*apiv1.WeightedResource{})
		}

		resourceBatches[batchIndex] = append(resourceBatches[batchIndex], resource)
		log.Info("Adding resource to current batch", "resource", resource.Object.GetGroupVersionResource(), "length", len(resourceBatches[batchIndex]))
	}

	log.Info("Resource batches generated", "batches", len(resourceBatches))

	for _, resourceBatch := range resourceBatches {
		log.Info("Cleaning up resource batch", "weight", resourceBatch[0].Weight, "length", len(resourceBatch))
		if err = cleanupResourceBatch(shutdownCtx, dcpclient, dynamicClient, resourceBatch, log); err != nil {
			return err
		}
	}

	return nil
}

func cleanupResourceBatch(
	batchCtx context.Context,
	dcpclient client.Client,
	dynamicClient *dynamic.DynamicClient,
	batch []*apiv1.WeightedResource,
	log logr.Logger,
) error {
	if err := batchCtx.Err(); err != nil {
		log.Info("context cancelled, skipping cleanup batch")
		return err
	}

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 20*time.Second)
	// Do not call factory.Shutdown() -- if for whatever reason there are some resources left not deleted,
	// factory.Shutdown() will block forever waiting for the informers to stop.

	resourceCount := &atomic.Int32{}
	cacheSyncDone := make(chan struct{})
	informerFactoryCtx, informerFactoryCtxCancel := context.WithCancel(batchCtx)
	defer informerFactoryCtxCancel()

	for _, resource := range batch {
		gvr := resource.Object.GetGroupVersionResource()
		informer := factory.ForResource(gvr)

		log.Info("shutting down resource", "resource", gvr)
		if _, err := informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				clientObj := obj.(client.Object)
				resourceCount.Add(1)

				parent := metav1.GetControllerOf(clientObj)
				if parent != nil && parent.APIVersion == apiv1.GroupVersion.String() {
					log.Info("resource has a parent, which should handle cleanup", "gvr", gvr, "resource", clientObj.GetName(), "namespace", clientObj.GetNamespace(), "parentKind", parent.Kind, "parentName", parent.Name)
					resourceCount.Add(-1)
					return
				}

				go func() {
					<-cacheSyncDone

					log.Info("deleting resource", "gvr", gvr, "resource", clientObj.GetName(), "namespace", clientObj.GetNamespace())

					// Limit the break between deletion attempts to 5 seconds
					b := backoff.NewExponentialBackOff(backoff.WithMaxInterval(5 * time.Second))

					retryErr := resiliency.Retry(batchCtx, b, func() error {
						// Limit the deletion attempt duration to 20 seconds.
						// This allows us to retry the deletion several times during the shutdown sequence,
						// instead of making one very long attempt.
						deleteCtx, deleteCtxCancel := context.WithTimeout(batchCtx, 20*time.Second)
						defer deleteCtxCancel()

						// In DCP it is parent object controllers that are responsible for cleaning up their children
						// (Endpoint objects owned by Container or Executable object, or ContainerNetworkConnection objects owned by Container object).
						// That is why we do not use deletion propagation policy here.
						err := dcpclient.Delete(deleteCtx, clientObj, client.PropagationPolicy(metav1.DeletePropagationOrphan))
						if err == nil {
							return nil
						}
						if k8serrors.IsNotFound(err) {
							resourceCount.Add(-1)
							return nil
						}
						return err
					})

					if retryErr != nil {
						resourceCount.Add(-1)
						log.Error(retryErr, "could not delete resource", "gvr", gvr, "resource", clientObj.GetName(), "namespace", clientObj.GetNamespace())
					}
				}()
			},
			DeleteFunc: func(obj interface{}) {
				clientObj := obj.(client.Object)
				resourceCount.Add(-1)
				log.Info("resource deleted", "gvr", gvr, "resource", clientObj.GetName(), "namespace", clientObj.GetNamespace())
			},
		}); err != nil {
			return err
		}
	}

	factory.Start(informerFactoryCtx.Done())
	_ = factory.WaitForCacheSync(batchCtx.Done())
	close(cacheSyncDone)

	initialCount := resourceCount.Load()
	if initialCount == 0 {
		log.Info("no resources in batch")
		return nil
	}

	log.Info("waiting for resource batch to be deleted", "initialCount", initialCount)

	const noNewResourcesCheckInterval = 1 * time.Second
	noNewResourcesTimer := time.NewTimer(noNewResourcesCheckInterval)

	for {
		select {

		case <-batchCtx.Done():
			log.Info("shutdown context cancelled, stopping batch cleanup")
			noNewResourcesTimer.Stop()
			return batchCtx.Err()

		case <-noNewResourcesTimer.C:
			if resourceCount.Load() <= 0 {
				log.Info("all known resources deleted, batch cleanup complete")
				return nil
			} else {
				noNewResourcesTimer.Reset(noNewResourcesCheckInterval)
			}
		}
	}
}
