package features

import (
	"fmt"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/workloadgen"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "features/workloadgen_verify",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Load deterministic workloadgen rows and verify the recomputed count/checksum matches the database.",
		Run:         runWorkloadgenVerify,
	})
}

// runWorkloadgenVerify schemas a couple of small tables, loads a few
// deterministic seed-derived rows into each, and verifies the recomputed
// expected count/checksum matches what landed in the database — the
// workloadgen oracle exercised end-to-end against any env exposing a
// PGEndpoint.
func runWorkloadgenVerify(t *core.T) error {
	env, ok := t.Env.(pgEnv)
	if !ok {
		t.Skip("requires an env with a PGEndpoint")
		return nil
	}
	client, err := connect(t, env)
	if err != nil {
		return err
	}
	defer client.Close()
	pool := client.Pool()

	spec := workloadgen.Spec{
		TablePrefix:  "wg_" + suffix(t),
		NumTables:    2,
		RowsPerTable: 50,
		BatchSize:    20,
	}

	if err := t.Step("schema", func(s *core.StepHandle) error {
		return workloadgen.Schema(t, pool, spec)
	}); err != nil {
		return err
	}
	defer dropWorkloadgenTables(t, client, spec)

	if err := t.Step("load", func(s *core.StepHandle) error {
		return workloadgen.Load(t, pool, spec, t.Seed.Value(), nil)
	}); err != nil {
		return err
	}

	return t.Step("verify", func(s *core.StepHandle) error {
		report, err := workloadgen.Verify(t, pool, spec, t.Seed.Value())
		if err != nil {
			return err
		}
		s.Set("tables", len(report.Tables))
		if !report.OK {
			for _, tr := range report.Tables {
				if !tr.OK {
					return fmt.Errorf("table %s mismatch: count %d/%d (actual/expected) checksum %s/%s",
						tr.Table, tr.ActualCount, tr.ExpectedCount, tr.ActualChecksum, tr.ExpectedChecksum)
				}
			}
		}
		return nil
	})
}

func dropWorkloadgenTables(t *core.T, client *pg.Client, spec workloadgen.Spec) {
	for i := 0; i < spec.NumTables; i++ {
		_, _ = client.Exec(t, "DROP TABLE IF EXISTS "+spec.TableName(i))
	}
}
