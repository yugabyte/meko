package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func cfgOf(t *testing.T, yaml string) *Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfigNoDefaults([]string{path}, nil, nil)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return c
}

// The provision budget scales with the yb_k8s store's node count — 10m base +
// 2m30s per node beyond the first — and only for that backing; an explicit
// timeouts.provision wins verbatim.
func TestProvisionTimeoutScalesWithStoreNodes(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want time.Duration
	}{
		{"no backing selected", `env: {yb_deploy: ""}`, 10 * time.Minute},
		{"yb_k8s single node", `env: {yb_deploy: yb_k8s}`, 10 * time.Minute},
		{"yb_k8s rf3", "env: {yb_deploy: yb_k8s}\napps: {ybk8s: {replication_factor: 3}}", 15 * time.Minute},
		{"yb_k8s six tservers", "env: {yb_deploy: yb_k8s}\napps: {ybk8s: {replication_factor: 3, tserver_replicas: 6}}", 10*time.Minute + 5*ProvisionPerExtraNode},
		{"builtin ignores shape", "env: {yb_deploy: \"\"}\napps: {ybk8s: {replication_factor: 3}}", 10 * time.Minute},
		{"explicit wins", "timeouts: {provision: 7m}\nenv: {yb_deploy: yb_k8s}\napps: {ybk8s: {replication_factor: 3}}", 7 * time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveProvisionTimeout(cfgOf(t, c.yaml)); got != c.want {
				t.Errorf("= %s, want %s", got, c.want)
			}
		})
	}
	if got := ResolveProvisionTimeout(nil); got != DefaultProvisionTimeout {
		t.Errorf("nil cfg = %s, want %s", got, DefaultProvisionTimeout)
	}
}
