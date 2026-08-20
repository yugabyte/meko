package stress

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func maintCfg(overrides map[string]any) (*core.Config, maintenanceConfig) {
	base := map[string]any{
		"stress.backing_maintenance.enabled": true,
		"secrets.gh_package.token":           "not-a-real-token",
	}
	for k, v := range overrides {
		base[k] = v
	}
	c := core.NewConfig()
	for k, v := range base {
		c.Set(k, v)
	}
	return c, maintenanceFromConfig(c)
}

func TestMaintenanceGateAcceptsAConfiguredRun(t *testing.T) {
	c, cfg := maintCfg(nil)
	if reason, ok := maintenanceGate(c, cfg); !ok {
		t.Fatalf("gate rejected a complete config: %s", reason)
	}
}

func TestMaintenanceGateSkipsUnlessEnabled(t *testing.T) {
	c, cfg := maintCfg(map[string]any{"stress.backing_maintenance.enabled": false})
	reason, ok := maintenanceGate(c, cfg)
	if ok {
		t.Fatal("the scenario upgrades the run's real backing; it must not run unopted")
	}
	if !strings.Contains(reason, "enabled") {
		t.Errorf("skip reason = %q, want it to name the gate key", reason)
	}
}

func TestMaintenanceGateRejectsRFBelowThree(t *testing.T) {
	c, cfg := maintCfg(map[string]any{"stress.backing_maintenance.rf": 1})
	reason, ok := maintenanceGate(c, cfg)
	if ok {
		t.Fatal("RF<3 cannot stay writable while a node is out; the run would assert the wrong thing")
	}
	if !strings.Contains(reason, "RF") {
		t.Errorf("skip reason = %q, want it to explain the RF requirement", reason)
	}
}

func TestMaintenanceGateRejectsAnUnknownOperation(t *testing.T) {
	c, cfg := maintCfg(map[string]any{"stress.backing_maintenance.operations": []string{"reboot_the_world"}})
	reason, ok := maintenanceGate(c, cfg)
	if ok {
		t.Fatal("a misspelled operation must fail the gate, not be silently dropped")
	}
	if !strings.Contains(reason, "reboot_the_world") {
		t.Errorf("skip reason = %q, want it to name the unknown operation", reason)
	}
}

// With no tenants, nothing writes through amp, so every per-operation assertion would be vacuous.
func TestMaintenanceGateRejectsZeroTenants(t *testing.T) {
	c, cfg := maintCfg(map[string]any{"stress.backing_maintenance.tenants": 0})
	if _, ok := maintenanceGate(c, cfg); ok {
		t.Fatal("a run with no tenants must not report success")
	}
}

func TestMaintenanceGateAllowsASubsetOfOperations(t *testing.T) {
	c, cfg := maintCfg(map[string]any{
		"stress.backing_maintenance.operations": []string{OpGflagChange, OpSoftwareUpgrade},
	})
	if reason, ok := maintenanceGate(c, cfg); !ok {
		t.Fatalf("a subset must be allowed: %s", reason)
	}
	if cfg.wants(OpDataMove) {
		t.Error("data_move should not be in a subset that omits it")
	}
}

func TestPlanRunsEveryOperationInOrder(t *testing.T) {
	var order []string
	commits := int64(0)
	p := &maintenancePlan{
		ops: []maintenanceOp{
			{"one", func(context.Context) error { order = append(order, "one"); commits += 5; return nil }},
			{"two", func(context.Context) error { order = append(order, "two"); commits += 7; return nil }},
		},
		progress:    func() int64 { return commits },
		minProgress: 1,
	}
	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Join(order, ",") != "one,two" {
		t.Errorf("order = %v, want the operations in sequence", order)
	}
}

func TestPlanFailsWhenNoCommitLandsDuringAnOperation(t *testing.T) {
	commits := int64(0)
	p := &maintenancePlan{
		ops: []maintenanceOp{
			{"rolling_restart", func(context.Context) error { commits += 9; return nil }},
			{"software_upgrade", func(context.Context) error { return nil }},
		},
		progress:    func() int64 { return commits },
		minProgress: 1,
	}
	err := p.run(context.Background())
	if err == nil {
		t.Fatal("an operation with zero acknowledged commits must fail the run")
	}
	if !strings.Contains(err.Error(), "software_upgrade") {
		t.Errorf("error = %v, want it to name the operation that stalled the writes", err)
	}
}

func TestPlanReportsCommitsPerOperation(t *testing.T) {
	commits := int64(0)
	seen := map[string]int64{}
	p := &maintenancePlan{
		ops: []maintenanceOp{
			{"one", func(context.Context) error { commits += 4; return nil }},
			{"two", func(context.Context) error { commits += 11; return nil }},
		},
		progress:    func() int64 { return commits },
		minProgress: 1,
		observe:     func(op string, c int64) { seen[op] = c },
	}
	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if seen["one"] != 4 || seen["two"] != 11 {
		t.Errorf("per-operation commits = %v, want one:4 two:11 (each measured across its own window)", seen)
	}
}

func TestPlanReportsEachOperationAsAStep(t *testing.T) {
	var steps []string
	p := &maintenancePlan{
		ops: []maintenanceOp{
			{"one", func(context.Context) error { return nil }},
			{"two", func(context.Context) error { return nil }},
		},
		progress:    func() int64 { return 1 },
		minProgress: 0,
		step: func(name string, fn func() error) error {
			steps = append(steps, name)
			return fn()
		},
	}
	if err := p.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Join(steps, ",") != "one,two" {
		t.Errorf("steps = %v, want one per operation", steps)
	}
}

func TestPlanStopsAtTheFirstFailedOperation(t *testing.T) {
	ran := false
	p := &maintenancePlan{
		ops: []maintenanceOp{
			{"one", func(context.Context) error { return errors.New("upgrade task failed") }},
			{"two", func(context.Context) error { ran = true; return nil }},
		},
		progress:    func() int64 { return 1 },
		minProgress: 0,
	}
	if err := p.run(context.Background()); err == nil {
		t.Fatal("a failed operation must fail the run")
	}
	if ran {
		t.Error("the sequence continued past a failed operation")
	}
}

func TestPlanRejectsAnEmptySequence(t *testing.T) {
	p := &maintenancePlan{progress: func() int64 { return 0 }}
	if err := p.run(context.Background()); err == nil {
		t.Fatal("a run with no maintenance operations must not report success")
	}
}

// YBA prefixes every object with the universe name, so the suite derives it from the pod name rather than guessing.
func TestTserverHeadlessDerivedFromThePodName(t *testing.T) {
	cases := map[string]string{
		"ybampqa-c8c38da1b869ce87-snkw-yb-tserver-0": "ybampqa-c8c38da1b869ce87-snkw-yb-tservers",
		"yb-tserver-2": "yb-tservers",
	}
	for pod, want := range cases {
		if got := tserverHeadlessOf(pod); got != want {
			t.Errorf("tserverHeadlessOf(%q) = %q, want %q", pod, got, want)
		}
	}
}

// No jar secret means no workload, so this must skip rather than fail.
func TestMaintenanceGateSkipsWithoutTheJarSecret(t *testing.T) {
	c := core.NewConfig()
	c.Set("stress.backing_maintenance.enabled", true)
	cfg := maintenanceFromConfig(c)
	reason, ok := maintenanceGate(c, cfg)
	if ok {
		t.Fatal("without the jar secret there is no workload; the run must skip, not fail")
	}
	if !strings.Contains(reason, "gh_package") {
		t.Errorf("skip reason = %q, want it to name the missing secret", reason)
	}
}
