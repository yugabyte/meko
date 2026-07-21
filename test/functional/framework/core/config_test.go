package core_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestConfigMultiFileMergePrecedence(t *testing.T) {
	dir := t.TempDir()
	base := writeFile(t, dir, "base.yaml", "a: 1\nnested:\n  x: 1\n  y: 2\nlist:\n  - a\n  - b\n")
	over := writeFile(t, dir, "over.yaml", "a: 2\nnested:\n  y: 20\n  z: 30\nlist:\n  - c\n")

	cfg, err := core.LoadConfig([]string{base, over}, nil, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.GetInt("a", 0); got != 2 {
		t.Errorf("a = %d, want 2 (later wins)", got)
	}
	if got := cfg.GetInt("nested.x", 0); got != 1 {
		t.Errorf("nested.x = %d, want 1 (map merge preserves)", got)
	}
	if got := cfg.GetInt("nested.y", 0); got != 20 {
		t.Errorf("nested.y = %d, want 20 (override wins)", got)
	}
	if got := cfg.GetInt("nested.z", 0); got != 30 {
		t.Errorf("nested.z = %d, want 30 (added)", got)
	}
	// Lists replace, not append.
	if got := cfg.GetStringSlice("list", nil); len(got) != 1 || got[0] != "c" {
		t.Errorf("list = %v, want [c] (list replaces)", got)
	}
}

func TestConfigHas(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "base.yaml", "present: value\nempty: \"\"\nnested:\n  key: 1\n")
	cfg, err := core.LoadConfig([]string{f}, nil, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cases := map[string]bool{
		"present":     true,
		"empty":       true, // present-but-empty counts as present
		"nested.key":  true,
		"nested":      true,
		"absent":      false,
		"nested.gone": false,
	}
	for key, want := range cases {
		if got := cfg.Has(key); got != want {
			t.Errorf("Has(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestConfigJSONMergedAfterYAML(t *testing.T) {
	dir := t.TempDir()
	y := writeFile(t, dir, "base.yaml", "a: 1\nb: 2\n")
	j := writeFile(t, dir, "over.json", `{"b": 99, "c": 3}`)

	cfg, err := core.LoadConfig([]string{y}, []string{j}, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.GetInt("a", 0) != 1 || cfg.GetInt("b", 0) != 99 || cfg.GetInt("c", 0) != 3 {
		t.Errorf("merged wrong: a=%d b=%d c=%d", cfg.GetInt("a", 0), cfg.GetInt("b", 0), cfg.GetInt("c", 0))
	}
}

func TestConfigTypedOverrides(t *testing.T) {
	dir := t.TempDir()
	jf := writeFile(t, dir, "v.json", `{"deep": {"val": 7}}`)

	cfg, err := core.LoadConfig(nil, nil, []string{
		"s=hello",
		"i=int:42",
		"f=float:3.5",
		"b=bool:true",
		"lst=list:a,b,c",
		"nested.deep.key=int:9",
		"fromtext=json_text:{\"k\":true}",
		"fromfile=json_file:" + jf,
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.GetString("s", "") != "hello" {
		t.Errorf("string override wrong")
	}
	if cfg.GetInt("i", 0) != 42 {
		t.Errorf("int override wrong: %d", cfg.GetInt("i", 0))
	}
	if cfg.GetDuration("f", 0) != time.Duration(3.5*float64(time.Second)) {
		t.Errorf("float-as-seconds wrong")
	}
	if !cfg.GetBool("b", false) {
		t.Errorf("bool override wrong")
	}
	if got := cfg.GetStringSlice("lst", nil); len(got) != 3 || got[2] != "c" {
		t.Errorf("list override wrong: %v", got)
	}
	if cfg.GetInt("nested.deep.key", 0) != 9 {
		t.Errorf("dotted nesting override wrong")
	}
	if !cfg.GetBool("fromtext.k", false) {
		t.Errorf("json_text override wrong")
	}
	if cfg.GetInt("fromfile.deep.val", 0) != 7 {
		t.Errorf("json_file override wrong")
	}
}

func TestConfigHOCONExtensionRejected(t *testing.T) {
	dir := t.TempDir()
	conf := writeFile(t, dir, "profile.conf", "a = 1\n")
	_, err := core.LoadConfig([]string{conf}, nil, nil)
	if err == nil {
		t.Fatalf("expected HOCON error, got nil")
	}
	if !contains(err.Error(), "HOCON support pending") {
		t.Errorf("error = %q, want a clear HOCON-pending message", err)
	}
}

func TestConfigResolvedSnapshotIsCopy(t *testing.T) {
	dir := t.TempDir()
	y := writeFile(t, dir, "b.yaml", "nested:\n  x: 1\n")
	cfg, err := core.LoadConfig([]string{y}, nil, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	snap := cfg.Resolved()
	nested := snap["nested"].(map[string]any)
	nested["x"] = 999
	if cfg.GetInt("nested.x", 0) != 1 {
		t.Errorf("Resolved() must be a deep copy; mutation leaked back")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
