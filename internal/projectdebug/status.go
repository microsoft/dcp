/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package projectdebug

import (
	"context"
	"fmt"
	"slices"

	tiltapiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	requestinfo "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

// projectDebugStatusWriter publishes API-owned observations through the shared status store.
type projectDebugStatusWriter struct {
	storage rest.Updater
}

func configureProjectDebugStatus(config *tiltapiserver.Config, writer *projectDebugStatusWriter) error {
	gvr := (&apiv2.Project{}).GetGroupVersionResource()
	gvr.Resource += "/status"
	provider, found := config.ExtraConfig.APIs[gvr]
	if !found || provider == nil {
		return fmt.Errorf("missing project status storage provider")
	}
	config.ExtraConfig.APIs[gvr] = func(scheme *runtime.Scheme, getter generic.RESTOptionsGetter) (rest.Storage, error) {
		inner, storageErr := provider(scheme, getter)
		if storageErr != nil {
			return nil, fmt.Errorf("create project status storage: %w", storageErr)
		}
		statusGetter, getterOK := inner.(rest.Getter)
		statusUpdater, updaterOK := inner.(rest.Updater)
		if !getterOK || !updaterOK {
			return nil, fmt.Errorf("project status storage does not support get and update: %T", inner)
		}
		writer.storage = statusUpdater
		return &projectControllerStatusStorage{Getter: statusGetter, Updater: statusUpdater}, nil
	}
	return nil
}

// projectControllerStatusStorage preserves API-owned observations on ordinary status updates and patches.
type projectControllerStatusStorage struct {
	rest.Getter
	rest.Updater
}

func (*projectControllerStatusStorage) Destroy() {}

func (storage *projectControllerStatusStorage) Update(
	ctx context.Context, name string, info rest.UpdatedObjectInfo,
	createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc,
	forceAllowCreate bool, options *metav1.UpdateOptions,
) (runtime.Object, bool, error) {
	isolated := rest.WrapUpdatedObjectInfo(info, func(_ context.Context, proposed, current runtime.Object) (runtime.Object, error) {
		next := proposed.(*apiv2.Project)
		if current == nil {
			next.Status.Debug = nil
			return next, nil
		}
		proposedDebug := next.Status.Debug
		next.Status.Debug = current.(*apiv2.Project).Status.Debug.DeepCopy()
		if next.Status.Debug != nil {
			if proposedDebug != nil {
				for sessionIndex := range next.Status.Debug.Sessions {
					storedSession := &next.Status.Debug.Sessions[sessionIndex]
					for _, proposedSession := range proposedDebug.Sessions {
						if proposedSession.ID != storedSession.ID {
							continue
						}
						for processIndex := range storedSession.Processes {
							storedProcess := &storedSession.Processes[processIndex]
							for _, proposedProcess := range proposedSession.Processes {
								if proposedProcess.ID == storedProcess.ID && proposedProcess.Observation.Phase != "" {
									storedProcess.Observation = proposedProcess.Observation
								}
							}
						}
					}
				}
			}
		}
		return next, nil
	})
	return storage.Updater.Update(ctx, name, isolated, createValidation, updateValidation, forceAllowCreate, options)
}

// update changes only the supplied execution/session entries, preserving sibling entries and application status.
func (writer *projectDebugStatusWriter) update(
	ctx context.Context, name types.NamespacedName, uid types.UID,
	execution *apiv2.ProjectDebugExecutionStatus, session *apiv2.ProjectDebugSessionStatus,
) (*apiv2.Project, error) {
	if writer.storage == nil {
		return nil, fmt.Errorf("project debug status storage is not initialized")
	}
	if uid == "" || (execution == nil && session == nil) ||
		(execution != nil && execution.ID == "") || (session != nil && session.ID == "") {
		return nil, fmt.Errorf("project UID and debug entry identities are required")
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	// Storage can retry the transformation, so snapshot inputs before invoking it.
	executionSnapshot, sessionSnapshot := execution.DeepCopy(), session.DeepCopy()
	return writer.mutate(ctx, name, uid, func(project *apiv2.Project) error {
		if project.Status.Debug == nil {
			project.Status.Debug = &apiv2.ProjectDebugStatus{}
		}
		debug := project.Status.Debug
		executionIndex, sessionIndex := -1, -1
		if executionSnapshot != nil {
			executionIndex = slices.IndexFunc(debug.Executions, func(entry apiv2.ProjectDebugExecutionStatus) bool { return entry.ID == executionSnapshot.ID })
		}
		if sessionSnapshot != nil {
			sessionIndex = slices.IndexFunc(debug.Sessions, func(entry apiv2.ProjectDebugSessionStatus) bool { return entry.ID == sessionSnapshot.ID })
		}
		if (!project.DeletionTimestamp.IsZero() || project.Spec.Stop) &&
			((executionSnapshot != nil && executionIndex < 0) || (sessionSnapshot != nil && sessionIndex < 0)) {
			return apierrors.NewForbidden(project.GetGroupVersionResource().GroupResource(), name.Name, fmt.Errorf("cannot add debug entries to a stopping or deleting project"))
		}
		if executionSnapshot != nil {
			if executionIndex < 0 {
				debug.Executions = append(debug.Executions, *executionSnapshot.DeepCopy())
			} else {
				debug.Executions[executionIndex] = *executionSnapshot.DeepCopy()
			}
		}
		if sessionSnapshot != nil {
			nextSession := sessionSnapshot.DeepCopy()
			if sessionIndex < 0 {
				nextSession.Progress = nil
				nextSession.Processes = nil
				nextSession.ProcessTrackingUnconfirmed = false
				nextSession.ProcessTrackingMessage = ""
				debug.Sessions = append(debug.Sessions, *nextSession)
			} else {
				storedSession := &debug.Sessions[sessionIndex]
				nextSession.Progress = storedSession.Progress.DeepCopy()
				if storedSession.Phase == apiv2.ProjectDebugPhaseExited || storedSession.Phase == apiv2.ProjectDebugPhaseFailed {
					nextSession.Phase, nextSession.FinishedAt = storedSession.Phase, storedSession.FinishedAt
					nextSession.Conditions = slices.Clone(storedSession.Conditions)
				}
				applyProjectDebugSessionProgress(nextSession, project.Generation)
				nextSession.Processes = storedSession.Processes
				nextSession.ProcessTrackingUnconfirmed = storedSession.ProcessTrackingUnconfirmed
				nextSession.ProcessTrackingMessage = storedSession.ProcessTrackingMessage
				debug.Sessions[sessionIndex] = *nextSession
			}
		}
		return nil
	})
}

func (writer *projectDebugStatusWriter) mutate(
	ctx context.Context, name types.NamespacedName, uid types.UID, mutate func(*apiv2.Project) error,
) (*apiv2.Project, error) {
	if writer.storage == nil || uid == "" {
		return nil, fmt.Errorf("project debug status storage and UID are required")
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	info := rest.DefaultUpdatedObjectInfo(nil, func(_ context.Context, _, current runtime.Object) (runtime.Object, error) {
		if current == nil {
			return nil, apierrors.NewNotFound((&apiv2.Project{}).GetGroupVersionResource().GroupResource(), name.Name)
		}
		project := current.(*apiv2.Project).DeepCopy()
		if project.UID != uid {
			return nil, apierrors.NewConflict(project.GetGroupVersionResource().GroupResource(), name.Name, fmt.Errorf("project UID changed"))
		}
		if mutationErr := mutate(project); mutationErr != nil {
			return nil, mutationErr
		}
		return project, nil
	})
	updated, _, updateErr := writer.storage.Update(requestinfo.WithNamespace(ctx, name.Namespace), name.Name, info, nil, nil, false, &metav1.UpdateOptions{})
	if updateErr != nil {
		return nil, updateErr
	}
	return updated.(*apiv2.Project), nil
}
