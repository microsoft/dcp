package v1

import (
	"fmt"
	stdmaps "maps"
	"net/url"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/microsoft/usvc-apiserver/pkg/pointers"
)

type HealthStatus string

const (
	HealthStatusHealthy   HealthStatus = "Healthy"
	HealthStatusCaution   HealthStatus = "Caution"
	HealthStatusUnhealthy HealthStatus = "Unhealthy"
)

type HealthProbeType string

const (
	HealthProbeTypeHttp          HealthProbeType = "HTTP"
	HealthProbeTypeExecutable    HealthProbeType = "Executable"
	HealthProbeTypeContainerExec HealthProbeType = "ContainerExec"
)

type HealthProbeScheduleKind string

const (
	// The probe runs periodically during the entire time when the parent object is running.
	HealthProbeScheduleContinuous HealthProbeScheduleKind = "Continuous"

	// The probe runs periodically until it succeeds. Once the probe succeeds, it stops running
	// and is considered successful for the remainder of the parent object's lifetime.
	HealthProbeScheduleUntilSuccess HealthProbeScheduleKind = "UntilSuccess"
)

// HealthProbeSchedule represents a schedule for running a health probe.
// +k8s:openapi-gen=true
type HealthProbeSchedule struct {
	// Kind of the schedule
	// +kubebuilder:default:=Continuous
	Kind HealthProbeScheduleKind `json:"kind,omitempty"`

	// Interval at which the probe should run
	Interval metav1.Duration `json:"interval"`

	// Timeout for the probe (if the probe does not complete within this time, it is considered failed)
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// How long to wait between parent object startup and the first probe run
	// +optional
	InitialDelay *metav1.Duration `json:"initialDelay,omitempty"`
}

func (hps *HealthProbeSchedule) Validate(schedulePath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}

	if hps.Kind != HealthProbeScheduleContinuous && hps.Kind != HealthProbeScheduleUntilSuccess && hps.Kind != "" {
		errorList = append(errorList, field.Invalid(schedulePath.Child("kind"), hps.Kind, "Invalid schedule kind; must be one of Continuous (assumed when not specified) or UntilSuccess"))
	}

	if hps.Interval.Duration <= 0 {
		errorList = append(errorList, field.Invalid(schedulePath.Child("interval"), hps.Interval, "Interval must be positive"))
	}

	if hps.Timeout != nil && hps.Timeout.Duration <= 0 {
		errorList = append(errorList, field.Invalid(schedulePath.Child("timeout"), hps.Timeout, "Timeout must be positive"))
	}

	if hps.InitialDelay != nil && hps.InitialDelay.Duration < 0 {
		errorList = append(errorList, field.Invalid(schedulePath.Child("initialDelay"), hps.InitialDelay, "Initial delay must be non-negative"))
	}

	return errorList
}

func (hps *HealthProbeSchedule) Equal(other *HealthProbeSchedule) bool {
	if hps.Kind != other.Kind {
		return false
	}

	if hps.Interval != other.Interval {
		return false
	}

	if !pointers.EqualValue(hps.Timeout, other.Timeout) {
		return false
	}

	if !pointers.EqualValue(hps.InitialDelay, other.InitialDelay) {
		return false
	}

	return true
}

// HealthProbe represents a health check to be performed on a Container or Executable.
// It is a discriminated union differentiated by the Type property.
// +k8s:openapi-gen=true
type HealthProbe struct {
	// Name of the probe. This is used to identify the probe in the status of the parent object.
	// Must be unique within the parent object.
	Name string `json:"name"`

	// Type of the health probe
	Type HealthProbeType `json:"type"`

	// For Executable-type health probes, the configuration for the Executable health probe.
	// +optional
	ExecutableProbe *ExecutableProbe `json:"executableProbe,omitempty"`

	// For HTTP-type health probes, the configuration for the HTTP health probe.
	// +optional
	HttpProbe *HttpProbe `json:"httpProbe,omitempty"`

	// Schedule for running the probe
	Schedule HealthProbeSchedule `json:"schedule"`

	// Annotations (metadata) for the health probe
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Validate checks the health probe for correctness.
// The probePath argument is used to construct the field path for any validation errors.
// Returs a list of errors, or nil if the probe is valid.
func (hp *HealthProbe) Validate(probePath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}

	if hp.Name == "" {
		errorList = append(errorList, field.Invalid(probePath.Child("name"), "", "A health probe must have a name"))
	}

	switch hp.Type {
	case HealthProbeTypeHttp:
		errorList = append(errorList, hp.HttpProbe.Validate(probePath.Child("httpProbe"))...)

	case HealthProbeTypeExecutable:
		errorList = append(errorList, hp.ExecutableProbe.Validate(probePath.Child("executableProbe"))...)

	default:
		errorList = append(errorList, field.Invalid(probePath.Child("type"), hp.Type, "unsupported health probe type"))
	}

	errorList = append(errorList, hp.Schedule.Validate(probePath.Child("schedule"))...)

	return errorList
}

func (hp *HealthProbe) Equal(other *HealthProbe) bool {
	if hp.Name != other.Name {
		return false
	}

	if hp.Type != other.Type {
		return false
	}

	switch hp.Type {
	case HealthProbeTypeHttp:
		if !hp.HttpProbe.Equal(other.HttpProbe) {
			return false
		}

	case HealthProbeTypeExecutable:
		if !hp.ExecutableProbe.Equal(other.ExecutableProbe) {
			return false
		}
	}

	if !hp.Schedule.Equal(&other.Schedule) {
		return false
	}

	if !stdmaps.Equal(hp.Annotations, other.Annotations) {
		return false
	}

	return true
}

func (hp HealthProbe) String() string {
	return hp.Name + " (" + string(hp.Type) + ")"
}

// HttpHeader represents a single HTTP header to be included in an HTTP health probe.
// +k8s:openapi-gen=true
type HttpHeader struct {
	// Name of the HTTP header
	Name string `json:"name"`

	// Value of the HTTP header
	// +optional
	Value string `json:"value,omitempty"`
}

// HttpProbe contains the configuration for an HTTP health probe.
// +k8s:openapi-gen=true
type HttpProbe struct {
	// URL to probe
	Url string `json:"url"`

	// Headers to include in the HTTP request
	// +optional
	// +listType=map
	// +listMapKey=name
	Headers []HttpHeader `json:"headers,omitempty"`
}

func (hhp *HttpProbe) Validate(probePath *field.Path) field.ErrorList {
	if hhp == nil {
		return field.ErrorList{field.Invalid(probePath, nil, "HTTP probe data missing (at least a URL is required)")}
	}

	if hhp.Url == "" {
		return field.ErrorList{field.Invalid(probePath.Child("url"), "", "URL is required for HTTP probe")}
	}

	url, err := url.Parse(hhp.Url)
	if err != nil {
		return field.ErrorList{field.Invalid(probePath.Child("url"), hhp.Url, err.Error())}
	}

	if !url.IsAbs() {
		return field.ErrorList{field.Invalid(probePath.Child("url"), hhp.Url, "HTTP probe URL must be absolute")}
	}

	return nil
}

func (hhp *HttpProbe) Equal(other *HttpProbe) bool {
	if hhp == nil && other == nil {
		return true
	}

	if hhp == nil || other == nil {
		return false
	}

	if hhp.Url != other.Url {
		return false
	}

	if len(hhp.Headers) != len(other.Headers) {
		return false
	}

	for i, header := range hhp.Headers {
		if header != other.Headers[i] {
			return false
		}
	}

	return true
}

// ExecutableProbe contains the configuration for an Executable health probe.
// +k8s:openapi-gen=true
type ExecutableProbe struct {
	// Template for creating the Executable that performs the health check
	ExecutableTemplate ExecutableTemplate `json:"executableTemplate"`
}

func (ep *ExecutableProbe) Validate(probePath *field.Path) field.ErrorList {
	if ep == nil {
		return field.ErrorList{field.Invalid(probePath, nil, "Executable probe data is missing")}
	}

	exeSpec := ep.ExecutableTemplate.Spec
	exeSpecPath := probePath.Child("executableTemplate", "spec")
	errorList := exeSpec.Validate(exeSpecPath)

	if exeSpec.Stop {
		errorList = append(errorList, field.Invalid(exeSpecPath.Child("stop"), exeSpec.Stop, "Health probe with Stop==true will never run"))
	}

	if len(exeSpec.HealthProbes) > 0 {
		errorList = append(errorList, field.Invalid(exeSpecPath.Child("healthProbes"), exeSpec.HealthProbes, "Health probe Executable cannot have its own health probes"))
	}

	return errorList
}

func (ep *ExecutableProbe) Equal(other *ExecutableProbe) bool {
	if ep == nil && other == nil {
		return true
	}

	if ep == nil || other == nil {
		return false
	}

	return ep.ExecutableTemplate.Equal(other.ExecutableTemplate)
}

type HealthProbeOutcome string

const (
	HealthProbeOutcomeSuccess HealthProbeOutcome = "Success"
	HealthProbeOutcomeFailure HealthProbeOutcome = "Failure"
	HealthProbeOutcomeUnknown HealthProbeOutcome = "Unknown"
)

// HealthProbeResult represents the result of a single execution of a health probe.
// +k8s:openapi-gen=true
type HealthProbeResult struct {
	// Outcome of the health probe
	Outcome HealthProbeOutcome `json:"outcome"`

	// Timestamp for the result
	Timestamp metav1.MicroTime `json:"timestamp"`

	// Name of the probe to which this result corresponds
	ProbeName string `json:"probeName"`

	// Human-readable message describing the outcome
	// +optional
	Reason string `json:"reason,omitempty"`
}

func (hpr HealthProbeResult) String() string {
	return fmt.Sprintf("%s -> %s (%s), Reason=%s", hpr.ProbeName, hpr.Outcome, hpr.Timestamp.Format(time.StampMilli), hpr.Reason)
}

type HealthProbeResultDiff int

const (
	DiffNone          HealthProbeResultDiff = 0
	DiffTimestampOnly HealthProbeResultDiff = 1
	Different         HealthProbeResultDiff = 2
)

func (hpr HealthProbeResult) Diff(other *HealthProbeResult) (HealthProbeResultDiff, time.Duration) {
	timestampDiff := hpr.Timestamp.Time.Sub(other.Timestamp.Time)

	switch {
	case hpr.Outcome != other.Outcome, hpr.ProbeName != other.ProbeName, hpr.Reason != other.Reason:
		return Different, timestampDiff
	case timestampDiff != 0:
		return DiffTimestampOnly, timestampDiff
	default:
		return DiffNone, 0
	}
}
