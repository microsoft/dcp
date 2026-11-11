/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package projectdebug serves Project debug sessions and owns host adapter execution.
package projectdebug

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	tiltapiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metainternal "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	requestinfo "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dap"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/randdata"
)

type projectDebugStorage struct {
	parent     rest.StandardStorage
	namespaces rest.Getter
	runtime    *projectDebugRuntime
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	active     sync.WaitGroup
	destroy    sync.Once
}

// Configure registers Project debug serving and isolates API-owned status observations.
func Configure(config *tiltapiserver.Config, ctx context.Context, log logr.Logger) error {
	writer := &projectDebugStatusWriter{}
	if statusErr := configureProjectDebugStatus(config, writer); statusErr != nil {
		return statusErr
	}
	projectGVR := (&apiv2.Project{}).GetGroupVersionResource()
	parentProvider := config.ExtraConfig.APIs[projectGVR]
	namespaceProvider := config.ExtraConfig.APIs[(&apiv2.Namespace{}).GetGroupVersionResource()]
	if parentProvider == nil || namespaceProvider == nil {
		return fmt.Errorf("project debug serving requires project and namespace storage")
	}
	debugGVR := projectGVR
	debugGVR.Resource += "/" + apiv2.DebugSubresourceName
	config.ExtraConfig.APIs[debugGVR] = func(scheme *runtime.Scheme, getter generic.RESTOptionsGetter) (rest.Storage, error) {
		parent, parentErr := parentProvider(scheme, getter)
		if parentErr != nil {
			return nil, parentErr
		}
		namespaces, namespaceErr := namespaceProvider(scheme, getter)
		if namespaceErr != nil {
			return nil, namespaceErr
		}
		parentStore, parentOK := parent.(rest.StandardStorage)
		namespaceStore, namespaceOK := namespaces.(rest.Getter)
		if !parentOK || !namespaceOK {
			return nil, fmt.Errorf("project debug serving requires standard parent storage and namespace getter")
		}
		return newProjectDebugStorage(ctx, parentStore, namespaceStore, writer, process.NewOSExecutor(log), log), nil
	}
	return nil
}

func newProjectDebugStorage(
	ctx context.Context, parent rest.StandardStorage, namespaces rest.Getter,
	writer *projectDebugStatusWriter, executor process.Executor, log logr.Logger,
) *projectDebugStorage {
	lifetime, cancel := context.WithCancel(ctx)
	storage := &projectDebugStorage{
		parent: parent, namespaces: namespaces, ctx: lifetime, cancel: cancel,
		runtime: newProjectDebugRuntime(lifetime, executor, writer, log),
	}
	context.AfterFunc(ctx, storage.Destroy)
	return storage
}

func (*projectDebugStorage) New() runtime.Object                               { return &apiv2.Project{} }
func (*projectDebugStorage) NamespaceScoped() bool                             { return true }
func (*projectDebugStorage) ConnectMethods() []string                          { return []string{http.MethodGet} }
func (*projectDebugStorage) NewConnectOptions() (runtime.Object, bool, string) { return nil, false, "" }

func (storage *projectDebugStorage) Destroy() {
	storage.destroy.Do(func() {
		storage.mu.Lock()
		storage.cancel()
		storage.mu.Unlock()
		storage.active.Wait()
		storage.runtime.executor.Dispose()
	})
}

func (storage *projectDebugStorage) Connect(
	ctx context.Context, name string, _ runtime.Object, responder rest.Responder,
) (http.Handler, error) {
	parent, getErr := storage.parent.Get(ctx, name, &metav1.GetOptions{})
	if getErr != nil {
		return nil, getErr
	}
	project := parent.(*apiv2.Project)
	if admissionErr := debugProjectAdmission(project); admissionErr != nil {
		return nil, admissionErr
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		storage.serve(writer, request, project, responder)
	}), nil
}

func debugProjectAdmission(project *apiv2.Project) error {
	resource := project.GetGroupVersionResource().GroupResource()
	if project.Spec.DebugAdapter == nil {
		return apierrors.NewBadRequest("project has no debugAdapter configuration")
	}
	if !project.DeletionTimestamp.IsZero() || project.Spec.Stop {
		return apierrors.NewForbidden(resource, project.Name, dap.ErrShutdownRequested)
	}
	return nil
}

func validateDebugUpgrade(request *http.Request) error {
	if !websocket.IsWebSocketUpgrade(request) || request.Header.Get("Sec-WebSocket-Version") != "13" {
		return apierrors.NewBadRequest("debug requires a WebSocket upgrade")
	}
	key, keyErr := base64.StdEncoding.DecodeString(request.Header.Get("Sec-WebSocket-Key"))
	if keyErr != nil || len(key) != 16 {
		return apierrors.NewBadRequest("invalid WebSocket key")
	}
	if origin := request.Header.Get("Origin"); origin != "" {
		location, originErr := url.Parse(origin)
		if originErr != nil || !strings.EqualFold(location.Host, request.Host) {
			return apierrors.NewForbidden((&apiv2.Project{}).GetGroupVersionResource().GroupResource(), "", fmt.Errorf("cross-origin debug request"))
		}
	}
	return nil
}

func validateDebugProjectSnapshot(selected, current *apiv2.Project) error {
	if current.UID != selected.UID {
		return apierrors.NewConflict(current.GetGroupVersionResource().GroupResource(), current.Name, fmt.Errorf("project identity changed"))
	}
	if admissionErr := debugProjectAdmission(current); admissionErr != nil {
		return admissionErr
	}
	if current.Status.Phase == apiv2.ProjectPhaseFailed {
		return apierrors.NewConflict(current.GetGroupVersionResource().GroupResource(), current.Name, fmt.Errorf("project execution context failed"))
	}
	if selected.Spec.Container != nil && selected.Status.ContainerID != "" &&
		(current.Status.ContainerID != selected.Status.ContainerID ||
			current.Status.ContainerRuntime != selected.Status.ContainerRuntime ||
			(selected.Status.DebugAdapterAddress != "" &&
				(current.Status.DebugAdapterAddress != selected.Status.DebugAdapterAddress ||
					current.Status.DebugAdapterPort != selected.Status.DebugAdapterPort))) {
		return apierrors.NewConflict(current.GetGroupVersionResource().GroupResource(), current.Name, fmt.Errorf("prepared container identity changed"))
	}
	return nil
}

// monitor watches the parent for prerequisite changes and cancellation for the entire debug request.
func (storage *projectDebugStorage) monitor(
	ctx context.Context, selected *apiv2.Project, ready chan<- *apiv2.Project, cancel context.CancelCauseFunc,
) {
	namespaceSource, supportsWatch := storage.namespaces.(rest.Watcher)
	if !supportsWatch {
		cancel(fmt.Errorf("project debug serving requires namespace watch support"))
		return
	}
	namespaceWatch, namespaceWatchErr := namespaceSource.Watch(ctx, &metainternal.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", selected.Namespace),
	})
	if namespaceWatchErr != nil {
		cancel(namespaceWatchErr)
		return
	}
	defer namespaceWatch.Stop()
	parentWatch, watchErr := storage.parent.Watch(requestinfo.WithNamespace(ctx, selected.Namespace), &metainternal.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", selected.Name),
	})
	if watchErr != nil {
		cancel(watchErr)
		return
	}
	defer parentWatch.Stop()
	// Tilt registers each watch before its initial snapshot. Check only for a Project
	// that disappeared during subscription; readiness comes exclusively from watch objects.
	parent, getErr := storage.parent.Get(requestinfo.WithNamespace(ctx, selected.Namespace), selected.Name, &metav1.GetOptions{})
	if getErr != nil {
		cancel(getErr)
		return
	}
	if parent.(*apiv2.Project).UID != selected.UID {
		cancel(fmt.Errorf("project identity changed during watch subscription"))
		return
	}
	prepared := false
	var current *apiv2.Project
	var namespace *apiv2.Namespace
	for {
		select {
		case <-ctx.Done():
			return
		case event, open := <-parentWatch.ResultChan():
			if !open {
				cancel(fmt.Errorf("project debug prerequisite watch closed"))
				return
			}
			if event.Type == watch.Error {
				cancel(apierrors.FromObject(event.Object))
				return
			}
			if event.Type == watch.Bookmark {
				continue
			}
			if event.Type != watch.Added && event.Type != watch.Modified && event.Type != watch.Deleted {
				cancel(fmt.Errorf("invalid Project watch event %q", event.Type))
				return
			}
			snapshot, valid := event.Object.(*apiv2.Project)
			if !valid || snapshot.Name != selected.Name || snapshot.Namespace != selected.Namespace {
				cancel(fmt.Errorf("invalid Project watch snapshot"))
				return
			}
			if event.Type == watch.Deleted {
				cancel(errors.Join(dap.ErrShutdownRequested, fmt.Errorf("project was deleted")))
				return
			}
			if validationErr := validateDebugProjectSnapshot(selected, snapshot); validationErr != nil {
				if apierrors.IsForbidden(validationErr) {
					validationErr = errors.Join(dap.ErrShutdownRequested, validationErr)
				}
				cancel(validationErr)
				return
			}
			current = snapshot
		case event, open := <-namespaceWatch.ResultChan():
			if !open {
				cancel(fmt.Errorf("project debug namespace watch closed"))
				return
			}
			if event.Type == watch.Error {
				cancel(apierrors.FromObject(event.Object))
				return
			}
			if event.Type == watch.Bookmark {
				continue
			}
			if event.Type != watch.Added && event.Type != watch.Modified && event.Type != watch.Deleted {
				cancel(fmt.Errorf("invalid Namespace watch event %q", event.Type))
				return
			}
			snapshot, valid := event.Object.(*apiv2.Namespace)
			if !valid || snapshot.Name != selected.Namespace {
				cancel(fmt.Errorf("invalid Namespace watch snapshot"))
				return
			}
			if event.Type == watch.Deleted || !snapshot.DeletionTimestamp.IsZero() ||
				snapshot.Status.Phase == apiv2.NamespacePhaseTerminating {
				cancel(errors.Join(dap.ErrShutdownRequested, fmt.Errorf("namespace is terminating")))
				return
			}
			if namespace != nil && namespace.UID != snapshot.UID {
				cancel(fmt.Errorf("namespace identity changed"))
				return
			}
			namespace = snapshot
		}
		isReady := false
		if current != nil && namespace != nil && namespace.Status.Phase == apiv2.NamespacePhaseActive {
			condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
			isReady = condition != nil && condition.Status == metav1.ConditionTrue &&
				((current.Status.Phase == apiv2.ProjectPhaseReady && condition.Reason == string(apiv2.ProjectReasonPrepared)) ||
					(current.Status.Phase == apiv2.ProjectPhaseActive && condition.Reason == string(apiv2.ProjectReasonDebugSessionsActive)))
		}
		if isReady && !prepared {
			prepared = true
			selected = current.DeepCopy()
			ready <- selected
		} else if prepared && !isReady {
			cancel(fmt.Errorf("project debug prerequisites are no longer available"))
			return
		}
	}
}

func (storage *projectDebugStorage) serve(writer http.ResponseWriter, request *http.Request, project *apiv2.Project, responder rest.Responder) {
	if upgradeErr := validateDebugUpgrade(request); upgradeErr != nil {
		responder.Error(upgradeErr)
		return
	}
	storage.mu.Lock()
	if storage.ctx.Err() != nil {
		storage.mu.Unlock()
		responder.Error(apierrors.NewServiceUnavailable("debug server is stopping"))
		return
	}
	storage.active.Add(1)
	storage.mu.Unlock()
	defer storage.active.Done()
	ctx, cancel := context.WithCancelCause(request.Context())
	defer cancel(nil)
	stopServer := context.AfterFunc(storage.ctx, func() { cancel(fmt.Errorf("debug server is stopping")) })
	defer stopServer()
	sessionID, idErr := randdata.MakeRandomString(16)
	if idErr != nil {
		responder.Error(idErr)
		return
	}
	session := &apiv2.ProjectDebugSessionStatus{ID: string(sessionID), Phase: apiv2.ProjectDebugPhasePending}
	if _, publishErr := storage.runtime.writer.update(ctx, project.NamespacedName(), project.UID, nil, session); publishErr != nil {
		responder.Error(publishErr)
		return
	}
	var sessionErr error
	defer func() {
		session.Phase, session.FinishedAt = apiv2.ProjectDebugPhaseExited, metav1.NowMicro()
		session.Conditions = debugConditions("SessionClosed", "The debug session closed.", false, project.Generation)
		if sessionErr != nil {
			session.Phase = apiv2.ProjectDebugPhaseFailed
			session.Conditions = debugConditions("SessionFailed", sessionErr.Error(), false, project.Generation)
		}
		if publishErr := storage.runtime.publish(project, nil, session); publishErr != nil {
			storage.runtime.log.Error(publishErr, "Failed to publish debug session completion")
		}
	}()
	ready := make(chan *apiv2.Project, 1)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		storage.monitor(ctx, project, ready, cancel)
	}()
	defer func() { cancel(nil); <-monitorDone }()
	var prepared *apiv2.Project
	select {
	case prepared = <-ready:
	case <-ctx.Done():
		sessionErr = context.Cause(ctx)
		responder.Error(sessionErr)
		return
	}
	project = prepared
	setupCtx, setupCancel := context.WithTimeout(ctx, project.Spec.DebugAdapter.GetConnectionTimeout())
	execution, transport, release, acquireErr := storage.runtime.acquire(setupCtx, project)
	setupCancel()
	if acquireErr != nil {
		sessionErr = acquireErr
		if errors.Is(acquireErr, context.DeadlineExceeded) {
			responder.Error(apierrors.NewTimeoutError("debug adapter transport setup timed out", 0))
		} else {
			responder.Error(apierrors.NewInternalError(acquireErr))
		}
		return
	}
	transport = bufferProjectDebugTransport(transport)
	stopOutputFailure := context.AfterFunc(transport.failure, func() {
		cancel(fmt.Errorf("debug adapter output failed: %w", context.Cause(transport.failure)))
	})
	defer stopOutputFailure()
	defer func() {
		if closeErr := transport.Close(); closeErr != nil {
			storage.runtime.log.Error(closeErr, "Failed to close debug adapter transport")
		}
		if releaseErr := release(); releaseErr != nil {
			sessionErr = errors.Join(sessionErr, releaseErr)
			storage.runtime.log.Error(releaseErr, "Failed to release debug adapter execution")
		}
	}()
	session.ExecutionID = execution.status.ID
	session.Phase = apiv2.ProjectDebugPhaseRunning
	session.Conditions = debugConditions("TransportReady", "The adapter transport is connected.", true, project.Generation)
	if _, publishErr := storage.runtime.writer.update(ctx, project.NamespacedName(), project.UID, nil, session); publishErr != nil {
		sessionErr = publishErr
		responder.Error(publishErr)
		return
	}
	upgrader := websocket.Upgrader{Subprotocols: []string{"dap"}}
	connection, upgradeErr := upgrader.Upgrade(writer, request, nil)
	if upgradeErr != nil {
		sessionErr = upgradeErr
		return
	}
	defer connection.Close()
	connection.SetReadLimit(9 * 1024 * 1024)
	stream := &debugWebSocketStream{connection: connection}
	sessionErr = dap.Bridge(ctx,
		&debugDAPTransport{reader: bufio.NewReader(stream), writer: stream, close: connection.Close},
		&debugDAPTransport{reader: bufio.NewReader(transport), writer: transport, close: transport.Close},
		execution.done, transport.readDone, apiv2.ProjectDebugShutdownGrace, func(direction dap.Direction, message dap.Message) error {
			if protocolErr := storage.runtime.observeProtocolMessage(project, session.ID, direction, message); protocolErr != nil {
				return protocolErr
			}
			if direction != dap.AdapterToClient {
				return nil
			}
			return storage.runtime.observeProcessMessage(project, session.ID, message)
		})
	if ctx.Err() != nil && !errors.Is(context.Cause(ctx), dap.ErrShutdownRequested) {
		sessionErr = errors.Join(sessionErr, context.Cause(ctx))
	}
}

var _ rest.Storage = (*projectDebugStorage)(nil)
var _ rest.Connecter = (*projectDebugStorage)(nil)
var _ rest.Scoper = (*projectDebugStorage)(nil)
