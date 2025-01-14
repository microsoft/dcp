package health

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	pq "github.com/emirpasic/gods/queues/priorityqueue"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/pubsub"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
)

var (
	// The value used for probe next execution time when the probe is not supposed to be executed anymore,
	// or when the next execution time has not been computed yet.
	unknownFuture = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)

	// The minimum interval between runs of the probe scheduling algorithm.
	minScheduleInterval = 100 * time.Millisecond
)

// HealthProbeSet holds a set of active health probes and schedules their execution.
type HealthProbeSet struct {
	// Lifetime context of the probe set. Used for cancelling probe execution and subscriptions.
	lifetimeCtx context.Context

	// The health probe result subscriptions (typically object controllers).
	resultSubscriptions map[schema.GroupVersionKind]*pubsub.SubscriptionSet[HealthProbeReport]

	// A map that allows finding a probe by its identifier.
	probesById map[healthProbeIdentifier]*healthProbeDescriptor

	// A priority queue used to schedule probe executions.
	probesByExecutionTime *pq.Queue

	// The cancellation function for the probe scheduler goroutine.
	// The function is nil if the probe scheduler goroutine is not running.
	cancelProbeScheduler context.CancelFunc

	// Log for diagnostic information
	log logr.Logger

	// Probe probeExecutors for different probe types.
	probeExecutors map[apiv1.HealthProbeType]HealthProbeExecutor

	// Execution queue for the probes.
	execQueue *resiliency.WorkQueue

	// An event that is set when there are probes to execute.
	haveProbesToExecute *concurrency.AutoResetEvent

	// The number of active probes (that are subject to execution).
	activeProbes uint

	// The mutex that makes the probe set goroutine-safe.
	lock *sync.Mutex
}

func NewHealthProbeSet(lifetimeCtx context.Context, log logr.Logger, executors map[apiv1.HealthProbeType]HealthProbeExecutor) *HealthProbeSet {
	hps := HealthProbeSet{
		lifetimeCtx:           lifetimeCtx,
		resultSubscriptions:   make(map[schema.GroupVersionKind]*pubsub.SubscriptionSet[HealthProbeReport]),
		probesById:            make(map[healthProbeIdentifier]*healthProbeDescriptor),
		probesByExecutionTime: pq.NewWith(compareExecutionTimes),
		cancelProbeScheduler:  nil,
		log:                   log,
		probeExecutors:        executors,
		execQueue:             resiliency.NewWorkQueue(lifetimeCtx, resiliency.DefaultConcurrency),
		haveProbesToExecute:   concurrency.NewAutoResetEvent(false),
		lock:                  &sync.Mutex{},
	}

	context.AfterFunc(lifetimeCtx, func() { hps.shutdown() })

	return &hps
}

// Subscribe to be notified about health probe results.
// Subscriptions are specific to the kind of object that owns health probes (ownerKind parameter).
func (hps *HealthProbeSet) Subscribe(sink chan<- HealthProbeReport, ownerKind schema.GroupVersionKind) (*pubsub.Subscription[HealthProbeReport], error) {
	if sink == nil {
		return nil, fmt.Errorf("sink cannot be nil")
	}
	if ownerKind.Empty() {
		return nil, fmt.Errorf("ownerKind cannot be empty")
	}
	if hps.lifetimeCtx.Err() != nil {
		return nil, hps.lifetimeCtx.Err()
	}

	hps.lock.Lock()
	defer hps.lock.Unlock()

	ss, found := hps.resultSubscriptions[ownerKind]
	if !found {
		ss = pubsub.NewSubscriptionSet[HealthProbeReport](nil, hps.lifetimeCtx)
		hps.resultSubscriptions[ownerKind] = ss
	}

	sub := ss.Subscribe(sink)
	return sub, nil
}

// Adds a set of health probes to the probe set and schedules their execution as necessary.
func (hps *HealthProbeSet) EnableProbes(owner apiv1.NamespacedNameWithKind, probes []apiv1.HealthProbe) error {
	if owner.Empty() {
		return fmt.Errorf("owner cannot be empty")
	}
	if hps.lifetimeCtx.Err() != nil {
		return hps.lifetimeCtx.Err()
	}

	hps.lock.Lock()
	defer hps.lock.Unlock()
	var err error

	hps.log.V(1).Info("Enabling health probes", "Owner", owner.String(), "NumberOfProbes", len(probes))

	for _, probe := range probes {
		pprobe := probe.DeepCopy()
		pd := newHealthProbeDescriptor(pprobe, owner)

		if existingPD, found := hps.probesById[pd.identifier]; found {
			// The clients can call EnableProbes multiple times with the same owner.
			// As long as the set of probes is the same, that's fine.

			if !existingPD.probe.Equal(pprobe) {
				err = errors.Join(err, fmt.Errorf("probe %s for %s already exists with different definition: %s vs %s", probe.Name, owner.String(), probe.String(), existingPD.probe.String()))
			} else {
				hps.log.V(1).Info("Probe already exists for owner, ignoring", "Probe", probe.Name, "Owner", owner.String())
			}

			continue
		}

		hps.probesById[pd.identifier] = pd
		if pd.nextExecutionTime != unknownFuture {
			hps.log.V(1).Info("Scheduling health probe", "Probe", probe.Name, "Owner", owner.String(), "NextExecutionTime", pd.nextExecutionTime)
			hps.probesByExecutionTime.Enqueue(pd)
			hps.activeProbes++
			hps.haveProbesToExecute.Set()
		}
	}

	hps.ensureSchedulerRunning()

	return nil
}

// Removes all health probes owned by the specified object and cancels remaining probe executions, if any.
func (hps *HealthProbeSet) DisableProbes(owner apiv1.NamespacedNameWithKind) {
	if owner.Empty() {
		return
	}

	hps.log.V(1).Info("Disabling health probes", "Owner", owner.String())

	hps.lock.Lock()
	defer hps.lock.Unlock()

	newProbesById := make(map[healthProbeIdentifier]*healthProbeDescriptor, len(hps.probesById))
	for _, pd := range hps.probesById {
		if pd.owner != owner {
			newProbesById[pd.identifier] = pd
		} else if pd.cancelExecution != nil {
			hps.log.V(1).Info("Cancelling health probe execution", "Probe", pd.probe.Name, "Owner", owner.String())
			pd.cancelExecution()
			pd.cancelExecution = nil
			if pd.nextExecutionTime != unknownFuture {
				hps.activeProbes--
			}
		}
	}
	hps.probesById = newProbesById

	newProbesByExecutionTime := pq.NewWith(compareExecutionTimes)
	it := hps.probesByExecutionTime.Iterator()
	for it.Next() {
		hd := it.Value().(*healthProbeDescriptor)
		if hd.owner != owner {
			newProbesByExecutionTime.Enqueue(hd)
		} else {
			hps.log.V(1).Info("Removing health probe from execution queue", "Probe", hd.probe.Name, "Owner", owner.String())
		}
	}
	hps.probesByExecutionTime = newProbesByExecutionTime

	if hps.cancelProbeScheduler != nil && hps.activeProbes == 0 {
		hps.log.V(1).Info("No more health probes that need execution, cancelling probe scheduler...")
		hps.cancelProbeScheduler()
		hps.cancelProbeScheduler = nil
	}
}

func (hps *HealthProbeSet) ensureSchedulerRunning() {
	// Assumes hps.lock is held
	if hps.cancelProbeScheduler == nil && hps.activeProbes > 0 && hps.lifetimeCtx.Err() == nil {
		hps.log.V(1).Info("Starting health probe scheduler...")
		ctx, cancel := context.WithCancel(hps.lifetimeCtx)
		hps.cancelProbeScheduler = cancel
		hps.haveProbesToExecute.Set()
		go hps.runProbeScheduler(ctx)
	}
}

// Schedules probe executions as necessary, according to HealthProbeDescriptor.nextExecutionTime.
// The scheduler wakes up under two conditions:
//  1. expiration of the timer that is set based on the next probe execution time
//  2. activation of the event set when a descriptor is added to the probe execution queue
func (hps *HealthProbeSet) runProbeScheduler(schedulerCtx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-schedulerCtx.Done():
			hps.log.V(1).Info("Health scheduler context was cancelled, scheduler is shutting down...")
			return
		case <-timer.C: // Continue below
			hps.log.V(1).Info("Probe scheduler timer expired, processing probe queue...")
		case <-hps.haveProbesToExecute.Wait(): // Continue below
			hps.log.V(1).Info("Probe scheduler was waken up, processing probe queue...")
		}

		// Use anonymous func to make it less likely that the lock will be left locked
		// before waiting on the timer.
		func() {
			hps.lock.Lock()
			defer hps.lock.Unlock()

			if hps.activeProbes == 0 {
				hps.log.V(1).Info("No more health probes that need execution, probe scheduler is cancelling itself...")
				if hps.cancelProbeScheduler != nil {
					hps.cancelProbeScheduler()
					hps.cancelProbeScheduler = nil
				}
				return
			}

			hps.scheduleProbes()

			pd, haveData := hps.peekNextProbeToExecute()
			if haveData {
				waitDuration := time.Until(pd.nextExecutionTime)
				if waitDuration < minScheduleInterval {
					waitDuration = minScheduleInterval
				}
				if hps.log.V(1).Enabled() {
					hps.log.V(1).Info("Next probe scheduler pass determined",
						"WaitDuration", waitDuration,
						"MostUrgentProbe", pd.probe.Name,
						"Owner", pd.owner.String(),
					)
				}
				timer.Reset(waitDuration)
			} else {
				hps.log.V(1).Info("No more probes to schedule, waiting...")
			}
		}()
	}
}

// Schedule executions of all probes that are due
func (hps *HealthProbeSet) scheduleProbes() {
	// Assumes hps.lock is held

	now := time.Now()

	for {
		pd, haveData := hps.peekNextProbeToExecute()
		if !haveData {
			if hps.log.V(1).Enabled() {
				hps.log.V(1).Info("The probe execution queue is empty; scheduling pass completed")
			}
			break
		}
		if pd.nextExecutionTime.After(now) {
			if hps.log.V(1).Enabled() {
				hps.log.V(1).Info("The most urgent probe is not due yet; scheduling pass completed",
					"MostUrgentProbe", pd.probe.Name,
					"Owner", pd.owner.String(),
					"NextExecutionTime", pd.nextExecutionTime,
				)
			}
			break
		}

		_, _ = hps.probesByExecutionTime.Dequeue()

		probeExecutor, found := hps.probeExecutors[pd.probe.Type]
		if !found {
			hps.log.Error(fmt.Errorf("no executor found for health probe"), "",
				"Probe", pd.probe.String(),
				"Owner", pd.owner.String(),
			)
			continue
		}

		executionCtx, executionCtxCancel := context.WithCancel(hps.lifetimeCtx)
		pd.cancelExecution = executionCtxCancel

		if hps.log.V(1).Enabled() {
			hps.log.V(1).Info("Scheduling health probe execution",
				"Probe", pd.probe.Name,
				"Owner", pd.owner.String(),
			)
		}
		enqueueErr := hps.execQueue.Enqueue(func(_ context.Context) {
			hps.executeSingleProbe(executionCtx, pd, probeExecutor)
			executionCtxCancel()
		})
		if enqueueErr != nil {
			// This can only happen if the lifetime context context is done
			hps.log.V(1).Info("The health probe set context expired, cancelling probe scheduling pass")
			break
		}
	}
}

// Peek into probe execution queue and return next probe descriptor if available.
func (hps *HealthProbeSet) peekNextProbeToExecute() (*healthProbeDescriptor, bool) {
	// Assumes hps.lock is held
tryAgain:
	val, hadData := hps.probesByExecutionTime.Peek()
	if !hadData {
		return nil, false
	}

	pd, ok := val.(*healthProbeDescriptor)
	if !ok || pd == nil {
		// Should never happen
		hps.log.Error(errors.New("unexpected nil value in the probe execution queue"), "")
		hps.probesByExecutionTime.Dequeue() // Get rid of the bad value
		goto tryAgain
	}

	return pd, true
}

func (hps *HealthProbeSet) executeSingleProbe(executionCtx context.Context, pd *healthProbeDescriptor, probeExecutor HealthProbeExecutor) {
	if hps.lifetimeCtx.Err() != nil {
		return
	}

	hps.lock.Lock()
	probe := pd.probe
	owner := pd.owner
	ss, ssFound := hps.resultSubscriptions[owner.Kind]
	_, found := hps.probesById[pd.identifier]
	hps.lock.Unlock()

	if !found {
		hps.log.V(1).Info("Health probe was disabled before execution",
			"Probe", probe.Name,
			"Owner", owner.String(),
		)
		return
	}

	if !ssFound {
		// This can happen if the subscription was cancelled just after the probe was scheduled.
		hps.log.V(1).Info("No subscribers found for health probe result",
			"Probe", probe.Name,
			"Owner", owner.String(),
		)
		return
	}

	var result apiv1.HealthProbeResult
	var executionErr error

	if hps.log.V(1).Enabled() {
		hps.log.V(1).Info("Starting health probe execution...",
			"Probe", probe.Name,
			"Owner", owner.String(),
		)
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				executionErr = fmt.Errorf("panic encountered during health probe execution: %v", r)
			}
		}()

		result, executionErr = probeExecutor.Execute(executionCtx, probe)
	}()

	if executionErr != nil {
		hps.log.Error(executionErr, "health probe execution failed",
			"Probe", probe.Name,
			"Owner", owner.String(),
		)
		result = apiv1.HealthProbeResult{
			Outcome:   apiv1.HealthProbeOutcomeUnknown,
			Timestamp: metav1.NowMicro(),
			ProbeName: probe.Name,
			Reason:    executionErr.Error(),
		}
	} else {
		if hps.log.V(1).Enabled() {
			hps.log.V(1).Info("Completed health probe execution",
				"Probe", probe.Name,
				"Owner", owner.String(),
				"Result", result.Outcome,
				"Reason", result.Reason,
			)
		}
	}
	report := HealthProbeReport{
		Probe:  probe,
		Result: result,
		Owner:  owner,
	}

	// Notify all subscribers first, then schedule the next execution
	ss.Notify(report)
	if hps.log.V(1).Enabled() {
		hps.log.V(1).Info("Subscribers notified about health probe result",
			"Probe", probe.Name,
			"Owner", owner.String(),
		)
	}

	hps.lock.Lock()
	defer hps.lock.Unlock()

	if hps.lifetimeCtx.Err() != nil {
		return
	}

	_, found = hps.probesById[pd.identifier]
	if !found {
		if hps.log.V(1).Enabled() {
			hps.log.V(1).Info("Health probe was disabled after execution",
				"Probe", probe.Name,
				"Owner", owner.String(),
			)
		}
		return
	}

	pd.lastResult = &result
	pd.computeNextExecutionTime()
	if pd.nextExecutionTime != unknownFuture && hps.lifetimeCtx.Err() == nil {
		if hps.log.V(1).Enabled() {
			hps.log.V(1).Info("Scheduling next health probe execution",
				"Probe", probe.Name,
				"Owner", owner.String(),
				"NextExecutionTime", pd.nextExecutionTime,
			)
		}
		hps.probesByExecutionTime.Enqueue(pd)
		hps.haveProbesToExecute.Set()
	} else {
		if hps.log.V(1).Enabled() {
			hps.log.V(1).Info("Health probe won't be scheduled for execution anymore (became inactive, or lifetime context expired)",
				"Probe", probe.Name,
				"Owner", owner.String(),
			)
		}
		hps.activeProbes--
	}
}

// Cancels all probe executions and subscriptions.
// Called when lifetime context is cancelled.
func (hps *HealthProbeSet) shutdown() {
	hps.lock.Lock()
	defer hps.lock.Unlock()

	hps.log.V(1).Info("Shutting down health probe set...")

	if hps.cancelProbeScheduler != nil {
		hps.cancelProbeScheduler()
		hps.cancelProbeScheduler = nil
	}

	for _, hd := range hps.probesById {
		if hd.cancelExecution != nil {
			hd.cancelExecution()
			hd.cancelExecution = nil
		}
	}

	for _, ss := range hps.resultSubscriptions {
		ss.CancelAll()
	}
}
