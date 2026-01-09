/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package templating

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/template"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/slices"
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

type errServiceDoesNotExist struct {
	Service       types.NamespacedName
	ContextObject types.NamespacedName
}

func (e errServiceDoesNotExist) Error() string {
	return fmt.Sprintf("service '%s' referenced by '%s' does not exist", e.Service.String(), e.ContextObject.String())
}

func IsTransientTemplateError(err error) bool {
	var ePort errServiceNotAssignedPort
	var eAddress errServiceNotAssignedAddress
	var eDoesNotExist errServiceDoesNotExist
	return errors.As(err, &ePort) || errors.As(err, &eAddress) || errors.As(err, &eDoesNotExist)
}

// Creates a template for evaluating a value of a spec field of a Container or Executable.
// Our template functions may need to query the API server, this is what ctx and client parameters are for.
// The contextObject parameter is the object that will be used as context during template evaluation (Executable, Container, etc.)
// If a port needs to be reserved for a service production by the context object,
// information about the reserved port will be added to reservedPorts map (currently only used for Executables).
func NewSpecValueTemplate(
	ctx context.Context,
	client ctrl_client.Client,
	contextObj commonapi.DcpModelObject,
	reservedPorts map[types.NamespacedName]int32,
	log logr.Logger,
) (*template.Template, error) {

	servicesProduced, err := commonapi.GetServiceProducersForObject(contextObj, log)
	if err != nil {
		return nil, err
	}

	tmpl := template.New("envar").Funcs(template.FuncMap{
		"portForServing": func(serviceNamespacedName string) (int32, error) {
			switch contextObj := contextObj.(type) {
			case *apiv1.Executable:
				return portForServingFromExecutable(serviceNamespacedName, contextObj, servicesProduced, reservedPorts, client, ctx, log)
			case *apiv1.Container:
				return portForServingFromContainer(serviceNamespacedName, contextObj, servicesProduced)
			default:
				// Should never happen
				return 0, fmt.Errorf("do not know how to determine service ports for object '%s'", contextObj.NamespacedName().String())
			}
		},

		"portFor": func(serviceNamespacedName string) (int32, error) {
			return portFor(serviceNamespacedName, contextObj, client, ctx)
		},

		"addressForServing": func(serviceNamespacedName string) (string, error) {
			return addressForServing(serviceNamespacedName, contextObj, servicesProduced)
		},

		"addressFor": func(serviceNamespacedName string) (string, error) {
			return addressFor(serviceNamespacedName, contextObj, client, ctx)
		},
	})

	return tmpl, nil
}

// Create a template for evaluating a value of a health probe belonging to a Container or Executable.
func NewHealthProbeSpecValueTemplate(
	ctx context.Context,
	client ctrl_client.Client,
	contextObj commonapi.DcpModelObject,
	log logr.Logger,
) (*template.Template, error) {

	servicesProduced, err := commonapi.GetServiceProducersForObject(contextObj, log)
	if err != nil {
		return nil, err
	}

	tmpl := template.New("healthProbeVal").Funcs(template.FuncMap{
		"portForServing": func(serviceNamespacedName string) (int32, error) {
			switch contextObj := contextObj.(type) {
			case *apiv1.Executable:
				return portForServingFromExecutableEndpoints(serviceNamespacedName, contextObj, servicesProduced, client, ctx, log)
			case *apiv1.Container:
				return portForServingFromContainer(serviceNamespacedName, contextObj, servicesProduced)
			default:
				// Should never happen
				fullName := commonapi.GetNamespacedNameWithKind(contextObj).String()
				return 0, fmt.Errorf("do not know how to determine service ports for object '%s'", fullName)
			}
		},

		"portFor": func(serviceNamespacedName string) (int32, error) {
			return portFor(serviceNamespacedName, contextObj, client, ctx)
		},

		"addressForServing": func(serviceNamespacedName string) (string, error) {
			return addressForServing(serviceNamespacedName, contextObj, servicesProduced)
		},

		"addressFor": func(serviceNamespacedName string) (string, error) {
			return addressFor(serviceNamespacedName, contextObj, client, ctx)
		},
	})

	return tmpl, nil
}

// Executes a template with the given input and substitution context (contextObj parameter).
// The 'substitutionContext' parameter provides additional information for logging if an issue occurs
// (e.g. "environment variable 'FOO' of object 'my-executable'").
func ExecuteTemplate(
	tmpl *template.Template,
	contextObj commonapi.DcpModelObject,
	input string,
	substitutionContext string,
	log logr.Logger,
) (string, error) {
	// We need to clone the template because if the input is empty, parsing is an no-op
	// and we will end up with data from previous template run.
	commonTmpl, err := tmpl.Clone()
	if err != nil {
		// This should really never happen, but the Clone() API returns an error, so we need to handle it.
		log.Error(err, fmt.Sprintf("Could not clone template for %s", substitutionContext), "Input", input)
		return input, nil // We are going to use the value as-is.
	}

	inputTmpl, err := commonTmpl.Parse(input)
	if err != nil {
		// This does not necessarily indicate a problem--the input might be a completely intentional string
		// that happens to be un-parsable as a text template.
		log.Info(fmt.Sprintf("Substitution is not possible for %s", substitutionContext), "Input", input)
		return input, nil
	}

	var sb strings.Builder
	err = inputTmpl.Execute(&sb, contextObj)
	if IsTransientTemplateError(err) {
		// Report the error to the caller and let them retry templating or fail, as necessary.
		return "", err
	} else if err != nil {
		// We could not apply the template, so we are going to use the input value as-is.
		// This will likely cause the context object Spec value to be something that is not useful,
		// but it will be easier for the user to diagnose the problem
		// if we attempt to run the context object anyway.
		// TODO: the error from applying the template should be reported to the user via an event
		// (compare with https://github.com/microsoft/usvc/issues/20)
		log.Error(err, fmt.Sprintf("Could not perform substitution for %s'", substitutionContext), "Input", input)
		return input, nil
	} else {
		return sb.String(), nil
	}
}

// This function is called in the context of starting an Executable.
// It will return the port for serving a service if the port is specified by the service producer annotation.
// If the port is not specified, it will "reserve" the port and return in via the reservedPorts map.
// The map is cached by Executable controller, so calling this function multiple times
// will return the same port for the same Executable + Service pair
func portForServingFromExecutable(
	serviceNamespacedName string,
	exe *apiv1.Executable,
	servicesProduced []commonapi.ServiceProducer,
	reservedPorts map[types.NamespacedName]int32,
	client ctrl_client.Client,
	ctx context.Context,
	log logr.Logger,
) (int32, error) {
	var serviceName = commonapi.AsNamespacedName(serviceNamespacedName, exe.GetObjectMeta().Namespace)

	for _, sp := range servicesProduced {
		if serviceName != sp.ServiceNamespacedName() {
			continue
		}

		if sp.Port != 0 {
			// The service producer annotation specifies the desired port, so we do not have to reserve anything,
			// and we do not need to report it via reservedPorts.
			return validateServiceProducerPort(sp, serviceName.String(), exe.NamespacedName().String())
		}

		if reservedPorts == nil {
			// This is a bug in the code, reservedPorts should never be nil for Executables.
			return 0, fmt.Errorf("reservedPorts map is nil for Executable '%s'", exe.NamespacedName().String())
		}

		if port, found := reservedPorts[serviceName]; found {
			return port, nil
		}

		// Need to take a peek at the Service to find out what protocol it is using.
		var svc apiv1.Service
		if err := client.Get(ctx, serviceName, &svc); err != nil {
			return 0, errServiceDoesNotExist{Service: serviceName, ContextObject: exe.NamespacedName()}
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
	servicesProduced []commonapi.ServiceProducer,
) (int32, error) {
	var serviceName = commonapi.AsNamespacedName(serviceNamespacedName, ctr.GetObjectMeta().Namespace)

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

// Called in the context of resolving health probe data for the Executable.
// The port is read from the service producer annotation.
// If dynamic port assignment is used (service producer annotation does not specify a port),
// the function will look for existing Endpoints for the Executable and Service pair and return the port from there.
func portForServingFromExecutableEndpoints(
	serviceNamespacedName string,
	exe *apiv1.Executable,
	servicesProduced []commonapi.ServiceProducer,
	client ctrl_client.Client,
	ctx context.Context,
	log logr.Logger,
) (int32, error) {
	var serviceName = commonapi.AsNamespacedName(serviceNamespacedName, exe.GetObjectMeta().Namespace)

	var childEndpoints apiv1.EndpointList
	listErr := client.List(ctx, &childEndpoints, ctrl_client.InNamespace(exe.GetNamespace()), ctrl_client.MatchingFields{commonapi.WorkloadOwnerKey: string(exe.GetUID())})
	if listErr != nil {
		log.Error(listErr, "Failed to list child Endpoint objects for Executable", "Executable", exe.NamespacedName().String())
	}

	for _, sp := range servicesProduced {
		if serviceName != sp.ServiceNamespacedName() {
			continue
		}

		if sp.Port != 0 {
			// The service producer annotation specifies the desired port, so we do not have to reserve anything,
			// and we do not need to report it via reservedPorts.
			return validateServiceProducerPort(sp, serviceName.String(), exe.NamespacedName().String())
		}

		// Need to take a peek at existing Endpoints for the Service to find out what port was assigned.
		existingEndpoints := slices.Select(childEndpoints.Items, func(e apiv1.Endpoint) bool {
			return e.Spec.ServiceName == sp.ServiceName && e.Spec.ServiceNamespace == sp.ServiceNamespace
		})

		// There will be at most one Endpoint for the Executable and Service pair.
		if len(existingEndpoints) == 0 || !networking.IsValidPort(int(existingEndpoints[0].Spec.Port)) {
			return 0, errServiceNotAssignedPort{Service: serviceName}
		} else {
			return existingEndpoints[0].Spec.Port, nil
		}
	}

	return 0, serviceNotProducedErr(serviceName, exe)
}

func addressForServing(serviceNamespacedName string, obj commonapi.DcpModelObject, servicesProduced []commonapi.ServiceProducer) (string, error) {
	var serviceName = commonapi.AsNamespacedName(serviceNamespacedName, obj.GetObjectMeta().Namespace)

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

func portFor(
	serviceNamespacedName string,
	obj commonapi.DcpModelObject,
	client ctrl_client.Client,
	ctx context.Context,
) (int32, error) {
	var serviceName = commonapi.AsNamespacedName(serviceNamespacedName, obj.GetObjectMeta().Namespace)

	// Need to take a peek at the Service to find out what port it is assigned.
	var svc apiv1.Service
	if svcQueryErr := client.Get(ctx, serviceName, &svc); svcQueryErr != nil {
		if apierrors.IsNotFound(svcQueryErr) {
			return 0, errServiceDoesNotExist{Service: serviceName, ContextObject: obj.NamespacedName()}
		} else {
			return 0, fmt.Errorf("could not deterimine the port for service '%s': %w", serviceName, svcQueryErr)
		}
	}

	if svc.Status.EffectivePort == 0 {
		return 0, errServiceNotAssignedPort{Service: serviceName}
	}

	return svc.Status.EffectivePort, nil
}

func addressFor(
	serviceNamespacedName string,
	obj commonapi.DcpModelObject,
	client ctrl_client.Client,
	ctx context.Context,
) (string, error) {
	var serviceName = commonapi.AsNamespacedName(serviceNamespacedName, obj.GetObjectMeta().Namespace)

	// Need to take a peek at the Service to find out what address it is assigned.
	var svc apiv1.Service
	if svcQueryErr := client.Get(ctx, serviceName, &svc); svcQueryErr != nil {
		if apierrors.IsNotFound(svcQueryErr) {
			return "", errServiceDoesNotExist{Service: serviceName, ContextObject: obj.NamespacedName()}
		} else {
			return "", fmt.Errorf("could not deterimine the address for service '%s': %w", serviceName, svcQueryErr)
		}
	}

	if svc.Status.EffectiveAddress == "" {
		return "", errServiceNotAssignedAddress{Service: serviceName}
	}

	return svc.Status.EffectiveAddress, nil
}

func serviceNotProducedErr(serviceName types.NamespacedName, obj commonapi.DcpModelObject) error {
	return fmt.Errorf(
		"service '%s' referenced by %s '%s' specification is not produced by this %s",
		serviceName,
		obj.GetObjectKind().GroupVersionKind().Kind,
		obj.NamespacedName().String(),
		obj.GetObjectKind().GroupVersionKind().Kind,
	)
}

func validateServiceProducerPort(
	sp commonapi.ServiceProducer,
	serviceName string,
	objectName string,
) (int32, error) {
	if networking.IsValidPort(int(sp.Port)) {
		return sp.Port, nil
	} else {
		return 0, fmt.Errorf(
			"port %d specified in service producer annotation for service '%s' on object '%s' is invalid",
			sp.Port, serviceName, objectName,
		)
	}
}
