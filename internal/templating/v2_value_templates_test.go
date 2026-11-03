/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package templating

import (
	"context"
	"testing"

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
		Status: apiv2.LogicalServiceStatus{EffectiveAddresses: []apiv2.ServiceAddress{{Address: "::1", Port: 5001}, {Address: "127.0.0.1", Port: 5000}}}}
	reader := &countedServiceReader{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(service).Build()}
	owner := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: "test-namespace"}}
	arguments := []string{`{{portForServing "http"}}`, `{{addressForServing "http"}}`, `{{addressFor "service"}}:{{portFor "service"}}`,
		`{{addressFor "service" "IPv6"}}:{{portFor "service" "IPv6"}}`, `{{addressFor "service" "IPv4"}}:{{portFor "service" "IPv4"}}`}
	environment := []commonapi.EnvVar{{Name: "PORT", Value: `{{portForServing "http"}}`}, {Name: "EMPTY", Value: ""}}
	resolvedArgs, resolvedEnv, evaluationErr := EvaluateV2Values(context.Background(), reader, owner,
		[]ServingPort{{Name: "http", Address: "0.0.0.0", Port: 8080}}, arguments, environment)
	require.NoError(t, evaluationErr)
	expectedDefault := "127.0.0.1:5000"
	if networking.GetIpVersionPreference() == networking.IpVersionPreference6 {
		expectedDefault = "::1:5001"
	}
	require.Equal(t, []string{"8080", "0.0.0.0", expectedDefault, "::1:5001", "127.0.0.1:5000"}, resolvedArgs)
	require.Equal(t, []commonapi.EnvVar{{Name: "PORT", Value: "8080"}, {Name: "EMPTY", Value: ""}}, resolvedEnv)
	require.Equal(t, 1, reader.reads)
	require.Equal(t, `{{portForServing "http"}}`, arguments[0])
	require.Equal(t, `{{portForServing "http"}}`, environment[0].Value)
}

// Verifies that EvaluateV2Values reports malformed or invalid templates instead of launching
// literal expressions, while missing services and unavailable address families remain retryable.
func TestEvaluateV2ValuesErrors(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "test-namespace"},
		Status: apiv2.LogicalServiceStatus{EffectiveAddresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 5000}}}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(service).Build()
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
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, evaluationErr := EvaluateV2Values(context.Background(), reader, owner, nil, []string{testCase.value}, nil)
			require.Error(t, evaluationErr)
			require.Equal(t, testCase.transient, IsTransientTemplateError(evaluationErr))
		})
	}
}
