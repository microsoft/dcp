/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import "strings"

// ParseListedLabels parses unescaped comma-separated CLI list labels, appending fragments without '=' to the preceding value.
// Segments containing '=' are treated as new labels, so values containing ',key=value' remain ambiguous.
func ParseListedLabels(rawLabels string) map[string]string {
	labels := make(map[string]string)
	var labelKey string
	var labelValue strings.Builder
	for _, part := range strings.Split(rawLabels, ",") {
		key, value, hasValue := strings.Cut(part, "=")
		if hasValue && key != "" {
			if labelKey != "" {
				labels[labelKey] = labelValue.String()
				labelValue.Reset()
			}
			labelKey = key
			labelValue.WriteString(value)
		} else if labelKey != "" {
			labelValue.WriteByte(',')
			labelValue.WriteString(part)
		}
	}
	if labelKey != "" {
		labels[labelKey] = labelValue.String()
	}
	return labels
}
