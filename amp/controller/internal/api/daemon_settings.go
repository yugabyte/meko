package api

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
)

// daemonConfigKeysJSON is the override key set the two daemons accept, generated
// from their own Rust config types by `dump-config-keys`. CI regenerates and diffs
// it, so it cannot drift from them silently.
//
//go:embed daemon_config_keys.json
var daemonConfigKeysJSON []byte

// daemonKeys is that file parsed: per daemon, the dotted paths it accepts and the
// toml scalar type each takes.
type daemonKeys struct {
	Protected   []string          `json:"protected"`
	Pageserver  map[string]string `json:"pageserver"`
	Safekeeper  map[string]string `json:"safekeeper"`
	protectedBy map[string]bool
}

var (
	loadKeysOnce sync.Once
	loadedKeys   *daemonKeys
	loadKeysErr  error
)

func configKeys() (*daemonKeys, error) {
	loadKeysOnce.Do(func() {
		k := &daemonKeys{}
		if err := json.Unmarshal(daemonConfigKeysJSON, k); err != nil {
			loadKeysErr = fmt.Errorf("parse daemon_config_keys.json: %w", err)
			return
		}
		if len(k.Pageserver) == 0 || len(k.Safekeeper) == 0 {
			loadKeysErr = fmt.Errorf("daemon_config_keys.json has an empty key set")
			return
		}
		k.protectedBy = make(map[string]bool, len(k.Protected))
		for _, p := range k.Protected {
			k.protectedBy[p] = true
		}
		loadedKeys = k
	})
	return loadedKeys, loadKeysErr
}

// daemon names as they appear in the generated file and in error messages.
const (
	daemonPageserver = "pageserver"
	daemonSafekeeper = "safekeeper"
)

// validateDaemonSettings checks one daemon's override map against the generated
// key set, returning a message suitable for a 400. A nil or empty map is valid.
//
// It has to happen here rather than at the pod: a bad key either stops the
// pageserver from starting — a crash loop whose fix lives in a row it cannot boot
// to read — or, if misspelled under a real table, is dropped with no error
// anywhere. Request time is the only place either is cheap.
func validateDaemonSettings(daemon string, settings map[string]any) error {
	if len(settings) == 0 {
		return nil
	}
	keys, err := configKeys()
	if err != nil {
		return err
	}
	var known map[string]string
	switch daemon {
	case daemonPageserver:
		known = keys.Pageserver
	case daemonSafekeeper:
		known = keys.Safekeeper
	default:
		return fmt.Errorf("unknown daemon %q", daemon)
	}

	// Sorted so the same bad request always names the same key.
	names := make([]string, 0, len(settings))
	for name := range settings {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if err := validateDaemonSetting(daemon, name, settings[name], known, keys.protectedBy); err != nil {
			return err
		}
	}
	return nil
}

func validateDaemonSetting(daemon, name string, value any, known map[string]string, protected map[string]bool) error {
	if name == "" {
		return fmt.Errorf("%s_settings: empty key", daemon)
	}
	// First segment, matching the supervisor. Before the known-key lookup so the
	// reason given is the useful one.
	root, _, _ := strings.Cut(name, ".")
	if protected[root] {
		return fmt.Errorf("%s_settings: %q is an identity/wiring key and cannot be overridden", daemon, name)
	}
	if slices.Contains(strings.Split(name, "."), "") {
		return fmt.Errorf("%s_settings: %q has an empty path segment", daemon, name)
	}

	// Shape before name: a nested value is a structural mistake, and "unknown key"
	// would send the caller hunting a typo they do not have.
	switch value.(type) {
	case nil, bool, string, float64, json.Number:
	default:
		return fmt.Errorf(
			"%s_settings: %q must be a scalar (boolean, number or string); "+
				"address a nested toml table with a dotted key, e.g. %q",
			daemon, name, name+".enable_gc")
	}

	want, ok := known[name]
	if !ok {
		return fmt.Errorf("%s_settings: unknown key %q%s", daemon, name, didYouMean(name, known))
	}
	return checkScalarType(daemon, name, value, want)
}

// checkScalarType rejects a value the daemon's config type would not accept.
//
// Type matters as much as name: hand-written JSON produces `"scale_factor": "8"`
// easily, and a quoted number fails the supervisor's render, so the pod comes up
// on derived defaults with the override silently absent.
func checkScalarType(daemon, name string, value any, want string) error {
	bad := func(got string) error {
		return fmt.Errorf("%s_settings: %q takes a %s, got %s", daemon, name, want, got)
	}
	switch v := value.(type) {
	case nil:
		return bad("null")
	case bool:
		if want != "boolean" {
			return bad("a boolean")
		}
	case string:
		if want != "string" {
			return bad("a string")
		}
	case float64:
		// encoding/json decodes every JSON number as float64, so "is this an
		// integer" is a question about the value, not the Go type.
		switch want {
		case "float":
		case "integer":
			if v != float64(int64(v)) {
				return bad("a fractional number")
			}
		default:
			return bad("a number")
		}
	case json.Number:
		// Present if a caller decodes with UseNumber; treat it by shape.
		if want != "integer" && want != "float" {
			return bad("a number")
		}
		if want == "integer" && strings.ContainsAny(v.String(), ".eE") {
			return bad("a fractional number")
		}
	default:
		// Unreachable: validateDaemonSetting rejects every non-scalar shape
		// before calling this, with a message about where the nesting goes.
		return bad("a non-scalar value")
	}
	return nil
}

// didYouMean suggests the dotted form when the caller gave a flat key whose only
// valid spelling is nested — the mistake this convention invites.
func didYouMean(name string, known map[string]string) string {
	suffix := "." + name
	var hits []string
	for candidate := range known {
		if strings.HasSuffix(candidate, suffix) {
			hits = append(hits, candidate)
		}
	}
	if len(hits) == 0 {
		return ""
	}
	sort.Strings(hits)
	quoted := make([]string, len(hits))
	for i, h := range hits {
		quoted[i] = fmt.Sprintf("%q", h)
	}
	return fmt.Sprintf(" (did you mean %s?)", strings.Join(quoted, " or "))
}
