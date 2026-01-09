/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commonapi

const (
	// The key used to create client cache indexes to query for object owners efficiently.
	// See controllers.SetupEndpointIndexWithManager() for an example of usage with Endpoint objects.
	WorkloadOwnerKey = ".metadata.owner"
)
