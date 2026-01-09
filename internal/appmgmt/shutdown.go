/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

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

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/dcpclient"
	"github.com/microsoft/dcp/internal/perftrace"
	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/kubeconfig"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/slices"
	"github.com/microsoft/dcp/pkg/syncmap"
)

var (
	cleanupJob         = concurrency.NewOneTimeJob[error]()
	beforeCleanupTasks = &syncmap.Map[string, func()]{}
)

type cleanupResourceState uint

const (
	initial cleanupResourceState = iota
	processing
	done
)

const (
	// It is called DCP_SHUTDOWN_TIMEOUT_SECONDS but strictly speaking it is a timeout for the resource cleanup process.
	DCP_SHUTDOWN_TIMEOUT_SECONDS = "DCP_SHUTDOWN_TIMEOUT_SECONDS"

	defaultCleanupTimeoutSeconds     = 120 // Two minutes
	beforeCleanupTasksTimeoutSeconds = 5
)

type cleanupResourceDescriptor struct {
	apiv1.CleanupResource
	State        cleanupResourceState
	CleanupError error
	WaitingFor   []schema.GroupVersionResource
}

func isReadyForCleanup(sr *cleanupResourceDescriptor) bool {
	return sr.State == initial && len(sr.WaitingFor) == 0
}

type resourceCleanupResult struct {
	GVR   schema.GroupVersionResource
	Error error
}

func CleanupAllResources(log logr.Logger) (cleanupResult error) {
	if len(apiv1.CleanupResources) <= 0 {
		log.Info("No resources to delete")
		cleanupResult = nil
		return
	}

	if cleanupJob.TryTake() {
		defer func() {
			panicErr := resiliency.MakePanicError(recover(), log)
			if panicErr != nil {
				cleanupJob.Complete(panicErr)
				cleanupResult = panicErr
			} else {
				cleanupJob.Complete(cleanupResult)
			}
		}()

		cleanupResult = doCleanup(log.WithName("cleanup").V(1))
	} else {
		cleanupResult = cleanupJob.WaitResult()
	}

	return // Make compiler happy
}

func AddBeforeCleanupTask(name string, task func()) {
	if task == nil {
		panic("task cannot be nil")
	}
	if name == "" {
		panic("name cannot be empty")
	}
	beforeCleanupTasks.Store(name, task)
}

func doCleanup(log logr.Logger) error {
	log.Info("Running before cleanup tasks...")
	runBeforeCleanupTasks(log)

	cleanupTimeout, cleanupTimeoutProvided := osutil.EnvVarIntVal(DCP_SHUTDOWN_TIMEOUT_SECONDS)
	if !cleanupTimeoutProvided {
		cleanupTimeout = defaultCleanupTimeoutSeconds
	}
	shutdownCtx, shutdownCtxCancel := context.WithTimeout(context.Background(), time.Duration(cleanupTimeout)*time.Second)
	defer shutdownCtxCancel()

	log.Info("Cleaning up resources...")

	// Disable new object creation once cleanup is started.
	apiv1.ResourceCreationProhibited.Store(true)

	err := perftrace.CaptureShutdownProfileIfRequested(shutdownCtx, log)
	if err != nil {
		log.Error(err, "Failed to capture shutdown profile")
	}

	// Using shorter timeout for the client to keep the shutdown process fail time as short as possible.
	dcpclient, err := dcpclient.NewClient(shutdownCtx, 5*time.Second)
	if err != nil {
		log.Error(err, "Could not get the client for the API server")
		return err
	}

	clusterConfig, err := config.GetConfig()
	if err != nil {
		log.Error(err, "Could not get config")
		return err
	}

	token, _ := os.LookupEnv(kubeconfig.DCP_SECURE_TOKEN)
	if token != "" {
		// If a token was supplied, use it to authenticate to the API server
		clusterConfig.BearerToken = token
	}

	dynamicClient, err := dynamic.NewForConfig(clusterConfig)
	if err != nil {
		log.Error(err, "Could not get client")
		return err
	}

	shutdownResources := make([]*cleanupResourceDescriptor, len(apiv1.CleanupResources))
	for i, cr := range apiv1.CleanupResources {
		shutdownResources[i] = &cleanupResourceDescriptor{
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
			log.Error(rcr.Error, "An error occurred while cleaning up a resource", "Resource", rcr.GVR.String())
		} else {
			log.Info("Finished cleaning up a resource", "Resource", rcr.GVR.String())
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
		if shutdownCtx.Err() != nil {
			break // We have run out of time
		}

		for _, sr := range readyForCleanup {
			sr.State = processing
			inProgress += 1
			log.Info("Cleaning up a resource...", "Resource", sr.GVR.String())
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

	notCleanedUp := slices.Select(shutdownResources, func(sr *cleanupResourceDescriptor) bool {
		return sr.State != done || sr.CleanupError != nil
	})
	cleanupErrors := slices.Map[error](notCleanedUp, func(sr *cleanupResourceDescriptor) error {
		switch {
		case sr.CleanupError != nil:
			return fmt.Errorf("resource '%s' could not be cleaned up: %w", sr.GVR.String(), sr.CleanupError)
		case len(sr.WaitingFor) > 0:
			dependencies := slices.Map[string](sr.WaitingFor, func(gvr schema.GroupVersionResource) string {
				return "'" + gvr.String() + "'"
			})
			return fmt.Errorf("resource '%s' could not be cleaned up: still waiting for: %s", sr.GVR.String(), strings.Join(dependencies, ", "))
		default:
			return fmt.Errorf("resource '%s' could not be cleaned up in alloted time", sr.GVR.String())
		}
	})

	retval := resiliency.Join(append([]error{shutdownCtx.Err()}, cleanupErrors...)...)
	if retval != nil {
		log.Error(retval, "Could not clean up all application resources")
	} else {
		log.Info("All application resources cleaned up")
	}
	return retval
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
				log.Info("Object has a parent, which should handle cleanup", "gvr", gvr, "ObjectName", clientObj.GetName(), "Namespace", clientObj.GetNamespace(), "ParentKind", parent.Kind, "ParentName", parent.Name)
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
			log.Info("Object deleted", "gvr", gvr, "Resource", clientObj.GetName(), "Namespace", clientObj.GetNamespace())
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
		log.Info("No resource instances found", "Resource", gvr.String())
		resourceDone <- resourceCleanupResult{GVR: gvr, Error: nil}
		return
	}

	log.Info("Waiting for all instances of a resource to be deleted...",
		"InitialCount", initialCount,
		"Resource", gvr.String(),
	)

	const noNewResourcesCheckInterval = 1 * time.Second
	noNewResourcesTimer := time.NewTimer(noNewResourcesCheckInterval)

	for {
		select {

		case <-ctx.Done():
			log.Info("Shutdown context cancelled, stopping resource cleanup", "Resource", gvr.String())
			noNewResourcesTimer.Stop()
			resourceDone <- resourceCleanupResult{GVR: gvr, Error: ctx.Err()}
			return

		case <-noNewResourcesTimer.C:
			if resourceCount.Load() <= 0 {
				log.Info("All resource instances processed", "Resource", gvr.String())
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

	log.Info("Deleting resource...", "gvr", gvr, "Resource", clientObj.GetName(), "Namespace", clientObj.GetNamespace())

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
		log.Error(retryErr, "Could not delete resource", "GVR", gvr, "Resource", clientObj.GetName(), "Namespace", clientObj.GetNamespace())
	}

	return retryErr
}

func runBeforeCleanupTasks(log logr.Logger) {
	beforeCleanupTasks.Range(func(name string, task func()) (proceed bool) {
		defer func() {
			_ = resiliency.MakePanicError(recover(), log)
			proceed = true
		}()
		defer beforeCleanupTasks.Delete(name)
		log.Info("Running before cleanup task...", "Name", name)
		task()
		return true // Always proceed to the next task
	})
}
