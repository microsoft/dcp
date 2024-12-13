package appmgmt

import (
	"context"
	"fmt"
	"os"
	stdslices "slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

type shutdownResourceState uint

const (
	initial shutdownResourceState = iota
	processing
	done
)

type shutdownResource struct {
	apiv1.CleanupResource
	State        shutdownResourceState
	CleanupError error
	WaitingFor   []schema.GroupVersionResource
}

func isReadyForCleanup(sr *shutdownResource) bool {
	return sr.State == initial && len(sr.WaitingFor) == 0
}

type resourceCleanupResult struct {
	GVR   schema.GroupVersionResource
	Error error
}

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

	shutdownResources := make([]*shutdownResource, len(apiv1.CleanupResources))
	for i, cr := range apiv1.CleanupResources {
		shutdownResources[i] = &shutdownResource{
			CleanupResource: *cr,
			State:           initial,
			WaitingFor:      stdslices.Clone(cr.CleanUpAfter),
		}
	}

	readyForCleanup := slices.Select(shutdownResources, isReadyForCleanup)
	resourceDone := make(chan resourceCleanupResult)
	inProgress := 0

	waitForResourceProcessed := func() {
		rcr := <-resourceDone

		if rcr.Error != nil {
			log.Error(rcr.Error, "an error occurred while cleaning up a resource", "resource", rcr.GVR.String())
		} else {
			log.Info("finished cleaning up a resource", "resource", rcr.GVR.String())
		}

		for _, sr := range shutdownResources {
			if sr.GVR == rcr.GVR {
				sr.State = done
				sr.CleanupError = rcr.Error
			} else if rcr.Error == nil {
				sr.WaitingFor = slices.Select(sr.WaitingFor, func(gvr schema.GroupVersionResource) bool {
					return gvr != rcr.GVR
				})
			}
		}

		readyForCleanup = slices.Select(shutdownResources, isReadyForCleanup)
	}

	for len(readyForCleanup) > 0 || inProgress > 0 {
		if ctx.Err() != nil {
			break // We have run out of time
		}

		for _, sr := range readyForCleanup {
			sr.State = processing
			inProgress += 1
			log.Info("cleaning up a resource...", "resource", sr.GVR.String())
			go cleanupResource(shutdownCtx, dcpclient, dynamicClient, sr.GVR, log, resourceDone)
		}

		waitForResourceProcessed()
		inProgress -= 1
	}

	// Need to wait for all resources in progress to finish
	for inProgress > 0 {
		waitForResourceProcessed()
		inProgress -= 1
	}

	notCleanedUp := slices.Select(shutdownResources, func(sr *shutdownResource) bool {
		return sr.State != done || sr.CleanupError != nil
	})
	cleanupErrors := slices.Map[*shutdownResource, error](notCleanedUp, func(sr *shutdownResource) error {
		switch {
		case sr.CleanupError != nil:
			return fmt.Errorf("Resource '%s' could not be cleaned up: %w", sr.GVR.String(), sr.CleanupError)
		case len(sr.WaitingFor) > 0:
			dependencies := slices.Map[schema.GroupVersionResource, string](sr.WaitingFor, func(gvr schema.GroupVersionResource) string {
				return "'" + gvr.String() + "'"
			})
			return fmt.Errorf("Resource '%s' could not be cleaned up: still waiting for: %s", sr.GVR.String(), strings.Join(dependencies, ", "))
		default:
			return fmt.Errorf("Resource '%s' could not be cleaned up in alloted time", sr.GVR.String())
		}
	})

	return resiliency.Join(append([]error{ctx.Err()}, cleanupErrors...)...)
}

func cleanupResource(
	ctx context.Context,
	dcpclient client.Client,
	dynamicClient *dynamic.DynamicClient,
	gvr schema.GroupVersionResource,
	log logr.Logger,
	resourceDone chan<- resourceCleanupResult,
) {
	// Note that this recovery does not affect the resource event handlers code, which runs in a separate goroutine,
	// managed by the informer. If handlers get more complex, consider adding panic recovery to them too.
	defer func() {
		panicErr := resiliency.MakePanicError(recover(), log)
		if panicErr != nil {
			// Note: make sure the code in the cleanupResource() function (code below) writes to resourceDone channel
			// as the LAST operation before returning. This minimizes the chance of writing to the channel twice
			// for the same resource (gvr).
			resourceDone <- resourceCleanupResult{GVR: gvr, Error: panicErr}
		}
	}()

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 20*time.Second)
	// Do not call factory.Shutdown() -- if for whatever reason there are some resources left not deleted,
	// factory.Shutdown() will block forever waiting for the informers to stop.
	informer := factory.ForResource(gvr)

	resourceCount := &atomic.Int32{}
	cacheSyncDone := make(chan struct{})
	informerFactoryCtx, informerFactoryCtxCancel := context.WithCancel(ctx)
	defer informerFactoryCtxCancel()
	var deleteErrors []error
	var deleteErrorsLock sync.Mutex

	handlers := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if obj == nil {
				return // Should never happen
			}
			clientObj, ok := obj.(client.Object)
			if !ok {
				return // Should never happen
			}

			resourceCount.Add(1)

			parent := metav1.GetControllerOf(clientObj)
			if parent != nil && parent.APIVersion == apiv1.GroupVersion.String() {
				log.Info("object has a parent, which should handle cleanup", "gvr", gvr, "objectName", clientObj.GetName(), "namespace", clientObj.GetNamespace(), "parentKind", parent.Kind, "parentName", parent.Name)
				resourceCount.Add(-1)
				return
			}

			go func() {
				deleteErr := deleteSingleObject(ctx, dcpclient, gvr, clientObj, log, cacheSyncDone)
				if deleteErr != nil {
					// Only decrement if error occurred--successful deletion will be accounted for by DeleteFunc handler below
					resourceCount.Add(-1)

					deleteErrorsLock.Lock()
					defer deleteErrorsLock.Unlock()
					deleteErrors = append(deleteErrors, fmt.Errorf("could not delete object '%s/%s' of type '%s': %w", clientObj.GetNamespace(), clientObj.GetName(), gvr.String(), deleteErr))
				}
			}()
		},

		DeleteFunc: func(obj interface{}) {
			if obj == nil {
				return // Should never happen
			}
			clientObj, ok := obj.(client.Object)
			if !ok {
				return // Should never happen
			}
			resourceCount.Add(-1)
			log.Info("object deleted", "gvr", gvr, "resource", clientObj.GetName(), "namespace", clientObj.GetNamespace())
		},
	}

	if _, err := informer.Informer().AddEventHandler(handlers); err != nil {
		resourceDone <- resourceCleanupResult{GVR: gvr, Error: err}
		return
	}

	factory.Start(informerFactoryCtx.Done())
	_ = factory.WaitForCacheSync(ctx.Done())
	initialCount := resourceCount.Load()
	close(cacheSyncDone)

	if initialCount == 0 {
		log.Info("no resource instances found", "resource", gvr.String())
		resourceDone <- resourceCleanupResult{GVR: gvr, Error: nil}
		return
	}

	log.Info("waiting for all instances of a resource to be deleted...",
		"initialCount", initialCount,
		"resource", gvr.String(),
	)

	const noNewResourcesCheckInterval = 1 * time.Second
	noNewResourcesTimer := time.NewTimer(noNewResourcesCheckInterval)

	for {
		select {

		case <-ctx.Done():
			log.Info("shutdown context cancelled, stopping resource cleanup", "resource", gvr.String())
			noNewResourcesTimer.Stop()
			resourceDone <- resourceCleanupResult{GVR: gvr, Error: ctx.Err()}
			return

		case <-noNewResourcesTimer.C:
			if resourceCount.Load() <= 0 {
				log.Info("all resource instances processed", "resource", gvr.String())
				deleteErrorsLock.Lock()
				defer deleteErrorsLock.Unlock()
				resourceDone <- resourceCleanupResult{GVR: gvr, Error: resiliency.Join(deleteErrors...)}
				return
			} else {
				noNewResourcesTimer.Reset(noNewResourcesCheckInterval)
			}
		}
	}
}

func deleteSingleObject(
	ctx context.Context,
	dcpclient client.Client,
	gvr schema.GroupVersionResource,
	clientObj client.Object,
	log logr.Logger,
	cacheSyncDone <-chan struct{},
) (retval error) {
	<-cacheSyncDone

	defer func() {
		panicErr := resiliency.MakePanicError(recover(), log)
		if panicErr != nil {
			retval = panicErr
		}
	}()

	log.Info("deleting resource...", "gvr", gvr, "resource", clientObj.GetName(), "namespace", clientObj.GetNamespace())

	// Limit the break between deletion attempts to 5 seconds
	b := backoff.NewExponentialBackOff(backoff.WithMaxInterval(5 * time.Second))

	retryErr := resiliency.Retry(ctx, b, func() error {
		// Limit the deletion attempt duration to 20 seconds.
		// This allows us to retry the deletion several times during the shutdown sequence,
		// instead of making one very long attempt.
		deleteCtx, deleteCtxCancel := context.WithTimeout(ctx, 20*time.Second)
		defer deleteCtxCancel()

		// In DCP it is parent object controllers that are responsible for cleaning up their children
		// (Endpoint objects owned by Container or Executable object, or ContainerNetworkConnection objects owned by Container object).
		// That is why we do not use deletion propagation policy here.
		err := dcpclient.Delete(deleteCtx, clientObj, client.PropagationPolicy(metav1.DeletePropagationOrphan))
		if err == nil || k8serrors.IsNotFound(err) {
			return nil
		} else {
			return err
		}
	})

	if retryErr != nil {
		log.Error(retryErr, "could not delete resource", "gvr", gvr, "resource", clientObj.GetName(), "namespace", clientObj.GetNamespace())
	}

	return retryErr
}
