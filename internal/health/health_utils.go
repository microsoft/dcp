// Copyright (c) Microsoft Corporation. All rights reserved.

package health

import (
	"errors"
	"fmt"
	stdslices "slices"
	"time"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/maps"
	"github.com/microsoft/dcp/pkg/slices"
)

const (
	// If the health probe result is different by timestamp only, we do not write it to the status
	// unless the existing result is older than this value.
	// This helps avoid updating the status very frequently if an object has health probes with tight intervals.
	maxStaleHealthProbeResultAge = 15 * time.Second
)

// Computes the health status of an object that has one or more health probes associated with it.
func HealthStatusFromProbeResults(results []apiv1.HealthProbeResult) apiv1.HealthStatus {
	if len(results) == 0 {
		return apiv1.HealthStatusCaution
	}

	numSuccess := 0
	for _, result := range results {
		switch result.Outcome {
		case apiv1.HealthProbeOutcomeFailure:
			return apiv1.HealthStatusUnhealthy
		case apiv1.HealthProbeOutcomeSuccess:
			numSuccess++
		}
	}

	if numSuccess == len(results) {
		return apiv1.HealthStatusHealthy
	} else {
		return apiv1.HealthStatusCaution
	}
}

// Computes updated health probe results based on the "previous" and "latest" results.
// Returns the updated results and a flag indicating whether the map has changed (true if the results have changed).
func UpdateHealthProbeResults(previous []apiv1.HealthProbeResult, latest map[string]apiv1.HealthProbeResult) ([]apiv1.HealthProbeResult, bool) {
	healthResultsChanged := false

	results := maps.SliceToMap(previous, func(hpr apiv1.HealthProbeResult) (string, apiv1.HealthProbeResult) {
		return hpr.ProbeName, hpr
	})
	if len(results) == 0 && len(latest) > 0 {
		results = make(map[string]apiv1.HealthProbeResult)
	}

	for probeName, hpr := range latest {
		statusHpr, found := results[probeName]

		if !found {
			results[probeName] = hpr
			healthResultsChanged = true
		} else {
			res, timestampDiff := hpr.Diff(&statusHpr)
			timestampDiff = timestampDiff.Abs()
			needsUpdate := res == apiv1.Different || (res == apiv1.DiffTimestampOnly && timestampDiff >= maxStaleHealthProbeResultAge)

			if needsUpdate {
				results[probeName] = hpr
				healthResultsChanged = true
			}
		}
	}

	if healthResultsChanged {
		return maps.Values(results), true
	} else {
		return previous, false
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
			err = errors.Join(err, fmt.Errorf("unexpected health probe outcome for probe '%s': expected %s, got %s", result.ProbeName, expectedOutcome, result.Outcome))
		}
		seen = append(seen, result.ProbeName)
	}

	unseen, _ := slices.Diff(maps.Keys(expected), seen)
	if len(unseen) > 0 {
		err = errors.Join(err, fmt.Errorf("results for the following probes are missing: %v", unseen))
	}

	return err
}

// Verifies that the LATEST health probe result for each probe and owner combination matches expectations.
func VerifyHealthReports(expected map[commonapi.NamespacedNameWithKind]map[string]apiv1.HealthProbeOutcome, reports []HealthProbeReport) error {
	var seen []commonapi.NamespacedNameWithKind
	var err error

	byOwner := slices.GroupBy[[]HealthProbeReport, commonapi.NamespacedNameWithKind](
		reports,
		func(report HealthProbeReport) commonapi.NamespacedNameWithKind { return report.Owner },
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

	unseen := slices.DiffFunc(maps.Keys(expected), seen, func(a, b commonapi.NamespacedNameWithKind) bool { return a == b })
	if len(unseen) > 0 {
		err = errors.Join(err, fmt.Errorf("results for the following owners are missing: %v", unseen))
	}

	return err
}
