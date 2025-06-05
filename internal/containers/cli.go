package containers

import (
	"bytes"
	"errors"
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

// NormalizeCliErrors takes an error buffer containing CLI output and attempts to normalize any
// lines that match the provided error patterns into a well-defined error. It returns a final error
// containing the normalized errors (or nil if there were no non-empty lines returned).
func NormalizeCliErrors(errBuf *bytes.Buffer, errorMatches ...ErrorMatch) error {
	if errBuf == nil {
		return nil
	}

	lines := bytes.Split(errBuf.Bytes(), osutil.LF())

	return slices.Accumulate[[]byte, error](lines, func(err error, line []byte) error {
		if len(line) == 0 {
			return err // Skip empty lines
		}

		for i := range errorMatches {
			if errorMatches[i].regex.Match(line) {
				return errors.Join(err, errors.Join(errorMatches[i].err, errors.New(string(line))))
			}
		}

		// If no match found, report the error as unmatched
		return errors.Join(err, errors.Join(ErrUnmatched, errors.New(string(line))))
	})
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
