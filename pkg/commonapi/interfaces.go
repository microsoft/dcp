/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commonapi

import (
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
)

type ObjectStruct interface {
}

type DcpModelObject interface {
	apiserver_resource.Object
	ctrl_client.Object
	NamespacedName() types.NamespacedName
}

type DeepCopyable[T ObjectStruct] interface {
	DeepCopy() *T
}

type PObjectStruct[T ObjectStruct] interface {
	*T
	DcpModelObject
}

type PCopyableObjectStruct[T ObjectStruct] interface {
	DeepCopyable[T]
	PObjectStruct[T]
}

type ObjectList interface {
}

type PObjectList[T ObjectStruct, LT ObjectList, PT PObjectStruct[T]] interface {
	*LT
	ctrl_client.ObjectList
	apiserver_resource.ObjectList
	ListWithObjectItems[T, PT]
}

type ListWithObjectItems[T ObjectStruct, PT PObjectStruct[T]] interface {
	ctrl_client.ObjectList

	ItemCount() uint32
	GetItems() []PT
}

type PObjectWithStatusStruct[T ObjectStruct] interface {
	PObjectStruct[T]
	apiserver_resource.ObjectWithStatusSubResource
}
