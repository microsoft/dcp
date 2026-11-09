/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"context"
	"fmt"
	"sync"
	"testing"

	tiltrest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/rest"
	tiltfilepath "github.com/tilt-dev/tilt-apiserver/pkg/storage/filepath"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	apiv1 "github.com/microsoft/dcp/api/v1"
)

const mutationBenchmarkNamespace = "test"

type reconciliationStatusUpdate struct{}

func (reconciliationStatusUpdate) Preconditions() *metav1.Preconditions {
	return nil
}

func (reconciliationStatusUpdate) UpdatedObject(_ context.Context, oldObj runtime.Object) (runtime.Object, error) {
	updated := oldObj.(*apiv1.Service).DeepCopy()
	updated.Status.State = apiv1.ServiceStateReady
	return updated, nil
}

func BenchmarkTiltStorageMutationConcurrency(b *testing.B) {
	for _, objectCount := range []int{1, 32} {
		b.Run(fmt.Sprintf("%d-objects", objectCount), func(b *testing.B) {
			for _, mode := range []struct {
				name                  string
				serializeResourceKind bool
			}{
				{name: "native-per-object-lock"},
				{name: "coarse-resource-lock", serializeResourceKind: true},
			} {
				b.Run(mode.name, func(b *testing.B) {
					ctx, updater, names := newMutationBenchmarkStorage(b, objectCount)
					var resourceKindLock sync.Mutex
					updateErrors := make(chan error, objectCount)

					b.ResetTimer()
					for range b.N {
						var updates sync.WaitGroup
						updates.Add(len(names))
						for _, name := range names {
							go func() {
								defer updates.Done()
								if mode.serializeResourceKind {
									resourceKindLock.Lock()
									defer resourceKindLock.Unlock()
								}
								_, _, updateErr := updater.Update(
									ctx,
									name,
									reconciliationStatusUpdate{},
									nil,
									nil,
									false,
									&metav1.UpdateOptions{},
								)
								updateErrors <- updateErr
							}()
						}
						updates.Wait()
						for range names {
							if updateErr := <-updateErrors; updateErr != nil {
								b.Fatal(updateErr)
							}
						}
					}
					b.StopTimer()
					b.ReportMetric(
						float64(b.N*len(names))/b.Elapsed().Seconds(),
						"mutations/s",
					)
				})
			}
		})
	}
}

func newMutationBenchmarkStorage(
	b *testing.B,
	objectCount int,
) (context.Context, rest.Updater, []string) {
	b.Helper()

	scheme := runtime.NewScheme()
	if addSchemeErr := apiv1.AddToScheme(scheme); addSchemeErr != nil {
		b.Fatal(addSchemeErr)
	}
	metav1.AddToGroupVersion(scheme, apiv1.GroupVersion)
	codec := serializer.NewCodecFactory(scheme).LegacyCodec(apiv1.GroupVersion)
	serviceResource := &apiv1.Service{}
	storage := tiltfilepath.NewFilepathREST(
		tiltfilepath.NewMemoryFS(),
		tiltfilepath.NewWatchSet(),
		tiltrest.DefaultStrategy{
			Object:      serviceResource,
			ObjectTyper: scheme,
		},
		serviceResource.GetGroupVersionResource().GroupResource(),
		codec,
		"data",
		serviceResource.New,
		serviceResource.NewList,
	)
	ctx := genericapirequest.WithNamespace(context.Background(), mutationBenchmarkNamespace)
	names := make([]string, objectCount)
	for index := range objectCount {
		name := fmt.Sprintf("service-%d", index)
		names[index] = name
		_, createErr := storage.(rest.Creater).Create(
			ctx,
			&apiv1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: mutationBenchmarkNamespace,
				},
			},
			nil,
			&metav1.CreateOptions{},
		)
		if createErr != nil {
			b.Fatal(createErr)
		}
	}
	return ctx, storage.(rest.Updater), names
}
