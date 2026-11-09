/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containertest

import (
	"strings"
	"testing"

	"github.com/microsoft/dcp/pkg/randdata"
)

const (
	randomNameSuffixLength = uint32(10)
	maxResourceNameLength  = 63
)

// UniqueName returns a randomized, DNS-label-compatible runtime resource name.
func UniqueName(t testing.TB, prefix string) string {
	t.Helper()

	normalized := strings.Map(func(character rune) rune {
		switch {
		case character >= 'A' && character <= 'Z':
			return character + ('a' - 'A')
		case character >= 'a' && character <= 'z',
			character >= '0' && character <= '9':
			return character
		default:
			return '-'
		}
	}, prefix)
	normalized = strings.Trim(normalized, "-")
	for strings.Contains(normalized, "--") {
		normalized = strings.ReplaceAll(normalized, "--", "-")
	}
	if normalized == "" {
		normalized = "dcp-test"
	}

	suffix, suffixErr := randdata.MakeRandomString(randomNameSuffixLength)
	if suffixErr != nil {
		t.Fatalf("could not generate random resource name suffix: %v", suffixErr)
	}

	maxPrefixLength := maxResourceNameLength - 1 - len(suffix)
	if len(normalized) > maxPrefixLength {
		normalized = strings.TrimRight(normalized[:maxPrefixLength], "-")
	}

	return normalized + "-" + string(suffix)
}
