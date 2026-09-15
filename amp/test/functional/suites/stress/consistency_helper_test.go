package stress

import (
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func TestConsistencyParamsDeclaredInDefaults(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load shipped defaults: %v", err)
	}
	for _, key := range []string{
		"stress.consistency.enabled",
		"stress.consistency.timeout",
		"stress.consistency.max_relations",
		"stress.consistency.probe_sql",
	} {
		if !cfg.Has(key) {
			t.Errorf("%s is read by the consistency step but not declared in configs/default.yaml", key)
		}
	}
	p := consistencyParamsFromConfig(cfg)
	if !p.Enabled {
		t.Error("the shipped default disables the consistency step")
	}
	if p.ProbeSQL != "" {
		t.Errorf("probe_sql ships as %q: it replaces the real check", p.ProbeSQL)
	}
}
