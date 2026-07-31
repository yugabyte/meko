package main

import (
	"fmt"
	"regexp"
)

// resolveTestFilter turns the --test (exact name) and --testr (regex) flags into
// the single test-name regex core.Registry.Filter expects. --test matches ONE
// test by its exact name (anchored + regex-escaped, so metacharacters like the
// "/" in "perf/pgbench" are literal, not regex); --testr is a raw regex. The two
// are mutually exclusive. Empty/empty selects everything.
func resolveTestFilter(exactName, regex string) (string, error) {
	if exactName != "" && regex != "" {
		return "", fmt.Errorf("--test (exact name) and --testr (regex) are mutually exclusive; pass only one")
	}
	if exactName != "" {
		return "^" + regexp.QuoteMeta(exactName) + "$", nil
	}
	return regex, nil
}
