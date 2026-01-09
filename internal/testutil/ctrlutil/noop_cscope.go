/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package ctrlutil

import (
	"k8s.io/apimachinery/pkg/conversion"
)

// Low-level K8s methods that handle "request options" objects require a "conversion scope" object.
// For our test use case, involving LogOptions objects and test container orchestrator,
// the instance of conversion.Scope is not really used, but must be provided.
// noopConversionScope fulfills this need.
type noopConversionScope struct{}

func (*noopConversionScope) Convert(src, dest interface{}) error {
	return nil
}

func (*noopConversionScope) Meta() *conversion.Meta {
	return nil
}

var _ conversion.Scope = (*noopConversionScope)(nil)
