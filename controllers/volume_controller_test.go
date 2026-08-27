/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestPersistentVolumeRecordPrecedesRuntimeCreation(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openVolumeControllerTestStore(t, ctx)
	defer func() {
		require.NoError(t, stateStore.Close())
	}()
	orchestrator := newVolumeControllerTestOrchestrator()
	volume := newPersistentVolume("record-before-create")
	recordingOrchestrator := &recordingVolumeCreateOrchestrator{
		VolumeOrchestrator: orchestrator,
		stateStore:         stateStore,
		resourceKey:        volume.GetLeaseKey(),
	}
	reconciler := newVolumeControllerTestReconciler(recordingOrchestrator, stateStore)

	change := handleNewContainerVolume(ctx, reconciler, volume, apiv1.ContainerVolumeStateEmpty, nil, logr.Discard())

	require.True(t, change&statusChanged != 0)
	require.Equal(t, apiv1.ContainerVolumeStateReady, volume.Status.State)
	require.True(t, recordingOrchestrator.recordMatchedCreate)
	inspectedVolumes, inspectErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volume.Spec.Name}})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedVolumes, 1)
	record, getRecordErr := stateStore.GetPersistentVolume(ctx, volume.GetLeaseKey())
	require.NoError(t, getRecordErr)
	require.Equal(t, record.OwnershipToken, inspectedVolumes[0].Labels[containers.VolumeOwnershipTokenLabel])
}

func TestPersistentVolumePersistenceFailurePreventsRuntimeCreation(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openVolumeControllerTestStore(t, ctx)
	require.NoError(t, stateStore.Close())
	orchestrator := newVolumeControllerTestOrchestrator()
	volume := newPersistentVolume("persistence-failure")
	recordingOrchestrator := &recordingVolumeCreateOrchestrator{
		VolumeOrchestrator: orchestrator,
		stateStore:         stateStore,
		resourceKey:        volume.GetLeaseKey(),
	}
	reconciler := newVolumeControllerTestReconciler(recordingOrchestrator, stateStore)

	_ = handleNewContainerVolume(ctx, reconciler, volume, apiv1.ContainerVolumeStateEmpty, nil, logr.Discard())

	require.False(t, recordingOrchestrator.createCalled)
	_, inspectErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volume.Spec.Name}})
	require.ErrorIs(t, inspectErr, containers.ErrNotFound)
}

func TestPersistentVolumeCreateRaceAdoptsUnlabeledVolume(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openVolumeControllerTestStore(t, ctx)
	defer func() {
		require.NoError(t, stateStore.Close())
	}()
	orchestrator := newVolumeControllerTestOrchestrator()
	raceOrchestrator := &idempotentVolumeCreateRaceOrchestrator{
		VolumeOrchestrator: orchestrator,
	}
	reconciler := newVolumeControllerTestReconciler(raceOrchestrator, stateStore)
	volume := newPersistentVolume("create-race")

	change := handleNewContainerVolume(ctx, reconciler, volume, apiv1.ContainerVolumeStateEmpty, nil, logr.Discard())

	require.True(t, change&statusChanged != 0)
	require.Equal(t, apiv1.ContainerVolumeStateReady, volume.Status.State)
	inspectedVolumes, inspectErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volume.Spec.Name}})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedVolumes, 1)
	require.Empty(t, inspectedVolumes[0].Labels)
	_, getRecordErr := stateStore.GetPersistentVolume(ctx, volume.GetLeaseKey())
	require.ErrorIs(t, getRecordErr, statestore.ErrPersistentVolumeNotFound)
}

func TestPersistentVolumeAmbiguousCreateFailureRetainsOwnershipRecord(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	stateStore := openVolumeControllerTestStore(t, ctx)
	defer func() {
		require.NoError(t, stateStore.Close())
	}()
	orchestrator := newVolumeControllerTestOrchestrator()
	attemptCtx, cancelAttempt := context.WithCancel(ctx)
	ambiguousOrchestrator := &ambiguousVolumeCreateOrchestrator{
		VolumeOrchestrator: orchestrator,
		cancelAttempt:      cancelAttempt,
	}
	reconciler := newVolumeControllerTestReconciler(ambiguousOrchestrator, stateStore)
	volume := newPersistentVolume("ambiguous-create")

	firstChange := handleNewContainerVolume(attemptCtx, reconciler, volume, apiv1.ContainerVolumeStateEmpty, nil, logr.Discard())

	require.True(t, firstChange&additionalReconciliationNeeded != 0)
	record, getRecordErr := stateStore.GetPersistentVolume(ctx, volume.GetLeaseKey())
	require.NoError(t, getRecordErr)
	inspectedVolumes, inspectErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volume.Spec.Name}})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedVolumes, 1)
	require.Equal(t, record.OwnershipToken, inspectedVolumes[0].Labels[containers.VolumeOwnershipTokenLabel])

	secondChange := reconciler.manageVolume(ctx, volume, logr.Discard())

	require.True(t, secondChange&statusChanged != 0)
	require.Equal(t, apiv1.ContainerVolumeStateReady, volume.Status.State)
	currentRecord, currentRecordErr := stateStore.GetPersistentVolume(ctx, volume.GetLeaseKey())
	require.NoError(t, currentRecordErr)
	require.Equal(t, record.OwnershipToken, currentRecord.OwnershipToken)
}

func newVolumeControllerTestReconciler(
	orchestrator containers.VolumeOrchestrator,
	stateStore *statestore.Store,
) *VolumeReconciler {
	return &VolumeReconciler{
		orchestrator: orchestrator,
		volumeData:   NewObjectStateMap[volumeName, containerVolumeData, *containerVolumeData, *apiv1.ContainerVolume](),
		config: VolumeReconcilerConfig{
			StateStore: stateStore,
			WorkloadID: commonapi.WorkloadID("workload-a"),
		},
	}
}

func newPersistentVolume(name string) *apiv1.ContainerVolume {
	persistent := true
	return &apiv1.ContainerVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiv1.ContainerVolumeSpec{
			Name:       name,
			Persistent: &persistent,
		},
	}
}

func openVolumeControllerTestStore(t *testing.T, ctx context.Context) *statestore.Store {
	t.Helper()

	stateStore, openErr := statestore.Open(ctx, statestore.Options{
		Path:        filepath.Join(t.TempDir(), "state-store", "state.sqlite3"),
		BusyTimeout: 500 * time.Millisecond,
	})
	require.NoError(t, openErr)
	return stateStore
}

func newVolumeControllerTestOrchestrator() containers.VolumeOrchestrator {
	return &volumeControllerTestOrchestrator{
		volumes: map[string]containers.InspectedVolume{},
	}
}

type recordingVolumeCreateOrchestrator struct {
	containers.VolumeOrchestrator
	stateStore          *statestore.Store
	resourceKey         string
	createCalled        bool
	recordMatchedCreate bool
}

func (o *recordingVolumeCreateOrchestrator) CreateVolume(ctx context.Context, options containers.CreateVolumeOptions) error {
	o.createCalled = true
	record, getRecordErr := o.stateStore.GetPersistentVolume(ctx, o.resourceKey)
	if getRecordErr == nil {
		o.recordMatchedCreate = record.OwnershipToken == options.Labels[containers.VolumeOwnershipTokenLabel]
	}
	return o.VolumeOrchestrator.CreateVolume(ctx, options)
}

type idempotentVolumeCreateRaceOrchestrator struct {
	containers.VolumeOrchestrator
	initialInspect bool
}

func (o *idempotentVolumeCreateRaceOrchestrator) InspectVolumes(
	ctx context.Context,
	options containers.InspectVolumesOptions,
) ([]containers.InspectedVolume, error) {
	if !o.initialInspect {
		o.initialInspect = true
		return nil, containers.ErrNotFound
	}
	return o.VolumeOrchestrator.InspectVolumes(ctx, options)
}

func (o *idempotentVolumeCreateRaceOrchestrator) CreateVolume(
	ctx context.Context,
	options containers.CreateVolumeOptions,
) error {
	externalCreateErr := o.VolumeOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{Name: options.Name})
	if externalCreateErr != nil && !errors.Is(externalCreateErr, containers.ErrAlreadyExists) {
		return externalCreateErr
	}
	return nil
}

type ambiguousVolumeCreateOrchestrator struct {
	containers.VolumeOrchestrator
	cancelAttempt context.CancelFunc
}

func (o *ambiguousVolumeCreateOrchestrator) CreateVolume(
	ctx context.Context,
	options containers.CreateVolumeOptions,
) error {
	createErr := o.VolumeOrchestrator.CreateVolume(ctx, options)
	if createErr != nil {
		return createErr
	}
	o.cancelAttempt()
	return errors.New("volume create result unavailable")
}

type volumeControllerTestOrchestrator struct {
	volumes map[string]containers.InspectedVolume
}

func (*volumeControllerTestOrchestrator) Name() string {
	return "test"
}

func (*volumeControllerTestOrchestrator) CheckStatus(
	context.Context,
	containers.CachedRuntimeStatusUsage,
) containers.ContainerRuntimeStatus {
	return containers.ContainerRuntimeStatus{Installed: true, Running: true}
}

func (o *volumeControllerTestOrchestrator) CreateVolume(
	ctx context.Context,
	options containers.CreateVolumeOptions,
) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if _, found := o.volumes[options.Name]; found {
		return containers.ErrAlreadyExists
	}
	labels := make(map[string]string, len(options.Labels))
	for key, value := range options.Labels {
		labels[key] = value
	}
	o.volumes[options.Name] = containers.InspectedVolume{
		Name:      options.Name,
		Labels:    labels,
		CreatedAt: time.Now().UTC(),
	}
	return nil
}

func (o *volumeControllerTestOrchestrator) InspectVolumes(
	ctx context.Context,
	options containers.InspectVolumesOptions,
) ([]containers.InspectedVolume, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	inspectedVolumes := make([]containers.InspectedVolume, 0, len(options.Volumes))
	for _, volumeName := range options.Volumes {
		volume, found := o.volumes[volumeName]
		if !found {
			return inspectedVolumes, containers.ErrNotFound
		}
		inspectedVolumes = append(inspectedVolumes, volume)
	}
	return inspectedVolumes, nil
}

func (o *volumeControllerTestOrchestrator) RemoveVolumes(
	ctx context.Context,
	options containers.RemoveVolumesOptions,
) ([]string, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	removedVolumes := make([]string, 0, len(options.Volumes))
	for _, volumeName := range options.Volumes {
		if _, found := o.volumes[volumeName]; !found {
			return removedVolumes, containers.ErrNotFound
		}
		delete(o.volumes, volumeName)
		removedVolumes = append(removedVolumes, volumeName)
	}
	return removedVolumes, nil
}
