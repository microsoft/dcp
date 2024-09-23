package health

import (
	"context"
	"fmt"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
)

// HealthProbeReport represents a result of a single health probe execution.
type HealthProbeReport struct {
	Probe  *apiv1.HealthProbe
	Result apiv1.HealthProbeResult
	Owner  apiv1.NamespacedNameWithKind
}

// An identifier uniquely identifying a health probe within the system, comprising of the owner information and the probe name.
// The order of components is optimized for quick equality checks
// (owner name/probe name/owner namespace/owner kind/owner version/owner group).
type healthProbeIdentifier string

func getIdentifier(probe *apiv1.HealthProbe, owner apiv1.NamespacedNameWithKind) healthProbeIdentifier {
	if probe == nil {
		panic("cannot compute health probe identifier for nil probe")
	}
	if owner.Empty() {
		panic("cannot compute health probe identifier for empty owner")
	}
	return healthProbeIdentifier(fmt.Sprintf(
		"%s/%s/%s/%s/%s/%s",
		owner.Name,
		probe.Name,
		owner.Namespace,
		owner.Kind.Kind,
		owner.Kind.Version,
		owner.Kind.Group,
	))
}

type HealthProbeExecutor interface {
	Execute(executionCtx context.Context, probe *apiv1.HealthProbe) (apiv1.HealthProbeResult, error)
}
type HealthProbeExecutorFunc func(executionCtx context.Context, probe *apiv1.HealthProbe) (apiv1.HealthProbeResult, error)

func (f HealthProbeExecutorFunc) Execute(executionCtx context.Context, probe *apiv1.HealthProbe) (apiv1.HealthProbeResult, error) {
	return f(executionCtx, probe)
}
