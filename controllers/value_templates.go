// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/template"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

type errServiceNotAssignedPort struct {
	Service types.NamespacedName
}

func (e errServiceNotAssignedPort) Error() string {
	return fmt.Sprintf("service '%s' does not have a port assigned yet", e.Service.String())
}

type errServiceNotAssignedAddress struct {
	Service types.NamespacedName
}

func (e errServiceNotAssignedAddress) Error() string {
	return fmt.Sprintf("service '%s' does not have an address assigned yet", e.Service.String())
}

func isTransientTemplateError(err error) bool {
	var ePort errServiceNotAssignedPort
	var eAddress errServiceNotAssignedAddress
	return errors.As(err, &ePort) || errors.As(err, &eAddress)
}

// Creates a template for evaluating a value of a spec field.
// Our template functions may need to query the API server, this is what ctx and client parameters are for.
// The contextObject parameter is the object that will be used as context during template evaluation (Executable, Container, etc.)
// If a port needs to be reserved for a service production by the context object,
// information about the reservved port will be added to reservedPorts map.
func newSpecValueTemplate(
	ctx context.Context,
	client ctrl_client.Client,
	contextObj dcpModelObject,
	reservedPorts map[types.NamespacedName]int32,
	log logr.Logger,
) (*template.Template, error) {

	servicesProduced, err := getServiceProducersForObject(contextObj, log)
	if err != nil {
		return nil, err
	}

	tmpl := template.New("envar").Funcs(template.FuncMap{
		"portForServing": func(serviceNamespacedName string) (int32, error) {
			switch contextObj := contextObj.(type) {
			case *apiv1.Executable:
				return portForServingFromExecutable(serviceNamespacedName, contextObj, servicesProduced, reservedPorts, client, ctx, log)
			case *apiv1.Container:
				return portForServingFromContainer(serviceNamespacedName, contextObj, servicesProduced, client, ctx)
			default:
				// Should never happen
				return 0, fmt.Errorf("do not know how to determine service ports for object '%s'", contextObj.NamespacedName().String())
			}
		},

		"portFor": func(serviceNamespacedName string) (int32, error) {
			var serviceName = asNamespacedName(serviceNamespacedName, contextObj.GetObjectMeta().Namespace)

			// Need to take a peek at the Service to find out what port it is assigned.
			var svc apiv1.Service
			if svcQueryErr := client.Get(ctx, serviceName, &svc); svcQueryErr != nil {
				// CONSIDER in future we could be smarter and delay the startup of the Executable until
				// the service appears in the system, leaving the Executable in "pending" state.
				// This would necessitate watching over Services (specifically, Service creation).
				return 0, fmt.Errorf("service '%s' referenced by an environment variable does not exist", serviceName)
			}

			if svc.Status.EffectivePort == 0 {
				return 0, errServiceNotAssignedPort{Service: serviceName}
			}

			return svc.Status.EffectivePort, nil
		},

		"addressForServing": func(serviceNamespacedName string) (string, error) {
			return addressForServing(serviceNamespacedName, contextObj, servicesProduced)
		},

		"addressFor": func(serviceNamespacedName string) (string, error) {
			var serviceName = asNamespacedName(serviceNamespacedName, contextObj.GetObjectMeta().Namespace)

			// Need to take a peek at the Service to find out what address it is assigned.
			var svc apiv1.Service
			if svcQueryErr := client.Get(ctx, serviceName, &svc); svcQueryErr != nil {
				// CONSIDER in future we could be smarter and delay the startup of the Executable until
				// the service appears in the system, leaving the Executable in "pending" state.
				// This would necessitate watching over Services (specifically, Service creation).
				return "", fmt.Errorf("service '%s' referenced by an environment variable does not exist", serviceName)
			}

			if svc.Status.EffectiveAddress == "" {
				return "", errServiceNotAssignedPort{Service: serviceName}
			}

			return svc.Status.EffectiveAddress, nil
		},
	})

	return tmpl, nil
}

// Executes a template with the given input and substitution context (contextObj parameter).
// The 'substitutionContext' parameter provides additional information for logging if an issue occurs
// (e.g. "environment variable 'FOO' of object 'my-executable'").
func executeTemplate(
	tmpl *template.Template,
	contextObj dcpModelObject,
	input string,
	substitutionContext string,
	log logr.Logger,
) (string, error) {
	// We need to clone the template because if the input is empty, parsing is an no-op
	// and we will end up with data from previous template run.
	commonTmpl, err := tmpl.Clone()
	if err != nil {
		// This should really never happen, but the Clone() API returns an error, so we need to handle it.
		log.Error(err, fmt.Sprintf("could not clone template for %s", substitutionContext), "Input", input)
		return input, nil // We are going to use the value as-is.
	}

	inputTmpl, err := commonTmpl.Parse(input)
	if err != nil {
		// This does not necessarily indicate a problem--the input might be a completely intentional string
		// that happens to be un-parseable as a text template.
		log.Info(fmt.Sprintf("substitution is not possible for %s", substitutionContext), "Input", input)
		return input, nil
	}

	var sb strings.Builder
	err = inputTmpl.Execute(&sb, contextObj)
	if isTransientTemplateError(err) {
		// Fatal error that should prevent the contextObj from running.
		return "", err
	} else if err != nil {
		// We could not apply the template, so we are going to use the input value as-is.
		// This will likely cause the context object Spec value to be something that is not useful,
		// but it will be easier for the user to diagnose the problem
		// if we attempt to run the context object anyway.
		// TODO: the error from applying the template should be reported to the user via an event
		// (compare with https://github.com/microsoft/usvc/issues/20)
		log.Error(err, fmt.Sprintf("could not perform substitution for %s'", substitutionContext), "Input", input)
		return input, nil
	} else {
		return sb.String(), nil
	}
}

func portForServingFromExecutable(
	serviceNamespacedName string,
	exe *apiv1.Executable,
	servicesProduced []ServiceProducer,
	reservedPorts map[types.NamespacedName]int32,
	client ctrl_client.Client,
	ctx context.Context,
	log logr.Logger,
) (int32, error) {
	var serviceName = asNamespacedName(serviceNamespacedName, exe.GetObjectMeta().Namespace)

	for _, sp := range servicesProduced {
		if serviceName != sp.ServiceNamespacedName() {
			continue
		}

		if sp.Port != 0 {
			// The service producer annotation specifies the desired port, so we do not have to reserve anything,
			// and we do not need to report it via reservedPorts.
			if networking.IsValidPort(int(sp.Port)) {
				return sp.Port, nil
			} else {
				return 0, fmt.Errorf(
					"port %d specified in service producer annotation for service '%s' on object '%s' is invalid",
					sp.Port,
					serviceName,
					exe.NamespacedName().String(),
				)
			}
		}

		if port, found := reservedPorts[serviceName]; found {
			return port, nil
		}

		// Need to take a peek at the Service to find out what protocol it is using.
		var svc apiv1.Service
		if err := client.Get(ctx, serviceName, &svc); err != nil {
			// CONSIDER in future we could be smarter and delay the startup of the Executable until
			// the service appears in the system, leaving the Executable in "pending" state.
			// This would necessitate watching over Services (specifically, Service creation).
			return 0, fmt.Errorf(
				"service '%s' referenced by executable '%s' specification does not exist",
				serviceName,
				exe.NamespacedName().String(),
			)
		}

		port, err := networking.GetFreePort(svc.Spec.Protocol, sp.Address, log)
		if err != nil {
			return 0, fmt.Errorf(
				"could not allocate a port for service '%s' with desired address '%s': %w",
				serviceName,
				sp.Address,
				err,
			)
		}
		reservedPorts[serviceName] = port
		return port, nil
	}

	return 0, serviceNotProducedErr(serviceName, exe)
}

func portForServingFromContainer(
	serviceNamespacedName string,
	ctr *apiv1.Container,
	servicesProduced []ServiceProducer,
	client ctrl_client.Client,
	ctx context.Context,
) (int32, error) {
	var serviceName = asNamespacedName(serviceNamespacedName, ctr.GetObjectMeta().Namespace)

	for _, sp := range servicesProduced {
		if serviceName != sp.ServiceNamespacedName() {
			continue
		}

		if sp.Port == 0 {
			// This is invalid service producer annotation for a Container.
			return 0, fmt.Errorf("the service producer annotation for service '%s' on object '%s' should have included port information",
				serviceName,
				ctr.NamespacedName().String(),
			)
		}

		// Follow the service producer annotation logic for Containers (compare with ContainerReconciler.getHostAddressAndPortForContainerPort()).
		var matchedPort apiv1.ContainerPort
		found := false

		matchedByHost := slices.Select(ctr.Spec.Ports, func(p apiv1.ContainerPort) bool {
			return p.HostPort == sp.Port
		})
		if len(matchedByHost) > 0 {
			matchedPort = matchedByHost[0]
			found = true
		} else {
			matchedByContainer := slices.Select(ctr.Spec.Ports, func(p apiv1.ContainerPort) bool {
				return p.ContainerPort == sp.Port
			})
			if len(matchedByContainer) > 0 {
				matchedPort = matchedByContainer[0]
				found = true
			}
		}

		if found {
			return matchedPort.ContainerPort, nil
		} else {
			return 0, fmt.Errorf(
				"service '%s' referenced by container '%s' specification does not have a matching port",
				serviceName,
				ctr.NamespacedName().String(),
			)
		}
	}

	return 0, serviceNotProducedErr(serviceName, ctr)
}

func addressForServing(serviceNamespacedName string, obj dcpModelObject, servicesProduced []ServiceProducer) (string, error) {
	var serviceName = asNamespacedName(serviceNamespacedName, obj.GetObjectMeta().Namespace)

	for _, sp := range servicesProduced {
		if serviceName != sp.ServiceNamespacedName() {
			continue
		}

		if sp.Address != "" {
			return sp.Address, nil
		} else {
			return networking.Localhost, nil
		}
	}

	return "", serviceNotProducedErr(serviceName, obj)
}

func serviceNotProducedErr(serviceName types.NamespacedName, obj dcpModelObject) error {
	return fmt.Errorf(
		"service '%s' referenced by %s '%s' specification is not produced by this %s",
		serviceName,
		obj.GetObjectKind().GroupVersionKind().Kind,
		obj.NamespacedName().String(),
		obj.GetObjectKind().GroupVersionKind().Kind,
	)
}
