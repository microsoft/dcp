package appmgmt

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/perftrace"
	"github.com/microsoft/usvc-apiserver/pkg/dcpclient"
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

	dcpclient, err := dcpclient.New(shutdownCtx, 5*time.Second)
	if err != nil {
		log.Error(err, "could not get dcpclient")
		return err
	}

	clusterConfig, err := config.GetConfig()
	if err != nil {
		log.Error(err, "could not get config")
		return err
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
	ctx context.Context,
	dcpclient client.Client,
	dynamicClient *dynamic.DynamicClient,
	batch []*apiv1.WeightedResource,
	log logr.Logger,
) error {
	if err := ctx.Err(); err != nil {
		log.Info("context cancelled, skipping cleanup batch")
		return err
	}

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 20*time.Second)
	defer factory.Shutdown()

	// The context for the informers needs to have it's defer cancellation registered AFTER
	// the call to defer factory.Shutdown() or shutdown will block and the informers won't
	// be terminated.
	cleanupCtx, cleanupCtxCancel := context.WithCancel(ctx)
	defer cleanupCtxCancel()

	initialResourceCounts := &atomic.Int32{}
	totalResourceCounts := &atomic.Int32{}

	cacheSyncCh := make(chan struct{})
	for _, resource := range batch {
		gvr := resource.Object.GetGroupVersionResource()
		informer := factory.ForResource(gvr)

		log.Info("shutting down resource", "resource", gvr)
		if _, err := informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				clientObj := obj.(client.Object)
				initialResourceCounts.Add(1)
				totalResources := totalResourceCounts.Add(1)

				parent := metav1.GetControllerOf(clientObj)
				if parent != nil && parent.APIVersion == apiv1.GroupVersion.String() {
					log.Info("resource has a parent, which should handle cleanup", "resource", clientObj, "parent", parent)
					return
				}

				go func(resourceNum int32) {
					<-cacheSyncCh
					log.Info("deleting resource", "resource", gvr, "total", resourceNum)
					if err := dcpclient.Delete(ctx, clientObj, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
						log.Error(err, "could not delete resource", "resource", clientObj)
					}
				}(totalResources)
			},
			DeleteFunc: func(obj interface{}) {
				totalResources := totalResourceCounts.Add(-1)
				log.Info("resource deleted", "resource", gvr, "total", totalResources)

				if totalResources <= 0 {
					log.Info("all resources in batch deleted", "resources", totalResources)
					cleanupCtxCancel()
				}
			},
		}); err != nil {
			return err
		}
	}

	factory.Start(cleanupCtx.Done())
	_ = factory.WaitForCacheSync(cleanupCtx.Done())
	close(cacheSyncCh)

	count := initialResourceCounts.Load()
	log.Info("waiting for resource batch to be deleted", "initialCount", count)
	if count <= 0 {
		log.Info("no resources in batch")
		cleanupCtxCancel()
	}

	// Wait for the current batch of resources to be deleted before starting the next batch
	<-cleanupCtx.Done()

	return nil
}
