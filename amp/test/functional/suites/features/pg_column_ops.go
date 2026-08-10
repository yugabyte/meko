// ALTER TABLE column commands against an amp endpoint.
//
// A column change is the one DDL that has to reach every stored row: adding a
// column with a default, widening a type, or dropping a column all change how
// existing tuples are read back. On amp those tuples live in the pageserver, not
// in the compute — so each phase verifies on a cold connection as well as the
// session that issued the ALTER. Anything that only updated the catalogue in
// memory shows up here as a value that changed when the session did.
package features

import (
	"fmt"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const columnOpsRows = 400

func init() {
	core.Register(core.Test{
		Name:        "features/pg_column_ops",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "ALTER TABLE column commands on a seeded table — ADD (with and without default), SET/DROP DEFAULT, SET/DROP NOT NULL, ALTER TYPE with a USING cast, RENAME, ADD/DROP CONSTRAINT, DROP COLUMN — each verified against the pre-existing rows and re-verified on a cold connection.",
		Run:         runColumnOps,
	})
}

func runColumnOps(t *core.T) error {
	env, ok := t.Env.(pgEnv)
	if !ok {
		t.Skip("requires an env that exposes a PGEndpoint (sharedk8s or universe)")
		return nil
	}
	client, err := connect(t, env)
	if err != nil {
		return err
	}
	defer client.Close()

	table := "colops_" + suffix(t)
	defer func() { _, _ = client.Exec(t, "DROP TABLE IF EXISTS "+table) }()

	rows, wantSum := genRows(t, columnOpsRows)

	if err := t.Step("seed", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, fmt.Sprintf(
			"CREATE TABLE %s (id int PRIMARY KEY, v bigint NOT NULL)", table)); err != nil {
			return err
		}
		for id := 0; id < columnOpsRows; id++ {
			if _, err := client.Exec(t,
				fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), id, rows[id]); err != nil {
				return err
			}
		}
		s.Set("rows", columnOpsRows)
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("add_columns", func(s *core.StepHandle) error {
		stmts := []string{
			fmt.Sprintf("ALTER TABLE %s ADD COLUMN note text", table),
			fmt.Sprintf("ALTER TABLE %s ADD COLUMN tier int NOT NULL DEFAULT 7", table),
			fmt.Sprintf("ALTER TABLE %s ADD COLUMN ratio numeric(10,2) DEFAULT 1.25", table),
		}
		for _, sql := range stmts {
			if _, err := client.Exec(t, sql); err != nil {
				return fmt.Errorf("%s: %w", sql, err)
			}
		}
		// The pre-existing rows must show the defaults for the new columns and
		// NULL for the one that has none — that is the rewrite (or the catalogue's
		// missing-value machinery) actually working for stored tuples.
		if err := expectCount(t, client, table, "note IS NULL", columnOpsRows,
			"a new column with no default did not read back as NULL on existing rows"); err != nil {
			return err
		}
		if err := expectCount(t, client, table, "tier = 7", columnOpsRows,
			"a new NOT NULL column's default did not apply to existing rows"); err != nil {
			return err
		}
		return expectCount(t, client, table, "ratio = 1.25", columnOpsRows,
			"a new column's numeric default did not apply to existing rows")
	}); err != nil {
		return err
	}

	if err := t.Step("defaults_and_nullability", func(s *core.StepHandle) error {
		// A changed default applies to new rows only; the old ones keep what they
		// were given when the column was added.
		if _, err := client.Exec(t, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN tier SET DEFAULT 9", table)); err != nil {
			return err
		}
		if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), columnOpsRows, 1); err != nil {
			return err
		}
		if err := expectCount(t, client, table, "tier = 9", 1,
			"a row inserted after SET DEFAULT did not take the new default"); err != nil {
			return err
		}
		if err := expectCount(t, client, table, "tier = 7", columnOpsRows,
			"SET DEFAULT rewrote rows that already existed"); err != nil {
			return err
		}

		if _, err := client.Exec(t, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN tier DROP DEFAULT", table)); err != nil {
			return err
		}
		if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), columnOpsRows+1, 1); err == nil {
			return fmt.Errorf("an insert omitting a NOT NULL column with no default was accepted")
		}

		// NOT NULL can only be set once the column has no NULLs — backfill, then
		// enforce, then check the constraint actually bites.
		if _, err := client.Exec(t, fmt.Sprintf("UPDATE %s SET note = 'filled' WHERE note IS NULL", table)); err != nil {
			return err
		}
		if _, err := client.Exec(t, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN note SET NOT NULL", table)); err != nil {
			return err
		}
		if _, err := client.Exec(t, fmt.Sprintf("UPDATE %s SET note = NULL WHERE id = 0", table)); err == nil {
			return fmt.Errorf("SET NOT NULL is not enforced: a NULL update was accepted")
		}
		if _, err := client.Exec(t, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN note DROP NOT NULL", table)); err != nil {
			return err
		}
		if _, err := client.Exec(t, fmt.Sprintf("UPDATE %s SET note = NULL WHERE id = 0", table)); err != nil {
			return fmt.Errorf("after DROP NOT NULL a NULL update was still rejected: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("retype_and_rename", func(s *core.StepHandle) error {
		// Widening keeps every value; the sum is the cheap total check.
		if _, err := client.Exec(t, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN v TYPE numeric", table)); err != nil {
			return err
		}
		sum, err := client.QueryInt(t, fmt.Sprintf("SELECT sum(v)::bigint FROM %s WHERE id < %d", table, columnOpsRows))
		if err != nil {
			return err
		}
		if sum != wantSum {
			return fmt.Errorf("after ALTER TYPE bigint→numeric the sum is %d, want %d — the rewrite lost or changed values", sum, wantSum)
		}
		// A cast that needs USING: text → int on a column whose values are digits.
		if _, err := client.Exec(t, fmt.Sprintf("UPDATE %s SET note = (id * 2)::text", table)); err != nil {
			return err
		}
		if _, err := client.Exec(t, fmt.Sprintf(
			"ALTER TABLE %s ALTER COLUMN note TYPE int USING note::int", table)); err != nil {
			return err
		}
		if err := expectCount(t, client, table, "note = id * 2", columnOpsRows+1,
			"a USING cast did not preserve the values it converted"); err != nil {
			return err
		}
		if _, err := client.Exec(t, fmt.Sprintf("ALTER TABLE %s RENAME COLUMN note TO doubled", table)); err != nil {
			return err
		}
		return expectCount(t, client, table, "doubled = id * 2", columnOpsRows+1,
			"the renamed column does not carry the values it had before the rename")
	}); err != nil {
		return err
	}

	if err := t.Step("constraints", func(s *core.StepHandle) error {
		cname := "chk_" + suffix(t)
		if _, err := client.Exec(t, fmt.Sprintf(
			"ALTER TABLE %s ADD CONSTRAINT %s CHECK (tier > 0)", table, cname)); err != nil {
			return err
		}
		if _, err := client.Exec(t, fmt.Sprintf("UPDATE %s SET tier = 0 WHERE id = 1", table)); err == nil {
			return fmt.Errorf("the CHECK added by ALTER TABLE is not enforced")
		}
		if _, err := client.Exec(t, fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", table, cname)); err != nil {
			return err
		}
		if _, err := client.Exec(t, fmt.Sprintf("UPDATE %s SET tier = 0 WHERE id = 1", table)); err != nil {
			return fmt.Errorf("after DROP CONSTRAINT the update was still rejected: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("drop_column", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, fmt.Sprintf("ALTER TABLE %s DROP COLUMN ratio", table)); err != nil {
			return err
		}
		if _, err := client.QueryInt(t, fmt.Sprintf("SELECT count(ratio) FROM %s", table)); err == nil {
			return fmt.Errorf("the dropped column ratio is still selectable")
		}
		// The surviving columns must be untouched by the drop.
		sum, err := client.QueryInt(t, fmt.Sprintf("SELECT sum(v)::bigint FROM %s WHERE id < %d", table, columnOpsRows))
		if err != nil {
			return err
		}
		if sum != wantSum {
			return fmt.Errorf("after DROP COLUMN the sum of v is %d, want %d", sum, wantSum)
		}
		return nil
	}); err != nil {
		return err
	}

	client.Close()

	return t.Step("verify_cold", func(s *core.StepHandle) error {
		cold, err := connect(t, env)
		if err != nil {
			return err
		}
		defer cold.Close()
		defer func() { _, _ = cold.Exec(t, "DROP TABLE IF EXISTS "+table) }()

		sum, err := cold.QueryInt(t, fmt.Sprintf("SELECT sum(v)::bigint FROM %s WHERE id < %d", table, columnOpsRows))
		if err != nil {
			return err
		}
		if sum != wantSum {
			return fmt.Errorf("a cold session reads sum(v) = %d, want %d — the column rewrites did not reach shared storage", sum, wantSum)
		}
		if err := expectCount(t, cold, table, "doubled = id * 2", columnOpsRows+1,
			"the renamed, retyped column does not read back on a cold session"); err != nil {
			return err
		}
		if err := expectCount(t, cold, table, "tier = 7", columnOpsRows-1,
			"the added column's defaults do not read back on a cold session"); err != nil {
			return err
		}
		if _, err := cold.QueryInt(t, fmt.Sprintf("SELECT count(ratio) FROM %s", table)); err == nil {
			return fmt.Errorf("a cold session can still select the dropped column ratio")
		}
		return nil
	})
}

// expectCount asserts how many rows satisfy a predicate, naming what the count
// means when it does not match.
func expectCount(t *core.T, client *pg.Client, table, predicate string, want int, why string) error {
	got, err := client.QueryInt(t, fmt.Sprintf("SELECT count(*) FROM %s WHERE %s", table, predicate))
	if err != nil {
		return err
	}
	if int(got) != want {
		return fmt.Errorf("%s: %d row(s) match %q, want %d", why, got, predicate, want)
	}
	return nil
}
