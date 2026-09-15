package stress

import (
	"context"
	"strings"
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

type planSession struct{ plan string }

func (p planSession) Exec(context.Context, string, ...any) (int64, error) { return 0, nil }
func (p planSession) QueryInt(context.Context, string, ...any) (int64, error) {
	return 0, nil
}
func (p planSession) QueryString(context.Context, string, ...any) (string, error) {
	return p.plan, nil
}
func (p planSession) Close() {}

func aggOver(child string) string {
	return `[{"Plan":{"Node Type":"Aggregate","Plans":[` + child + `]}}]`
}

// The regression: the planner served the count from bankaccountsidx rather than
// the primary key, and the leg is still independent of the heap (result 4303c440).
func TestIndexLegAcceptsAnyIndex(t *testing.T) {
	for _, idx := range []string{"bankaccountsidx", "bankaccounts_pkey"} {
		sess := planSession{plan: aggOver(`{"Node Type":"Index Only Scan","Index Name":"` + idx + `"}`)}
		if err := requireSessionPlanUsesAnIndex(context.Background(), sess, "SELECT count(*) FROM t"); err != nil {
			t.Errorf("index %s rejected: %v", idx, err)
		}
	}
}

func TestIndexLegRejectsASequentialPlan(t *testing.T) {
	for _, plan := range []string{
		aggOver(`{"Node Type": "Seq Scan","Relation Name":"bankaccounts"}`),
		// The clause that earns its keep: an index child does not excuse a
		// sequential one, and the spacing here differs from the case above.
		aggOver(`{"Node Type":"Seq Scan","Relation Name":"bankaccounts"},{"Node Type":"Index Only Scan","Index Name":"bankaccountsidx"}`),
	} {
		err := requireSessionPlanUsesAnIndex(context.Background(), planSession{plan: plan}, "SELECT count(*) FROM t")
		if err == nil {
			t.Fatalf("a plan with a Seq Scan was accepted, so the leg could agree with the heap by construction: %s", plan)
		}
		if !strings.Contains(err.Error(), "does not use one") {
			t.Errorf("unexpected error: %v", err)
		}
	}
}
