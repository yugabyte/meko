// k8s endpoints ran with full_page_writes=on for weeks because spec.rs never set
// the intended off (meko#1050). With it off, visibility-map and freespace pages
// are rebuilt by the pageserver from WAL records alone, so this test asserts the
// GUCs and then that VM bits stay correct across a compute restart.
package features

import (
	"context"
	"fmt"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	walDefaultsTable = "amp_wal_defaults"
	walDefaultsRows  = 20000
)

// walDefaultGUCs is the WAL shape every k8s endpoint must run with.
var walDefaultGUCs = map[string]string{
	"full_page_writes": "off",
	"wal_log_hints":    "off",
}

// vmSnapshot is what the visibility map says about the table, plus the data.
type vmSnapshot struct {
	rows       int64
	checksum   string
	allVisible int64
	allFrozen  int64
	badVisible int64
	badFrozen  int64
}

func init() {
	core.Register(core.Test{
		Name:        "features/pg_wal_defaults",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "Assert full_page_writes and wal_log_hints are off on a k8s endpoint, then that a VACUUMed table's visibility map is correct before a compute restart, identical after it, and still correct after more updates without full-page images.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runPGWALDefaults,
	})
}

func runPGWALDefaults(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"wal_defaults", a.walDefaults}}
	})
}

func (a *ampFeatureRun) walDefaults(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	ep, err := createEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID)
	if err != nil {
		return err
	}
	c, err := a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	for guc, want := range walDefaultGUCs {
		got, err := c.QueryString(ctx, "SHOW "+guc)
		if err != nil {
			c.Close()
			return fmt.Errorf("SHOW %s: %w", guc, err)
		}
		if got != want {
			c.Close()
			return fmt.Errorf("%s is %q on a k8s endpoint, want %q (meko#1050: the supervisor's spec must set it, the local install is not the only place)", guc, got, want)
		}
	}
	if err := seedAndVacuum(ctx, c, 1); err != nil {
		c.Close()
		return err
	}
	before, err := snapshotVM(ctx, c)
	c.Close()
	if err != nil {
		return err
	}
	if err := before.assertCorrect("before the compute restart"); err != nil {
		return err
	}
	if before.allVisible == 0 {
		return fmt.Errorf("VACUUM FREEZE left no page all-visible in %s, so the visibility map is not being maintained", walDefaultsTable)
	}
	a.f.logf("wal_defaults: %d rows, %d all-visible and %d all-frozen pages before the restart", before.rows, before.allVisible, before.allFrozen)

	c2, ok, err := a.restartEndpointCompute(ctx, ep)
	if err != nil || !ok {
		return err
	}
	defer c2.Close()
	after, err := snapshotVM(ctx, c2)
	if err != nil {
		return err
	}
	if after.rows != before.rows || after.checksum != before.checksum {
		return fmt.Errorf("%s changed across the compute restart: %d rows/%s before, %d rows/%s after", walDefaultsTable, before.rows, before.checksum, after.rows, after.checksum)
	}
	if err := after.assertCorrect("after the compute restart"); err != nil {
		return err
	}
	if after.allVisible != before.allVisible || after.allFrozen != before.allFrozen {
		return fmt.Errorf("the visibility map of %s changed across a compute restart with full_page_writes=off: %d all-visible/%d all-frozen pages before, %d/%d after; the pageserver rebuilt the VM fork differently from what Postgres wrote", walDefaultsTable, before.allVisible, before.allFrozen, after.allVisible, after.allFrozen)
	}
	if err := seedAndVacuum(ctx, c2, 2); err != nil {
		return err
	}
	again, err := snapshotVM(ctx, c2)
	if err != nil {
		return err
	}
	if again.rows != after.rows {
		return fmt.Errorf("row count moved from %d to %d across an UPDATE-only round", after.rows, again.rows)
	}
	if err := again.assertCorrect("after updates on the restarted compute"); err != nil {
		return err
	}
	return assertIndexOnlyAgrees(ctx, c2)
}

// assertIndexOnlyAgrees is the user-visible form of a wrong VM bit: an
// index-only scan trusts all-visible pages and would count dead rows.
func assertIndexOnlyAgrees(ctx context.Context, c sqlConn) error {
	if _, err := c.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, "SET enable_bitmapscan = off"); err != nil {
		return err
	}
	plan, err := c.QueryString(ctx, "EXPLAIN (FORMAT JSON) SELECT count(id) FROM "+walDefaultsTable+" WHERE id > 0")
	if err != nil {
		return fmt.Errorf("explain the index-only count: %w", err)
	}
	indexOnly, err := c.QueryInt(ctx, "SELECT count(id) FROM "+walDefaultsTable+" WHERE id > 0")
	if err != nil {
		return fmt.Errorf("index-only count: %w", err)
	}
	if _, err := c.Exec(ctx, "RESET enable_seqscan"); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, "RESET enable_bitmapscan"); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, "SET enable_indexonlyscan = off"); err != nil {
		return err
	}
	heap, err := c.QueryInt(ctx, "SELECT count(id) FROM "+walDefaultsTable+" WHERE id > 0")
	if err != nil {
		return fmt.Errorf("heap count: %w", err)
	}
	if _, err := c.Exec(ctx, "RESET enable_indexonlyscan"); err != nil {
		return err
	}
	if !strings.Contains(plan, "Index Only Scan") {
		return fmt.Errorf("the planner did not pick an index-only scan for the count (%s), so the visibility map cannot be judged through it", plan)
	}
	if indexOnly != heap {
		return fmt.Errorf("an index-only scan counts %d rows but a heap scan counts %d after the compute restart: the visibility map claims pages all-visible that hold dead tuples", indexOnly, heap)
	}
	return nil
}

// seedAndVacuum creates the table on round 1, then updates a third of it, deletes
// on round 1 only, checkpoints and freezes so VM bits are set without FPIs.
func seedAndVacuum(ctx context.Context, c sqlConn, round int) error {
	if round == 1 {
		if _, err := c.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_visibility"); err != nil {
			return fmt.Errorf("CREATE EXTENSION pg_visibility: %w", err)
		}
		if _, err := c.Exec(ctx, "CREATE TABLE "+walDefaultsTable+" (id int primary key, v int, pad text)"); err != nil {
			return fmt.Errorf("create table: %w", err)
		}
		if _, err := c.Exec(ctx, "INSERT INTO "+walDefaultsTable+" SELECT g, g, md5(g::text) FROM generate_series(1, $1) g", walDefaultsRows); err != nil {
			return fmt.Errorf("seed rows: %w", err)
		}
		if _, err := c.Exec(ctx, "DELETE FROM "+walDefaultsTable+" WHERE id % 10 = 0"); err != nil {
			return fmt.Errorf("delete rows: %w", err)
		}
	}
	if _, err := c.Exec(ctx, "UPDATE "+walDefaultsTable+" SET v = v + $1 WHERE id % 3 = 0", round); err != nil {
		return fmt.Errorf("update rows: %w", err)
	}
	if _, err := c.Exec(ctx, "CHECKPOINT"); err != nil {
		return fmt.Errorf("CHECKPOINT: %w", err)
	}
	if _, err := c.Exec(ctx, "VACUUM (FREEZE) "+walDefaultsTable); err != nil {
		return fmt.Errorf("VACUUM FREEZE: %w", err)
	}
	return nil
}

func snapshotVM(ctx context.Context, c sqlConn) (vmSnapshot, error) {
	var s vmSnapshot
	var err error
	if s.rows, err = c.QueryInt(ctx, "SELECT count(*) FROM "+walDefaultsTable); err != nil {
		return s, fmt.Errorf("count rows: %w", err)
	}
	if s.checksum, err = c.QueryString(ctx, "SELECT coalesce(md5(string_agg(id || ':' || v, ',' ORDER BY id)), '') FROM "+walDefaultsTable); err != nil {
		return s, fmt.Errorf("checksum rows: %w", err)
	}
	if s.allVisible, err = c.QueryInt(ctx, "SELECT count(*) FROM pg_visibility_map($1) WHERE all_visible", walDefaultsTable); err != nil {
		return s, fmt.Errorf("pg_visibility_map all_visible: %w", err)
	}
	if s.allFrozen, err = c.QueryInt(ctx, "SELECT count(*) FROM pg_visibility_map($1) WHERE all_frozen", walDefaultsTable); err != nil {
		return s, fmt.Errorf("pg_visibility_map all_frozen: %w", err)
	}
	if s.badVisible, err = c.QueryInt(ctx, "SELECT count(*) FROM pg_check_visible($1)", walDefaultsTable); err != nil {
		return s, fmt.Errorf("pg_check_visible: %w", err)
	}
	if s.badFrozen, err = c.QueryInt(ctx, "SELECT count(*) FROM pg_check_frozen($1)", walDefaultsTable); err != nil {
		return s, fmt.Errorf("pg_check_frozen: %w", err)
	}
	return s, nil
}

// assertCorrect is the wrong-results check: a VM bit set over a page holding a
// tuple that is not visible or frozen makes index-only scans return dead rows.
func (s vmSnapshot) assertCorrect(when string) error {
	if s.badVisible != 0 || s.badFrozen != 0 {
		return fmt.Errorf("%s, pg_check_visible reports %d and pg_check_frozen %d tuples whose visibility-map bits are wrong on %s; with full_page_writes=off the VM fork is rebuilt from WAL and index-only scans would now return dead rows", when, s.badVisible, s.badFrozen, walDefaultsTable)
	}
	return nil
}
