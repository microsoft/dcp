/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logstream

import (
	"fmt"

	"k8s.io/apimachinery/pkg/types"
)

// LogFileName identifies captured resource output. A nonempty suffix distinguishes capture instances.
func LogFileName(name string, uid types.UID, source LogStreamSource, suffix string) (string, error) {
	var stream string
	switch source {
	case "", LogStreamSourceStdout:
		stream = "out"
	case LogStreamSourceStderr:
		stream = "err"
	case LogStreamSourceStartupStdout:
		stream = "startout"
	case LogStreamSourceStartupStderr:
		stream = "starterr"
	default:
		return "", fmt.Errorf("unknown capture log source %q", source)
	}
	result := fmt.Sprintf("%s_%s_%s", name, stream, uid)
	if suffix != "" {
		result += "_" + suffix
	}
	return result, nil
}
