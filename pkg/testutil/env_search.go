package testutil

import (
	"regexp"
)

// FindAllMatching tries the passed regular expression on all strings in the passed slice
// and returns the result for re.FindStringSubmatch() for each string in the slice that matches.
// For each match, the slice returned will contain the full match, followed by all captured groups (subexpressions).
func FindAllMatching(ss []string, re *regexp.Regexp) [][]string {
	if len(ss) == 0 {
		return nil
	}

	retval := make([][]string, 0)
	for _, s := range ss {
		matches := re.FindStringSubmatch(s)
		if matches != nil {
			retval = append(retval, matches)
		}
	}

	return retval
}
