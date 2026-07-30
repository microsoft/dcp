/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package v2 contains API Schema definitions for the usvc-dev v2 API group.
//
// The V2 API is unstable and under active development. Its resources, fields, and
// semantics may change in breaking ways between DCP releases until the physical and
// logical resource layers described in docs/v2-resource-plan.md are complete. In
// particular, PhysicalContainer is expected to replace direct runtime network and
// volume names with references to V2 resources. Callers outside DCP should not depend
// on V2 API stability yet.
//
// +kubebuilder:object:generate=true
// +groupName=usvc-dev.developer.microsoft.com
// +k8s:openapi-model-package=github.com/microsoft/dcp/api/v2
package v2
