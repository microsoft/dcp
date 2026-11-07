/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package templating

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"text/template"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/maps"
)

type errV2ServiceLookup struct {
	cause error
}

func (failure errV2ServiceLookup) Error() string {
	return fmt.Sprintf("get logical service: %v", failure.cause)
}

func (failure errV2ServiceLookup) Unwrap() error {
	return failure.cause
}

// ServingPort describes a named workload listening address after port allocation.
// IPv6 addresses use bracketed literals.
type ServingPort struct {
	Name    string
	Address string
	Port    int32
}

// EvaluateV2Values evaluates declared arguments and environment against one service-address snapshot.
// Serving helpers use logical port names; consumer helpers accept same-namespace service references
// and an optional "IPv4" or "IPv6" selector. Address helpers preserve the stored bracketed IPv6
// representation. ignoreEnvironmentCase controls case folding of environment names before evaluation.
// Inputs are never modified.
func EvaluateV2Values(
	ctx context.Context, reader ctrl_client.Reader, owner commonapi.DcpModelObject,
	ports []ServingPort, arguments []string, environment []commonapi.EnvVar,
	ignoreEnvironmentCase bool,
) ([]string, []commonapi.EnvVar, error) {
	selectedAddresses := make(map[string]apiv2.ServiceAddress)
	serviceSnapshots := make(map[types.NamespacedName]*apiv2.LogicalService)
	selectAddress := func(reference string, family []string) (apiv2.ServiceAddress, error) {
		if len(family) > 1 || (len(family) == 1 && family[0] != "IPv4" && family[0] != "IPv6") {
			return apiv2.ServiceAddress{}, fmt.Errorf("service address selector must be IPv4 or IPv6")
		}
		referenceErrors := apiv2.ValidateSameNamespaceResourceReference(reference, owner.GetNamespace(), field.NewPath("serviceRef"))
		if len(referenceErrors) > 0 {
			return apiv2.ServiceAddress{}, fmt.Errorf("invalid service reference %q: %w", reference, referenceErrors.ToAggregate())
		}
		serviceName := commonapi.AsNamespacedName(reference, owner.GetNamespace())
		selector := ""
		if len(family) == 1 {
			selector = family[0]
		}
		key := serviceName.String() + "/" + selector
		if selected, found := selectedAddresses[key]; found {
			return selected, nil
		}
		service, found := serviceSnapshots[serviceName]
		if !found {
			service = &apiv2.LogicalService{}
			getErr := reader.Get(ctx, serviceName, service)
			if apierrors.IsNotFound(getErr) {
				return apiv2.ServiceAddress{}, errServiceDoesNotExist{Service: serviceName, ContextObject: owner.NamespacedName()}
			}
			if getErr != nil {
				return apiv2.ServiceAddress{}, errV2ServiceLookup{cause: fmt.Errorf("service %q: %w", serviceName, getErr)}
			}
			serviceSnapshots[serviceName] = service
		}
		if !service.DeletionTimestamp.IsZero() {
			return apiv2.ServiceAddress{}, fmt.Errorf("service %q is terminating", serviceName)
		}
		if service.Status.Phase == apiv2.LogicalServicePhaseFailed {
			if condition := apimeta.FindStatusCondition(service.Status.Conditions, string(apiv2.ConditionReady)); condition != nil {
				return apiv2.ServiceAddress{}, fmt.Errorf("service %q failed: %s: %s", serviceName, condition.Reason, condition.Message)
			}
			return apiv2.ServiceAddress{}, fmt.Errorf("service %q failed", serviceName)
		}
		selected, addressFound, selectionErr := networking.SelectServiceAddress(
			service.Status.EffectiveAddresses, networking.IpVersionPreference(selector), networking.GetIpVersionPreference())
		if selectionErr != nil {
			return apiv2.ServiceAddress{}, errV2ServiceLookup{cause: fmt.Errorf("select address for service %q: %w", serviceName, selectionErr)}
		}
		if !addressFound {
			return apiv2.ServiceAddress{}, errServiceNotAssignedAddress{Service: serviceName}
		}
		selectedAddresses[key] = selected
		return selected, nil
	}
	findPort := func(name string) (ServingPort, error) {
		for _, port := range ports {
			if port.Name == name {
				return port, nil
			}
		}
		return ServingPort{}, fmt.Errorf("logical port %q is not declared by %s", name, owner.NamespacedName())
	}
	base := template.New("logical-value").Option("missingkey=error").Funcs(template.FuncMap{
		"portForServing": func(name string) (int32, error) {
			port, findErr := findPort(name)
			return port.Port, findErr
		},
		"addressForServing": func(name string) (string, error) {
			port, findErr := findPort(name)
			return port.Address, findErr
		},
		"portFor": func(reference string, family ...string) (int32, error) {
			address, selectErr := selectAddress(reference, family)
			return address.Port, selectErr
		},
		"addressFor": func(reference string, family ...string) (string, error) {
			address, selectErr := selectAddress(reference, family)
			return address.Address, selectErr
		},
	})
	evaluate := func(value string) (string, error) {
		if !strings.Contains(value, "{{") {
			return value, nil
		}
		cloned, cloneErr := base.Clone()
		if cloneErr != nil {
			return "", fmt.Errorf("clone logical value template: %w", cloneErr)
		}
		parsed, parseErr := cloned.Parse(value)
		if parseErr != nil {
			return "", fmt.Errorf("parse logical value template: %w", parseErr)
		}
		var output strings.Builder
		if executeErr := parsed.Execute(&output, owner); executeErr != nil {
			return "", fmt.Errorf("evaluate logical value template: %w", executeErr)
		}
		return output.String(), nil
	}
	environmentMode := maps.StringMapModeCaseSensitive
	if ignoreEnvironmentCase {
		environmentMode = maps.StringMapModeCaseInsensitive
	}
	lastDeclarations := maps.NewStringKeyMap[int](environmentMode)
	for declarationIndex, variable := range environment {
		lastDeclarations.Override(variable.Name, declarationIndex)
	}
	resolvedArgs := slices.Clone(arguments)
	var resolvedEnv []commonapi.EnvVar
	if environment != nil {
		resolvedEnv = make([]commonapi.EnvVar, 0, lastDeclarations.Len())
	}
	for argumentIndex, argument := range arguments {
		value, argumentErr := evaluate(argument)
		if argumentErr != nil {
			return nil, nil, fmt.Errorf("argument %d: %w", argumentIndex, argumentErr)
		}
		resolvedArgs[argumentIndex] = value
	}
	for environmentIndex, variable := range environment {
		winningIndex, found := lastDeclarations.Get(variable.Name)
		if !found || winningIndex != environmentIndex {
			continue
		}
		value, environmentErr := evaluate(variable.Value)
		if environmentErr != nil {
			return nil, nil, fmt.Errorf("environment variable %q: %w", variable.Name, environmentErr)
		}
		resolvedEnv = append(resolvedEnv, commonapi.EnvVar{Name: variable.Name, Value: value})
	}
	return resolvedArgs, resolvedEnv, nil
}
