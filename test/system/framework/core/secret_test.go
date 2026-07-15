package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secretsFromYAMLish builds a Config carrying a secrets: section from a plain
// map, mirroring what LoadConfig would produce, without a YAML round-trip.
func secretsConfig(t *testing.T, decls map[string]any) *Config {
	t.Helper()
	c := NewConfig()
	c.root["secrets"] = decls
	return c
}

func TestSecretsTokenResolution(t *testing.T) {
	cfg := secretsConfig(t, map[string]any{
		"inline": map[string]any{"token": "s3cr3t-value"},
	})
	s, err := NewSecrets(cfg)
	if err != nil {
		t.Fatalf("NewSecrets: %v", err)
	}
	if v, err := s.Value("inline"); err != nil || v != "s3cr3t-value" {
		t.Errorf("Value(inline) = %q, %v; want the literal", v, err)
	}
	// A token has no filesystem path.
	if _, err := s.Path("inline"); err == nil {
		t.Errorf("Path(inline) on a token secret should error")
	}
	if v, ok, err := s.Get("inline"); !ok || err != nil || v != "s3cr3t-value" {
		t.Errorf("Get(inline) = %q, ok=%v, err=%v", v, ok, err)
	}
}

func TestSecretsEnvResolution(t *testing.T) {
	cfg := secretsConfig(t, map[string]any{
		"tok": map[string]any{"env": "AMPQA_TEST_TOKEN"},
	})
	s, err := NewSecrets(cfg)
	if err != nil {
		t.Fatalf("NewSecrets: %v", err)
	}

	// Unset env var → a clear error, not an empty value.
	os.Unsetenv("AMPQA_TEST_TOKEN")
	if _, err := s.Value("tok"); err == nil {
		t.Errorf("Value on an unset env secret should error")
	}

	// Set → the env var's value.
	t.Setenv("AMPQA_TEST_TOKEN", "env-token")
	if v, err := s.Value("tok"); err != nil || v != "env-token" {
		t.Errorf("Value(tok) = %q, %v; want env-token", v, err)
	}
	// Path on an env secret is the env var value (a path-holding env var).
	if p, err := s.Path("tok"); err != nil || p != "env-token" {
		t.Errorf("Path(tok) = %q, %v; want the env var value", p, err)
	}

	// Present but empty is allowed (empty-ok), distinct from unset.
	t.Setenv("AMPQA_TEST_TOKEN", "")
	if v, err := s.Value("tok"); err != nil || v != "" {
		t.Errorf("Value(tok) on empty env = %q, %v; want empty, no error", v, err)
	}
}

func TestSecretsFileResolution(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.json")
	contents := `{"type":"service_account","private_key":"XYZ"}`
	if err := os.WriteFile(keyPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := secretsConfig(t, map[string]any{
		"gcp_key": map[string]any{"file": keyPath},
	})
	s, err := NewSecrets(cfg)
	if err != nil {
		t.Fatalf("NewSecrets: %v", err)
	}
	// Value = file CONTENTS.
	if v, err := s.Value("gcp_key"); err != nil || v != contents {
		t.Errorf("Value(gcp_key) = %q, %v; want the file contents", v, err)
	}
	// Path = the file PATH.
	if p, err := s.Path("gcp_key"); err != nil || p != keyPath {
		t.Errorf("Path(gcp_key) = %q, %v; want %q", p, err, keyPath)
	}

	// Unreadable file → error.
	bad := secretsConfig(t, map[string]any{
		"missing": map[string]any{"file": filepath.Join(dir, "nope.json")},
	})
	sb, _ := NewSecrets(bad)
	if _, err := sb.Value("missing"); err == nil {
		t.Errorf("Value on an unreadable file should error")
	}
}

func TestSecretsErrorCases(t *testing.T) {
	// Multiple variants set.
	if _, err := NewSecrets(secretsConfig(t, map[string]any{
		"bad": map[string]any{"token": "x", "env": "Y"},
	})); err == nil {
		t.Errorf("multiple variants should error at NewSecrets")
	}
	// Zero variants set.
	if _, err := NewSecrets(secretsConfig(t, map[string]any{
		"empty": map[string]any{},
	})); err == nil {
		t.Errorf("zero variants should error at NewSecrets")
	}
	// Unknown secret name.
	s, _ := NewSecrets(secretsConfig(t, map[string]any{
		"known": map[string]any{"token": "x"},
	}))
	if _, err := s.Value("nope"); err == nil {
		t.Errorf("Value on an unknown name should error")
	}
	if _, ok, err := s.Get("nope"); ok || err != nil {
		t.Errorf("Get on an unknown name = ok=%v err=%v; want ok=false, nil", ok, err)
	}
}

func TestSecretsAbsentSection(t *testing.T) {
	s, err := NewSecrets(NewConfig())
	if err != nil {
		t.Fatalf("NewSecrets on empty config: %v", err)
	}
	if len(s.Names()) != 0 {
		t.Errorf("empty config should yield no secrets, got %v", s.Names())
	}
}

func TestMaskerReplacesValuesOnly(t *testing.T) {
	m := NewMasker("supersecret", "aws-key-abc", "")
	got := m.Mask("token=supersecret and key=aws-key-abc but user=alice")
	if strings.Contains(got, "supersecret") || strings.Contains(got, "aws-key-abc") {
		t.Errorf("secret survived masking: %q", got)
	}
	if !strings.Contains(got, "user=alice") {
		t.Errorf("non-secret text was altered: %q", got)
	}
	if strings.Count(got, MaskWith) != 2 {
		t.Errorf("want two masks, got %q", got)
	}
	// Nil and empty maskers are no-ops.
	var nilM *Masker
	if nilM.Mask("supersecret") != "supersecret" {
		t.Errorf("nil masker should be a no-op")
	}
	if NewMasker().Mask("supersecret") != "supersecret" {
		t.Errorf("empty masker should be a no-op")
	}
}

func TestMaskerFromSecretsAndLogf(t *testing.T) {
	t.Setenv("AMPQA_MASK_ENV", "leaky-token-123")
	cfg := secretsConfig(t, map[string]any{
		"a": map[string]any{"env": "AMPQA_MASK_ENV"},
		"b": map[string]any{"token": "inline-pw"},
		"c": map[string]any{"env": "AMPQA_UNSET_ENV"}, // never set → contributes nothing
	})
	s, _ := NewSecrets(cfg)
	m := NewMaskerFromSecrets(s)

	var captured string
	logf := m.Logf(func(format string, args ...any) {
		captured = format
		if len(args) > 0 {
			captured = args[0].(string)
		}
	})
	logf("connecting with %s / %s", "leaky-token-123", "inline-pw")
	if strings.Contains(captured, "leaky-token-123") || strings.Contains(captured, "inline-pw") {
		t.Errorf("Logf leaked a secret: %q", captured)
	}
}

func TestSanitizeConfigSnapshotRedactsSecrets(t *testing.T) {
	t.Setenv("AMPQA_SNAP_ENV", "env-secret-val")
	root := map[string]any{
		"secrets": map[string]any{
			"tok":  map[string]any{"token": "literal-secret"},
			"envv": map[string]any{"env": "AMPQA_SNAP_ENV"},
		},
		// A secret value leaked into an unrelated config key must be masked too.
		"apps": map[string]any{"cloud": map[string]any{"note": "used env-secret-val here"}},
	}
	m := NewMasker("literal-secret", "env-secret-val")
	sanitizeConfigSnapshot(root, m)

	sec, ok := root["secrets"].(map[string]any)
	if !ok {
		t.Fatalf("secrets subtree not redacted to a map: %T", root["secrets"])
	}
	// Redacted subtree holds variant names, never values.
	if sec["tok"] != "token" || sec["envv"] != "env" {
		t.Errorf("redacted secrets = %v; want variant names only", sec)
	}
	// The literal token must NOT survive anywhere in the snapshot.
	flat := flatten(root)
	if strings.Contains(flat, "literal-secret") {
		t.Errorf("literal token survived in snapshot: %s", flat)
	}
	if strings.Contains(flat, "env-secret-val") {
		t.Errorf("leaked env secret survived in snapshot: %s", flat)
	}
	note := root["apps"].(map[string]any)["cloud"].(map[string]any)["note"].(string)
	if !strings.Contains(note, MaskWith) {
		t.Errorf("leaked secret in a config value was not masked: %q", note)
	}
}

func TestFinishResultMasksErrorSummaryAndSteps(t *testing.T) {
	m := NewMasker("db-password-xyz")
	r := &Result{
		ErrorSummary: `[XX000] auth failed with password "db-password-xyz"`,
		SkipReason:   "",
		Steps: []*Step{
			{Name: "connect db-password-xyz", Properties: map[string]any{"pw": "db-password-xyz", "n": 3}},
		},
	}
	// nil sink: we only assert the in-place masking of the recorded result.
	finishResult(nil, "t", r, m)

	if strings.Contains(r.ErrorSummary, "db-password-xyz") {
		t.Errorf("ErrorSummary leaked a secret: %q", r.ErrorSummary)
	}
	if strings.Contains(r.Steps[0].Name, "db-password-xyz") {
		t.Errorf("step name leaked a secret: %q", r.Steps[0].Name)
	}
	if r.Steps[0].Properties["pw"] != MaskWith {
		t.Errorf("step property leaked a secret: %v", r.Steps[0].Properties["pw"])
	}
	if r.Steps[0].Properties["n"] != 3 {
		t.Errorf("non-string step property should be untouched, got %v", r.Steps[0].Properties["n"])
	}
}

// flatten renders every string value nested in v into one blob, so a test can
// assert a secret appears nowhere in a config snapshot.
func flatten(v any) string {
	var b strings.Builder
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			for k, e := range t {
				b.WriteString(k + " ")
				walk(e)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		case string:
			b.WriteString(t + " ")
		}
	}
	walk(v)
	return b.String()
}
