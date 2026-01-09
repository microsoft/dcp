// Copyright (c) Microsoft Corporation. All rights reserved.

package health

import (
	"context"
	"fmt"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/commonapi"
)

// HealthProbeReport represents a result of a single health probe execution.
type HealthProbeReport struct {
	Probe  *apiv1.HealthProbe
	Result apiv1.HealthProbeResult
	Owner  commonapi.NamespacedNameWithKind
}

// An identifier uniquely identifying a health probe within the system, comprising of the owner information and the probe name.
// The order of components is optimized for quick equality checks
// (owner name/probe name/owner namespace/owner kind/owner version/owner group).
type healthProbeIdentifier string

func getIdentifier(probe *apiv1.HealthProbe, owner commonapi.DcpModelObject) healthProbeIdentifier {
	if probe == nil {
		panic("cannot compute health probe identifier for nil probe")
	}
	if owner == nil {
		panic("cannot compute health probe identifier without an owner")
	}

	ownerName := owner.GetName()
	ownerNamespace := owner.GetNamespace()
	ownerGVK := owner.GetObjectKind().GroupVersionKind()

	return healthProbeIdentifier(fmt.Sprintf(
		"%s/%s/%s/%s/%s/%s",
		ownerName,
		probe.Name,
		ownerNamespace,
		ownerGVK.Kind,
		ownerGVK.Version,
		ownerGVK.Group,
	))
}

type HealthProbeExecutor interface {
	Execute(
		executionCtx context.Context,
		probe *apiv1.HealthProbe,
		probeOwner commonapi.DcpModelObject,
		probeID healthProbeIdentifier,
	) (apiv1.HealthProbeResult, error)
}

type HealthProbeExecutorFunc func(
	executionCtx context.Context,
	probe *apiv1.HealthProbe,
	probeOwner commonapi.DcpModelObject,
	probeID healthProbeIdentifier,
) (apiv1.HealthProbeResult, error)

func (f HealthProbeExecutorFunc) Execute(
	executionCtx context.Context,
	probe *apiv1.HealthProbe,
	probeOwner commonapi.DcpModelObject,
	probeID healthProbeIdentifier,
) (apiv1.HealthProbeResult, error) {
	return f(executionCtx, probe, probeOwner, probeID)
}
