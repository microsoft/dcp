/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"

	"github.com/go-logr/logr"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/ide"
	"github.com/microsoft/dcp/internal/logs"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/pointers"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

// IdeSessionClient captures the subset of ide.Client behavior the IdeSession reconciler depends on.
// It is defined as an interface so tests can substitute a fake implementation without standing up
// a live IDE.
type IdeSessionClient interface {
	StartSession(ctx context.Context, req ide.StartSessionRequest, handler ide.SessionHandler, log logr.Logger) (*ide.StartSessionResult, error)
	StopSession(ctx context.Context, sessionID ide.SessionID, log logr.Logger) error
	ReleaseSession(ctx context.Context, sessionID ide.SessionID, log logr.Logger) error
}

const (
	// startingIdeSessionPrefix prefixes the temporary state-map key used while a
	// StartSession request is in flight (the IDE-assigned session ID is not yet known).
	startingIdeSessionPrefix = "starting-"

	// maxParallelIdeSessionStartups bounds concurrent IDE-session start operations.
	maxParallelIdeSessionStartups = 8

	// maxParallelIdeSessionStops bounds concurrent IDE-session stop operations.
	maxParallelIdeSessionStops = math.MaxUint8
)

// ideSessionStateKey is the secondary key for the ObjectStateMap.
// While a session is being started it carries a startingIdeSessionPrefix-prefixed
// value tied to the object's namespaced name; once the IDE returns a session ID,
// the entry is rekeyed to that ID via UpdateChangingStateKey.
type ideSessionStateKey string

type ideSessionStateInitializerFunc = stateInitializerFunc[
	apiv1.IdeSession, *apiv1.IdeSession,
	IdeSessionReconciler, *IdeSessionReconciler,
	apiv1.IdeSessionState,
	IdeSessionRunInfo, *IdeSessionRunInfo,
]

var ideSessionStateInitializers = map[apiv1.IdeSessionState]ideSessionStateInitializerFunc{
	apiv1.IdeSessionStateInitial:  manageIdeSessionInitial,
	apiv1.IdeSessionStateStarting: manageIdeSessionStarting,
	apiv1.IdeSessionStateRunning:  manageIdeSessionRunning,
	apiv1.IdeSessionStateStopping: manageIdeSessionStopping,
	apiv1.IdeSessionStateStopped:  manageIdeSessionFinal,
	apiv1.IdeSessionStateFailed:   manageIdeSessionFinal,
}

type ideSessionRunMap = ObjectStateMap[ideSessionStateKey, IdeSessionRunInfo, *IdeSessionRunInfo, *apiv1.IdeSession]

var (
	ideSessionFinalizer = fmt.Sprintf("%s/ide-session-reconciler", apiv1.GroupVersion.Group)
)

// IdeSessionReconciler reconciles IdeSession objects by delegating IDE-side work to an
// injected IdeSessionClient (typically a shared ide.Client). It uses the same
// ReconcilerBase + state-initializer-map approach used by the other DCP controllers.
type IdeSessionReconciler struct {
	*ReconcilerBase[apiv1.IdeSession, *apiv1.IdeSession]

	client IdeSessionClient

	// runs stores per-session run info, keyed by namespaced name (primary key)
	// and by IDE session ID or starting key (secondary key).
	runs *ideSessionRunMap

	// startupQueue bounds concurrent IDE-session start operations.
	startupQueue *resiliency.WorkQueue

	// stopQueue bounds concurrent IDE-session stop operations.
	stopQueue *resiliency.WorkQueue
}

// NewIdeSessionReconciler builds a reconciler bound to the supplied IDE-side client.
func NewIdeSessionReconciler(
	lifetimeCtx context.Context,
	k8sClient ctrl_client.Client,
	noCacheClient ctrl_client.Reader,
	log logr.Logger,
	ideClient IdeSessionClient,
) *IdeSessionReconciler {
	base := NewReconcilerBase[apiv1.IdeSession](k8sClient, noCacheClient, log, lifetimeCtx)
	return &IdeSessionReconciler{
		ReconcilerBase: base,
		client:         ideClient,
		runs:           NewObjectStateMap[ideSessionStateKey, IdeSessionRunInfo, *IdeSessionRunInfo, *apiv1.IdeSession](),
		startupQueue:   resiliency.NewWorkQueue(lifetimeCtx, maxParallelIdeSessionStartups),
		stopQueue:      resiliency.NewWorkQueue(lifetimeCtx, maxParallelIdeSessionStops),
	}
}

func (r *IdeSessionReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv1.IdeSession{}).
		WatchesRawSource(r.GetReconciliationEventSource()).
		Named(name).
		Complete(r)
}

func (r *IdeSessionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reader, log := r.StartReconciliation(req)

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	s := apiv1.IdeSession{}
	if err := reader.Get(ctx, req.NamespacedName, &s); err != nil {
		if apierrors.IsNotFound(err) {
			r.runs.DeleteByNamespacedName(req.NamespacedName)
			log.V(1).Info("IdeSession not found, nothing to do...")
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to Get() the IdeSession", "IdeSession", s)
		getFailedCounter.Add(ctx, 1)
		return ctrl.Result{}, err
	}
	getSucceededCounter.Add(ctx, 1)

	log = log.WithValues(logger.RESOURCE_LOG_STREAM_ID, s.GetResourceId())

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(s.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	r.runs.RunDeferredOps(req.NamespacedName, &s)

	if s.DeletionTimestamp != nil && !s.DeletionTimestamp.IsZero() {
		change = r.handleDeletionRequest(ctx, &s, log)
	} else if change = ensureFinalizer(&s, ideSessionFinalizer, log); change != noChange {
		// Finalizer added; defer the rest of the work to the next reconciliation pass.
	} else {
		change = r.manageIdeSession(ctx, &s, log)
	}

	return r.SaveChanges(ctx, &s, patch, change, nil, log)
}

func (r *IdeSessionReconciler) handleDeletionRequest(ctx context.Context, s *apiv1.IdeSession, log logr.Logger) objectChange {
	_, runInfo := r.runs.BorrowByNamespacedName(s.NamespacedName())

	switch {
	case runInfo == nil:
		log.V(1).Info("IdeSession is being deleted (deleting finalizer only)...")
		return deleteFinalizer(s, ideSessionFinalizer, log)

	case runInfo.State == apiv1.IdeSessionStateStarting || runInfo.State == apiv1.IdeSessionStateStopping:
		log.V(1).Info("IdeSession is being deleted, waiting for transient state to settle...",
			"CurrentState", runInfo.State)
		return r.manageIdeSession(ctx, s, log)

	case runInfo.State == apiv1.IdeSessionStateRunning:
		log.V(1).Info("IdeSession is being deleted, but it needs to be stopped first...")
		runInfo.State = apiv1.IdeSessionStateStopping
		change := manageIdeSessionStopping(ctx, r, s, apiv1.IdeSessionStateStopping, runInfo, log)
		_ = r.runs.Update(s.NamespacedName(), runInfoStateKey(runInfo), runInfo)
		return change

	default:
		log.V(1).Info("IdeSession is being deleted (in initial or terminal state; releasing resources)...",
			"CurrentState", runInfo.State)
		r.releaseIdeSessionResources(s, runInfo, log)
		r.runs.DeleteByNamespacedName(s.NamespacedName())
		return deleteFinalizer(s, ideSessionFinalizer, log)
	}
}

func (r *IdeSessionReconciler) manageIdeSession(ctx context.Context, s *apiv1.IdeSession, log logr.Logger) objectChange {
	targetState := s.Status.State
	_, runInfo := r.runs.BorrowByNamespacedName(s.NamespacedName())
	if runInfo != nil {
		// In-memory state is the source of truth.
		targetState = runInfo.State
	}

	initializer := getStateInitializer(ideSessionStateInitializers, targetState, log)
	change := initializer(ctx, r, s, targetState, runInfo, log)

	if runInfo != nil {
		_ = r.runs.Update(s.NamespacedName(), runInfoStateKey(runInfo), runInfo)
	}

	return change
}

// runInfoStateKey produces the secondary state-map key for the given run info.
// While the session is being started (no SessionID yet) the key is derived
// from the IdeSession UID; once the IDE returns a session ID, that value is used.
func runInfoStateKey(ri *IdeSessionRunInfo) ideSessionStateKey {
	if ri.SessionID != "" {
		return ideSessionStateKey(ri.SessionID)
	}
	return startingStateKey(ri.UID)
}

func startingStateKey(uid types.UID) ideSessionStateKey {
	return ideSessionStateKey(startingIdeSessionPrefix + string(uid))
}

func manageIdeSessionInitial(
	_ context.Context,
	r *IdeSessionReconciler,
	s *apiv1.IdeSession,
	_ apiv1.IdeSessionState,
	runInfo *IdeSessionRunInfo,
	log logr.Logger,
) objectChange {
	if s.Spec.DesiredState != apiv1.IdeSessionStateRunning {
		// Nothing to do until the user asks for the session to be started.
		return noChange
	}

	// If runInfo is nil, we need to bootstrap a new entry in the run map. We must Store
	// the run info AFTER beginIdeSessionStartup has applied its mutations (state and the
	// startQueued latch), otherwise the next reconciliation pass would observe a stale
	// initial-state entry and would re-enqueue the startup. The caller (manageIdeSession)
	// only Update()s the map when runInfo was non-nil on entry, so the bootstrap path
	// has to perform its own Store().
	bootstrap := runInfo == nil
	if bootstrap {
		runInfo = NewIdeSessionRunInfo(s)
	}

	runInfo.State = apiv1.IdeSessionStateStarting
	log.V(1).Info("Beginning IdeSession startup")
	change := r.beginIdeSessionStartup(s, runInfo, log)

	if bootstrap {
		r.runs.Store(s.NamespacedName(), runInfoStateKey(runInfo), runInfo.Clone())
	}

	return change
}

func manageIdeSessionStarting(
	_ context.Context,
	r *IdeSessionReconciler,
	s *apiv1.IdeSession,
	_ apiv1.IdeSessionState,
	runInfo *IdeSessionRunInfo,
	log logr.Logger,
) objectChange {
	if runInfo == nil {
		// Should not happen, but be defensive: fall back to the initial handler so the
		// session can be (re)started.
		return manageIdeSessionInitial(nil, r, s, apiv1.IdeSessionStateInitial, runInfo, log)
	}

	if !runInfo.startQueued {
		return r.beginIdeSessionStartup(s, runInfo, log)
	}

	// Startup is in flight; just keep the IdeSession status in sync.
	return r.setIdeSessionState(s, apiv1.IdeSessionStateStarting) | runInfo.ApplyTo(s, log)
}

func manageIdeSessionRunning(
	_ context.Context,
	r *IdeSessionReconciler,
	s *apiv1.IdeSession,
	_ apiv1.IdeSessionState,
	runInfo *IdeSessionRunInfo,
	log logr.Logger,
) objectChange {
	change := r.setIdeSessionState(s, apiv1.IdeSessionStateRunning)

	if runInfo == nil {
		return change
	}

	if s.Spec.DesiredState == apiv1.IdeSessionStateStopped {
		runInfo.State = apiv1.IdeSessionStateStopping
		change |= r.setIdeSessionState(s, apiv1.IdeSessionStateStopping)
		return change
	}

	change |= runInfo.ApplyTo(s, log)
	return change
}

func manageIdeSessionStopping(
	_ context.Context,
	r *IdeSessionReconciler,
	s *apiv1.IdeSession,
	_ apiv1.IdeSessionState,
	runInfo *IdeSessionRunInfo,
	log logr.Logger,
) objectChange {
	change := r.setIdeSessionState(s, apiv1.IdeSessionStateStopping)
	if runInfo == nil {
		return change
	}

	if !runInfo.stopAttemptInitiated {
		runInfo.stopAttemptInitiated = true
		runInfoCopy := runInfo.Clone()
		enqErr := r.stopQueue.Enqueue(r.makeStopOp(s.NamespacedName(), runInfoCopy, log))
		if enqErr != nil {
			log.Error(enqErr, "Could not enqueue IdeSession stop operation")
			runInfo.State = apiv1.IdeSessionStateFailed
			runInfo.Message = fmt.Sprintf("could not enqueue stop operation: %v", enqErr)
			runInfo.FinishTimestamp = metav1.NowMicro()
			return change | manageIdeSessionFinal(nil, r, s, apiv1.IdeSessionStateFailed, runInfo, log)
		}
	}

	change |= runInfo.ApplyTo(s, log)
	return change
}

func manageIdeSessionFinal(
	_ context.Context,
	r *IdeSessionReconciler,
	s *apiv1.IdeSession,
	desiredState apiv1.IdeSessionState,
	runInfo *IdeSessionRunInfo,
	log logr.Logger,
) objectChange {
	change := r.setIdeSessionState(s, desiredState)
	if runInfo == nil {
		return change
	}

	change |= runInfo.ApplyTo(s, log)
	return change
}

func (r *IdeSessionReconciler) setIdeSessionState(s *apiv1.IdeSession, state apiv1.IdeSessionState) objectChange {
	if s.Status.State == state {
		return noChange
	}
	s.Status.State = state
	return statusChanged
}

// beginIdeSessionStartup queues the asynchronous startup operation, marks the run info
// as having a startup in flight, and stamps Status with the Starting state.
func (r *IdeSessionReconciler) beginIdeSessionStartup(s *apiv1.IdeSession, runInfo *IdeSessionRunInfo, log logr.Logger) objectChange {
	runInfo.State = apiv1.IdeSessionStateStarting
	runInfo.startQueued = true

	enqErr := r.startupQueue.Enqueue(func(opCtx context.Context) {
		r.doStartIdeSession(opCtx, s.NamespacedName(), runInfo.UID, s.Spec.LaunchConfigurations, log)
	})
	if enqErr != nil {
		log.Error(enqErr, "Could not enqueue IdeSession start operation")
		runInfo.State = apiv1.IdeSessionStateFailed
		runInfo.Message = fmt.Sprintf("could not enqueue start operation: %v", enqErr)
		runInfo.StartupTimestamp = metav1.NowMicro()
		runInfo.FinishTimestamp = runInfo.StartupTimestamp
		return r.setIdeSessionState(s, apiv1.IdeSessionStateFailed) | runInfo.ApplyTo(s, log)
	}

	return r.setIdeSessionState(s, apiv1.IdeSessionStateStarting) | runInfo.ApplyTo(s, log)
}

// doStartIdeSession is run on the startup work queue; it creates the stdout/stderr capture
// files, builds the StartSessionRequest, and submits it to the IDE. On completion (success
// or failure) it queues a deferred op so that the reconciliation loop observes the result.
func (r *IdeSessionReconciler) doStartIdeSession(
	ctx context.Context,
	name types.NamespacedName,
	uid types.UID,
	launchConfigurations string,
	log logr.Logger,
) {
	pendingUpdate := &IdeSessionRunInfo{UID: uid}

	finalizeStartup := func(failureErr error) {
		if failureErr != nil {
			pendingUpdate.State = apiv1.IdeSessionStateFailed
			pendingUpdate.Message = failureErr.Error()
			pendingUpdate.FinishTimestamp = metav1.NowMicro()
		}
		runMap := r.runs
		r.runs.QueueDeferredOp(name, func(_ types.NamespacedName, _ ideSessionStateKey, _ *apiv1.IdeSession) {
			// Find the entry under whatever key it lives under and rekey if needed.
			currentKey, _ := runMap.BorrowByNamespacedName(name)
			newKey := currentKey
			if pendingUpdate.SessionID != "" {
				newKey = ideSessionStateKey(pendingUpdate.SessionID)
			}
			if currentKey == newKey {
				_ = runMap.Update(name, currentKey, pendingUpdate)
			} else {
				_ = runMap.UpdateChangingStateKey(name, currentKey, newKey, pendingUpdate)
			}
		})
		r.ScheduleReconciliation(name)
	}

	var lcs json.RawMessage
	if err := json.Unmarshal([]byte(launchConfigurations), &lcs); err != nil {
		finalizeStartup(fmt.Errorf("launch configurations could not be parsed: %w", err))
		return
	}

	stdOutFile, stdOutErr := usvc_io.OpenTempFile(fmt.Sprintf("%s_out_%s", name.Name, uid), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if stdOutErr != nil {
		log.Error(stdOutErr, "Failed to create temporary file for capturing IdeSession standard output")
	}
	stdErrFile, stdErrErr := usvc_io.OpenTempFile(fmt.Sprintf("%s_err_%s", name.Name, uid), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if stdErrErr != nil {
		log.Error(stdErrErr, "Failed to create temporary file for capturing IdeSession standard error")
	}

	bridge := newIdeSessionBridge(r, name, uid, stdOutFile, stdErrFile)
	if stdOutFile != nil {
		pendingUpdate.StdOutFile = stdOutFile.Name()
	}
	if stdErrFile != nil {
		pendingUpdate.StdErrFile = stdErrFile.Name()
	}

	startRes, startErr := r.client.StartSession(ctx, ide.StartSessionRequest{
		LaunchConfigurations: lcs,
	}, bridge, log)
	pendingUpdate.StartupTimestamp = metav1.NowMicro()
	if startErr != nil {
		bridge.closeOutputFiles()
		finalizeStartup(fmt.Errorf("IdeSession could not be started: %w", startErr))
		return
	}

	pendingUpdate.SessionID = string(startRes.SessionID)
	bridge.sessionID = startRes.SessionID

	if startRes.EarlyTermination != nil {
		bridge.closeOutputFiles()
		if startRes.EarlyTermination.Failed {
			pendingUpdate.State = apiv1.IdeSessionStateFailed
			pendingUpdate.Message = "IDE reported session start failure"
		} else {
			pendingUpdate.State = apiv1.IdeSessionStateStopped
		}
		if startRes.EarlyTermination.ExitCode != apiv1.UnknownExitCode {
			pendingUpdate.ExitCode = pointers.Duplicate(startRes.EarlyTermination.ExitCode)
		}
		pendingUpdate.FinishTimestamp = metav1.NowMicro()
		finalizeStartup(nil)
		return
	}

	pendingUpdate.State = apiv1.IdeSessionStateRunning
	finalizeStartup(nil)
	startRes.ConfirmHandlerReady()
}

// makeStopOp returns a function (suitable for enqueueing on stopQueue) that asks the IDE
// to stop the session described by runInfoCopy. On error, the session is recorded as Failed.
func (r *IdeSessionReconciler) makeStopOp(name types.NamespacedName, runInfoCopy *IdeSessionRunInfo, log logr.Logger) func(context.Context) {
	return func(opCtx context.Context) {
		if runInfoCopy.SessionID == "" {
			log.V(1).Info("Stop requested for an IdeSession that never received a session ID; nothing to do")
			return
		}

		stopErr := r.client.StopSession(opCtx, ide.SessionID(runInfoCopy.SessionID), log)
		if stopErr == nil {
			// OnTerminated will land via the bridge and drive the transition to Stopped.
			return
		}

		log.Error(stopErr, "Could not stop the IdeSession", "SessionID", runInfoCopy.SessionID)
		update := &IdeSessionRunInfo{
			UID:             runInfoCopy.UID,
			SessionID:       runInfoCopy.SessionID,
			State:           apiv1.IdeSessionStateFailed,
			Message:         fmt.Sprintf("could not stop IDE session: %v", stopErr),
			FinishTimestamp: metav1.NowMicro(),
		}
		runMap := r.runs
		stateKey := ideSessionStateKey(runInfoCopy.SessionID)
		r.runs.QueueDeferredOp(name, func(_ types.NamespacedName, _ ideSessionStateKey, _ *apiv1.IdeSession) {
			_ = runMap.Update(name, stateKey, update)
		})
		r.ScheduleReconciliation(name)
	}
}

func (r *IdeSessionReconciler) releaseIdeSessionResources(s *apiv1.IdeSession, runInfo *IdeSessionRunInfo, log logr.Logger) {
	r.deleteOutputFiles(s, log)
	logger.ReleaseResourceLog(s.GetResourceId())
	if runInfo != nil && runInfo.SessionID != "" {
		if releaseErr := r.client.ReleaseSession(context.Background(), ide.SessionID(runInfo.SessionID), log); releaseErr != nil {
			log.Error(releaseErr, "Could not release IdeSession with the IDE client", "SessionID", runInfo.SessionID)
		}
	}
}

func (r *IdeSessionReconciler) deleteOutputFiles(s *apiv1.IdeSession, log logr.Logger) {
	if osutil.EnvVarSwitchEnabled(usvc_io.DCP_PRESERVE_EXECUTABLE_LOGS) {
		return
	}

	if s.Status.StdOutFile != "" {
		path := s.Status.StdOutFile
		_ = r.stopQueue.Enqueue(func(opCtx context.Context) {
			if err := logs.RemoveWithRetry(opCtx, path); err != nil {
				log.Error(err, "Could not remove IdeSession standard output file", "Path", path)
			}
		})
	}
	if s.Status.StdErrFile != "" {
		path := s.Status.StdErrFile
		_ = r.stopQueue.Enqueue(func(opCtx context.Context) {
			if err := logs.RemoveWithRetry(opCtx, path); err != nil {
				log.Error(err, "Could not remove IdeSession standard error file", "Path", path)
			}
		})
	}
}

// ----- Notification handling (ide.SessionHandler) -----

// processSessionNotification queues an update of the in-memory run info derived from "update"
// and triggers a reconciliation. It is the single funnel through which the bridge feeds the
// reconciler.
func (r *IdeSessionReconciler) processSessionNotification(sessionID ide.SessionID, name types.NamespacedName, update *IdeSessionRunInfo) {
	stateKey := ideSessionStateKey(sessionID)
	runMap := r.runs
	r.runs.QueueDeferredOp(name, func(_ types.NamespacedName, _ ideSessionStateKey, _ *apiv1.IdeSession) {
		_ = runMap.Update(name, stateKey, update)
	})
	r.ScheduleReconciliation(name)
}

// ----- ideSessionBridge -----

// ideSessionBridge adapts ide.SessionHandler callbacks to the reconciler's notification path.
// It owns the per-session stdout/stderr capture files.
type ideSessionBridge struct {
	reconciler *IdeSessionReconciler
	name       types.NamespacedName
	uid        types.UID
	// sessionID is assigned right after ide.Client.StartSession returns successfully.
	sessionID ide.SessionID

	output *ideSessionOutputSink
}

func newIdeSessionBridge(
	reconciler *IdeSessionReconciler,
	name types.NamespacedName,
	uid types.UID,
	stdOutFile, stdErrFile *os.File,
) *ideSessionBridge {
	return &ideSessionBridge{
		reconciler: reconciler,
		name:       name,
		uid:        uid,
		output:     newIdeSessionOutputSink(stdOutFile, stdErrFile),
	}
}

func (b *ideSessionBridge) closeOutputFiles() {
	b.output.Close()
}

func (b *ideSessionBridge) OnProcessChanged(_ ide.SessionID, pid process.Pid_t) {
	if pid <= 0 {
		return
	}
	pidVal := int64(pid)
	b.reconciler.processSessionNotification(b.sessionID, b.name, &IdeSessionRunInfo{
		UID:       b.uid,
		SessionID: string(b.sessionID),
		State:     apiv1.IdeSessionStateRunning,
		PID:       &pidVal,
	})
}

func (b *ideSessionBridge) OnTerminated(_ ide.SessionID, exitCode *int32) {
	b.output.Close()
	update := &IdeSessionRunInfo{
		UID:             b.uid,
		SessionID:       string(b.sessionID),
		State:           apiv1.IdeSessionStateStopped,
		FinishTimestamp: metav1.NowMicro(),
	}
	if exitCode != apiv1.UnknownExitCode && *exitCode != 0 {
		update.State = apiv1.IdeSessionStateFailed
		update.Message = fmt.Sprintf("session terminated with non-zero exit code %d", *exitCode)
	}
	if exitCode != apiv1.UnknownExitCode {
		update.ExitCode = pointers.Duplicate(exitCode)
	}
	b.reconciler.processSessionNotification(b.sessionID, b.name, update)
}

func (b *ideSessionBridge) OnLog(_ ide.SessionID, isStdErr bool, message string) {
	if err := b.output.Write(isStdErr, message); err != nil {
		b.reconciler.Log.Error(err, "Failed to persist an IDE-emitted log message for an IdeSession",
			"IdeSession", b.name,
			"SessionID", b.sessionID,
		)
	}
}

func (b *ideSessionBridge) OnMessage(_ ide.SessionID, level ide.MessageLevel, message string) {
	log := b.reconciler.Log.WithValues(
		"IdeSession", b.name,
		"SessionID", b.sessionID,
	)
	switch level {
	case ide.MessageLevelDebug:
		log.V(1).Info(message)
	case ide.MessageLevelError:
		log.Error(errors.New("IDE reported an error for IdeSession"), message)
		// Surface the error to the IdeSession status.
		b.reconciler.processSessionNotification(b.sessionID, b.name, &IdeSessionRunInfo{
			UID:       b.uid,
			SessionID: string(b.sessionID),
			Message:   message,
		})
	default: // ide.MessageLevelInfo and anything else
		log.Info(message)
	}
}

var _ ide.SessionHandler = (*ideSessionBridge)(nil)

// ideSessionOutputSink wraps stdout/stderr capture files with TimestampWriters
// and is safe to call concurrently with Close (ide.Client serializes notifications
// per session, so OnLog calls themselves don't race against each other).
type ideSessionOutputSink struct {
	stdOutFile *os.File
	stdErrFile *os.File
	stdOutW    usvc_io.WriteSyncerCloser
	stdErrW    usvc_io.WriteSyncerCloser
	closed     bool
}

func newIdeSessionOutputSink(stdOutFile, stdErrFile *os.File) *ideSessionOutputSink {
	sink := &ideSessionOutputSink{
		stdOutFile: stdOutFile,
		stdErrFile: stdErrFile,
	}
	if stdOutFile != nil {
		sink.stdOutW = usvc_io.NewTimestampWriter(stdOutFile)
	}
	if stdErrFile != nil {
		sink.stdErrW = usvc_io.NewTimestampWriter(stdErrFile)
	}
	return sink
}

func (s *ideSessionOutputSink) Write(isStdErr bool, message string) error {
	if s.closed {
		return nil
	}
	target := s.stdOutW
	if isStdErr {
		target = s.stdErrW
	}
	if target == nil {
		return nil
	}
	_, err := target.Write(osutil.WithNewline([]byte(message)))
	return err
}

func (s *ideSessionOutputSink) Close() {
	if s.closed {
		return
	}
	s.closed = true
	if s.stdOutW != nil {
		_ = s.stdOutW.Close()
		s.stdOutW = nil
	}
	if s.stdErrW != nil {
		_ = s.stdErrW.Close()
		s.stdErrW = nil
	}
}

// Compile-time assertion that *ide.Client satisfies our IdeSessionClient interface.
var _ IdeSessionClient = (*ide.Client)(nil)
