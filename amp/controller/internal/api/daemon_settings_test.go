package api

import (
	"strings"
	"testing"
)

// If the generated file fails to load, every override request 500s — so fail
// loudly here rather than discovering it from one.
func TestDaemonConfigKeysLoad(t *testing.T) {
	keys, err := configKeys()
	if err != nil {
		t.Fatalf("configKeys: %v", err)
	}
	// The keys the GC experiment needs. An upstream rename should surface here,
	// not in a perf run that silently could not set one.
	for _, want := range []string{
		"tenant_config.enable_gc",
		"tenant_config.gc_horizon",
		"tenant_config.gc_period",
		"tenant_config.pitr_interval",
	} {
		if _, ok := keys.Pageserver[want]; !ok {
			t.Errorf("pageserver key %q missing from the generated set", want)
		}
	}
	if _, ok := keys.Safekeeper["wal_cache_size_mib"]; !ok {
		t.Error("safekeeper key wal_cache_size_mib missing from the generated set")
	}
	// The deny-list has to arrive too, or protection silently stops applying.
	if !keys.protectedBy["branch_id"] {
		t.Error("branch_id is not in the protected set")
	}
}

func TestValidateDaemonSettingsAccepts(t *testing.T) {
	for name, settings := range map[string]map[string]any{
		"nil":            nil,
		"empty":          {},
		"dotted bool":    {"tenant_config.enable_gc": true},
		"dotted string":  {"tenant_config.pitr_interval": "7 days"},
		"integer":        {"tenant_config.gc_horizon": float64(1024)},
		"float":          {"scale_factor": 8.5},
		"top-level bool": {"dev_mode": true},
		"several at once": {
			"tenant_config.enable_gc":     true,
			"tenant_config.pitr_interval": "0s",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateDaemonSettings(daemonPageserver, settings); err != nil {
				t.Errorf("validateDaemonSettings(%v) = %v, want nil", settings, err)
			}
		})
	}
}

func TestValidateDaemonSettingsRejects(t *testing.T) {
	cases := map[string]struct {
		daemon   string
		settings map[string]any
		contains string
	}{
		// The motivating mistake: the flat spelling of a nested key. The
		// pageserver would refuse to start on it, so the message has to point at
		// the dotted form rather than just saying "unknown".
		"flat spelling of a nested key": {
			daemonPageserver,
			map[string]any{"enable_gc": true},
			`did you mean "tenant_config.enable_gc"`,
		},
		"typo": {
			daemonPageserver,
			map[string]any{"tenant_config.enabel_gc": true},
			`unknown key "tenant_config.enabel_gc"`,
		},
		// Wrong types are what hand-written jsonb produces most easily, and the
		// supervisor would fail the render rather than apply them.
		"quoted number": {
			daemonPageserver,
			map[string]any{"scale_factor": "8"},
			`takes a float, got a string`,
		},
		"fractional integer": {
			daemonPageserver,
			map[string]any{"tenant_config.gc_horizon": 1.5},
			`takes a integer, got a fractional number`,
		},
		"bool for a string": {
			daemonPageserver,
			map[string]any{"tenant_config.pitr_interval": true},
			`takes a string, got a boolean`,
		},
		"null": {
			daemonPageserver,
			map[string]any{"tenant_config.enable_gc": nil},
			`got null`,
		},
		// Nesting belongs in the key. A nested VALUE is what someone reaches for
		// first, so the error says where the nesting goes.
		"nested object as a value": {
			daemonPageserver,
			map[string]any{"tenant_config": map[string]any{"enable_gc": true}},
			"address a nested toml table with a dotted key",
		},
		"protected key": {
			daemonPageserver,
			map[string]any{"branch_id": "someone-elses"},
			"identity/wiring key",
		},
		// Protection is decided on the first segment, so this cannot be smuggled
		// past by nesting it.
		"protected root of a dotted path": {
			daemonPageserver,
			map[string]any{"branch_id.sneaky": "x"},
			"identity/wiring key",
		},
		"empty segment": {
			daemonPageserver,
			map[string]any{"tenant_config..enable_gc": true},
			"empty path segment",
		},
		"empty key": {
			daemonPageserver,
			map[string]any{"": true},
			"empty key",
		},
		// The two daemons have separate config surfaces; a pageserver key is not
		// silently accepted into safekeeper.toml, which denies unknown fields and
		// would fail the render.
		"pageserver key on the safekeeper": {
			daemonSafekeeper,
			map[string]any{"tenant_config.enable_gc": true},
			`unknown key "tenant_config.enable_gc"`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateDaemonSettings(tc.daemon, tc.settings)
			if err == nil {
				t.Fatalf("validateDaemonSettings(%v) = nil, want an error", tc.settings)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("error = %q, want it to contain %q", err, tc.contains)
			}
		})
	}
}

// Map iteration order would otherwise make the reported key vary between
// identical calls.
func TestValidateDaemonSettingsIsDeterministic(t *testing.T) {
	settings := map[string]any{
		"zzz_unknown": true,
		"aaa_unknown": true,
		"mmm_unknown": true,
	}
	first := validateDaemonSettings(daemonPageserver, settings)
	if first == nil {
		t.Fatal("expected an error")
	}
	for range 20 {
		if got := validateDaemonSettings(daemonPageserver, settings); got.Error() != first.Error() {
			t.Fatalf("error varies between calls: %q then %q", first, got)
		}
	}
	if !strings.Contains(first.Error(), "aaa_unknown") {
		t.Errorf("error = %q, want the lexicographically first key", first)
	}
}
