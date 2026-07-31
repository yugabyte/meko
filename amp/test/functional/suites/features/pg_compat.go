// PG-compatibility regression pack: light tests that exercise Postgres surface
// area (COPY, prepared statements, savepoints, TOAST, sequences, partitions,
// concurrent writers) through amp compute, asserting against Go-computed
// expectations.
package features

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	shared := func(name, desc string, run func(*core.T) error) core.Test {
		return core.Test{
			Name:        "features/" + name,
			Suite:       "features",
			Category:    core.Functional,
			Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
			Tags:        []string{"regression", "nightly"},
			OwningTeam:  core.TeamCompute,
			Description: desc,
			Run:         run,
		}
	}

	core.Register(shared("pg_copy",
		"Bulk-load rows via the COPY protocol (pgx CopyFrom), verify count+sum, then re-COPY a read-back subset into a second table and verify it.",
		runCopy))
	core.Register(shared("pg_prepared_statements",
		"Server-side PREPARE/EXECUTE/DEALLOCATE on one pinned session: prepared insert and select loops verified against Go-computed values.",
		runPreparedStatements))
	core.Register(shared("pg_savepoints",
		"Nested SAVEPOINT/ROLLBACK TO/RELEASE inside one transaction; verify the exact surviving id set after commit.",
		runSavepoints))
	core.Register(shared("pg_toast_large_values",
		"Write 256KB-1MB seed-derived text values (TOAST), CHECKPOINT, cold reconnect, and verify per-row md5 against Go-computed digests.",
		runToastLargeValues))
	core.Register(shared("pg_sequences",
		"Identity column + explicit sequence: insert, capture the high-water mark, cold reconnect, insert more, assert no id reuse or regression.",
		runSequences))
	core.Register(shared("pg_partitioned_tables",
		"RANGE-partitioned table: per-partition routing counts, temp-table invisibility from a second connection, and data after a partition drop.",
		runPartitionedTables))

	core.Register(core.Test{
		Name:        "features/pg_concurrent_writers",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SingleContainer},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "8 goroutines on own connections: disjoint-range concurrent inserts verified by exact count+sum, then concurrent increments verified against Go-side atomic counters.",
		Run:         runConcurrentWriters,
	})
}

const copyRows = 2000

// runCopy bulk-loads via CopyFrom, verifies count+sum, then reads a subset back
// and re-COPYs it into a second table.
func runCopy(t *core.T) error {
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

	table := "cp_" + suffix(t)
	table2 := "cp2_" + suffix(t)
	rows, wantSum := genRows(t, copyRows)
	src := make([][]any, 0, copyRows)
	for id := 0; id < copyRows; id++ {
		src = append(src, []any{id, rows[id]})
	}

	if err := t.Step("copy_load", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v bigint)", table)); err != nil {
			return err
		}
		n, err := client.Pool().CopyFrom(t, pgx.Identifier{table}, []string{"id", "v"}, pgx.CopyFromRows(src))
		if err != nil {
			return err
		}
		if n != copyRows {
			return fmt.Errorf("CopyFrom into %s reported %d rows loaded, want %d (the bulk COPY protocol dropped or duplicated rows)", table, n, copyRows)
		}
		count, err := client.QueryInt(t, "SELECT count(*) FROM "+table)
		if err != nil {
			return err
		}
		if int(count) != copyRows {
			return fmt.Errorf("after CopyFrom, table %s has %d rows, want %d", table, count, copyRows)
		}
		gotSum, err := client.QueryInt(t, "SELECT coalesce(sum(v),0) FROM "+table)
		if err != nil {
			return err
		}
		if gotSum != wantSum {
			return fmt.Errorf("after CopyFrom, sum(v) over %s = %d, want %d (checksum mismatch on the bulk-loaded values)", table, gotSum, wantSum)
		}
		s.Set("rows", copyRows)
		return nil
	}); err != nil {
		_, _ = client.Exec(t, "DROP TABLE IF EXISTS "+table)
		return err
	}

	return t.Step("copy_subset", func(s *core.StepHandle) error {
		defer func() {
			_, _ = client.Exec(t, "DROP TABLE IF EXISTS "+table)
			_, _ = client.Exec(t, "DROP TABLE IF EXISTS "+table2)
		}()
		const subset = 500
		var wantSubSum int64
		for id := 0; id < subset; id++ {
			wantSubSum += rows[id]
		}
		back := make([][]any, 0, subset)
		err := client.Query(t, func(r pgx.Rows) error {
			var id int
			var v int64
			if err := r.Scan(&id, &v); err != nil {
				return err
			}
			back = append(back, []any{id, v})
			return nil
		}, fmt.Sprintf("SELECT id, v FROM %s WHERE id < $1 ORDER BY id", table), subset)
		if err != nil {
			return err
		}
		if len(back) != subset {
			return fmt.Errorf("read back %d rows with id < %d from %s, want %d (the subset read-back before re-COPY returned the wrong row count)", len(back), subset, table, subset)
		}
		if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v bigint)", table2)); err != nil {
			return err
		}
		if _, err := client.Pool().CopyFrom(t, pgx.Identifier{table2}, []string{"id", "v"}, pgx.CopyFromRows(back)); err != nil {
			return err
		}
		count, err := client.QueryInt(t, "SELECT count(*) FROM "+table2)
		if err != nil {
			return err
		}
		if int(count) != subset {
			return fmt.Errorf("after re-COPY of the %d-row subset into %s, count = %d, want %d", subset, table2, count, subset)
		}
		gotSum, err := client.QueryInt(t, "SELECT coalesce(sum(v),0) FROM "+table2)
		if err != nil {
			return err
		}
		if gotSum != wantSubSum {
			return fmt.Errorf("after re-COPY into %s, sum(v) = %d, want %d (checksum mismatch on the re-loaded subset)", table2, gotSum, wantSubSum)
		}
		s.Set("subset_sum", gotSum)
		return nil
	})
}

// runPreparedStatements drives SQL-level PREPARE/EXECUTE/DEALLOCATE on one
// pinned session (prepared statements are session-scoped, so the pool must not
// swap connections underneath).
func runPreparedStatements(t *core.T) error {
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

	table := "ps_" + suffix(t)
	insName := "ins_" + suffix(t)
	selName := "sel_" + suffix(t)
	rows, wantSum := genRows(t, 150)

	if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v bigint)", table)); err != nil {
		return err
	}
	defer func() { _, _ = client.Exec(t, "DROP TABLE IF EXISTS "+table) }()

	return t.Step("prepare_execute", func(s *core.StepHandle) error {
		conn, err := client.Pool().Acquire(t)
		if err != nil {
			return err
		}
		defer conn.Release()

		if _, err := conn.Exec(t, fmt.Sprintf("PREPARE %s (int, bigint) AS INSERT INTO %s (id, v) VALUES ($1, $2)", insName, table)); err != nil {
			return err
		}
		if _, err := conn.Exec(t, fmt.Sprintf("PREPARE %s (int) AS SELECT v FROM %s WHERE id = $1", selName, table)); err != nil {
			return err
		}
		for id := 0; id < len(rows); id++ {
			if _, err := conn.Exec(t, fmt.Sprintf("EXECUTE %s(%d, %d)", insName, id, rows[id])); err != nil {
				return err
			}
		}
		for _, id := range []int{0, len(rows) / 2, len(rows) - 1} {
			var v int64
			if err := conn.QueryRow(t, fmt.Sprintf("EXECUTE %s(%d)", selName, id)).Scan(&v); err != nil {
				return err
			}
			if v != rows[id] {
				return fmt.Errorf("EXECUTE %s(%d) returned %d, want %d (prepared SELECT on the pinned session returned a stale or wrong value)", selName, id, v, rows[id])
			}
		}
		gotSum, err := client.QueryInt(t, "SELECT coalesce(sum(v),0) FROM "+table)
		if err != nil {
			return err
		}
		if gotSum != wantSum {
			return fmt.Errorf("after the prepared-INSERT loop, sum(v) over %s = %d, want %d", table, gotSum, wantSum)
		}

		if _, err := conn.Exec(t, "DEALLOCATE "+insName); err != nil {
			return err
		}
		if _, err := conn.Exec(t, "DEALLOCATE "+selName); err != nil {
			return err
		}
		if _, err := conn.Exec(t, fmt.Sprintf("EXECUTE %s(1)", selName)); err == nil {
			return fmt.Errorf("EXECUTE %s(1) succeeded after DEALLOCATE %s, want it to fail (a deallocated prepared statement must no longer be executable)", selName, selName)
		}
		s.Set("rows", len(rows))
		return nil
	})
}

// runSavepoints nests SAVEPOINT/ROLLBACK TO/RELEASE in one transaction and
// verifies the exact surviving id set: {1,2} outer, {3,4} rolled back, 5 kept,
// 6 released, 7 rolled back nested, 8 post-savepoint.
func runSavepoints(t *core.T) error {
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

	table := "sp_" + suffix(t)
	if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY)", table)); err != nil {
		return err
	}
	defer func() { _, _ = client.Exec(t, "DROP TABLE IF EXISTS "+table) }()

	return t.Step("savepoints", func(s *core.StepHandle) error {
		tx, err := client.Pool().Begin(t)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(t) }()
		ins := func(ids ...int) error {
			for _, id := range ids {
				if _, err := tx.Exec(t, fmt.Sprintf("INSERT INTO %s (id) VALUES ($1)", table), id); err != nil {
					return err
				}
			}
			return nil
		}
		if err := ins(1, 2); err != nil {
			return err
		}
		if _, err := tx.Exec(t, "SAVEPOINT sp1"); err != nil {
			return err
		}
		if err := ins(3, 4); err != nil {
			return err
		}
		if _, err := tx.Exec(t, "ROLLBACK TO SAVEPOINT sp1"); err != nil {
			return err
		}
		if err := ins(5); err != nil {
			return err
		}
		if _, err := tx.Exec(t, "SAVEPOINT sp2"); err != nil {
			return err
		}
		if err := ins(6); err != nil {
			return err
		}
		if _, err := tx.Exec(t, "SAVEPOINT sp3"); err != nil {
			return err
		}
		if err := ins(7); err != nil {
			return err
		}
		if _, err := tx.Exec(t, "ROLLBACK TO SAVEPOINT sp3"); err != nil {
			return err
		}
		if _, err := tx.Exec(t, "RELEASE SAVEPOINT sp2"); err != nil {
			return err
		}
		if err := ins(8); err != nil {
			return err
		}
		if err := tx.Commit(t); err != nil {
			return err
		}

		want := []int{1, 2, 5, 6, 8}
		var got []int
		err = client.Query(t, func(r pgx.Rows) error {
			var id int
			if err := r.Scan(&id); err != nil {
				return err
			}
			got = append(got, id)
			return nil
		}, fmt.Sprintf("SELECT id FROM %s ORDER BY id", table))
		if err != nil {
			return err
		}
		if len(got) != len(want) {
			return fmt.Errorf("surviving row ids after nested SAVEPOINT/ROLLBACK TO/RELEASE = %v, want %v (the savepoint rollback/release sequence kept or discarded the wrong rows)", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				return fmt.Errorf("surviving row ids after nested SAVEPOINT/ROLLBACK TO/RELEASE = %v, want %v (the savepoint rollback/release sequence kept or discarded the wrong rows)", got, want)
			}
		}
		s.Set("ids", fmt.Sprint(got))
		return nil
	})
}

const toastRowCount = 6

// runToastLargeValues writes 256KB-1MB seed-derived text values, checkpoints,
// reconnects cold, and verifies per-row md5 digests against Go-computed ones.
func runToastLargeValues(t *core.T) error {
	env, ok := t.Env.(pgEnv)
	if !ok {
		t.Skip("requires a docker single-container env")
		return nil
	}
	client, err := connect(t, env)
	if err != nil {
		return err
	}

	table := "toast_" + suffix(t)
	r := t.Seed.Fork("toast").Rand()
	vals := make([]string, toastRowCount)
	digests := make([]string, toastRowCount)
	for i := range vals {
		size := 256*1024 + int(r.Uint64()%(768*1024))
		b := make([]byte, size)
		for j := range b {
			b[j] = 'a' + byte(r.Uint64()%26)
		}
		vals[i] = string(b)
		sum := md5.Sum(b)
		digests[i] = hex.EncodeToString(sum[:])
	}

	if err := t.Step("write_large_values", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v text)", table)); err != nil {
			return err
		}
		for i, v := range vals {
			if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), i, v); err != nil {
				return err
			}
		}
		s.Set("rows", toastRowCount)
		_, err := client.Exec(t, "CHECKPOINT")
		return err
	}); err != nil {
		client.Close()
		return err
	}
	client.Close()

	return t.Step("verify_cold_md5", func(s *core.StepHandle) error {
		cold, err := connect(t, env)
		if err != nil {
			return err
		}
		defer cold.Close()
		defer func() { _, _ = cold.Exec(t, "DROP TABLE IF EXISTS "+table) }()

		for i := range vals {
			got, err := cold.QueryString(t, fmt.Sprintf("SELECT md5(v) FROM %s WHERE id = $1", table), i)
			if err != nil {
				return err
			}
			if got != digests[i] {
				return fmt.Errorf("cold reconnect: row %d md5(v) = %s, want %s (%d bytes) (the TOASTed large value did not reconstruct to the written content)", i, got, digests[i], len(vals[i]))
			}
			gotLen, err := cold.QueryInt(t, fmt.Sprintf("SELECT length(v) FROM %s WHERE id = $1", table), i)
			if err != nil {
				return err
			}
			if int(gotLen) != len(vals[i]) {
				return fmt.Errorf("cold reconnect: row %d length(v) = %d, want %d (the TOASTed value's length changed across the cold reconnect)", i, gotLen, len(vals[i]))
			}
		}
		return nil
	})
}

// runSequences inserts through an identity column and an explicit-sequence
// default, reconnects cold, inserts more, and asserts no id reuse or regression
// across the reconnect (a sequence cache gap is allowed; going backwards is not).
func runSequences(t *core.T) error {
	env, ok := t.Env.(pgEnv)
	if !ok {
		t.Skip("requires a docker single-container env")
		return nil
	}
	client, err := connect(t, env)
	if err != nil {
		return err
	}

	sfx := suffix(t)
	idTable := "seqid_" + sfx
	seqName := "seq_" + sfx
	seqTable := "seqex_" + sfx
	const first, second = 100, 50

	var idMax, seqLast int64
	if err := t.Step("insert_first_batch", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, v int)", idTable)); err != nil {
			return err
		}
		if _, err := client.Exec(t, "CREATE SEQUENCE "+seqName); err != nil {
			return err
		}
		if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id bigint PRIMARY KEY DEFAULT nextval('%s'), v int)", seqTable, seqName)); err != nil {
			return err
		}
		for i := 0; i < first; i++ {
			if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (v) VALUES ($1)", idTable), i); err != nil {
				return err
			}
			if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (v) VALUES ($1)", seqTable), i); err != nil {
				return err
			}
		}
		var err error
		if idMax, err = client.QueryInt(t, "SELECT max(id) FROM "+idTable); err != nil {
			return err
		}
		if seqLast, err = client.QueryInt(t, "SELECT last_value FROM "+seqName); err != nil {
			return err
		}
		s.Set("id_max", idMax)
		s.Set("seq_last_value", seqLast)
		return nil
	}); err != nil {
		client.Close()
		return err
	}
	client.Close()

	return t.Step("insert_after_reconnect", func(s *core.StepHandle) error {
		cold, err := connect(t, env)
		if err != nil {
			return err
		}
		defer cold.Close()
		defer func() {
			_, _ = cold.Exec(t, "DROP TABLE IF EXISTS "+idTable)
			_, _ = cold.Exec(t, "DROP TABLE IF EXISTS "+seqTable)
			_, _ = cold.Exec(t, "DROP SEQUENCE IF EXISTS "+seqName)
		}()

		for i := 0; i < second; i++ {
			if _, err := cold.Exec(t, fmt.Sprintf("INSERT INTO %s (v) VALUES ($1)", idTable), 1000+i); err != nil {
				return err
			}
			if _, err := cold.Exec(t, fmt.Sprintf("INSERT INTO %s (v) VALUES ($1)", seqTable), 1000+i); err != nil {
				return err
			}
		}
		for _, tbl := range []struct {
			name string
			mark int64
		}{{idTable, idMax}, {seqTable, seqLast}} {
			count, err := cold.QueryInt(t, "SELECT count(*) FROM "+tbl.name)
			if err != nil {
				return err
			}
			if int(count) != first+second {
				return fmt.Errorf("%s has %d rows after inserting %d then %d more across a cold reconnect, want %d", tbl.name, count, first, second, first+second)
			}
			distinct, err := cold.QueryInt(t, "SELECT count(DISTINCT id) FROM "+tbl.name)
			if err != nil {
				return err
			}
			if distinct != count {
				return fmt.Errorf("%s has id reuse after the cold reconnect: %d distinct ids among %d rows, want them equal (the sequence/identity generator produced a duplicate id)", tbl.name, distinct, count)
			}
			newMin, err := cold.QueryInt(t, fmt.Sprintf("SELECT min(id) FROM %s WHERE v >= 1000", tbl.name))
			if err != nil {
				return err
			}
			if newMin <= tbl.mark {
				return fmt.Errorf("%s sequence regressed across the cold reconnect: the first post-reconnect id %d is <= the pre-reconnect high-water mark %d, want strictly greater (a sequence must never go backwards or reuse ids)", tbl.name, newMin, tbl.mark)
			}
		}
		return nil
	})
}

// runPartitionedTables checks RANGE-partition routing, temp-table invisibility
// from a second connection, and remaining data after a partition drop.
func runPartitionedTables(t *core.T) error {
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

	sfx := suffix(t)
	parent := "part_" + sfx
	parts := []string{parent + "_p0", parent + "_p1", parent + "_p2"}
	tempTable := "tmp_" + sfx
	rows, _ := genRows(t, 300)

	defer func() { _, _ = client.Exec(t, "DROP TABLE IF EXISTS "+parent) }()

	if err := t.Step("route_to_partitions", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int, v bigint, PRIMARY KEY (id)) PARTITION BY RANGE (id)", parent)); err != nil {
			return err
		}
		for i, p := range parts {
			if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s PARTITION OF %s FOR VALUES FROM (%d) TO (%d)", p, parent, i*100, (i+1)*100)); err != nil {
				return err
			}
		}
		for id, v := range rows {
			if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", parent), id, v); err != nil {
				return err
			}
		}
		for i, p := range parts {
			count, err := client.QueryInt(t, "SELECT count(*) FROM "+p)
			if err != nil {
				return err
			}
			if count != 100 {
				return fmt.Errorf("RANGE partition %s (partition %d of %s) has %d rows, want 100 (rows were routed to the wrong partition)", p, i, parent, count)
			}
		}
		total, err := client.QueryInt(t, "SELECT count(*) FROM "+parent)
		if err != nil {
			return err
		}
		if int(total) != len(rows) {
			return fmt.Errorf("parent table %s has %d rows visible across its partitions, want %d", parent, total, len(rows))
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("temp_table_invisible", func(s *core.StepHandle) error {
		// Temp tables are session-scoped; pin one pool connection for them.
		conn, err := client.Pool().Acquire(t)
		if err != nil {
			return err
		}
		defer conn.Release()
		if _, err := conn.Exec(t, fmt.Sprintf("CREATE TEMP TABLE %s (id int PRIMARY KEY)", tempTable)); err != nil {
			return err
		}
		if _, err := conn.Exec(t, fmt.Sprintf("INSERT INTO %s SELECT generate_series(1, 10)", tempTable)); err != nil {
			return err
		}
		var own int64
		if err := conn.QueryRow(t, "SELECT count(*) FROM "+tempTable).Scan(&own); err != nil {
			return err
		}
		if own != 10 {
			return fmt.Errorf("temp table %s has %d rows on its owning session, want 10", tempTable, own)
		}
		other, err := pg.Connect(t, env.PGEndpoint())
		if err != nil {
			return err
		}
		defer other.Close()
		if _, err := other.QueryInt(t, "SELECT count(*) FROM "+tempTable); err == nil {
			return fmt.Errorf("temp table %s is visible from a second connection, want it invisible (a CREATE TEMP TABLE is session-scoped and must not resolve on a foreign session)", tempTable)
		}
		return nil
	}); err != nil {
		return err
	}

	return t.Step("drop_partition", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, "DROP TABLE "+parts[2]); err != nil {
			return err
		}
		var wantSum int64
		for id, v := range rows {
			if id < 200 {
				wantSum += v
			}
		}
		count, err := client.QueryInt(t, "SELECT count(*) FROM "+parent)
		if err != nil {
			return err
		}
		if count != 200 {
			return fmt.Errorf("after DROP TABLE %s (one partition), parent %s has %d rows, want 200 (the dropped partition's rows should be gone, the rest intact)", parts[2], parent, count)
		}
		gotSum, err := client.QueryInt(t, "SELECT coalesce(sum(v),0) FROM "+parent)
		if err != nil {
			return err
		}
		if gotSum != wantSum {
			return fmt.Errorf("after dropping partition %s, sum(v) over %s = %d, want %d", parts[2], parent, gotSum, wantSum)
		}
		s.Set("remaining", count)
		return nil
	})
}

const (
	concWriters  = 8
	concRowsEach = 200
	concHotRows  = 10
	concIncrEach = 100
)

// runConcurrentWriters runs 8 goroutines on their own connections: disjoint-id
// inserts verified by exact count+sum, then concurrent per-row increments on a
// hot row-set verified against Go-side atomic counters.
func runConcurrentWriters(t *core.T) error {
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

	sfx := suffix(t)
	table := "conc_" + sfx
	hot := "conchot_" + sfx

	workers := make([]map[int]int64, concWriters)
	var wantSum int64
	for w := 0; w < concWriters; w++ {
		r := t.Seed.Fork(fmt.Sprintf("writer%d", w)).Rand()
		rows := make(map[int]int64, concRowsEach)
		for i := 0; i < concRowsEach; i++ {
			v := int64(r.Uint64() >> 24)
			rows[w*concRowsEach+i] = v
			wantSum += v
		}
		workers[w] = rows
	}

	if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v bigint)", table)); err != nil {
		return err
	}
	defer func() { _, _ = client.Exec(t, "DROP TABLE IF EXISTS "+table) }()

	if err := t.Step("concurrent_inserts", func(s *core.StepHandle) error {
		errs := make([]error, concWriters)
		var wg sync.WaitGroup
		for w := 0; w < concWriters; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				c, err := pg.Connect(t, env.PGEndpoint())
				if err != nil {
					errs[w] = err
					return
				}
				defer c.Close()
				for id, v := range workers[w] {
					if _, err := c.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), id, v); err != nil {
						errs[w] = err
						return
					}
				}
			}(w)
		}
		wg.Wait()
		for w, err := range errs {
			if err != nil {
				return fmt.Errorf("writer %d: %w", w, err)
			}
		}
		count, err := client.QueryInt(t, "SELECT count(*) FROM "+table)
		if err != nil {
			return err
		}
		if int(count) != concWriters*concRowsEach {
			return fmt.Errorf("after %d goroutines each inserted %d rows into disjoint id ranges concurrently, %s has %d rows, want %d (a concurrent insert was lost or duplicated)", concWriters, concRowsEach, table, count, concWriters*concRowsEach)
		}
		gotSum, err := client.QueryInt(t, "SELECT coalesce(sum(v),0) FROM "+table)
		if err != nil {
			return err
		}
		if gotSum != wantSum {
			return fmt.Errorf("after the concurrent-insert phase, sum(v) over %s = %d, want %d", table, gotSum, wantSum)
		}
		s.Set("rows", count)
		return nil
	}); err != nil {
		return err
	}

	if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v bigint)", hot)); err != nil {
		return err
	}
	defer func() { _, _ = client.Exec(t, "DROP TABLE IF EXISTS "+hot) }()

	return t.Step("concurrent_increments", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s SELECT g, 0 FROM generate_series(0, %d) g", hot, concHotRows-1)); err != nil {
			return err
		}
		var counters [concHotRows]int64
		errs := make([]error, concWriters)
		var wg sync.WaitGroup
		for w := 0; w < concWriters; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				c, err := pg.Connect(t, env.PGEndpoint())
				if err != nil {
					errs[w] = err
					return
				}
				defer c.Close()
				r := t.Seed.Fork(fmt.Sprintf("incr%d", w)).Rand()
				for i := 0; i < concIncrEach; i++ {
					id := int(r.Uint64() % concHotRows)
					if _, err := c.Exec(t, fmt.Sprintf("UPDATE %s SET v = v + 1 WHERE id = $1", hot), id); err != nil {
						errs[w] = err
						return
					}
					atomic.AddInt64(&counters[id], 1)
				}
			}(w)
		}
		wg.Wait()
		for w, err := range errs {
			if err != nil {
				return fmt.Errorf("incrementer %d: %w", w, err)
			}
		}
		var total int64
		for id := 0; id < concHotRows; id++ {
			got, err := client.QueryInt(t, fmt.Sprintf("SELECT v FROM %s WHERE id = $1", hot), id)
			if err != nil {
				return err
			}
			want := atomic.LoadInt64(&counters[id])
			if got != want {
				return fmt.Errorf("hot row id=%d has v=%d after %d goroutines concurrently incremented it, want %d (lost or extra UPDATE under concurrency)", id, got, concWriters, want)
			}
			total += got
		}
		if total != int64(concWriters*concIncrEach) {
			return fmt.Errorf("total increments across %d hot rows = %d, want %d (%d writers x %d increments each)", concHotRows, total, concWriters*concIncrEach, concWriters, concIncrEach)
		}
		s.Set("increments", total)
		return nil
	})
}
