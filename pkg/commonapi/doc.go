/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package commonapi contains helpers and simple value types shared by all DCP API versions.
//
// Only types that are trivially simple, stable, and genuinely used across multiple API
// versions and non-API packages belong here. Versioned resource shapes (container ports,
// volume mounts, build contexts, and the like) are owned by their respective API packages,
// so that evolving one API version cannot change the wire shape of another. This matters in
// particular for api/v1, whose container lifecycle keys are derived from gob encodings of its
// types; see TestContainerSpecLifecycleKeyIsStable. Deepcopy generation is opt-in per type
// because this package also declares generic interfaces that controller-gen cannot process.
// +kubebuilder:object:generate=false
// +k8s:openapi-model-package=github.com/microsoft/dcp/pkg/commonapi
package commonapi
