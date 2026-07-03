/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package applecontainer

import (
	"encoding/base64"
	"strings"
)

// The Apple `container` CLI rejects `--label` values that contain '=' with
// "invalid label format ...", unlike Docker/OCI which permit arbitrary label values.
// DCP stamps labels whose values contain '=' — notably the mounts-tracking label, whose
// value looks like "type=volume,src=<name>" (read back in the container controller to clean
// up mounts). To preserve such values we base64-encode (RawURL, which emits no '=' padding)
// any label value that contains '=', mark it with a sentinel prefix on the way in, and
// decode it again when we read labels back from `container ls`/`container inspect`. Values
// without '=' are passed through unchanged so labels stay human-readable in the common case.
const appleEncodedLabelPrefix = "dcp-b64."

// encodeLabelValue encodes a label value for the Apple container CLI if it would otherwise
// be rejected (i.e. it contains '='). The returned value never contains '='.
func encodeLabelValue(value string) string {
	if !strings.Contains(value, "=") {
		return value
	}
	return appleEncodedLabelPrefix + base64.RawURLEncoding.EncodeToString([]byte(value))
}

// decodeLabelValue reverses encodeLabelValue. Values without the sentinel prefix (including
// labels not set by DCP) are returned unchanged.
func decodeLabelValue(value string) string {
	encoded, found := strings.CutPrefix(value, appleEncodedLabelPrefix)
	if !found {
		return value
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		// Not something we encoded after all; leave it untouched.
		return value
	}
	return string(decoded)
}

// labelArg renders a "key=value" argument for the Apple container CLI's `--label` flag,
// encoding the value if necessary.
func labelArg(key, value string) string {
	return key + "=" + encodeLabelValue(value)
}

// decodeLabels returns a copy of labels with any encoded values decoded back to plaintext.
func decodeLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return nil
	}
	decoded := make(map[string]string, len(labels))
	for key, value := range labels {
		decoded[key] = decodeLabelValue(value)
	}
	return decoded
}
