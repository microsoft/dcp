/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logstream

import (
	"fmt"

	"k8s.io/apimachinery/pkg/types"
)

// StartupLogFileName identifies a resource's captured startup output.
func StartupLogFileName(name string, uid types.UID, source LogStreamSource) (string, error) {
	switch source {
	case LogStreamSourceStartupStdout:
		return fmt.Sprintf("%s_startout_%s", name, uid), nil
	case LogStreamSourceStartupStderr:
		return fmt.Sprintf("%s_starterr_%s", name, uid), nil
	default:
		return "", fmt.Errorf("unknown startup log source %q", source)
	}
}
