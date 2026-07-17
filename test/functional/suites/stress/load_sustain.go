// Package stress registers real stress tests. stress/load_sustain is the
// first: it runs entirely through workloadgen against whatever env exposes a
// PGEndpoint (single-container docker today), so it needs no client-pod
// image yet. Env is Exclusive — a stress test never shares its environment
// with co-tenants. Blank-imported by cmd/ampqa.
package stress

import (
	"fmt"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/workloadgen"
	"github.com/yugabyte/yb-amp/test/functional/framework/core"
)

// pgEnv is the accessor a test needs to reach the endpoint's Postgres. The
// docker env implements it; core stays untouched (same convention as
// suites/functional).
type pgEnv interface {
	PGEndpoint() pg.ConnInfo
}

const readyTimeout = 3 * time.Minute

// defaultDuration is the RandomOps phase length when -o stress.duration isn't
// set.
const defaultDuration = 60 * time.Second

func init() {
	core.Register(core.Test{
		Name:        "stress/load_sustain",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SingleContainer},
		Tags:        []string{"nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Sustain a seed-driven mixed insert/update/delete/select load via workloadgen and assert live-row accounting matches the database.",
		Params:      []string{"stress.duration"},
		Run:         runLoadSustain,
	})
}

// runLoadSustain schemas and loads a small workloadgen dataset, sustains a
// seed-driven mixed insert/update/delete/select load against it for a
// config-tunable duration (default 60s, -o stress.duration=<duration>), and
// asserts the live-row accounting RandomOps tracked matches the database at
// the end of that phase.
func runLoadSustain(t *core.T) error {
	env, ok := t.Env.(pgEnv)
	if !ok {
		t.Skip("requires a docker single-container env (PGEndpoint)")
		return nil
	}
	ci := env.PGEndpoint()
	if err := pg.WaitReady(t, ci, readyTimeout); err != nil {
		return err
	}
	client, err := pg.Connect(t, ci)
	if err != nil {
		return err
	}
	defer client.Close()
	pool := client.Pool()

	spec := workloadgen.Spec{
		TablePrefix:  "ls_" + suffix(t),
		NumTables:    4,
		RowsPerTable: 2000,
		BatchSize:    500,
	}
	duration := t.Options.GetDuration("stress.duration", defaultDuration)

	if err := t.Step("schema", func(s *core.StepHandle) error {
		s.Set("tables", spec.NumTables)
		return workloadgen.Schema(t, pool, spec)
	}); err != nil {
		return err
	}
	defer dropTables(t, client, spec)

	if err := t.Step("load", func(s *core.StepHandle) error {
		s.Set("rows_per_table", spec.RowsPerTable)
		return workloadgen.Load(t, pool, spec, t.Seed.Value(), nil)
	}); err != nil {
		return err
	}

	var report workloadgen.OpsReport
	if err := t.Step("random_ops", func(s *core.StepHandle) error {
		s.Set("duration", duration.String())
		r, err := workloadgen.RandomOps(t, pool, spec, t.Seed.Fork("randomops").Value(), duration)
		if err != nil {
			return err
		}
		report = r
		return nil
	}); err != nil {
		return err
	}

	return t.Step("verify", func(s *core.StepHandle) error {
		mismatches := 0
		for _, tr := range report.Tables {
			if !tr.OK {
				mismatches++
				s.Set("mismatch_"+tr.Table, fmt.Sprintf("expected=%d actual=%d", tr.ExpectedLiveRows, tr.ActualLiveRows))
			}
		}
		s.Set("tables", len(report.Tables))
		if mismatches > 0 {
			return fmt.Errorf("random ops live-row accounting mismatch on %d/%d table(s)", mismatches, len(report.Tables))
		}
		return nil
	})
}

func dropTables(t *core.T, client *pg.Client, spec workloadgen.Spec) {
	for i := 0; i < spec.NumTables; i++ {
		_, _ = client.Exec(t, "DROP TABLE IF EXISTS "+spec.TableName(i))
	}
}

func suffix(t *core.T) string {
	return t.ResourceSuffix()
}
