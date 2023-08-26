// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"sigs.k8s.io/controller-runtime/pkg/event"
)

type CreateDeletePredicate struct {
}

func (p *CreateDeletePredicate) Create(e event.CreateEvent) bool {
	return true
}

func (p *CreateDeletePredicate) Delete(e event.DeleteEvent) bool {
	return true
}

func (p *CreateDeletePredicate) Update(e event.UpdateEvent) bool {
	return false
}

func (p *CreateDeletePredicate) Generic(e event.GenericEvent) bool {
	return false
}
