/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package templating_test

import (
	"testing"
	"text/template"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/templating"
)

func TestExecuteTemplateFastPath(t *testing.T) {
	t.Parallel()

	tmpl := template.New("test")

	tests := []struct {
		name  string
		input string
	}{
		{name: "empty"},
		{name: "plain text", input: "hello"},
		{name: "number", input: "8080"},
		{name: "path", input: "/usr/bin/env"},
		{name: "values with spaces", input: "value with  spaces  and\t tabs"},
		{name: "single opening brace", input: "{not a template}"},
		{name: "single closing brace", input: "}"},
		{name: "closing delimiter only", input: "}}"},
	}

	for _, tc := range tests {
		tc := tc // https://github.com/golang/go/wiki/CommonMistakes#using-goroutines-on-loop-iterator-variables
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result, err := templating.ExecuteTemplate(tmpl, &apiv1.Container{}, tc.input, "test", logr.Discard())

			require.NoError(t, err)
			require.Equal(t, tc.input, result)
		})
	}
}

func TestExecuteTemplateSubstitution(t *testing.T) {
	t.Parallel()

	tmpl := template.New("test")
	container := &apiv1.Container{}

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "builtin function", input: `{{ printf "hello" }}`, expected: "hello"},
		{name: "multiple actions", input: `address: {{ printf "localhost" }}:{{ printf "8080" }}`, expected: "address: localhost:8080"},
		{name: "promoted metadata field", input: `{{ .Name }}`, expected: ""},
		{name: "control structure", input: `{{- if true -}}yes{{- end -}}`, expected: "yes"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result, err := templating.ExecuteTemplate(tmpl, container, tc.input, "test", logr.Discard())

			require.NoError(t, err)
			require.Equal(t, tc.expected, result)
		})
	}
}

func TestExecuteTemplateUnparsableInput(t *testing.T) {
	t.Parallel()

	tmpl := template.New("test")

	unparsableInputs := []string{
		`{{`,
		`unclosed {{ template`,
		`{{-`,
	}

	for _, input := range unparsableInputs {
		input := input
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			result, err := templating.ExecuteTemplate(tmpl, &apiv1.Container{}, input, "test", logr.Discard())

			require.NoError(t, err)
			require.Equal(t, input, result)
		})
	}
}
