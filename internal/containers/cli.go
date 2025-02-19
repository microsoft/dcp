package containers

import (
	"bytes"
	"fmt"
	"regexp"

	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

type ErrorMatch struct {
	regex              *regexp.Regexp
	err                error
	maxObjectsAffected int
}

func NewCliErrorMatch(regex *regexp.Regexp, err ...error) ErrorMatch {
	realErr := ErrNotFound
	if len(err) > 0 {
		realErr = err[0]
	}
	return ErrorMatch{
		regex:              regex,
		err:                realErr,
		maxObjectsAffected: 1,
	}
}

func (em ErrorMatch) MaxObjects(maxObjects int) ErrorMatch {
	em.maxObjectsAffected = maxObjects
	return em
}

func NormalizeCliError(originalError error, errBuf *bytes.Buffer, errorMatches ...ErrorMatch) error {
	if originalError == nil {
		return nil
	}

	if errBuf == nil {
		return originalError
	}

	if len(errorMatches) == 0 {
		return originalError
	}

	lines := bytes.Split(errBuf.Bytes(), osutil.LF())

	for i := range errorMatches {
		allMatch := slices.All(lines, func(l []byte) bool {
			line := bytes.TrimSpace(l)
			return len(line) == 0 || errorMatches[i].regex.Match(line)
		})

		// We might have some empty lines, hence it is possible that #lines >= #(objects affected by the command)
		if allMatch && len(lines) >= errorMatches[i].maxObjectsAffected {
			// All errors matched, so we can return the error from the matcher
			return errorMatches[i].err
		}
	}

	return originalError
}

func ExpectCliStrings(b *bytes.Buffer, expected []string) error {
	if len(expected) == 0 {
		return fmt.Errorf("need to provide at least one expected string") // Should never happen
	}
	if slices.LenIf(expected, func(s string) bool { return len(s) == 0 }) > 0 {
		return fmt.Errorf("expected strings must not be empty") // Also should never happen
	}

	notFoundErr := func() error {
		return fmt.Errorf("'%s' not found in output ('%s')", expected, b.String())
	}

	chunks := bytes.Split(b.Bytes(), osutil.LF())
	i := 0

	for _, chunk := range chunks {
		chunk = bytes.TrimSpace(chunk)
		if len(chunk) == 0 {
			continue
		}

		if string(chunk) != expected[i] {
			return notFoundErr()
		}

		i++
		if i == len(expected) {
			return nil // We found all the strings we wanted
		}
	}

	// We run out of chunks before finding all expected strings
	return notFoundErr()
}
