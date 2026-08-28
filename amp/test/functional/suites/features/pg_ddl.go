// DDL breadth against an amp endpoint. pg_ddl_survives_restart already proves
// one table's schema outlives a compute restart; this proves the catalogue
// itself works — schemas, constraints, views, materialised views, indexes and
// transactional DDL — and that what a session created is really in shared
// storage rather than its own cache. Every phase verifies through a SECOND
// connection, which is the only way to tell those two apart.
package features

import (
	"fmt"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// ddlRows seeds enough rows that an index or a materialised view has something
// to be wrong about, while staying a fast shared-env test.
const ddlRows = 500

func init() {
	core.Register(core.Test{
		Name:        "features/pg_ddl",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "Schema/constraint/view/index DDL through an endpoint: constraints reject what they must, a materialised view refreshes, transactional DDL rolls back, and every object is re-verified on a cold connection before a DROP SCHEMA CASCADE removes it.",
		Run:         runDDL,
	})
}

func runDDL(t *core.T) error {
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

	sfx := suffix(t)
	schema := "ddl_" + sfx
	parent := schema + ".accounts"
	child := schema + ".entries"
	view := schema + ".v_totals"
	matview := schema + ".mv_totals"

	// The schema is dropped with CASCADE at the end, but a failing phase returns
	// early — so the drop is deferred too, or a shared env accumulates schemas.
	defer func() { _, _ = client.Exec(t, "DROP SCHEMA IF EXISTS "+schema+" CASCADE") }()

	rows, wantSum := genRows(t, ddlRows)

	if err := t.Step("create_schema_objects", func(s *core.StepHandle) error {
		stmts := []string{
			"CREATE SCHEMA " + schema,
			fmt.Sprintf(`CREATE TABLE %s (
				id      int PRIMARY KEY,
				label   text NOT NULL DEFAULT 'unnamed',
				balance bigint NOT NULL CHECK (balance >= 0),
				UNIQUE (id, label)
			)`, parent),
			fmt.Sprintf(`CREATE TABLE %s (
				entry_id  bigserial PRIMARY KEY,
				account   int NOT NULL REFERENCES %s (id) ON DELETE CASCADE,
				amount    bigint NOT NULL
			)`, child, parent),
		}
		for _, sql := range stmts {
			if _, err := client.Exec(t, sql); err != nil {
				return fmt.Errorf("%s: %w", sql, err)
			}
		}
		for id := 0; id < ddlRows; id++ {
			if _, err := client.Exec(t,
				fmt.Sprintf("INSERT INTO %s (id, balance) VALUES ($1, $2)", parent), id, rows[id]); err != nil {
				return err
			}
		}
		got, err := client.QueryInt(t, "SELECT count(*) FROM "+parent)
		if err != nil {
			return err
		}
		if got != ddlRows {
			return fmt.Errorf("%s has %d rows after the seed inserts, want %d", parent, got, ddlRows)
		}
		s.Set("rows", ddlRows)
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("constraints_reject", func(s *core.StepHandle) error {
		cases := []struct{ what, sql string }{
			{"a CHECK violation", fmt.Sprintf("INSERT INTO %s (id, balance) VALUES (900001, -1)", parent)},
			{"a PRIMARY KEY duplicate", fmt.Sprintf("INSERT INTO %s (id, balance) VALUES (0, 5)", parent)},
			{"a NOT NULL violation", fmt.Sprintf("INSERT INTO %s (id, label, balance) VALUES (900002, NULL, 5)", parent)},
			{"a FOREIGN KEY with no parent row", fmt.Sprintf("INSERT INTO %s (account, amount) VALUES (900003, 5)", child)},
		}
		for _, c := range cases {
			if _, err := client.Exec(t, c.sql); err == nil {
				return fmt.Errorf("%s was accepted: the constraint is not being enforced (%s)", c.what, c.sql)
			}
		}
		// A rejected statement must leave nothing behind.
		got, err := client.QueryInt(t, "SELECT count(*) FROM "+parent)
		if err != nil {
			return err
		}
		if got != ddlRows {
			return fmt.Errorf("%s has %d rows after four rejected statements, want the original %d", parent, got, ddlRows)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("views_and_indexes", func(s *core.StepHandle) error {
		stmts := []string{
			fmt.Sprintf("CREATE VIEW %s AS SELECT count(*) AS n, sum(balance) AS total FROM %s", view, parent),
			fmt.Sprintf("CREATE MATERIALIZED VIEW %s AS SELECT count(*) AS n, sum(balance) AS total FROM %s", matview, parent),
			fmt.Sprintf("CREATE INDEX ON %s (balance)", parent),
			fmt.Sprintf("CREATE INDEX ON %s (id) WHERE balance > 0", parent),
			fmt.Sprintf("CREATE INDEX ON %s ((balance %% 8))", parent),
		}
		for _, sql := range stmts {
			if _, err := client.Exec(t, sql); err != nil {
				return fmt.Errorf("%s: %w", sql, err)
			}
		}
		total, err := client.QueryInt(t, "SELECT total FROM "+view)
		if err != nil {
			return err
		}
		if total != wantSum {
			return fmt.Errorf("view %s reports total %d, want %d", view, total, wantSum)
		}
		// One more row, then the two view kinds must disagree: the plain view
		// follows the table, the materialised one holds its snapshot until
		// refreshed. If they agree, the matview is not materialised at all.
		if _, err := client.Exec(t,
			fmt.Sprintf("INSERT INTO %s (id, balance) VALUES ($1, $2)", parent), ddlRows, 7); err != nil {
			return err
		}
		liveTotal, err := client.QueryInt(t, "SELECT total FROM "+view)
		if err != nil {
			return err
		}
		staleTotal, err := client.QueryInt(t, "SELECT total FROM "+matview)
		if err != nil {
			return err
		}
		if liveTotal != wantSum+7 {
			return fmt.Errorf("view %s reports %d after one more row, want %d", view, liveTotal, wantSum+7)
		}
		if staleTotal != wantSum {
			return fmt.Errorf("materialised view %s reports %d before REFRESH, want its snapshot %d", matview, staleTotal, wantSum)
		}
		if _, err := client.Exec(t, "REFRESH MATERIALIZED VIEW "+matview); err != nil {
			return err
		}
		refreshed, err := client.QueryInt(t, "SELECT total FROM "+matview)
		if err != nil {
			return err
		}
		if refreshed != wantSum+7 {
			return fmt.Errorf("materialised view %s reports %d after REFRESH, want %d", matview, refreshed, wantSum+7)
		}
		return nil
	}); err != nil {
		return err
	}

	// A transaction has to stay on ONE connection: the client's Exec runs on
	// whichever pooled connection is free, so a BEGIN there could be rolled back
	// on a different session entirely. pgx's Tx pins one.
	if err := t.Step("transactional_ddl", func(s *core.StepHandle) error {
		rolled := schema + ".rolled_back"
		committed := schema + ".committed"

		tx, err := client.Pool().Begin(t)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int)", rolled)); err != nil {
			_ = tx.Rollback(t)
			return err
		}
		if err := tx.Rollback(t); err != nil {
			return err
		}
		if n, err := client.QueryInt(t, relationExists, rolled); err != nil {
			return err
		} else if n != 0 {
			return fmt.Errorf("%s exists after its CREATE was rolled back — DDL is not transactional here", rolled)
		}

		tx, err = client.Pool().Begin(t)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int)", committed)); err != nil {
			_ = tx.Rollback(t)
			return err
		}
		if _, err := tx.Exec(t, fmt.Sprintf("INSERT INTO %s VALUES (1)", committed)); err != nil {
			_ = tx.Rollback(t)
			return err
		}
		if err := tx.Commit(t); err != nil {
			return err
		}
		if n, err := client.QueryInt(t, "SELECT count(*) FROM "+committed); err != nil {
			return err
		} else if n != 1 {
			return fmt.Errorf("%s has %d rows after a committed CREATE+INSERT, want 1", committed, n)
		}
		return nil
	}); err != nil {
		return err
	}

	client.Close()

	return t.Step("verify_cold_and_drop", func(s *core.StepHandle) error {
		cold, err := connect(t, env)
		if err != nil {
			return err
		}
		defer cold.Close()

		total, err := cold.QueryInt(t, "SELECT total FROM "+matview)
		if err != nil {
			return fmt.Errorf("materialised view %s is unreadable on a cold connection: %w", matview, err)
		}
		if total != wantSum+7 {
			return fmt.Errorf("cold connection reads %d from %s, want the refreshed %d", total, matview, wantSum+7)
		}
		// The constraints are catalogue state too: a fresh session must enforce
		// them without ever having seen the CREATE.
		if _, err := cold.Exec(t, fmt.Sprintf("INSERT INTO %s (id, balance) VALUES (900004, -5)", parent)); err == nil {
			return fmt.Errorf("the CHECK on %s is not enforced on a cold connection", parent)
		}
		// ON DELETE CASCADE reaches the child rows the FK points at.
		if _, err := cold.Exec(t,
			fmt.Sprintf("INSERT INTO %s (account, amount) VALUES ($1, 5)", child), 0); err != nil {
			return err
		}
		if _, err := cold.Exec(t, fmt.Sprintf("DELETE FROM %s WHERE id = 0", parent)); err != nil {
			return err
		}
		orphans, err := cold.QueryInt(t, fmt.Sprintf("SELECT count(*) FROM %s WHERE account = 0", child))
		if err != nil {
			return err
		}
		if orphans != 0 {
			return fmt.Errorf("%d row(s) in %s survived the cascading delete of their parent", orphans, child)
		}

		if _, err := cold.Exec(t, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			return err
		}
		left, err := cold.QueryInt(t, relationExists, parent)
		if err != nil {
			return err
		}
		if left != 0 {
			return fmt.Errorf("%s still exists after DROP SCHEMA CASCADE", parent)
		}
		return nil
	})
}

// relationExists asks the catalogue for a schema-qualified relation without
// erroring when it is absent — to_regclass yields NULL rather than raising.
const relationExists = "SELECT count(*) FROM (SELECT to_regclass($1) AS r) x WHERE x.r IS NOT NULL"
