/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commonapi

import (
	"strings"

	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
)

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

// +kubebuilder:object:generate=false
// +k8s:openapi-gen=false
type NamespacedNameWithKindAndUid struct {
	NamespacedNameWithKind
	Uid types.UID
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
