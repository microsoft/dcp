// Copyright (c) Microsoft Corporation. All rights reserved.

package health

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/slices"
	"github.com/microsoft/dcp/pkg/testutil"
)

const (
	defaultHealthTestTimeout  = 60 * time.Second
	healthProbeInterval       = 100 * time.Millisecond
	expectedStatePollInterval = 200 * time.Millisecond
)

var (
	executableGVK = apiv1.GroupVersion.WithKind(reflect.TypeOf(apiv1.Executable{}).Name())
	containerGVK  = apiv1.GroupVersion.WithKind(reflect.TypeOf(apiv1.Container{}).Name())
)

// Ensure that the health probe set can support multiple subscribers
func TestHealthProbeSetMultipleSubscribers(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultHealthTestTimeout)
	defer cancel()

	log := testutil.NewLogForTesting("health-probe-set-multiple-subscribers")
	httpPE := newMockProbeExecutor()
	executors := map[apiv1.HealthProbeType]HealthProbeExecutor{
		apiv1.HealthProbeTypeHttp: httpPE,
	}

	hps := NewHealthProbeSet(ctx, log, executors)

	reports := []healthProbeReportWithSubscription{}
	lock := &sync.Mutex{}

	// Subscriptions for health probe results
	exeSub1Sink := concurrency.NewUnboundedChan[HealthProbeReport](ctx)
	go storeResults(ctx, exeSub1Sink.Out, &reports, "exe1Sub", lock)
	exeSub2Sink := concurrency.NewUnboundedChan[HealthProbeReport](ctx)
	go storeResults(ctx, exeSub2Sink.Out, &reports, "exe2Sub", lock)
	containerSub1Sink := concurrency.NewUnboundedChan[HealthProbeReport](ctx)
	go storeResults(ctx, containerSub1Sink.Out, &reports, "containerSub1", lock)
	containerSub2Sink := concurrency.NewUnboundedChan[HealthProbeReport](ctx)
	go storeResults(ctx, containerSub2Sink.Out, &reports, "containerSub2", lock)

	exeSub1, exeSub1Err := hps.Subscribe(exeSub1Sink.In, executableGVK)
	require.NoError(t, exeSub1Err)
	defer exeSub1.Cancel()
	exeSub2, exeSub2Err := hps.Subscribe(exeSub2Sink.In, executableGVK)
	require.NoError(t, exeSub2Err)
	defer exeSub2.Cancel()
	containerSub1, containerSub1Err := hps.Subscribe(containerSub1Sink.In, containerGVK)
	require.NoError(t, containerSub1Err)
	defer containerSub1.Cancel()
	containerSub2, containerSub2Err := hps.Subscribe(containerSub2Sink.In, containerGVK)
	require.NoError(t, containerSub2Err)
	defer containerSub2.Cancel()

	exe1 := apiv1.Executable{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1.GroupVersion.String(),
			Kind:       executableGVK.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "exe1",
			Namespace: metav1.NamespaceNone,
		},
	}
	ctr1 := apiv1.Container{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1.GroupVersion.String(),
			Kind:       containerGVK.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "container1",
			Namespace: metav1.NamespaceNone,
		},
	}
	ctr2 := apiv1.Container{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1.GroupVersion.String(),
			Kind:       containerGVK.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "container2",
			Namespace: metav1.NamespaceNone,
		},
	}

	// Enable probes for an executable and a container
	require.NoError(t, hps.EnableProbes(&exe1, []apiv1.HealthProbe{
		getHttpProbe("exe-http-probe1"), getHttpProbe("exe-http-probe2"), getHttpProbe("exe-http-probe3"),
	}))
	require.NoError(t, hps.EnableProbes(&ctr1, []apiv1.HealthProbe{
		getHttpProbe("container-http-probe1"), getHttpProbe("container-http-probe2"),
	}))
	require.NoError(t, hps.EnableProbes(&ctr2, []apiv1.HealthProbe{getHttpProbe("container-http-probe3")}))

	// We expect
	// -- All Executable subscribers to receive results for all Executable probes (3 probes for exe1)
	// -- All Container subscribers to receive results for all Container probes (2 probes for container1, 1 probe for container2)
	// -- Executor called for all probes

	exeExpectedResults := map[commonapi.NamespacedNameWithKind]map[string]apiv1.HealthProbeOutcome{
		commonapi.GetNamespacedNameWithKind(&exe1): {
			"exe-http-probe1": apiv1.HealthProbeOutcomeSuccess,
			"exe-http-probe2": apiv1.HealthProbeOutcomeSuccess,
			"exe-http-probe3": apiv1.HealthProbeOutcomeSuccess,
		},
	}
	containerExpectedResults := map[commonapi.NamespacedNameWithKind]map[string]apiv1.HealthProbeOutcome{
		commonapi.GetNamespacedNameWithKind(&ctr1): {
			"container-http-probe1": apiv1.HealthProbeOutcomeSuccess,
			"container-http-probe2": apiv1.HealthProbeOutcomeSuccess,
		},
		commonapi.GetNamespacedNameWithKind(&ctr2): {
			"container-http-probe3": apiv1.HealthProbeOutcomeSuccess,
		},
	}

	var subscriberErrors error
	waitErr := wait.PollUntilContextCancel(ctx, expectedStatePollInterval, false /* poll immediately */, func(ctx context.Context) (bool, error) {
		subscriberErrors = errors.Join(
			subscriberHasResults("exe1Sub", &reports, exeExpectedResults, lock),
			subscriberHasResults("exe2Sub", &reports, exeExpectedResults, lock),
			subscriberHasResults("containerSub1", &reports, containerExpectedResults, lock),
			subscriberHasResults("containerSub2", &reports, containerExpectedResults, lock),
		)
		return subscriberErrors == nil, nil
	})
	require.NoError(t, waitErr, "Not all subscribers received expected results: %v", subscriberErrors)

	require.ElementsMatch(t, []string{"exe-http-probe1", "exe-http-probe2", "exe-http-probe3", "container-http-probe1", "container-http-probe2", "container-http-probe3"}, httpPE.AllProbesCalled())
}

// Ensure that health probe execution can be enabled and disabled per owner.
// When all executions are disabled, the scheduler goroutine should stop.
func TestHealthProbeSetEnableDisable(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultHealthTestTimeout)
	defer cancel()

	log := testutil.NewLogForTesting("health-probe-set-enable-disable")
	httpPE := newMockProbeExecutor()
	executors := map[apiv1.HealthProbeType]HealthProbeExecutor{
		apiv1.HealthProbeTypeHttp: httpPE,
	}

	hps := NewHealthProbeSet(ctx, log, executors)

	reports := []healthProbeReportWithSubscription{}
	lock := &sync.Mutex{}

	// Start capturing health reports
	reportsChan := concurrency.NewUnboundedChan[HealthProbeReport](ctx)
	go storeResults(ctx, reportsChan.Out, &reports, "sub", lock)
	sub, subErr := hps.Subscribe(reportsChan.In, executableGVK)
	require.NoError(t, subErr)
	defer sub.Cancel()

	exe1 := apiv1.Executable{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1.GroupVersion.String(),
			Kind:       executableGVK.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "exe1",
			Namespace: metav1.NamespaceNone,
		},
	}
	exe2 := apiv1.Executable{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1.GroupVersion.String(),
			Kind:       executableGVK.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "exe2",
			Namespace: metav1.NamespaceNone,
		},
	}

	// Enable health probes for first Executable
	require.NoError(t, hps.EnableProbes(&exe1, []apiv1.HealthProbe{getHttpProbe("exe1-http-probe")}))

	// Verify reports are coming in
	waitErr := wait.PollUntilContextCancel(ctx, expectedStatePollInterval, false /* poll immediately */, func(ctx context.Context) (bool, error) {
		return subscriberHasResults("sub", &reports, map[commonapi.NamespacedNameWithKind]map[string]apiv1.HealthProbeOutcome{
			commonapi.GetNamespacedNameWithKind(&exe1): {"exe1-http-probe": apiv1.HealthProbeOutcomeSuccess},
		}, lock) == nil, nil
	})
	require.NoError(t, waitErr, "Expected health reports for Executable `exe1` are missing")

	// Enable health probes for the second Executable and disable probes for the first Executable
	require.NoError(t, hps.EnableProbes(&exe2, []apiv1.HealthProbe{getHttpProbe("exe2-http-probe")}))
	hps.DisableProbes(&exe1)

	// Verify reports are coming in for the second owner ONLY (wait till last 10 reports are for the second Executable only)
	waitErr = wait.PollUntilContextCancel(ctx, expectedStatePollInterval, true /* poll immediately */, func(ctx context.Context) (bool, error) {
		lock.Lock()
		defer lock.Unlock()

		const numReportsToCheck = 10
		var lastReports []healthProbeReportWithSubscription
		if len(reports) >= numReportsToCheck {
			lastReports = reports[len(reports)-numReportsToCheck:]
		} else {
			lastReports = reports
		}
		return slices.All(lastReports, func(rws healthProbeReportWithSubscription) bool {
			return rws.Report.Owner == commonapi.GetNamespacedNameWithKind(&exe2)
		}), nil
	})
	require.NoError(t, waitErr, "Expected health reports for Executable `exe2` are missing")

	// Disable execution for the second Executable and verify the reports stop coming in
	hps.DisableProbes(&exe2)
	const periodsToWait = 5
	noMoreReportsErr := testutil.LengthNotChanging(ctx, &reports, expectedStatePollInterval, periodsToWait, lock)
	require.NoError(t, noMoreReportsErr, "Reports should stop coming in after disabling all probes")

	lock.Lock()
	reports = []healthProbeReportWithSubscription{}
	lock.Unlock()

	// Enable probes for the first Executable again and verify reports are coming in
	require.NoError(t, hps.EnableProbes(&exe1, []apiv1.HealthProbe{getHttpProbe("exe1-http-probe")}))
	waitErr = wait.PollUntilContextCancel(ctx, expectedStatePollInterval, false /* poll immediately */, func(ctx context.Context) (bool, error) {
		return subscriberHasResults("sub", &reports, map[commonapi.NamespacedNameWithKind]map[string]apiv1.HealthProbeOutcome{
			commonapi.GetNamespacedNameWithKind(&exe1): {"exe1-http-probe": apiv1.HealthProbeOutcomeSuccess},
		}, lock) == nil, nil
	})
	require.NoError(t, waitErr, "Expected health reports for Executable `exe1` (after re-enabling health checks) are missing")
}

// Ensure that health probe on a tight schedule is not starving a health probe on much slower schedule
func TestHealthProbeSetNoStarvation(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultHealthTestTimeout)
	defer cancel()

	log := testutil.NewLogForTesting("health-probe-set-no-starvation")
	httpPE := newMockProbeExecutor()
	executors := map[apiv1.HealthProbeType]HealthProbeExecutor{
		apiv1.HealthProbeTypeHttp: httpPE,
	}

	hps := NewHealthProbeSet(ctx, log, executors)

	reports := []healthProbeReportWithSubscription{}
	lock := &sync.Mutex{}

	// Start capturing health reports
	reportsChan := concurrency.NewUnboundedChan[HealthProbeReport](ctx)
	go storeResults(ctx, reportsChan.Out, &reports, "sub", lock)
	sub, subErr := hps.Subscribe(reportsChan.In, executableGVK)
	require.NoError(t, subErr)
	defer sub.Cancel()

	exe1 := apiv1.Executable{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1.GroupVersion.String(),
			Kind:       executableGVK.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "exe1",
			Namespace: metav1.NamespaceNone,
		},
	}

	// Enable two health probes for the Executable, one with tight schedule, one much more relaxed
	require.NoError(t, hps.EnableProbes(&exe1, []apiv1.HealthProbe{
		getHttpProbeWithInterval("exe1-http-probe-tight", 10*time.Millisecond),
		getHttpProbeWithInterval("exe1-http-probe-relaxed", 200*time.Millisecond),
	}))

	// Verify reports are coming in for both probes
	waitErr := wait.PollUntilContextCancel(ctx, expectedStatePollInterval, false /* poll immediately */, func(ctx context.Context) (bool, error) {
		return subscriberHasResults("sub", &reports, map[commonapi.NamespacedNameWithKind]map[string]apiv1.HealthProbeOutcome{
			commonapi.GetNamespacedNameWithKind(&exe1): {
				"exe1-http-probe-tight":   apiv1.HealthProbeOutcomeSuccess,
				"exe1-http-probe-relaxed": apiv1.HealthProbeOutcomeSuccess,
			},
		}, lock) == nil, nil
	})
	require.NoError(t, waitErr, "Expected health reports for Executable `exe1` are missing")

	// Make sure we gather more than one result for the relaxed probe
	const numResultsToWaitFor = 5
	waitErr = wait.PollUntilContextCancel(ctx, expectedStatePollInterval, true /* poll immediately */, func(ctx context.Context) (bool, error) {
		lock.Lock()
		defer lock.Unlock()

		relaxedProbeResultCount := slices.LenIf(reports, func(rws healthProbeReportWithSubscription) bool {
			return rws.Report.Owner == commonapi.GetNamespacedNameWithKind(&exe1) && rws.Report.Probe.Name == "exe1-http-probe-relaxed"
		})
		return relaxedProbeResultCount >= numResultsToWaitFor, nil
	})
	require.NoError(t, waitErr, "Expected more than one health report for the relaxed probe")
}

// Ensure that health probe execution is stopped and subscriptions are cancelled when the context is cancelled
func TestHealthProbeSetContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultHealthTestTimeout)
	defer cancel()

	log := testutil.NewLogForTesting("health-probe-set-context-cancellation")
	httpPE := newMockProbeExecutor()
	executors := map[apiv1.HealthProbeType]HealthProbeExecutor{
		apiv1.HealthProbeTypeHttp: httpPE,
	}

	hpsCtx, hpsCtxCancel := context.WithCancel(ctx)
	hps := NewHealthProbeSet(hpsCtx, log, executors)

	reports := []healthProbeReportWithSubscription{}
	lock := &sync.Mutex{}

	// Start capturing health reports
	reportsChan := concurrency.NewUnboundedChan[HealthProbeReport](ctx)
	go storeResults(ctx, reportsChan.Out, &reports, "sub", lock) // Deliberately using text context, not HealthProbeSet context
	sub, subErr := hps.Subscribe(reportsChan.In, executableGVK)
	require.NoError(t, subErr)
	defer sub.Cancel()

	exe1 := apiv1.Executable{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1.GroupVersion.String(),
			Kind:       executableGVK.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "exe1",
			Namespace: metav1.NamespaceNone,
		},
	}

	// Enable health probes for the Executable
	require.NoError(t, hps.EnableProbes(&exe1, []apiv1.HealthProbe{getHttpProbe("exe1-http-probe")}))

	// Verify reports are coming in
	waitErr := wait.PollUntilContextCancel(ctx, expectedStatePollInterval, false /* poll immediately */, func(ctx context.Context) (bool, error) {
		return subscriberHasResults("sub", &reports, map[commonapi.NamespacedNameWithKind]map[string]apiv1.HealthProbeOutcome{
			commonapi.GetNamespacedNameWithKind(&exe1): {"exe1-http-probe": apiv1.HealthProbeOutcomeSuccess},
		}, lock) == nil, nil
	})
	require.NoError(t, waitErr, "Expected health reports for Executable `exe1` are missing")

	// Cancel the context and verify that the subscription is cancelled
	hpsCtxCancel()

	// Verify reports stopped coming in
	const periodsToWait = 5
	noMoreReportsErr := testutil.LengthNotChanging(ctx, &reports, expectedStatePollInterval, periodsToWait, lock)
	require.NoError(t, noMoreReportsErr, "Reports should stop coming in after cancelling the HealthProbeSet context")

	require.True(t, sub.Cancelled(), "Subscription should be cancelled after cancelling the HealthProbeSet context")
}

type healthProbeReportWithSubscription struct {
	Report       HealthProbeReport
	Subscription string
}

func getHttpProbe(name string) apiv1.HealthProbe {
	return getHttpProbeWithInterval(name, healthProbeInterval)
}

func getHttpProbeWithInterval(name string, interval time.Duration) apiv1.HealthProbe {
	return apiv1.HealthProbe{
		Name: name,
		Type: apiv1.HealthProbeTypeHttp,
		HttpProbe: &apiv1.HttpProbe{
			Url: "http://localhost:8080/healthz", // Dummy URL, won't be really called by tests in this file
		},
		Schedule: apiv1.HealthProbeSchedule{
			Interval: metav1.Duration{Duration: interval},
		},
	}
}

func storeResults(
	lifetimeCtx context.Context,
	in <-chan HealthProbeReport,
	reports *[]healthProbeReportWithSubscription,
	subscription string,
	lock *sync.Mutex,
) {
	for {
		select {
		case report, isOpen := <-in:
			if !isOpen {
				return
			}

			lock.Lock()
			rws := healthProbeReportWithSubscription{
				Report:       report,
				Subscription: subscription,
			}
			*reports = append(*reports, rws)
			lock.Unlock()

		case <-lifetimeCtx.Done():
			return
		}
	}
}

func subscriberHasResults(
	subscription string,
	reports *[]healthProbeReportWithSubscription,
	expected map[commonapi.NamespacedNameWithKind]map[string]apiv1.HealthProbeOutcome,
	lock *sync.Mutex,
) error {
	lock.Lock()
	defer lock.Unlock()

	subscriptionReports := slices.Select(*reports, func(rws *healthProbeReportWithSubscription) bool {
		return rws.Subscription == subscription
	})
	err := VerifyHealthReports(expected, slices.Map[HealthProbeReport](
		subscriptionReports, func(rws *healthProbeReportWithSubscription) HealthProbeReport { return rws.Report },
	))
	return err
}
