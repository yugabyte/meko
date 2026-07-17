package features

import (
	"fmt"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/functional/framework/core"
)

// This file adds regression-tagged functional tests that assert core amp
// storage guarantees the original suite did not cover: non-insert WAL
// (UPDATE/DELETE delta materialisation), transaction commit/rollback/atomicity,
// index-page reconstruction, the cold read path (forcing Pageserver GetPage
// rather than a Local File Cache hit), and catalog+index durability across a
// compute restart. Each asserts a real invariant against a Go-computed
// expected value; all skip cleanly on a non-docker env via the same type
// assertion the rest of the package uses.

func init() {
	core.Register(core.Test{
		Name:        "features/pg_mutations",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Insert rows, UPDATE a subset and DELETE another, then verify count+sum match a Go-computed expectation (proves UPDATE/DELETE WAL + delta application).",
		Run:         runMutations,
	})

	core.Register(core.Test{
		Name:        "features/pg_transaction",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Prove commit durability, rollback discard, and single-statement-error atomicity on amp storage.",
		Run:         runTransaction,
	})

	core.Register(core.Test{
		Name:        "features/pg_index",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Create a secondary index and verify a point lookup and a range count match a Go-computed expectation (proves index relations/pages reconstruct).",
		Run:         runIndex,
	})

	core.Register(core.Test{
		Name:        "features/pg_read_cold_cache",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Write a large set, CHECKPOINT, reconnect on a cold Local File Cache, and verify full-scan count+sum and a point lookup (forces Pageserver GetPage reconstruction).",
		Run:         runReadColdCache,
	})

	core.Register(core.Test{
		Name:        "features/pg_ddl_survives_restart",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SingleContainer},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Create a table+index, CHECKPOINT, restart the endpoint, and verify the table, the index, an index-backed lookup, and the data all survive the crash boundary.",
		Run:         runDDLSurvivesRestart,
	})

	core.Register(core.Test{
		Name:        "features/pg_multi_db_workload",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Run independent workloads in separate databases on one amp endpoint and verify each has correct, distinct, isolated data (the multitenant use case).",
		Run:         runMultiDBWorkload,
	})
}

// runMutations inserts rows, updates even ids (v += 1000) and deletes ids that
// are multiples of 5, then asserts count and sum against a Go-computed
// expectation applying the same subsets in the same order.
func runMutations(t *core.T) error {
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

	table := "mut_" + suffix(t)
	rows, _ := genRows(t, 400)

	// Expected final state: UPDATE evens (v += 1000) happens before DELETE of
	// id % 5 == 0; a row that is both is deleted, so only its membership in the
	// surviving set matters.
	var wantCount int
	var wantSum int64
	for id, v := range rows {
		if id%5 == 0 {
			continue
		}
		if id%2 == 0 {
			v += 1000
		}
		wantCount++
		wantSum += v
	}

	return t.Step("mutations", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v bigint)", table)); err != nil {
			return err
		}
		defer func() { _, _ = client.Exec(t, "DROP TABLE IF EXISTS "+table) }()

		for id, v := range rows {
			if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), id, v); err != nil {
				return err
			}
		}
		if _, err := client.Exec(t, fmt.Sprintf("UPDATE %s SET v = v + 1000 WHERE id %% 2 = 0", table)); err != nil {
			return err
		}
		if _, err := client.Exec(t, fmt.Sprintf("DELETE FROM %s WHERE id %% 5 = 0", table)); err != nil {
			return err
		}

		count, err := client.QueryInt(t, "SELECT count(*) FROM "+table)
		if err != nil {
			return err
		}
		if int(count) != wantCount {
			return fmt.Errorf("post-mutation count = %d, want %d", count, wantCount)
		}
		gotSum, err := client.QueryInt(t, "SELECT coalesce(sum(v),0) FROM "+table)
		if err != nil {
			return err
		}
		if gotSum != wantSum {
			return fmt.Errorf("post-mutation sum = %d, want %d", gotSum, wantSum)
		}
		s.Set("count", wantCount)
		s.Set("sum", wantSum)
		return nil
	})
}

// runTransaction proves commit durability, rollback discard, and atomicity of a
// transaction that fails on a statement error, all against amp storage.
func runTransaction(t *core.T) error {
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

	table := "tx_" + suffix(t)
	const k = 50

	if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v bigint)", table)); err != nil {
		return err
	}
	defer func() { _, _ = client.Exec(t, "DROP TABLE IF EXISTS "+table) }()

	// (a) A committed transaction is durable: all k rows are present after COMMIT.
	if err := t.Step("commit_durable", func(s *core.StepHandle) error {
		tx, err := client.Pool().Begin(t)
		if err != nil {
			return err
		}
		for i := 0; i < k; i++ {
			if _, err := tx.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), i, int64(i)); err != nil {
				_ = tx.Rollback(t)
				return err
			}
		}
		if err := tx.Commit(t); err != nil {
			return err
		}
		count, err := client.QueryInt(t, fmt.Sprintf("SELECT count(*) FROM %s WHERE id < $1", table), k)
		if err != nil {
			return err
		}
		if int(count) != k {
			return fmt.Errorf("after commit count = %d, want %d", count, k)
		}
		return nil
	}); err != nil {
		return err
	}

	// (b) A rolled-back transaction leaves nothing durable.
	if err := t.Step("rollback_discards", func(s *core.StepHandle) error {
		tx, err := client.Pool().Begin(t)
		if err != nil {
			return err
		}
		for i := 1000; i < 1000+k; i++ {
			if _, err := tx.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), i, int64(i)); err != nil {
				_ = tx.Rollback(t)
				return err
			}
		}
		if err := tx.Rollback(t); err != nil {
			return err
		}
		count, err := client.QueryInt(t, fmt.Sprintf("SELECT count(*) FROM %s WHERE id >= $1", table), 1000)
		if err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("after rollback %d rolled-back rows are visible, want 0", count)
		}
		return nil
	}); err != nil {
		return err
	}

	// (c) A statement error aborts the whole transaction: a valid row inserted
	// before a duplicate-PK failure is not durable after rollback.
	return t.Step("atomicity_aborts", func(s *core.StepHandle) error {
		const badID = 2000
		tx, err := client.Pool().Begin(t)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), badID, int64(1)); err != nil {
			_ = tx.Rollback(t)
			return err
		}
		if _, dupErr := tx.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), badID, int64(2)); dupErr == nil {
			_ = tx.Rollback(t)
			return fmt.Errorf("expected duplicate-PK insert to error, got nil")
		}
		if err := tx.Rollback(t); err != nil {
			return err
		}
		count, err := client.QueryInt(t, fmt.Sprintf("SELECT count(*) FROM %s WHERE id = $1", table), badID)
		if err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("after aborted tx the valid row is present (%d), want 0", count)
		}
		return nil
	})
}

// runIndex creates a secondary index and verifies a point lookup returns the
// right id and a range count matches a Go-computed expectation.
func runIndex(t *core.T) error {
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

	table := "idx_" + suffix(t)
	index := "idxv_" + suffix(t)
	rows := genDistinctRows(t, 300)

	return t.Step("index", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v bigint)", table)); err != nil {
			return err
		}
		defer func() { _, _ = client.Exec(t, "DROP TABLE IF EXISTS "+table) }()
		if _, err := client.Exec(t, fmt.Sprintf("CREATE INDEX %s ON %s (v)", index, table)); err != nil {
			return err
		}
		for _, r := range rows {
			if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), r.id, r.v); err != nil {
				return err
			}
		}

		// Point lookup: distinct v → exactly one id.
		want := rows[len(rows)/2]
		gotID, err := client.QueryInt(t, fmt.Sprintf("SELECT id FROM %s WHERE v = $1", table), want.v)
		if err != nil {
			return err
		}
		if int(gotID) != want.id {
			return fmt.Errorf("point lookup v=%d returned id %d, want %d", want.v, gotID, want.id)
		}

		// Range count over v.
		lo := rows[10].v
		hi := rows[200].v
		var wantRange int
		for _, r := range rows {
			if r.v >= lo && r.v <= hi {
				wantRange++
			}
		}
		gotRange, err := client.QueryInt(t, fmt.Sprintf("SELECT count(*) FROM %s WHERE v BETWEEN $1 AND $2", table), lo, hi)
		if err != nil {
			return err
		}
		if int(gotRange) != wantRange {
			return fmt.Errorf("range count [%d,%d] = %d, want %d", lo, hi, gotRange, wantRange)
		}
		s.Set("range_count", wantRange)
		return nil
	})
}

const coldRows = 5000

// runReadColdCache writes a large set, checkpoints, reconnects on a cold Local
// File Cache, and verifies full-scan count+sum and a point lookup — forcing the
// Pageserver GetPage read path rather than a cache hit.
func runReadColdCache(t *core.T) error {
	env, ok := t.Env.(pgEnv)
	if !ok {
		t.Skip("requires a docker single-container env")
		return nil
	}
	client, err := connect(t, env)
	if err != nil {
		return err
	}

	table := "cold_" + suffix(t)
	rows, wantSum := genRows(t, coldRows)

	if err := t.Step("write_and_checkpoint", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v bigint)", table)); err != nil {
			return err
		}
		for id, v := range rows {
			if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), id, v); err != nil {
				return err
			}
		}
		s.Set("rows", len(rows))
		_, err := client.Exec(t, "CHECKPOINT")
		return err
	}); err != nil {
		client.Close()
		return err
	}
	// Drop the compute-side cache: a fresh pool starts with a cold Local File
	// Cache, so read-back must reconstruct pages from the Pageserver.
	client.Close()

	return t.Step("verify_cold", func(s *core.StepHandle) error {
		cold, err := connect(t, env)
		if err != nil {
			return err
		}
		defer cold.Close()
		defer func() { _, _ = cold.Exec(t, "DROP TABLE IF EXISTS "+table) }()

		count, err := cold.QueryInt(t, "SELECT count(*) FROM "+table)
		if err != nil {
			return err
		}
		if int(count) != len(rows) {
			return fmt.Errorf("cold read count = %d, want %d", count, len(rows))
		}
		gotSum, err := cold.QueryInt(t, "SELECT coalesce(sum(v),0) FROM "+table)
		if err != nil {
			return err
		}
		if gotSum != wantSum {
			return fmt.Errorf("cold read sum = %d, want %d", gotSum, wantSum)
		}

		knownID := coldRows / 2
		gotV, err := cold.QueryInt(t, "SELECT v FROM "+table+" WHERE id = $1", knownID)
		if err != nil {
			return err
		}
		if gotV != rows[knownID] {
			return fmt.Errorf("cold point lookup id=%d returned v %d, want %d", knownID, gotV, rows[knownID])
		}
		s.Set("checksum", gotSum)
		return nil
	})
}

// runDDLSurvivesRestart creates a table and a secondary index, checkpoints,
// restarts the endpoint container, and verifies the table, the index, an
// index-backed lookup, and the row data all survive the crash boundary.
func runDDLSurvivesRestart(t *core.T) error {
	env, ok := t.Env.(computeRestarter)
	if !ok {
		t.Skip("requires an env with compute-restart capability (docker or sharedk8s)")
		return nil
	}
	client, err := connect(t, env)
	if err != nil {
		return err
	}

	table := "ddl_" + suffix(t)
	index := "ddlv_" + suffix(t)
	rows := genDistinctRows(t, 200)
	var wantSum int64
	for _, r := range rows {
		wantSum += r.v
	}

	if err := t.Step("write", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v bigint)", table)); err != nil {
			return err
		}
		if _, err := client.Exec(t, fmt.Sprintf("CREATE INDEX %s ON %s (v)", index, table)); err != nil {
			return err
		}
		for _, r := range rows {
			if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), r.id, r.v); err != nil {
				return err
			}
		}
		_, err := client.Exec(t, "CHECKPOINT")
		return err
	}); err != nil {
		client.Close()
		return err
	}
	client.Close()

	if err := t.Step("restart_endpoint", func(s *core.StepHandle) error {
		return env.RestartCompute(t.Context)
	}); err != nil {
		return err
	}

	return t.Step("verify_catalog_and_data", func(s *core.StepHandle) error {
		client, err := connect(t, env)
		if err != nil {
			return err
		}
		defer client.Close()
		defer func() { _, _ = client.Exec(t, "DROP TABLE IF EXISTS "+table) }()

		tblCount, err := client.QueryInt(t, "SELECT count(*) FROM pg_class WHERE relname = $1 AND relkind = 'r'", table)
		if err != nil {
			return err
		}
		if tblCount != 1 {
			return fmt.Errorf("after restart table %s not found in pg_class (count %d)", table, tblCount)
		}
		idxCount, err := client.QueryInt(t, "SELECT count(*) FROM pg_indexes WHERE indexname = $1", index)
		if err != nil {
			return err
		}
		if idxCount != 1 {
			return fmt.Errorf("after restart index %s not found in pg_indexes (count %d)", index, idxCount)
		}

		// Index-backed lookup: distinct v → exactly one id.
		want := rows[len(rows)/2]
		gotID, err := client.QueryInt(t, fmt.Sprintf("SELECT id FROM %s WHERE v = $1", table), want.v)
		if err != nil {
			return err
		}
		if int(gotID) != want.id {
			return fmt.Errorf("after restart lookup v=%d returned id %d, want %d", want.v, gotID, want.id)
		}

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
		return nil
	})
}

// runMultiDBWorkload creates K separate databases on one amp endpoint, runs a
// distinct workload in each, verifies each database's count+sum against a
// Go-computed expectation, and asserts cross-database isolation (no database
// sees another's tables). This is the multitenant "the whole system works"
// case.
func runMultiDBWorkload(t *core.T) error {
	env, ok := t.Env.(pgEnv)
	if !ok {
		t.Skip("requires a docker single-container env")
		return nil
	}
	admin, err := connect(t, env)
	if err != nil {
		return err
	}
	defer admin.Close()

	const k = 3
	dbs := make([]string, k)
	for i := range dbs {
		dbs[i] = fmt.Sprintf("app%d_%s", i+1, suffix(t))
	}
	tableFor := func(i int) string { return fmt.Sprintf("t%d_%s", i+1, suffix(t)) }

	// Safety net: DROP DATABASE needs no active connections, and the admin pool
	// is on the default database, so it can drop the app databases once their
	// per-db pools are closed. Best-effort; the cleanup step does the same.
	defer func() {
		for _, db := range dbs {
			_, _ = admin.Exec(t, "DROP DATABASE IF EXISTS "+db)
		}
	}()

	if err := t.Step("create_databases", func(s *core.StepHandle) error {
		for _, db := range dbs {
			if _, err := admin.Exec(t, "CREATE DATABASE "+db); err != nil {
				return err
			}
		}
		s.Set("databases", k)
		return nil
	}); err != nil {
		return err
	}

	ci := env.PGEndpoint()
	checksums := make([]int64, k)

	if err := t.Step("workload_per_db", func(s *core.StepHandle) error {
		for i, db := range dbs {
			dci := ci
			dci.Database = db
			client, err := pg.Connect(t, dci)
			if err != nil {
				return err
			}
			offset := int64(i+1) * 1_000_000
			rows, baseSum := genRows(t, 100)
			wantSum := baseSum + offset*int64(len(rows))
			table := tableFor(i)
			err = func() error {
				defer client.Close()
				if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v bigint)", table)); err != nil {
					return err
				}
				for id, v := range rows {
					if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), id, v+offset); err != nil {
						return err
					}
				}
				count, err := client.QueryInt(t, "SELECT count(*) FROM "+table)
				if err != nil {
					return err
				}
				if int(count) != len(rows) {
					return fmt.Errorf("db %s count = %d, want %d", db, count, len(rows))
				}
				gotSum, err := client.QueryInt(t, "SELECT coalesce(sum(v),0) FROM "+table)
				if err != nil {
					return err
				}
				if gotSum != wantSum {
					return fmt.Errorf("db %s sum = %d, want %d", db, gotSum, wantSum)
				}
				checksums[i] = gotSum
				return nil
			}()
			if err != nil {
				return err
			}
			s.Set(db, checksums[i])
		}
		// The per-db offsets must make the checksums pairwise distinct.
		for i := 0; i < k; i++ {
			for j := i + 1; j < k; j++ {
				if checksums[i] == checksums[j] {
					return fmt.Errorf("db %s and %s produced identical checksum %d, want distinct", dbs[i], dbs[j], checksums[i])
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("isolate", func(s *core.StepHandle) error {
		for i, db := range dbs {
			dci := ci
			dci.Database = db
			client, err := pg.Connect(t, dci)
			if err != nil {
				return err
			}
			err = func() error {
				defer client.Close()
				own, err := client.QueryInt(t, "SELECT count(*) FROM pg_class WHERE relname = $1 AND relkind = 'r'", tableFor(i))
				if err != nil {
					return err
				}
				if own != 1 {
					return fmt.Errorf("db %s is missing its own table %s (count %d)", db, tableFor(i), own)
				}
				for j := range dbs {
					if j == i {
						continue
					}
					foreign, err := client.QueryInt(t, "SELECT count(*) FROM pg_class WHERE relname = $1 AND relkind = 'r'", tableFor(j))
					if err != nil {
						return err
					}
					if foreign != 0 {
						return fmt.Errorf("db %s sees foreign table %s (count %d), want 0", db, tableFor(j), foreign)
					}
				}
				return nil
			}()
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	return t.Step("cleanup", func(s *core.StepHandle) error {
		// Best-effort: a drop failure must not fail the test.
		for _, db := range dbs {
			if _, err := admin.Exec(t, "DROP DATABASE IF EXISTS "+db); err != nil {
				s.Set("cleanup_error_"+db, err.Error())
			}
		}
		return nil
	})
}

// row is an (id, v) pair with a distinct v across the set, so a point lookup by
// v resolves to exactly one id.
type row struct {
	id int
	v  int64
}

// genDistinctRows builds n rows keyed by id 0..n-1 with strictly increasing
// (hence distinct) seed-derived v, so index point lookups by v are unambiguous.
func genDistinctRows(t *core.T, n int) []row {
	r := t.Seed.Fork("distinct").Rand()
	out := make([]row, n)
	v := int64(r.Uint64() >> 20)
	for id := 0; id < n; id++ {
		v += 1 + int64(r.Uint64()%16)
		out[id] = row{id: id, v: v}
	}
	return out
}
