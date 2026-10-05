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

// The provision budget scales with the yb_k8s store's node count, 10m base +
// 2m30s per node beyond the first, and only for that backing; an explicit
// timeouts.provision wins verbatim.
func TestProvisionTimeoutScalesWithStoreNodes(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want time.Duration
	}{
		{"single node", "db: {provider: yugabyted, rf: 1}", DefaultProvisionTimeout},
		{"rf3", "db: {provider: yugabyted, rf: 3}", DefaultProvisionTimeout + 2*ProvisionPerExtraNode},
		{"six tservers", "db: {provider: yba, rf: 3, nodes: 6}", DefaultProvisionTimeout + 5*ProvisionPerExtraNode},
		{"chart ignores shape", "db: {provider: chart, rf: 3}", DefaultProvisionTimeout},
		{"explicit wins", "timeouts: {provision: 7m}\ndb: {rf: 3}", 7 * time.Minute},
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
