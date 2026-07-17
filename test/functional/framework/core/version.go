package core

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// DetectedVersions is what an environment reports at setup: the product build
// and the YugabyteDB backing version.
type DetectedVersions struct {
	Product string
	YB      string
}

// VersionSpec bounds the versions a test supports. Empty fields are unbounded.
type VersionSpec struct {
	MinProduct string
	MaxProduct string
	MinYB      string
	MaxYB      string
}

// Version is a parsed, comparable version. It tolerates 4-part versions
// (2025.1.0.0) and a trailing -bNNN build suffix.
type Version struct {
	nums     []int
	build    int
	hasBuild bool
}

// ParseVersion parses a version string. It accepts a leading "v", dotted
// numeric parts of any length, and an optional "-bNNN" build suffix. Non-numeric
// or empty parts are treated as 0.
func ParseVersion(s string) (Version, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Version{}, fmt.Errorf("empty version")
	}
	s = strings.TrimPrefix(s, "v")

	build := 0
	hasBuild := false
	// Split off a build suffix like "-b123" (or a bare "-123").
	if i := strings.IndexByte(s, '-'); i >= 0 {
		suffix := s[i+1:]
		s = s[:i]
		suffix = strings.TrimPrefix(suffix, "b")
		if n, err := strconv.Atoi(suffix); err == nil {
			build = n
			hasBuild = true
		}
	}

	parts := strings.Split(s, ".")
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			n = 0
		}
		nums = append(nums, n)
	}
	return Version{nums: nums, build: build, hasBuild: hasBuild}, nil
}

// Base returns the dotted numeric version without the build suffix
// (e.g. "2025.2.1.0-b141" → "2025.2.1.0"). Path components that key the public
// release DIRECTORY use this form.
func (v Version) Base() string {
	parts := make([]string, len(v.nums))
	for i, n := range v.nums {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ".")
}

// String returns the full version, re-appending "-bNNN" when a build suffix was
// present (e.g. "2025.2.1.0-b141"). The chart tarball, image tag, release import,
// and universe software version use this form.
func (v Version) String() string {
	if v.hasBuild {
		return fmt.Sprintf("%s-b%d", v.Base(), v.build)
	}
	return v.Base()
}

// Build returns the build number and whether a "-bNNN" suffix was present.
func (v Version) Build() (int, bool) { return v.build, v.hasBuild }

// BaseVersion parses s and returns its build-stripped base form, or "" if s is
// unparseable. Convenience for callers that only need the release-directory form.
func BaseVersion(s string) string {
	v, err := ParseVersion(s)
	if err != nil {
		return ""
	}
	return v.Base()
}

// Compare returns -1, 0, or 1 for a<b, a==b, a>b. Shorter version numbers are
// zero-padded; the build suffix is the least-significant component.
func (a Version) Compare(b Version) int {
	n := len(a.nums)
	if len(b.nums) > n {
		n = len(b.nums)
	}
	for i := 0; i < n; i++ {
		av, bv := 0, 0
		if i < len(a.nums) {
			av = a.nums[i]
		}
		if i < len(b.nums) {
			bv = b.nums[i]
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	switch {
	case a.build < b.build:
		return -1
	case a.build > b.build:
		return 1
	default:
		return 0
	}
}

// Compare parses two version strings and compares them. A parse error on either
// side yields an error and a 0 result.
func Compare(a, b string) (int, error) {
	av, err := ParseVersion(a)
	if err != nil {
		return 0, err
	}
	bv, err := ParseVersion(b)
	if err != nil {
		return 0, err
	}
	return av.Compare(bv), nil
}

// LooksLikeVersion reports whether s (after an optional leading "v") is a
// bare dotted-numeric version, optionally with a trailing "-bNNN" build
// suffix — e.g. "2.21.1.0", "2.21.1.0-b1", "18.4". It rejects free-form text
// (a prose SELECT version() response, an error message, an empty string).
// ParseVersion itself never errors on such text — non-numeric parts silently
// become 0 — so this syntactic gate is what a live version-detection result
// must pass before it is trusted, rather than corrupting Compare/Qualifies
// with a garbage parse of prose.
func LooksLikeVersion(s string) bool {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return false
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		suffix := strings.TrimPrefix(s[i+1:], "b")
		if _, err := strconv.Atoi(suffix); err != nil {
			return false
		}
		s = s[:i]
	}
	for _, p := range strings.Split(s, ".") {
		if _, err := strconv.Atoi(p); err != nil {
			return false
		}
	}
	return true
}

// ybVersionMarker matches the YugabyteDB build embedded in a YSQL `SELECT
// version()` response: YSQL reports "PostgreSQL <pgver>-YB-<yb version> on
// ..." (verified against yugabyte-db's
// TestPgSelect.java#testPgsqlVersion, which asserts the response matches
// "PostgreSQL.*-YB-.*"). The captured group runs to the next whitespace.
var ybVersionMarker = regexp.MustCompile(`-YB-(\S+)`)

// ExtractYBVersionMarker pulls the YugabyteDB version out of a raw `SELECT
// version()` string and reports whether a clean, Compare-able version was
// found. It returns ("", false) when the marker is absent (e.g. the
// connection is not a YugabyteDB YSQL endpoint) or the captured text is not
// itself a bare version (trailing punctuation swept in by the match).
func ExtractYBVersionMarker(raw string) (string, bool) {
	m := ybVersionMarker.FindStringSubmatch(raw)
	if m == nil {
		return "", false
	}
	v := strings.TrimRight(m[1], ",;")
	if !LooksLikeVersion(v) {
		return "", false
	}
	return v, true
}

// Qualifies reports whether detected versions satisfy the spec. When a detected
// version is empty (unknown), the corresponding bound is not enforced.
func (spec VersionSpec) Qualifies(d DetectedVersions) (bool, string) {
	if ok, reason := checkBound("product", d.Product, spec.MinProduct, spec.MaxProduct); !ok {
		return false, reason
	}
	if ok, reason := checkBound("yb", d.YB, spec.MinYB, spec.MaxYB); !ok {
		return false, reason
	}
	return true, ""
}

func checkBound(label, detected, min, max string) (bool, string) {
	if detected == "" {
		return true, ""
	}
	if min != "" {
		c, err := Compare(detected, min)
		if err == nil && c < 0 {
			return false, fmt.Sprintf("%s version %s < required min %s", label, detected, min)
		}
	}
	if max != "" {
		c, err := Compare(detected, max)
		if err == nil && c > 0 {
			return false, fmt.Sprintf("%s version %s > allowed max %s", label, detected, max)
		}
	}
	return true, ""
}
