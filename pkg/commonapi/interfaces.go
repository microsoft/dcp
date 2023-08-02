package commonapi

import (
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
)

type ListWithObjectItems interface {
	ctrl_client.ObjectList

	ItemCount() uint32
	GetItems() []ctrl_client.Object
}
