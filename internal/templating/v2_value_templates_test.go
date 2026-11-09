/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package templating

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/commonapi"
)

type countedServiceReader struct {
	ctrl_client.Reader
	reads int
}

func (reader *countedServiceReader) Get(ctx context.Context, key ctrl_client.ObjectKey, object ctrl_client.Object, options ...ctrl_client.GetOption) error {
	reader.reads++
	return reader.Reader.Get(ctx, key, object, options...)
}

// Verifies that EvaluateV2Values uses named serving ports and one consumer-service snapshot,
// consistently selecting address/port pairs by the default preference or explicit address family.
func TestEvaluateV2ValuesServiceAddresses(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "test-namespace"},
		Status: apiv2.LogicalServiceStatus{EffectiveAddresses: []apiv2.ServiceAddress{{Address: "[::1]", Port: 5001}, {Address: "127.0.0.1", Port: 5000}}}}
	reader := &countedServiceReader{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(service).Build()}
	owner := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: "test-namespace"}}
	expectedDefault := "127.0.0.1:5000"
	if networking.GetIpVersionPreference() == networking.IpVersionPreference6 && networking.IsIPv6("::1") {
		expectedDefault = "[::1]:5001"
	}
	arguments := []string{`{{portForServing "http"}}`, `{{addressForServing "http"}}`, `{{addressFor "service"}}:{{portFor "service"}}`}
	expectedArguments := []string{"8080", "0.0.0.0", expectedDefault}
	if networking.IsIPv6("::1") {
		arguments = append(arguments, `{{addressFor "service" "IPv6"}}:{{portFor "service" "IPv6"}}`)
		expectedArguments = append(expectedArguments, "[::1]:5001")
	}
	arguments = append(arguments, `{{addressFor "service" "IPv4"}}:{{portFor "service" "IPv4"}}`)
	expectedArguments = append(expectedArguments, "127.0.0.1:5000")
	environment := []commonapi.EnvVar{{Name: "PORT", Value: `{{portForServing "http"}}`}, {Name: "EMPTY", Value: ""}}
	resolvedArgs, resolvedEnv, evaluationErr := EvaluateV2Values(context.Background(), reader, owner,
		[]ServingPort{{Name: "http", Address: "0.0.0.0", Port: 8080}}, arguments, environment, false)
	require.NoError(t, evaluationErr)
	require.Equal(t, expectedArguments, resolvedArgs)
	require.Equal(t, []commonapi.EnvVar{{Name: "PORT", Value: "8080"}, {Name: "EMPTY", Value: ""}}, resolvedEnv)
	require.Equal(t, 1, reader.reads)
	require.Equal(t, `{{portForServing "http"}}`, arguments[0])
	require.Equal(t, `{{portForServing "http"}}`, environment[0].Value)
}

// Verifies that EvaluateV2Values reports malformed or invalid templates instead of launching
// literal expressions or using terminal services, while missing services and unavailable address families remain retryable.
func TestEvaluateV2ValuesErrors(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "test-namespace"},
		Status: apiv2.LogicalServiceStatus{EffectiveAddresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 5000}}}}
	terminating := service.DeepCopy()
	terminating.Name = "terminating"
	terminating.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	terminating.Finalizers = []string{"tests.dcp.microsoft.com/hold"}
	failed := service.DeepCopy()
	failed.Name, failed.Status.Phase = "failed", apiv2.LogicalServicePhaseFailed
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(service, terminating, failed).Build()
	owner := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: "test-namespace"}}
	for _, testCase := range []struct {
		name      string
		value     string
		transient bool
	}{
		{"malformed", "{{", false},
		{"unknown function", "{{unknown}}", false},
		{"undeclared serving port", `{{portForServing "missing"}}`, false},
		{"cross namespace", `{{portFor "other/service"}}`, false},
		{"invalid selector", `{{portFor "service" "unknown"}}`, false},
		{"extra selectors", `{{portFor "service" "IPv4" "IPv6"}}`, false},
		{"missing service", `{{portFor "missing"}}`, true},
		{"unavailable family", `{{portFor "service" "IPv6"}}`, true},
		{"terminating service", `{{portFor "terminating"}}`, false},
		{"failed service", `{{portFor "failed"}}`, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, evaluationErr := EvaluateV2Values(context.Background(), reader, owner, nil, []string{testCase.value}, nil, false)
			require.Error(t, evaluationErr)
			require.Equal(t, testCase.transient, IsTransientTemplateError(evaluationErr))
		})
	}
}

// Verifies that EvaluateV2Values rejects invalid consumer references before lookup in arguments
// and environment, while valid unqualified and same-namespace references retain normal lookup behavior.
func TestEvaluateV2ValuesValidatesConsumerReferences(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service.name", Namespace: "test-namespace"},
		Status: apiv2.LogicalServiceStatus{EffectiveAddresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 5000}}}}
	owner := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: "test-namespace"}}
	for _, testCase := range []struct {
		name      string
		reference string
		invalid   bool
		missing   bool
	}{
		{name: "empty", invalid: true},
		{name: "uppercase name", reference: "INVALID", invalid: true},
		{name: "whitespace", reference: " service", invalid: true},
		{name: "missing explicit name", reference: "test-namespace/", invalid: true},
		{name: "missing explicit namespace", reference: "/service", invalid: true},
		{name: "uppercase namespace", reference: "INVALID/service", invalid: true},
		{name: "additional separator", reference: "test-namespace/service/extra", invalid: true},
		{name: "cross namespace", reference: "other/service", invalid: true},
		{name: "overlong name", reference: strings.Repeat("a", 254), invalid: true},
		{name: "unqualified", reference: service.Name},
		{name: "qualified", reference: service.Namespace + "/" + service.Name},
		{name: "missing unqualified service", reference: "missing", missing: true},
		{name: "missing qualified service", reference: service.Namespace + "/missing", missing: true},
	} {
		for _, helper := range []string{"portFor", "addressFor"} {
			for _, fieldName := range []string{"arguments", "environment"} {
				t.Run(testCase.name+"/"+helper+"/"+fieldName, func(t *testing.T) {
					t.Parallel()
					reader := &countedServiceReader{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(service.DeepCopy()).Build()}
					expression := fmt.Sprintf("{{%s %q}}", helper, testCase.reference)
					var arguments []string
					var environment []commonapi.EnvVar
					if fieldName == "arguments" {
						arguments = []string{expression}
					} else {
						environment = []commonapi.EnvVar{{Name: "VALUE", Value: expression}}
					}
					resolvedArgs, resolvedEnv, evaluationErr := EvaluateV2Values(
						context.Background(), reader, owner, nil, arguments, environment, false)
					if testCase.invalid {
						require.ErrorContains(t, evaluationErr, "invalid service reference")
						require.False(t, IsTransientTemplateError(evaluationErr))
						require.Zero(t, reader.reads)
					} else if testCase.missing {
						require.Error(t, evaluationErr)
						require.True(t, IsTransientTemplateError(evaluationErr))
						require.Equal(t, 1, reader.reads)
					} else {
						require.NoError(t, evaluationErr)
						require.Equal(t, 1, reader.reads)
						expected := "5000"
						if helper == "addressFor" {
							expected = "127.0.0.1"
						}
						if fieldName == "arguments" {
							require.Equal(t, []string{expected}, resolvedArgs)
						} else {
							require.Equal(t, []commonapi.EnvVar{{Name: "VALUE", Value: expected}}, resolvedEnv)
						}
					}
				})
			}
		}
	}
}

// Verifies that EvaluateV2Values ignores overwritten environment declarations before evaluating
// their templates, so unavailable references and malformed shadowed values cannot block startup.
func TestEvaluateV2ValuesIgnoresOverriddenEnvironment(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	owner := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: "test-namespace"}}
	environment := []commonapi.EnvVar{
		{Name: "PORT", Value: `{{portFor "missing"}}`},
		{Name: "OTHER", Value: "{{"},
		{Name: "PORT", Value: "8080"},
		{Name: "OTHER", Value: ""},
	}
	for _, ignoreEnvironmentCase := range []bool{false, true} {
		t.Run(fmt.Sprint(ignoreEnvironmentCase), func(t *testing.T) {
			t.Parallel()
			reader := &countedServiceReader{Reader: fake.NewClientBuilder().WithScheme(scheme).Build()}
			_, resolvedEnv, evaluationErr := EvaluateV2Values(context.Background(), reader, owner, nil, nil, environment, ignoreEnvironmentCase)
			require.NoError(t, evaluationErr)
			require.Equal(t, []commonapi.EnvVar{{Name: "PORT", Value: "8080"}, {Name: "OTHER", Value: ""}}, resolvedEnv)
			require.Zero(t, reader.reads)
			require.Equal(t, `{{portFor "missing"}}`, environment[0].Value)
		})
	}
}

// Verifies that EvaluateV2Values uses the supplied environment comparison mode independently
// of the host OS, preserving distinct names or ignoring overwritten templates as appropriate.
func TestEvaluateV2ValuesEnvironmentCasing(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	owner := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: "test-namespace"}}
	for _, ignoreEnvironmentCase := range []bool{false, true} {
		for _, testCase := range []struct {
			name      string
			value     string
			failure   bool
			transient bool
		}{
			{name: "literal", value: "distinct"},
			{name: "malformed", value: "{{", failure: true},
			{name: "missing service", value: `{{portFor "missing"}}`, failure: true, transient: true},
		} {
			t.Run(fmt.Sprint(ignoreEnvironmentCase)+"/"+testCase.name, func(t *testing.T) {
				t.Parallel()
				reader := &countedServiceReader{Reader: fake.NewClientBuilder().WithScheme(scheme).Build()}
				environment := []commonapi.EnvVar{{Name: "VALUE", Value: testCase.value}, {Name: "value", Value: "effective"}}
				_, resolvedEnv, evaluationErr := EvaluateV2Values(context.Background(), reader, owner, nil, nil, environment, ignoreEnvironmentCase)
				if ignoreEnvironmentCase {
					require.NoError(t, evaluationErr)
					require.Equal(t, []commonapi.EnvVar{{Name: "value", Value: "effective"}}, resolvedEnv)
					require.Zero(t, reader.reads)
				} else if testCase.failure {
					require.Error(t, evaluationErr)
					require.Equal(t, testCase.transient, IsTransientTemplateError(evaluationErr))
					if testCase.transient {
						require.Equal(t, 1, reader.reads)
					} else {
						require.Zero(t, reader.reads)
					}
				} else {
					require.NoError(t, evaluationErr)
					require.Equal(t, environment, resolvedEnv)
				}
				require.Equal(t, testCase.value, environment[0].Value)
			})
		}
	}
}

// Verifies that consumer and serving templates preserve bracketed IPv6 addresses
// and produce valid URL authorities without modifying the API's effective address snapshot.
func TestEvaluateV2ValuesIPv6Authorities(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "test-namespace"},
		Status: apiv2.LogicalServiceStatus{EffectiveAddresses: []apiv2.ServiceAddress{{Address: "[::1]", Port: 5001}}}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(service).Build()
	owner := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: "test-namespace"}}
	arguments := []string{
		`http://{{addressFor "service"}}:{{portFor "service"}}`,
		`http://{{addressForServing "http"}}:{{portForServing "http"}}`,
	}
	resolvedArgs, _, evaluationErr := EvaluateV2Values(context.Background(), reader, owner,
		[]ServingPort{{Name: "http", Address: "[::]", Port: 8080}}, arguments, nil, false)
	require.NoError(t, evaluationErr)
	require.Equal(t, []string{"http://[::1]:5001", "http://[::]:8080"}, resolvedArgs)
	for _, argument := range resolvedArgs {
		parsed, parseErr := url.Parse(argument)
		require.NoError(t, parseErr)
		_, _, splitErr := net.SplitHostPort(parsed.Host)
		require.NoError(t, splitErr)
	}
	require.Equal(t, "[::1]", service.Status.EffectiveAddresses[0].Address)
}
