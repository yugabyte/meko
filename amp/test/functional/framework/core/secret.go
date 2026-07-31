package core

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SecretVariant is how a named secret supplies its value.
type SecretVariant string

const (
	// SecretToken is an inline literal value (token: "<literal>").
	SecretToken SecretVariant = "token"
	// SecretFile is a path to a file whose CONTENTS are the value
	// (file: /path/to/key.json) — a key, cert, or service-account JSON.
	SecretFile SecretVariant = "file"
	// SecretEnv is the NAME of an environment variable read at resolve time
	// (env: GH_PACKAGE_TOKEN). The env var's value is the secret; for a "path"
	// secret the env var holds a filesystem path (e.g. GCP_KEY=/path/to.json),
	// reached via Path.
	SecretEnv SecretVariant = "env"
)

// Secret is one declared credential. Exactly one variant is populated; ref is
// the literal (token), the file path (file), or the env-var name (env).
type Secret struct {
	Name    string
	Variant SecretVariant
	ref     string
}

// Secrets resolves credentials declared by name in the config `secrets:`
// section. Each named secret supplies its value one of three ways — token
// (inline literal), file (contents of a file), env (value of a named env var,
// read at resolve time). Values are resolved on demand and never cached, so an
// env var or file changed between resolves is picked up.
//
// Value vs Path — the load-bearing distinction:
//   - Value(name) returns the secret's VALUE: the literal (token), the file's
//     CONTENTS (file), or the env var's value (env). This is what a bearer
//     token / key material / password is.
//   - Path(name) returns a filesystem PATH: the file path (file), or the env
//     var's value interpreted as a path (env, e.g. GCP_KEY=/path/to.json).
//     A token secret has no path, so Path errors on it.
//
// Use Value for a GitHub token or an inline password; use Path when a caller
// needs the file on disk (an SSH private-key path, a GCP service-account JSON
// path handed to a CLI).
type Secrets struct {
	byName map[string]Secret
}

// NewSecrets parses and validates the config `secrets:` section. An absent or
// empty section yields an empty resolver (not an error). Each entry is either
// the string shorthand "<variant>:<ref>" (e.g. env:QUAY_TOKEN, file:/path,
// token:literal) — the natural -o form, `-o secrets.foo=env:BAR` — or a map with
// EXACTLY ONE of token/file/env populated (the YAML-file form). Anything else is
// a clear configuration error surfaced here rather than at point of use.
func NewSecrets(cfg *Config) (*Secrets, error) {
	s := &Secrets{byName: map[string]Secret{}}
	if cfg == nil {
		return s, nil
	}
	raw, ok := cfg.lookup("secrets")
	if !ok || raw == nil {
		return s, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("secrets: expected a map, got %T", raw)
	}
	for name, decl := range m {
		sec, err := parseSecret(name, decl)
		if err != nil {
			return nil, err
		}
		s.byName[name] = sec
	}
	return s, nil
}

func parseSecret(name string, decl any) (Secret, error) {
	// String shorthand: "<variant>:<ref>" — e.g. env:QUAY_TOKEN, file:/path/key.json,
	// token:literal. This is the -o form (a nested map can't be expressed on the
	// CLI): `-o secrets.foo=env:BAR` is far cleaner than the json_text hack. The
	// map form below stays valid for YAML config files.
	if str, ok := decl.(string); ok {
		return parseSecretShorthand(name, str)
	}
	d, ok := decl.(map[string]any)
	if !ok {
		return Secret{}, fmt.Errorf("secret %q: expected \"<variant>:<ref>\" (e.g. env:MY_VAR) or a map with one of token/file/env, got %T", name, decl)
	}
	var set []SecretVariant
	var ref string
	for _, v := range []SecretVariant{SecretToken, SecretFile, SecretEnv} {
		if raw, present := d[string(v)]; present {
			str, ok := raw.(string)
			if !ok {
				return Secret{}, fmt.Errorf("secret %q: %s must be a string, got %T", name, v, raw)
			}
			if str == "" {
				continue
			}
			set = append(set, v)
			ref = str
		}
	}
	switch len(set) {
	case 0:
		return Secret{}, fmt.Errorf("secret %q: exactly one of token/file/env must be set (none were)", name)
	case 1:
		return Secret{Name: name, Variant: set[0], ref: ref}, nil
	default:
		return Secret{}, fmt.Errorf("secret %q: exactly one of token/file/env must be set (got %v)", name, set)
	}
}

// parseSecretShorthand parses the "<variant>:<ref>" string form. The variant is
// the part before the FIRST colon (so a token/ref value may itself contain
// colons); the ref is everything after it.
func parseSecretShorthand(name, s string) (Secret, error) {
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return Secret{}, fmt.Errorf("secret %q: shorthand must be \"<variant>:<ref>\" with variant token/file/env (e.g. env:MY_VAR), got %q", name, s)
	}
	variant := SecretVariant(s[:i])
	ref := s[i+1:]
	switch variant {
	case SecretToken, SecretFile, SecretEnv:
	default:
		return Secret{}, fmt.Errorf("secret %q: unknown source %q — want token:, file:, or env:", name, s[:i])
	}
	if ref == "" {
		return Secret{}, fmt.Errorf("secret %q: %s: has an empty value", name, variant)
	}
	return Secret{Name: name, Variant: variant, ref: ref}, nil
}

// Names lists the declared secret names, sorted.
func (s *Secrets) Names() []string {
	out := make([]string, 0, len(s.byName))
	for n := range s.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Get resolves the named secret's VALUE leniently: ok is false (err nil) when
// no secret of that name is declared, so a caller that treats a missing secret
// as "use the default / ambient path" can do so. A declared secret that fails
// to resolve (env var unset, file unreadable) returns ok true and a non-nil
// err. See the Secrets doc for Value vs Path.
func (s *Secrets) Get(name string) (value string, ok bool, err error) {
	sec, declared := s.byName[name]
	if !declared {
		return "", false, nil
	}
	v, err := resolveValue(sec)
	return v, true, err
}

// Value resolves the named secret's VALUE strictly: an unknown name is an
// error (as opposed to Get, which reports it via ok=false).
func (s *Secrets) Value(name string) (string, error) {
	sec, ok := s.byName[name]
	if !ok {
		return "", fmt.Errorf("unknown secret %q (not declared under secrets:)", name)
	}
	return resolveValue(sec)
}

// Path resolves the named secret to a filesystem PATH: the file path (file
// variant) or the env var's value as a path (env variant). A token secret has
// no path and is an error. An unknown name is an error.
func (s *Secrets) Path(name string) (string, error) {
	sec, ok := s.byName[name]
	if !ok {
		return "", fmt.Errorf("unknown secret %q (not declared under secrets:)", name)
	}
	switch sec.Variant {
	case SecretFile:
		return expandHome(sec.ref), nil
	case SecretEnv:
		v, ok := os.LookupEnv(sec.ref)
		if !ok {
			return "", fmt.Errorf("secret %q: env var %s is not set", name, sec.ref)
		}
		return v, nil
	default: // SecretToken
		return "", fmt.Errorf("secret %q is a token; it has no file path (use Value)", name)
	}
}

func resolveValue(sec Secret) (string, error) {
	switch sec.Variant {
	case SecretToken:
		return sec.ref, nil
	case SecretEnv:
		v, ok := os.LookupEnv(sec.ref)
		if !ok {
			return "", fmt.Errorf("secret %q: env var %s is not set", sec.Name, sec.ref)
		}
		return v, nil
	case SecretFile:
		data, err := os.ReadFile(expandHome(sec.ref))
		if err != nil {
			return "", fmt.Errorf("secret %q: read file: %w", sec.Name, err)
		}
		return string(data), nil
	default:
		return "", fmt.Errorf("secret %q: unknown variant %q", sec.Name, sec.Variant)
	}
}

// expandHome expands a leading ~ to the user's home directory. On failure it
// returns the path unchanged (os.ReadFile then surfaces the real error).
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// --- masking ---

// Masker replaces known secret VALUES with a fixed mask wherever they appear in
// a string, so a resolved credential never survives into a log line, an error
// summary, or a recorded config snapshot. It is built once from the run's
// resolved secret values and consulted at every framework emission boundary
// (reporter result recording, the config snapshot, the app logf wrapper).
//
// Scope (deliberately bounded, per AGENTS.md §7 KISS): the Masker covers the
// framework's OWN emission points. It does not intercept arbitrary third-party
// or stdlib log output — a caller that wants that wraps its logger via Logf.
// A nil *Masker is valid and a no-op, so callers need not nil-check.
type Masker struct {
	// values sorted longest-first so overlapping secrets mask fully.
	values []string
}

// MaskWith is the replacement written in place of a secret value.
const MaskWith = "***"

// NewMasker builds a Masker over the given secret values. Empty and duplicate
// values are dropped; the rest are sorted longest-first so a value that
// contains another still masks completely.
func NewMasker(values ...string) *Masker {
	seen := map[string]struct{}{}
	var uniq []string
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		uniq = append(uniq, v)
	}
	sort.SliceStable(uniq, func(i, j int) bool { return len(uniq[i]) > len(uniq[j]) })
	return &Masker{values: uniq}
}

// NewMaskerFromSecrets resolves every declared secret best-effort and builds a
// Masker over the values that resolve. Secrets that fail to resolve (env unset,
// file unreadable) contribute nothing — there is no value to leak. Resolution
// errors are intentionally swallowed here: this is the masking-safety path, not
// the use path (a genuinely-needed secret errors clearly at its point of use).
func NewMaskerFromSecrets(s *Secrets) *Masker {
	if s == nil {
		return NewMasker()
	}
	var vals []string
	for _, name := range s.Names() {
		if v, err := s.Value(name); err == nil && v != "" {
			vals = append(vals, v)
		}
	}
	return NewMasker(vals...)
}

// Mask returns s with every known secret value replaced by MaskWith. A nil
// Masker, an empty Masker, or a string containing no secret is returned
// unchanged.
func (m *Masker) Mask(s string) string {
	if m == nil || len(m.values) == 0 || s == "" {
		return s
	}
	for _, v := range m.values {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, MaskWith)
		}
	}
	return s
}

// Logf wraps a printf-style log sink so every formatted line is masked before
// it is written. This is how the app `logf` convention (apps/*.logf,
// env/*.logf) is covered: pass masker.Logf(log.Printf) as the sink. A nil
// Masker returns base unchanged.
func (m *Masker) Logf(base func(format string, args ...any)) func(format string, args ...any) {
	if m == nil || len(m.values) == 0 || base == nil {
		return base
	}
	return func(format string, args ...any) {
		base("%s", m.Mask(fmt.Sprintf(format, args...)))
	}
}

// maskValuesInPlace masks every string value nested in v (maps and slices
// walked recursively). Used on the resolved-config snapshot recorded into a
// result. A nil/empty Masker is a no-op.
func (m *Masker) maskValuesInPlace(v any) {
	if m == nil || len(m.values) == 0 {
		return
	}
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			if s, ok := e.(string); ok {
				t[k] = m.Mask(s)
			} else {
				m.maskValuesInPlace(e)
			}
		}
	case []any:
		for i, e := range t {
			if s, ok := e.(string); ok {
				t[i] = m.Mask(s)
			} else {
				m.maskValuesInPlace(e)
			}
		}
	}
}

// sanitizeConfigSnapshot prepares a resolved-config snapshot for recording into
// a result: it REDACTS the secrets subtree (replacing each declaration with its
// variant name only — never a value, since a token declaration holds the literal
// inline) and then masks any secret value that leaked into the rest of the tree.
// root is mutated in place; callers pass a fresh deep copy (Config.Resolved()).
func sanitizeConfigSnapshot(root map[string]any, m *Masker) {
	if root == nil {
		return
	}
	redactSecretsSubtree(root)
	m.maskValuesInPlace(root)
}

func redactSecretsSubtree(root map[string]any) {
	raw, present := root["secrets"]
	if !present {
		return
	}
	m, ok := raw.(map[string]any)
	if !ok {
		root["secrets"] = MaskWith
		return
	}
	red := make(map[string]any, len(m))
	for name, decl := range m {
		red[name] = declaredVariant(decl)
	}
	root["secrets"] = red
}

// declaredVariant reports a declaration's variant name without exposing any
// value — for the redacted snapshot ("gh_package": "env").
func declaredVariant(decl any) string {
	// String shorthand "<variant>:<ref>": report the variant only, never the ref
	// (a token: shorthand carries the literal inline).
	if s, ok := decl.(string); ok {
		if i := strings.IndexByte(s, ':'); i >= 0 {
			switch v := SecretVariant(s[:i]); v {
			case SecretToken, SecretFile, SecretEnv:
				return string(v)
			}
		}
		return "redacted"
	}
	d, ok := decl.(map[string]any)
	if !ok {
		return "redacted"
	}
	for _, v := range []SecretVariant{SecretToken, SecretFile, SecretEnv} {
		if s, ok := d[string(v)].(string); ok && s != "" {
			return string(v)
		}
	}
	return "redacted"
}
