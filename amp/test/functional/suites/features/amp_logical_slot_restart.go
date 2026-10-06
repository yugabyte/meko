package features

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	lsTable    = "amp_ls"
	lsSlot     = "ampqa_ls"
	lsTxns     = 10
	lsConsumed = lsTxns / 2
)

var lsInsertID = regexp.MustCompile(`table public\.` + lsTable + `: INSERT: id\[integer\]:(\d+)`)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_logical_slot_restart",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "A logical replication slot across a compute restart. wal_level=logical is on for every compute. Create a test_decoding slot, commit ten transactions, consume half, then restart the compute pod: the slot must exist with the same confirmed_flush_lsn, the other half must decode exactly once and in order (which needs WAL older than the basebackup, fetched from the safekeeper), and new commits must keep decoding. A fork taken afterwards records whether the slot comes with it.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     30 * time.Minute,
		Run:         runAmpLogicalSlotRestart,
	})
}

func runAmpLogicalSlotRestart(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"logical_slot_restart", a.logicalSlotRestart}}
	})
}

func decodedIDs(ctx context.Context, c sqlReader, upto int) ([]int, error) {
	limit := "NULL"
	if upto > 0 {
		limit = strconv.Itoa(upto)
	}
	out, err := c.QueryString(ctx, fmt.Sprintf(
		"SELECT coalesce(string_agg(data, E'\\n' ORDER BY ord), '') FROM pg_logical_slot_get_changes('%s', NULL, %s) WITH ORDINALITY AS c(lsn, xid, data, ord)", lsSlot, limit))
	if err != nil {
		return nil, fmt.Errorf("decode from slot %s: %w", lsSlot, err)
	}
	var ids []int
	for _, m := range lsInsertID.FindAllStringSubmatch(out, -1) {
		n, _ := strconv.Atoi(m[1])
		ids = append(ids, n)
	}
	return ids, nil
}

func idRange(from, to int) []int {
	var out []int
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

func sameInts(a, b []int) bool {
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func (a *ampFeatureRun) logicalSlotRestart(ctx context.Context) error {
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
	defer func() { c.Close() }()

	if lvl, err := c.QueryString(ctx, "SHOW wal_level"); err != nil {
		return err
	} else if lvl != "logical" {
		return fmt.Errorf("wal_level is %q, but the endpoint supervisor sets logical on every compute", lvl)
	}
	if _, err := c.Exec(ctx, "CREATE TABLE "+lsTable+" (id int PRIMARY KEY, v text NOT NULL)"); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, fmt.Sprintf("SELECT pg_create_logical_replication_slot('%s', 'test_decoding')", lsSlot)); err != nil {
		return fmt.Errorf("create logical slot %s: %w", lsSlot, err)
	}
	for i := 1; i <= lsTxns; i++ {
		if _, err := c.Exec(ctx, fmt.Sprintf("INSERT INTO %s VALUES (%d, 'txn-%d')", lsTable, i, i)); err != nil {
			return err
		}
	}
	// Each transaction decodes to BEGIN, INSERT and COMMIT.
	first, err := decodedIDs(ctx, c, 3*lsConsumed)
	if err != nil {
		return err
	}
	if !sameInts(first, idRange(1, lsConsumed)) {
		return fmt.Errorf("before any restart, consuming %d transactions from the slot decoded ids %v, want %v", lsConsumed, first, idRange(1, lsConsumed))
	}
	confirmed, err := c.QueryString(ctx, "SELECT confirmed_flush_lsn::text FROM pg_replication_slots WHERE slot_name = $1", lsSlot)
	if err != nil {
		return err
	}
	c.Close()

	c, ok, err := a.restartEndpointCompute(ctx, ep)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	after, err := c.QueryString(ctx, "SELECT coalesce(max(confirmed_flush_lsn::text), 'missing') FROM pg_replication_slots WHERE slot_name = $1", lsSlot)
	if err != nil {
		return err
	}
	if after == "missing" {
		return fmt.Errorf("logical slot %s is gone after a compute restart: a slot is persistent state, and its consumer would have to resync from scratch", lsSlot)
	}
	if after != confirmed {
		return fmt.Errorf("logical slot %s has confirmed_flush_lsn %s after a compute restart, was %s: the consumer's acknowledged position was not kept", lsSlot, after, confirmed)
	}
	rest, err := decodedIDs(ctx, c, 0)
	if err != nil {
		return fmt.Errorf("decode the rest after a compute restart (this reads WAL older than the basebackup): %w", err)
	}
	if !sameInts(rest, idRange(lsConsumed+1, lsTxns)) {
		return fmt.Errorf("after a compute restart the slot decoded ids %v, want exactly %v once and in order", rest, idRange(lsConsumed+1, lsTxns))
	}
	for i := lsTxns + 1; i <= lsTxns+2; i++ {
		if _, err := c.Exec(ctx, fmt.Sprintf("INSERT INTO %s VALUES (%d, 'after')", lsTable, i)); err != nil {
			return err
		}
	}
	more, err := decodedIDs(ctx, c, 0)
	if err != nil {
		return err
	}
	if !sameInts(more, idRange(lsTxns+1, lsTxns+2)) {
		return fmt.Errorf("commits after the restart decoded as %v, want %v", more, idRange(lsTxns+1, lsTxns+2))
	}
	lsn, err := c.QueryString(ctx, "SELECT pg_current_wal_flush_lsn()::text")
	if err != nil {
		return err
	}
	return a.recordSlotOnFork(ctx, cl, lsn)
}

// The product has not said whether a fork inherits its parent's slots, so this
// records what it does rather than asserting either way.
func (a *ampFeatureRun) recordSlotOnFork(ctx context.Context, cl ampCtl, lsn string) error {
	name := "ls-fork-" + a.f.suffix()
	if _, err := cl.CreateBranchAndWait(ctx, a.proj.poolID, a.proj.projID, controller.CreateBranchRequest{Name: name, ParentBranchID: a.proj.branchID, ForkPointLSN: lsn}); err != nil {
		return fmt.Errorf("fork at %s: %w", lsn, err)
	}
	br, err := findBranchCtl(ctx, cl, a.proj.poolID, a.proj.projID, name)
	if err != nil {
		return err
	}
	ep, err := createEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, br.BranchID)
	if err != nil {
		return err
	}
	c, err := a.f.endpointSQL(ctx, ep)
	if err != nil {
		return err
	}
	defer c.Close()
	n, err := c.QueryInt(ctx, "SELECT count(*) FROM pg_replication_slots WHERE slot_name = $1", lsSlot)
	if err != nil {
		return err
	}
	rows, err := c.QueryInt(ctx, "SELECT count(*) FROM "+lsTable)
	if err != nil {
		return fmt.Errorf("read the fork: %w", err)
	}
	if rows != lsTxns+2 {
		return fmt.Errorf("the fork at %s holds %d rows, want %d", lsn, rows, lsTxns+2)
	}
	a.f.t.Metric("slot_on_fork", float64(n), "count")
	a.f.logf("amp_logical_slot_restart: a fork at %s carries %d copies of slot %s", lsn, n, lsSlot)
	if n > 0 {
		_, _ = c.Exec(ctx, fmt.Sprintf("SELECT pg_drop_replication_slot('%s')", lsSlot))
	}
	return nil
}
