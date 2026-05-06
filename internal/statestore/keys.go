/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package statestore

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

const (
	ResourceKindExecutable = "executable"
	ResourceKindContainer  = "container"
	ResourceKindNetwork    = "container-network"
)

func ResourceKey(kind string, name types.NamespacedName) string {
	return fmt.Sprintf("%s/%s/%s", strings.TrimSpace(kind), name.Namespace, name.Name)
}

func RuntimeResourceKey(kind string, runtimeName string) string {
	return fmt.Sprintf("%s/%s", strings.TrimSpace(kind), strings.TrimSpace(runtimeName))
}
