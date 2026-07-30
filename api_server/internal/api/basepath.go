package api

import (
	"fmt"
	"strings"
)

// normalizeBasePath canonicalises an operator-supplied URL prefix into the one
// form Echo's Group() wants: either "" (mounted at the domain root, the
// default) or "/prefix" with a leading slash and no trailing slash.
//
// The YCP console is fronted at https://devcloud.yugabyte.com/ycp/ on the same
// domain as YBM rather than its own subdomain, so browser auth tokens and
// session storage carry over. That proxy forwards the /ycp prefix through
// untouched, which means every route this server registers has to move under
// it. Accepting "ycp", "/ycp", "/ycp/" and "//ycp" interchangeably keeps a
// chart value or a hand-typed flag from producing "//api/v1" or "/ycp//api/v1",
// neither of which any route would match.
//
// Anything that is not a plain path prefix is an error rather than something to
// coerce, because the failure is otherwise invisible: a full URL (the likely
// typo — the agent's sibling YCP_URL setting *is* a full URL) would mount the
// whole surface under "/https:/devcloud.yugabyte.com/ycp", the console and API
// would 404, and the deployment would still go Ready because the health probe
// hits the unprefixed alias. Refusing to boot turns that into a crash-loop with
// the reason in the logs.
func normalizeBasePath(p string) (string, error) {
	raw := strings.TrimSpace(p)
	segments := strings.FieldsFunc(raw, func(r rune) bool { return r == '/' })
	if len(segments) == 0 {
		if strings.ContainsAny(raw, "?#") {
			return "", fmt.Errorf("base path %q is not a path prefix", p)
		}
		return "", nil
	}
	for _, s := range segments {
		if err := validateBasePathSegment(s); err != nil {
			return "", fmt.Errorf("base path %q: %w", p, err)
		}
	}
	return "/" + strings.Join(segments, "/"), nil
}

// validateBasePathSegment accepts one path segment of the RFC 3986 unreserved
// set (letters, digits, '-', '.', '_', '~'). That rejects the whole class of
// values that are not path prefixes at all — a scheme+host ("https:",
// "devcloud.yugabyte.com" is fine but the colon is not), a route pattern
// ("*", ":id"), a query or fragment, an escape ("%2e"), or embedded
// whitespace — plus the dot segments that would silently relocate the mount.
func validateBasePathSegment(s string) error {
	if s == "." || s == ".." {
		return fmt.Errorf("segment %q is a relative path element", s)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '.', r == '_', r == '~':
		default:
			return fmt.Errorf("segment %q contains %q; expected a plain path "+
				"prefix such as /ycp, not a URL or a route pattern", s, r)
		}
	}
	return nil
}
