/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package osutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHasOnlyValidFilenameChars(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		// Valid filenames
		{"simple name", "test", true},
		{"name with extension", "test.txt", true},
		{"name with multiple dots", "file.tar.gz", true},
		{"name with hyphen", "my-file", true},
		{"name with underscore", "my_file", true},
		{"name with numbers", "file123", true},
		{"name with spaces in middle", "my file", true},
		{"single character", "a", true},
		{"unicode characters", "文件名", true},
		{"mixed alphanumeric", "Test_File-123.txt", true},

		// Invalid filenames - empty
		{"empty string", "", false},

		// Invalid filenames - forbidden characters
		{"contains less than", "test<file", false},
		{"contains greater than", "test>file", false},
		{"contains colon", "test:file", false},
		{"contains double quote", "test\"file", false},
		{"contains forward slash", "test/file", false},
		{"contains backslash", "test\\file", false},
		{"contains pipe", "test|file", false},
		{"contains question mark", "test?file", false},
		{"contains asterisk", "test*file", false},
		{"contains null character", "test\x00file", false},
		{"contains control character", "test\x1Ffile", false},
		{"contains tab", "test\tfile", false},

		// Invalid filenames - trailing space or dot
		{"ends with space", "test ", false},
		{"ends with dot", "test.", false},
		{"ends with multiple spaces", "test   ", false},
		{"only spaces", "   ", false},
		{"only dot", ".", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := HasOnlyValidFilenameChars(tt.input)
			assert.Equal(t, tt.expected, result, "HasOnlyValidFilenameChars(%q)", tt.input)
		})
	}
}
