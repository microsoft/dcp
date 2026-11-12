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
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/dcpclient"
	"github.com/microsoft/dcp/internal/perftrace"
	"github.com/microsoft/dcp/internal/resourcecleanup"
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
	resourcecleanup.CleanupResource
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

// CleanupAllResources deletes all resource kinds listed in resourcecleanup.ShutdownResources,
// in cleanup order, and waits for the deletions to complete.
//
// This must be called while the controllers are still running and their lifetime context is
// still live. Controllers hand long-running cleanup work to resiliency.WorkQueue instances,
// which drop queued items once their lifetime context is done, so calling this after the
// controller host has shut down would silently skip cleanup and leak containers.
func CleanupAllResources(log logr.Logger) (cleanupResult error) {
	if len(resourcecleanup.ShutdownResources) <= 0 {
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

	shutdownResources := make([]*cleanupResourceDescriptor, len(resourcecleanup.ShutdownResources))
	for i, cr := range resourcecleanup.ShutdownResources {
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
			go func(gvr schema.GroupVersionResource) {
				var cleanupErr error
				defer func() {
					if panicErr := resiliency.MakePanicError(recover(), log); panicErr != nil {
						cleanupErr = panicErr
					}
					resourceDone <- resourceCleanupResult{GVR: gvr, Error: cleanupErr}
				}()
				cleanupErr = cleanupResource(shutdownCtx, dcpclient, dynamicClient, gvr, log)
			}(sr.GVR)
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

// cleanupResource deletes all instances of the given resource kind (except those owned by a DCP parent object)
// and waits until none of them remain. Completion is determined by periodically listing the remaining instances,
// so it does not depend on observing a watch event for every deletion.
func cleanupResource(
	ctx context.Context,
	dcpclient client.Client,
	dynamicClient dynamic.Interface,
	gvr schema.GroupVersionResource,
	log logr.Logger,
) error {
	// Instances (by UID) for which deletion has been initiated, or which are left to their parent to clean up.
	handled := make(map[types.UID]bool)
	// Instances (by UID) that could not be deleted, mapped to the deletion error.
	failedDeletions := &syncmap.Map[types.UID, error]{}
	var lastListErr error
	firstList := true

	const remainingResourcesCheckInterval = 1 * time.Second
	checkTimer := time.NewTimer(0)
	defer checkTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("Shutdown context cancelled, stopping resource cleanup", "Resource", gvr.String())
			return resiliency.Join(ctx.Err(), lastListErr, joinDeletionErrors(failedDeletions))

		case <-checkTimer.C:
		}

		// Always list directly from the API server: it is the authoritative source of the remaining instances.
		instances, listErr := dynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{})
		if listErr != nil {
			lastListErr = fmt.Errorf("could not list instances of resource '%s': %w", gvr.String(), listErr)
			log.Error(listErr, "Could not list resource instances, will retry", "Resource", gvr.String())
			checkTimer.Reset(remainingResourcesCheckInterval)
			continue
		}
		lastListErr = nil

		remaining := 0
		for i := range instances.Items {
			instance := &instances.Items[i]
			uid := instance.GetUID()

			if _, failed := failedDeletions.Load(uid); failed {
				continue
			}

			parent := metav1.GetControllerOf(instance)
			if parent != nil && parent.APIVersion == apiv1.GroupVersion.String() {
				if !handled[uid] {
					handled[uid] = true
					log.Info("Object has a parent, which should handle cleanup", "gvr", gvr, "ObjectName", instance.GetName(), "Namespace", instance.GetNamespace(), "ParentKind", parent.Kind, "ParentName", parent.Name)
				}
				continue
			}

			remaining++
			if !handled[uid] {
				handled[uid] = true
				go func() {
					deleteErr := deleteSingleObject(ctx, dcpclient, gvr, instance, log)
					if deleteErr != nil {
						failedDeletions.Store(uid, fmt.Errorf("could not delete object '%s/%s' of type '%s': %w", instance.GetNamespace(), instance.GetName(), gvr.String(), deleteErr))
					}
				}()
			}
		}

		if firstList {
			firstList = false
			if remaining == 0 {
				log.Info("No resource instances found", "Resource", gvr.String())
				return nil
			}
			log.Info("Waiting for all instances of a resource to be deleted...",
				"InitialCount", remaining,
				"Resource", gvr.String(),
			)
		} else if remaining == 0 {
			log.Info("All resource instances processed", "Resource", gvr.String())
			return joinDeletionErrors(failedDeletions)
		}

		checkTimer.Reset(remainingResourcesCheckInterval)
	}
}

func joinDeletionErrors(failedDeletions *syncmap.Map[types.UID, error]) error {
	var deleteErrors []error
	failedDeletions.Range(func(_ types.UID, deleteErr error) bool {
		deleteErrors = append(deleteErrors, deleteErr)
		return true
	})
	return resiliency.Join(deleteErrors...)
}

func deleteSingleObject(
	ctx context.Context,
	dcpclient client.Client,
	gvr schema.GroupVersionResource,
	clientObj client.Object,
	log logr.Logger,
) (retval error) {
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
