package health

import (
	"errors"
	"fmt"
	stdslices "slices"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

// Computes the health status of an object that has one or more health probes associated with it.
func HealthStatusFromProbeResults(results []apiv1.HealthProbeResult) apiv1.HealthStatus {
	if len(results) == 0 {
		return apiv1.HealthStatusCaution
	}

	numSuccess := 0
	for _, result := range results {
		if result.Outcome == apiv1.HealthProbeOutcomeFailure {
			return apiv1.HealthStatusUnhealthy
		} else if result.Outcome == apiv1.HealthProbeOutcomeSuccess {
			numSuccess++
		}
	}

	if numSuccess == len(results) {
		return apiv1.HealthStatusHealthy
	} else {
		return apiv1.HealthStatusCaution
	}
}

// Verify that all health probe results that we care about match expectations.
// The expected map contains the expected outcome for each probe name.
// If the probe name is not in the map, we do not care about the result.
func VerifyHealthResults(expected map[string]apiv1.HealthProbeOutcome, results []apiv1.HealthProbeResult) error {
	var seen []string
	var err error

	for _, result := range results {
		expectedOutcome, found := expected[result.ProbeName]
		if !found {
			continue // We do not care about this result
		}

		if result.Outcome != expectedOutcome {
			err = errors.Join(err, fmt.Errorf("Unexpected health probe outcome for probe '%s': expected %s, got %s", result.ProbeName, expectedOutcome, result.Outcome))
		}
		seen = append(seen, result.ProbeName)
	}

	unseen, _ := slices.Diff(maps.Keys(expected), seen)
	if len(unseen) > 0 {
		err = errors.Join(err, fmt.Errorf("Results for the following probes are missing: %v", unseen))
	}

	return err
}

// Verifies that the LATEST health probe result for each probe and owner combination matches expectations.
func VerifyHealthReports(expected map[apiv1.NamespacedNameWithKind]map[string]apiv1.HealthProbeOutcome, reports []HealthProbeReport) error {
	var seen []apiv1.NamespacedNameWithKind
	var err error

	byOwner := slices.GroupBy[[]HealthProbeReport, apiv1.NamespacedNameWithKind](
		reports,
		func(report HealthProbeReport) apiv1.NamespacedNameWithKind { return report.Owner },
	)

	for owner, reports := range byOwner {
		expectedResults, found := expected[owner]
		if !found {
			continue // We do not care about this owner
		}

		// Only keep the latest results for each probe
		stdslices.SortFunc(reports, func(a, b HealthProbeReport) int {
			return a.Result.Timestamp.Compare(b.Result.Timestamp.Time)
		})
		latestResults := make(map[string]apiv1.HealthProbeResult)
		for _, report := range reports {
			latestResults[report.Result.ProbeName] = report.Result
		}

		err = errors.Join(err, VerifyHealthResults(expectedResults, maps.Values(latestResults)))
		seen = append(seen, owner)
	}

	unseen := slices.DiffFunc(maps.Keys(expected), seen, func(a, b apiv1.NamespacedNameWithKind) bool { return a == b })
	if len(unseen) > 0 {
		err = errors.Join(err, fmt.Errorf("Results for the following owners are missing: %v", unseen))
	}

	return err
}
