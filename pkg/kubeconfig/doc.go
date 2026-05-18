/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package kubeconfig provides utilities for materializing the kubeconfig file that
// DCP's API server clients (kubectl, controller-runtime, Aspire's hosting layer) use
// to connect to the embedded API server.
package kubeconfig

// TracerName is the OpenTelemetry instrumentation scope name used by this package.
// It is exported so that profiling pipelines (see internal/telemetry) can allowlist
// these sub-spans without having to hard-code the string in multiple places.
const TracerName = "Microsoft.Dcp.Kubeconfig"
