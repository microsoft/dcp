/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"errors"
	"fmt"
)

const maxProcessErrorExamples = 5

type processErrorSummary struct {
	count    int
	examples []error
}

func (summary *processErrorSummary) Error() string {
	return fmt.Sprintf(
		"%d errors occurred (showing first %d): %v",
		summary.count,
		len(summary.examples),
		errors.Join(summary.examples...),
	)
}

func (summary *processErrorSummary) Unwrap() []error {
	return summary.examples
}

func summarizeProcessErrors(processErrors []error) error {
	processErrors = nonNilErrors(processErrors)
	if len(processErrors) == 0 {
		return nil
	}
	if len(processErrors) <= maxProcessErrorExamples {
		return errors.Join(processErrors...)
	}
	return &processErrorSummary{
		count:    len(processErrors),
		examples: processErrors[:maxProcessErrorExamples],
	}
}

func nonNilErrors(processErrors []error) []error {
	result := make([]error, 0, len(processErrors))
	for _, processErr := range processErrors {
		if processErr != nil {
			result = append(result, processErr)
		}
	}
	return result
}
