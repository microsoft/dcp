/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// This tool exercises controller-manager startup against a real DCP API server.
// The tests using it verify cancellation, deadlines, late readiness callbacks,
// runnable errors, and normal startup/shutdown by gating informer requests or
// cache readiness where needed. They guard against the cancellation busy loop
// tracked in https://github.com/kubernetes-sigs/controller-runtime/issues/3629,
// which the temporary build overlay addresses until an upstream fix is adopted.
//
// Startup-cancellation regressions can busy-loop indefinitely even after their context is
// cancelled. Running each scenario in a subprocess lets the test terminate a stuck manager
// instead of leaking a CPU-spinning goroutine into the integration test runner. The tool's
// watchdog bounds its lifetime independently, while the parent test checks its exit status
// and kills it if the test context expires.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgorest "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/dcpclient"
)

func main() {
	flags := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	kubeconfig := flags.String("kubeconfig", "", "Integration API server kubeconfig")
	scenario := flags.String("scenario", "", "Startup cancellation scenario")
	timeout := flags.Duration("timeout", time.Minute, "Maximum process lifetime")
	if parseErr := flags.Parse(os.Args[1:]); parseErr != nil {
		_, _ = fmt.Fprintln(os.Stderr, parseErr)
		os.Exit(1)
	}
	if *kubeconfig == "" || *timeout <= 0 || *timeout > 3*time.Minute {
		_, _ = fmt.Fprintln(os.Stderr, "kubeconfig and a positive timeout no greater than three minutes are required")
		os.Exit(1)
	}

	// A broken startup loop must not outlive the test runner or its cleanup.
	watchdog := time.AfterFunc(*timeout, func() { os.Exit(3) })
	defer watchdog.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	ctrl.SetLogger(logr.Discard())
	if runErr := run(ctx, *kubeconfig, *scenario); runErr != nil {
		_, _ = fmt.Fprintln(os.Stderr, runErr)
		os.Exit(1)
	}
}

type requestGate struct {
	http.RoundTripper
	entered chan struct{}
	once    sync.Once
}

func (gate *requestGate) RoundTrip(request *http.Request) (*http.Response, error) {
	if strings.HasSuffix(request.URL.Path, "/endpoints") {
		gate.once.Do(func() { close(gate.entered) })
		<-request.Context().Done()
		return nil, request.Context().Err()
	}
	return gate.RoundTripper.RoundTrip(request)
}

type readinessGateCache struct {
	cache.Cache
	ready    chan struct{}
	release  chan struct{}
	returned chan struct{}
}

func (gatedCache *readinessGateCache) WaitForCacheSync(ctx context.Context) bool {
	defer close(gatedCache.returned)
	synced := gatedCache.Cache.WaitForCacheSync(ctx)
	if synced {
		close(gatedCache.ready)
		<-gatedCache.release
	}
	return synced
}

func run(ctx context.Context, kubeconfig string, scenario string) error {
	managerCtx, cancelManager := context.WithCancel(ctx)
	defer cancelManager()
	if scenario == "deadline-before-start" {
		deadlineCtx, cancelDeadline := context.WithDeadline(managerCtx, time.Now())
		defer cancelDeadline()
		managerCtx = deadlineCtx
	}

	config, configErr := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if configErr != nil {
		return fmt.Errorf("loading API server configuration: %w", configErr)
	}
	dcpclient.ApplyDcpOptions(config)
	options := controllers.NewControllerManagerOptions(managerCtx, dcpclient.NewScheme(), logr.Discard())
	ready := make(chan struct{})
	releaseReady := make(chan struct{})
	readinessReturned := make(chan struct{})
	defer close(releaseReady)
	requestEntered := make(chan struct{})

	switch scenario {
	case "cancel-before-start", "deadline-before-start", "healthy", "runnable-error":
	case "cancel-during-start":
		config.WrapTransport = func(transport http.RoundTripper) http.RoundTripper {
			return &requestGate{RoundTripper: transport, entered: requestEntered}
		}
	case "late-ready":
		options.NewCache = func(restConfig *clientgorest.Config, cacheOptions cache.Options) (cache.Cache, error) {
			realCache, cacheErr := cache.New(restConfig, cacheOptions)
			if cacheErr != nil {
				return nil, cacheErr
			}
			return &readinessGateCache{
				Cache: realCache, ready: ready, release: releaseReady, returned: readinessReturned,
			}, nil
		}
	default:
		return fmt.Errorf("unknown startup scenario %q", scenario)
	}

	mgr, managerErr := ctrl.NewManager(config, options)
	if managerErr != nil {
		return fmt.Errorf("creating controller manager: %w", managerErr)
	}
	if _, informerErr := mgr.GetCache().GetInformer(ctx, &apiv1.Endpoint{}, cache.BlockUntilSynced(false)); informerErr != nil {
		return fmt.Errorf("registering real endpoint informer: %w", informerErr)
	}

	if scenario == "deadline-before-start" {
		startErr := mgr.Start(managerCtx)
		if !errors.Is(startErr, context.DeadlineExceeded) {
			return fmt.Errorf("expected startup deadline error, received %v", startErr)
		}
		return nil
	}
	if scenario == "cancel-before-start" {
		cancelManager()
		return mgr.Start(managerCtx)
	}
	if scenario == "runnable-error" {
		runnableErr := errors.New("startup runnable failed")
		addRunnableErr := mgr.Add(manager.RunnableFunc(func(context.Context) error { return runnableErr }))
		if addRunnableErr != nil {
			return fmt.Errorf("adding failing runnable: %w", addRunnableErr)
		}
		managerRunErr := mgr.Start(managerCtx)
		if !errors.Is(managerRunErr, runnableErr) {
			return fmt.Errorf("expected runnable error, received %v", managerRunErr)
		}
		return nil
	}
	if scenario == "healthy" {
		addErr := mgr.Add(manager.RunnableFunc(func(runnableCtx context.Context) error {
			close(ready)
			<-runnableCtx.Done()
			return nil
		}))
		if addErr != nil {
			return fmt.Errorf("adding startup notification: %w", addErr)
		}
	}

	startResult := make(chan error, 1)
	go func() { startResult <- mgr.Start(managerCtx) }()
	started := (<-chan struct{})(ready)
	if scenario == "cancel-during-start" {
		started = requestEntered
	}
	select {
	case <-started:
	case <-ctx.Done():
		return ctx.Err()
	}
	cancelManager()
	select {
	case startErr, received := <-startResult:
		if !received {
			return fmt.Errorf("manager result channel closed without a result")
		}
		if startErr != nil {
			return fmt.Errorf("manager shutdown failed: %w", startErr)
		}
	case <-ctx.Done():
		return ctx.Err()
	}

	if scenario == "late-ready" {
		releaseReady <- struct{}{}
		select {
		case <-readinessReturned:
		case <-ctx.Done():
			return ctx.Err()
		}
		return wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
			var stacks bytes.Buffer
			if profileErr := pprof.Lookup("goroutine").WriteTo(&stacks, 1); profileErr != nil {
				return false, profileErr
			}
			return !strings.Contains(stacks.String(), "(*runnableGroup).reconcile.func1.1"), nil
		})
	}
	return nil
}
