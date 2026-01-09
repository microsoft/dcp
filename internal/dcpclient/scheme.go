// Copyright (c) Microsoft Corporation. All rights reserved.

package dcpclient

import (
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	apiv1 "github.com/microsoft/dcp/api/v1"
)

func NewScheme() *apiruntime.Scheme {
	scheme := apiruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiv1.AddToScheme(scheme))
	return scheme
}
