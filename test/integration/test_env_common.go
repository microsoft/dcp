/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

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
