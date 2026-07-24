/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commonapi

import (
	"fmt"
	"strings"

	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
)

// MaxWorkloadIDLength is the maximum UTF-8 byte length for a workload ID.
const MaxWorkloadIDLength = 1024

// EnvVar represents an environment variable present in a resource.
// +k8s:openapi-gen=true
type EnvVar struct {
	// Name of the environment variable.
	Name string `json:"name"`

	// Value of the environment variable. Defaults to "" (empty string).
	// +optional
	Value string `json:"value"`
	// CONSIDER allowing expansion of existing variable references e.g. using ${VAR_NAME} syntax and $$ to escape the $ sign
}

// Label represents a key/value label to apply to a resource managed outside the API server.
// +k8s:openapi-gen=true
type Label struct {
	// Key is the label key.
	Key string `json:"key"`

	// Value is the label value.
	Value string `json:"value"`
}

// WorkloadID identifies persistent resources that belong to a logical workload.
type WorkloadID string

// NormalizeWorkloadID trims surrounding whitespace from a workload ID.
func NormalizeWorkloadID(workloadID string) WorkloadID {
	return WorkloadID(strings.TrimSpace(workloadID))
}

// Normalized returns the workload ID with surrounding whitespace removed.
func (id WorkloadID) Normalized() WorkloadID {
	return NormalizeWorkloadID(string(id))
}

// Validate returns an error if the workload ID violates DCP limits.
func (id WorkloadID) Validate() error {
	if len(id) > MaxWorkloadIDLength {
		return fmt.Errorf("workload ID cannot be longer than %d bytes", MaxWorkloadIDLength)
	}
	return nil
}

// +kubebuilder:object:generate=false
// +k8s:openapi-gen=false
type NamespacedNameWithKind struct {
	types.NamespacedName
	Kind schema.GroupVersionKind
}

func (nnk NamespacedNameWithKind) Empty() bool {
	return len(nnk.Name) == 0 && len(nnk.Namespace) == 0 && nnk.Kind.Empty()
}

func (nnk NamespacedNameWithKind) String() string {
	return nnk.NamespacedName.String() + " (" + nnk.Kind.String() + ")"
}

func GetNamespacedNameWithKind(obj ctrl_client.Object) NamespacedNameWithKind {
	return NamespacedNameWithKind{
		NamespacedName: types.NamespacedName{
			Namespace: obj.GetNamespace(),
			Name:      obj.GetName(),
		},
		Kind: obj.GetObjectKind().GroupVersionKind(),
	}
}

func GetNamespacedNameWithKindForResourceObject(obj apiserver_resource.Object) NamespacedNameWithKind {
	name := "(unknown)"
	namespace := "(unknown)"
	objMeta := obj.GetObjectMeta()
	if objMeta != nil {
		name = objMeta.GetName()
		namespace = objMeta.GetNamespace()
	}
	return NamespacedNameWithKind{
		NamespacedName: types.NamespacedName{
			Namespace: namespace,
			Name:      name,
		},
		Kind: obj.GetObjectKind().GroupVersionKind(),
	}
}

func AsNamespacedName(maybeNamespacedName, defaultNamespace string) types.NamespacedName {
	if !strings.Contains(maybeNamespacedName, string(types.Separator)) {
		return types.NamespacedName{Namespace: defaultNamespace, Name: maybeNamespacedName}
	}

	parts := strings.SplitN(maybeNamespacedName, string(types.Separator), 2)
	return types.NamespacedName{
		Namespace: parts[0],
		Name:      parts[1],
	}
}
