// Package functional registers real functional tests. The light tests here run
// against a single-container amp environment (env/docker) and skip cleanly via
// a type-assert on the environment when none is provisioned (dry-run/fake). The
// heavier amp-on-backing tests (backing.go) are end-to-end: for one YugabyteDB
// backing store they provision the cluster, run a YB-native workload (layer 1),
// deploy amp on it (ampstack external), run a workload through amp compute
// (layer 2), cross-check, and tear everything down as one unit. They
// self-provision real infrastructure, so they are opt-in behind
// functional.enabled and each shape's config gate (backing_config.go). The
// shared provision-and-prepare machinery (standup.go) also backs the presets
// suite. All tests are blank-imported by cmd/ampqa.
package features

import (
	"context"
	"fmt"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/system/framework/core"
)

// pgEnv is the accessor a test needs to reach the endpoint's Postgres. Both the
// docker single-container env and the sharedk8s project-per-test env implement
// it; core stays untouched. The non-restart regression tests depend only on this
// capability, so they run identically on either env (and any future env that can
// serve an endpoint's Postgres).
type pgEnv interface {
	PGEndpoint() pg.ConnInfo
}

// computeRestarter is the capability the restart tests depend on: restart the
// amp compute and block until it serves again. Depending on this capability
// (rather than a concrete env) is what makes the restart tests env-portable —
// the docker env restarts its endpoint container, the sharedk8s env rolls the
// endpoint's compute pod and re-resolves the connection. After RestartCompute
// returns, PGEndpoint() points at a live endpoint again.
type computeRestarter interface {
	pgEnv
	RestartCompute(ctx context.Context) error
}

const readyTimeout = 3 * time.Minute

func init() {
	core.Register(core.Test{
		Name:        "features/pg_roundtrip",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Create a table, insert seed-derived rows through amp compute, and verify count + checksum on read-back.",
		Run:         runRoundtrip,
	})

	core.Register(core.Test{
		Name:        "features/pg_restart_compute",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SingleContainer},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Write rows, restart the endpoint container, and verify every row survived WAL replay through the stack.",
		Run:         runRestartCompute,
	})

	// Two SHARED-safe registrations, each isolated to its own schema, prove no
	// cross-contamination between co-tenant tests on one environment.
	for _, name := range []string{"alpha", "beta"} {
		core.Register(core.Test{
			Name:        "features/pg_parallel_isolation_" + name,
			Suite:       "features",
			Category:    core.Functional,
			Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
			Tags:        []string{"regression", "nightly"},
			OwningTeam:  core.TeamCompute,
			Description: "Co-tenant isolation: write rows in an own schema and assert no foreign-tagged rows are visible.",
			Run:         isolationRun(name),
		})
	}
}

// runRoundtrip creates a table, inserts seed-derived rows, verifies count and
// checksum on read-back, and drops the table.
func runRoundtrip(t *core.T) error {
	env, ok := t.Env.(pgEnv)
	if !ok {
		t.Skip("requires a docker single-container env")
		return nil
	}
	client, err := connect(t, env)
	if err != nil {
		return err
	}
	defer client.Close()

	table := "rt_" + suffix(t)
	rows, wantSum := genRows(t, 200)

	return t.Step("roundtrip", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v bigint)", table)); err != nil {
			return err
		}
		defer func() { _, _ = client.Exec(t, "DROP TABLE IF EXISTS "+table) }()

		for id, v := range rows {
			if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), id, v); err != nil {
				return err
			}
		}
		s.Set("rows", len(rows))

		count, err := client.QueryInt(t, "SELECT count(*) FROM "+table)
		if err != nil {
			return err
		}
		if int(count) != len(rows) {
			return fmt.Errorf("row count = %d, want %d", count, len(rows))
		}
		gotSum, err := client.QueryInt(t, "SELECT coalesce(sum(v),0) FROM "+table)
		if err != nil {
			return err
		}
		if gotSum != wantSum {
			return fmt.Errorf("checksum = %d, want %d", gotSum, wantSum)
		}
		s.Set("checksum", gotSum)
		return nil
	})
}

// runRestartCompute writes rows, restarts the endpoint container, waits for
// readiness, and verifies every row survived (WAL replay through the stack).
func runRestartCompute(t *core.T) error {
	env, ok := t.Env.(computeRestarter)
	if !ok {
		t.Skip("requires an env with compute-restart capability (docker or sharedk8s)")
		return nil
	}
	client, err := connect(t, env)
	if err != nil {
		return err
	}
	defer client.Close()

	table := "wal_" + suffix(t)
	rows, wantSum := genRows(t, 200)

	err = t.Step("write", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v bigint)", table)); err != nil {
			return err
		}
		for id, v := range rows {
			if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), id, v); err != nil {
				return err
			}
		}
		// Force the writes down through safekeeper → pageserver → YugabyteDB.
		_, err := client.Exec(t, "CHECKPOINT")
		return err
	})
	if err != nil {
		return err
	}
	client.Close()

	if err := t.Step("restart_endpoint", func(s *core.StepHandle) error {
		return env.RestartCompute(t.Context)
	}); err != nil {
		return err
	}

	return t.Step("verify_survived", func(s *core.StepHandle) error {
		client, err := connect(t, env)
		if err != nil {
			return err
		}
		defer client.Close()
		count, err := client.QueryInt(t, "SELECT count(*) FROM "+table)
		if err != nil {
			return err
		}
		if int(count) != len(rows) {
			return fmt.Errorf("after restart row count = %d, want %d", count, len(rows))
		}
		gotSum, err := client.QueryInt(t, "SELECT coalesce(sum(v),0) FROM "+table)
		if err != nil {
			return err
		}
		if gotSum != wantSum {
			return fmt.Errorf("after restart checksum = %d, want %d", gotSum, wantSum)
		}
		_, _ = client.Exec(t, "DROP TABLE IF EXISTS "+table)
		return nil
	})
}

// isolationRun returns a test body that writes rows tagged with its own schema
// and verifies no rows from any other tag are visible.
func isolationRun(tag string) func(*core.T) error {
	return func(t *core.T) error {
		env, ok := t.Env.(pgEnv)
		if !ok {
			t.Skip("requires a docker single-container env")
			return nil
		}
		client, err := connect(t, env)
		if err != nil {
			return err
		}
		defer client.Close()

		schema := fmt.Sprintf("iso_%s_%s", tag, suffix(t))
		rows, _ := genRows(t, 100)

		return t.Step("isolate", func(s *core.StepHandle) error {
			s.Set("schema", schema)
			if _, err := client.Exec(t, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
				return err
			}
			defer func() { _, _ = client.Exec(t, "DROP SCHEMA IF EXISTS "+schema+" CASCADE") }()

			if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, tag text)", schema)); err != nil {
				return err
			}
			for id := range rows {
				if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s.t (id, tag) VALUES ($1, $2)", schema), id, tag); err != nil {
					return err
				}
			}
			count, err := client.QueryInt(t, fmt.Sprintf("SELECT count(*) FROM %s.t", schema))
			if err != nil {
				return err
			}
			if int(count) != len(rows) {
				return fmt.Errorf("own row count = %d, want %d", count, len(rows))
			}
			foreign, err := client.QueryInt(t, fmt.Sprintf("SELECT count(*) FROM %s.t WHERE tag <> $1", schema), tag)
			if err != nil {
				return err
			}
			if foreign != 0 {
				return fmt.Errorf("cross-contamination: %d rows with a foreign tag", foreign)
			}
			return nil
		})
	}
}

func connect(t *core.T, env pgEnv) (*pg.Client, error) {
	ci := env.PGEndpoint()
	if err := pg.WaitReady(t, ci, readyTimeout); err != nil {
		return nil, err
	}
	return pg.Connect(t, ci)
}

// genRows builds n deterministic rows keyed by id 0..n-1 and returns the
// expected sum of values for checksum verification.
func genRows(t *core.T, n int) (map[int]int64, int64) {
	r := t.Seed.Fork("rows").Rand()
	rows := make(map[int]int64, n)
	var sum int64
	for id := 0; id < n; id++ {
		// Bound v < 2^40 so the running sum stays well inside int64 (SQL sum(v)
		// promotes to a bignum and never wraps; Go int64 does — they must agree).
		// At 2^40 the sum fits int64 for n up to ~8M rows, far above any caller.
		v := int64(r.Uint64() >> 24)
		rows[id] = v
		sum += v
	}
	return rows, sum
}

func suffix(t *core.T) string {
	return t.ResourceSuffix()
}
