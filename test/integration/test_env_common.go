// Copyright (c) Microsoft Corporation. All rights reserved.

package integration_test

type IncludedController uint32

const (
	ExecutableController IncludedController = 1 << iota
	ExecutableReplicaSetController
	NetworkController
	ContainerController
	ContainerExecController
	VolumeController
	ServiceController
	ContainerNetworkTunnelProxyController
	NoControllers  IncludedController = 0
	AllControllers IncludedController = ^NoControllers
)

const (
	NoSeparateWorkingDir = ""
)
