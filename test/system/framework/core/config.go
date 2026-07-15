package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/yugabyte/yb-amp/test/system/framework/configs"
)

// Config is the merged, resolved configuration a run reads from. It is built by
// deep-merging layered YAML/JSON files (later wins) and then applying typed
// -o overrides. The resolved tree is recorded into every result.
type Config struct {
	root map[string]any
}

// NewConfig returns an empty config.
func NewConfig() *Config {
	return &Config{root: map[string]any{}}
}

// defaultBase is the parsed embedded configs/default.yaml — the canonical
// home for every framework default (architecture: "one place every default
// lives", ported from the stress framework's default.conf). Parsed once at
// package init; deep-merge only ever reads it, never mutates it in place
// (deepMerge always returns a fresh map), so sharing it across every
// LoadConfig call is safe.
var defaultBase = mustParseDefaultYAML()

func mustParseDefaultYAML() map[string]any {
	var m map[string]any
	if err := yaml.Unmarshal(configs.DefaultYAML, &m); err != nil {
		panic(fmt.Sprintf("parse embedded configs/default.yaml: %v", err))
	}
	if m == nil {
		m = map[string]any{}
	}
	return normalizeMap(m)
}

// LoadConfig builds a Config from, in order: the embedded shipped defaults
// (configs/default.yaml, layer zero), YAML/HOCON files (-c) deep-merged in
// order, JSON files (-jc) merged on top, then key=value overrides (-o) with
// typed prefixes. Use LoadConfigNoDefaults to skip the embedded layer
// (the --no-default-config escape hatch — debugging only).
func LoadConfig(configFiles, jsonFiles, overrides []string) (*Config, error) {
	return loadConfig(true, configFiles, jsonFiles, overrides)
}

// LoadConfigNoDefaults is LoadConfig without the embedded configs/default.yaml
// base layer.
func LoadConfigNoDefaults(configFiles, jsonFiles, overrides []string) (*Config, error) {
	return loadConfig(false, configFiles, jsonFiles, overrides)
}

func loadConfig(withDefaults bool, configFiles, jsonFiles, overrides []string) (*Config, error) {
	c := NewConfig()
	if withDefaults {
		// A genuine deep copy, not deepMerge(empty, defaultBase): that would
		// carry defaultBase's nested maps into c.root by reference (nothing to
		// merge them against yet), and Config.set mutates nested maps in
		// place — a later -o override would then permanently corrupt the
		// shared package-level defaultBase for every subsequent LoadConfig
		// call in the process.
		c.root = deepCopyMap(defaultBase)
	}
	for _, f := range configFiles {
		m, err := loadFile(f)
		if err != nil {
			return nil, err
		}
		c.root = deepMerge(c.root, m)
	}
	for _, f := range jsonFiles {
		m, err := loadJSONFile(f)
		if err != nil {
			return nil, err
		}
		c.root = deepMerge(c.root, m)
	}
	for _, o := range overrides {
		if err := c.applyOverride(o); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func loadFile(path string) (map[string]any, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".yaml", ".yml":
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		var m map[string]any
		if err := yaml.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("parse yaml %s: %w", path, err)
		}
		if m == nil {
			m = map[string]any{}
		}
		return normalizeMap(m), nil
	case ".conf", ".hocon":
		return nil, fmt.Errorf("HOCON support pending a vetted parser — use YAML (%s)", path)
	default:
		return nil, fmt.Errorf("unsupported config extension %q (%s); use .yaml/.yml or .json", ext, path)
	}
}

func loadJSONFile(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read json config %s: %w", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse json %s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// deepMerge merges src onto dst: maps merge recursively, scalars and lists
// replace. dst is not mutated.
func deepMerge(dst, src map[string]any) map[string]any {
	out := make(map[string]any, len(dst)+len(src))
	for k, v := range dst {
		out[k] = v
	}
	for k, sv := range src {
		if dv, ok := out[k]; ok {
			dm, dok := dv.(map[string]any)
			sm, sok := sv.(map[string]any)
			if dok && sok {
				out[k] = deepMerge(dm, sm)
				continue
			}
		}
		out[k] = sv
	}
	return out
}

// normalizeMap converts any map[any]any (older YAML shapes) into
// map[string]any recursively so lookups and JSON snapshots are uniform.
func normalizeMap(m map[string]any) map[string]any {
	for k, v := range m {
		m[k] = normalizeValue(v)
	}
	return m
}

func normalizeValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return normalizeMap(t)
	case map[any]any:
		nm := make(map[string]any, len(t))
		for k, vv := range t {
			nm[fmt.Sprint(k)] = normalizeValue(vv)
		}
		return nm
	case []any:
		for i := range t {
			t[i] = normalizeValue(t[i])
		}
		return t
	default:
		return v
	}
}

func (c *Config) applyOverride(o string) error {
	eq := strings.IndexByte(o, '=')
	if eq < 0 {
		return fmt.Errorf("override %q must be key=value", o)
	}
	key := o[:eq]
	raw := o[eq+1:]
	val, err := parseTypedValue(raw)
	if err != nil {
		return fmt.Errorf("override %q: %w", o, err)
	}
	c.set(key, val)
	return nil
}

func parseTypedValue(raw string) (any, error) {
	switch {
	case strings.HasPrefix(raw, "int:"):
		n, err := strconv.ParseInt(strings.TrimPrefix(raw, "int:"), 10, 64)
		if err != nil {
			return nil, err
		}
		return n, nil
	case strings.HasPrefix(raw, "float:"):
		f, err := strconv.ParseFloat(strings.TrimPrefix(raw, "float:"), 64)
		if err != nil {
			return nil, err
		}
		return f, nil
	case strings.HasPrefix(raw, "bool:"):
		b, err := strconv.ParseBool(strings.TrimPrefix(raw, "bool:"))
		if err != nil {
			return nil, err
		}
		return b, nil
	case strings.HasPrefix(raw, "list:"):
		items := strings.Split(strings.TrimPrefix(raw, "list:"), ",")
		out := make([]any, len(items))
		for i, s := range items {
			out[i] = s
		}
		return out, nil
	case strings.HasPrefix(raw, "json_file:"):
		data, err := os.ReadFile(strings.TrimPrefix(raw, "json_file:"))
		if err != nil {
			return nil, err
		}
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		return normalizeValue(v), nil
	case strings.HasPrefix(raw, "json_text:"):
		var v any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(raw, "json_text:")), &v); err != nil {
			return nil, err
		}
		return normalizeValue(v), nil
	default:
		return raw, nil
	}
}

// set writes val at a dotted path, creating intermediate maps.
func (c *Config) set(dottedKey string, val any) {
	parts := strings.Split(dottedKey, ".")
	m := c.root
	for i, p := range parts {
		if i == len(parts)-1 {
			m[p] = val
			return
		}
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
}

// lookup resolves a dotted path to a value.
func (c *Config) lookup(dottedKey string) (any, bool) {
	parts := strings.Split(dottedKey, ".")
	var cur any = c.root
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// GetString returns the string at key, or def.
func (c *Config) GetString(key, def string) string {
	v, ok := c.lookup(key)
	if !ok {
		return def
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// GetInt returns the int at key, or def. Numeric and numeric-string forms work.
func (c *Config) GetInt(key string, def int) int {
	v, ok := c.lookup(key)
	if !ok {
		return def
	}
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		if n, err := strconv.Atoi(t); err == nil {
			return n
		}
	}
	return def
}

// GetFloat returns the float64 at key, or def. Numeric and numeric-string
// forms work.
func (c *Config) GetFloat(key string, def float64) float64 {
	v, ok := c.lookup(key)
	if !ok {
		return def
	}
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		if f, err := strconv.ParseFloat(t, 64); err == nil {
			return f
		}
	}
	return def
}

// GetBool returns the bool at key, or def.
func (c *Config) GetBool(key string, def bool) bool {
	v, ok := c.lookup(key)
	if !ok {
		return def
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		if b, err := strconv.ParseBool(t); err == nil {
			return b
		}
	}
	return def
}

// GetDuration returns the duration at key, or def. Strings parse via
// time.ParseDuration; numbers are treated as seconds.
func (c *Config) GetDuration(key string, def time.Duration) time.Duration {
	v, ok := c.lookup(key)
	if !ok {
		return def
	}
	switch t := v.(type) {
	case string:
		if d, err := time.ParseDuration(t); err == nil {
			return d
		}
	case int:
		return time.Duration(t) * time.Second
	case int64:
		return time.Duration(t) * time.Second
	case float64:
		return time.Duration(t * float64(time.Second))
	}
	return def
}

// GetStringSlice returns a []string at key, or def.
func (c *Config) GetStringSlice(key string, def []string) []string {
	v, ok := c.lookup(key)
	if !ok {
		return def
	}
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, len(t))
		for i, e := range t {
			out[i] = fmt.Sprint(e)
		}
		return out
	}
	return def
}

// GetMap returns the nested map at key (a deep copy), or def. Used for
// free-form nested structures supplied via -c YAML, -jc JSON, or a json_text/
// json_file override (e.g. a raw universe-create spec).
func (c *Config) GetMap(key string, def map[string]any) map[string]any {
	v, ok := c.lookup(key)
	if !ok {
		return def
	}
	if m, ok := v.(map[string]any); ok {
		return deepCopyMap(m)
	}
	return def
}

// GetMapSlice returns a []map[string]any at key (each element deep-copied), or
// def. Used for free-form LIST-of-maps structures (e.g. a provider's regions →
// zones), the list analogue of GetMap. Non-map elements are skipped.
func (c *Config) GetMapSlice(key string, def []map[string]any) []map[string]any {
	v, ok := c.lookup(key)
	if !ok {
		return def
	}
	s, ok := v.([]any)
	if !ok {
		return def
	}
	out := make([]map[string]any, 0, len(s))
	for _, e := range s {
		if m, ok := e.(map[string]any); ok {
			out = append(out, deepCopyMap(m))
		}
	}
	return out
}

// Derive returns a deep-copied Config with the given key=value overrides
// applied on top, leaving the receiver untouched. Values parse with the same
// typed prefixes as -o overrides (a bare string stays a string), so a plain
// image ref sets cleanly. It is how a perf comparison builds an ISOLATED
// per-variant config — each variant resolves its subsystem Configs (apps.ybk8s,
// env.ampstack) from its own derived tree, never by mutating (or globally
// re-resolving) the shared config. Overrides apply in sorted-key order so the
// result is deterministic.
func (c *Config) Derive(overrides map[string]string) *Config {
	d := &Config{root: deepCopyMap(c.root)}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, err := parseTypedValue(overrides[k])
		if err != nil {
			v = overrides[k]
		}
		d.set(k, v)
	}
	return d
}

// Resolved returns a deep copy of the merged tree for recording into a result.
func (c *Config) Resolved() map[string]any {
	return deepCopyMap(c.root)
}

func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		cp := make([]any, len(t))
		for i, e := range t {
			cp[i] = deepCopyValue(e)
		}
		return cp
	default:
		return v
	}
}
