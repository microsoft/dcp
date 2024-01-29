package appmgmt

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
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

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 5*time.Second)
	defer factory.Shutdown()

	// The context for the informers needs to have it's defer cancellation registered AFTER
	// the call to defer factory.Shutdown() or shutdown will block and the informers won't
	// be terminated.
	informerCtx, informerCtxCancel := context.WithCancel(shutdownCtx)
	defer informerCtxCancel()

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
		if err = cleanupResourceBatch(informerCtx, dcpclient, factory, resourceBatch, log); err != nil {
			return err
		}
	}

	return nil
}

func cleanupResourceBatch(
	ctx context.Context,
	dcpclient client.Client,
	factory dynamicinformer.DynamicSharedInformerFactory,
	batch []*apiv1.WeightedResource,
	log logr.Logger,
) error {
	cleanupCtx, cleanupCtxCancel := context.WithCancel(ctx)
	defer cleanupCtxCancel()

	initialResourceCounts := &atomic.Int32{}
	totalResourceCounts := &atomic.Int32{}

	for _, resource := range batch {
		gvr := resource.Object.GetGroupVersionResource()
		informer := factory.ForResource(gvr)

		log.Info("Shutting down resource", "resource", gvr)
		if _, err := informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				clientObj := obj.(client.Object)
				initialResourceCounts.Add(1)
				totalResources := totalResourceCounts.Add(1)
				log.Info("Deleting resource", "resource", gvr, "total", totalResources)
				if err := dcpclient.Delete(ctx, clientObj); err != nil {
					log.Error(err, "could not delete resource", "resource", clientObj)
				}
			},
			DeleteFunc: func(obj interface{}) {
				totalResources := totalResourceCounts.Add(-1)
				log.Info("Resource removed", "resource", gvr, "total", totalResources)

				if totalResources <= 0 {
					cleanupCtxCancel()
				}
			},
		}); err != nil {
			return err
		}
	}

	factory.Start(ctx.Done())
	_ = factory.WaitForCacheSync(ctx.Done())

	count := initialResourceCounts.Load()
	log.Info("Waiting for resource batch to be deleted", "initialCount", count)
	if count <= 0 {
		cleanupCtxCancel()
	}

	// Wait for the current batch of resources to be deleted before starting the next batch
	<-cleanupCtx.Done()

	return nil
}
