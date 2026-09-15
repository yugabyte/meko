// Physical consistency of what the compute serves after a fault window.
//
// Every other verification in this tier reads rows back, which holds a page
// reconstructed from a delta chain to nothing: a heap whose line pointers
// disagree with its tuples answers a count perfectly well as long as the ids it
// returns are the right ones. amcheck reads the pages themselves.
package stress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

type endpointConnector interface {
	ConnectEndpoint(ctx context.Context, ep controller.Endpoint) (*pg.Client, error)
}

type consistencyTarget struct {
	Name     string
	Endpoint controller.Endpoint
}

type consistencyParams struct {
	Enabled      bool
	Timeout      time.Duration
	MaxRelations int
	// ProbeSQL replaces bt_index_check per index. Debug only: it exists so the
	// step can be shown to fail, since a page cannot be corrupted over the wire.
	ProbeSQL string
}

func consistencyParamsFromConfig(c *core.Config) consistencyParams {
	return consistencyParams{
		Enabled:      c.GetBool("stress.consistency.enabled", true),
		Timeout:      c.GetDuration("stress.consistency.timeout", 10*time.Minute),
		MaxRelations: c.GetInt("stress.consistency.max_relations", 0),
		ProbeSQL:     c.GetString("stress.consistency.probe_sql", ""),
	}
}

// Call this last, after the fault window has healed and the acked-id
// verification has run.
func verifyPhysicalConsistency(t *core.T, s *core.StepHandle, env endpointConnector, tenants []tenant) error {
	targets := make([]consistencyTarget, 0, len(tenants))
	for i := range tenants {
		targets = append(targets, consistencyTarget{
			Name:     fmt.Sprintf("tenant %d (%s)", tenants[i].Index, tenants[i].Name),
			Endpoint: tenants[i].Endpoint,
		})
	}
	return verifyEndpointConsistency(t, s, env, targets)
}

func verifyEndpointConsistency(t *core.T, s *core.StepHandle, env endpointConnector, targets []consistencyTarget) error {
	params := consistencyParamsFromConfig(t.Options)
	if !params.Enabled {
		s.Set("skipped", "stress.consistency.enabled is false")
		return nil
	}
	if len(targets) == 0 {
		return fmt.Errorf("no endpoint to check: a consistency step over nothing would pass without reading a page")
	}
	var rels, idxs int
	for _, tgt := range targets {
		r, i, timedOut, err := checkTarget(t, s, env, tgt, params)
		if err != nil {
			if timedOut {
				return fmt.Errorf("%s: the consistency check did not finish within stress.consistency.timeout (%s): %w",
					tgt.Name, params.Timeout, err)
			}
			return err
		}
		// Per endpoint, not over the sum: every target here owns at least one
		// table with a primary key, so a target that reads back none had its
		// relations lost, and a sum over its neighbours would hide that.
		if r == 0 && i == 0 {
			return fmt.Errorf("%s: the consistency check found no user relation and no index there: nothing was verified on that endpoint", tgt.Name)
		}
		s.Set(tgt.Name+".checked", fmt.Sprintf("%d relation(s), %d index(es)", r, i))
		rels += r
		idxs += i
	}
	s.Set("relations_checked", rels)
	s.Set("indexes_checked", idxs)
	// A run that checked nothing is a run that proved nothing, and without these
	// two numbers it looks identical to a run that checked everything.
	t.Metric("consistency_relations_checked", float64(rels), "count")
	t.Metric("consistency_indexes_checked", float64(idxs), "count")
	return nil
}

// The budget is per endpoint, and the whole check is retried once on a second
// connection: these are full-relation scans over a port-forward, and a dropped
// socket mid-scan is an environment failure reported as corruption.
func checkTarget(t *core.T, s *core.StepHandle, env endpointConnector, tgt consistencyTarget, params consistencyParams) (rels, idxs int, timedOut bool, err error) {
	ctx, cancel := context.WithTimeout(t.Context, params.Timeout)
	defer cancel()
	defer func() { timedOut = err != nil && ctx.Err() != nil && t.Context.Err() == nil }()
	var firstErr error
	for attempt := 1; attempt <= 2; attempt++ {
		conn, cerr := env.ConnectEndpoint(ctx, tgt.Endpoint)
		if cerr != nil {
			cerr = fmt.Errorf("%s: connection for the consistency check: %w", tgt.Name, cerr)
			if attempt == 2 || ctx.Err() != nil {
				return 0, 0, false, errors.Join(firstErr, cerr)
			}
			firstErr = cerr
			continue
		}
		r, i, cerr := checkEndpoint(ctx, s, conn, tgt.Name, params)
		conn.Close()
		if cerr == nil {
			return r, i, false, nil
		}
		// Only a severed socket is retried. A finding is about the pages and
		// must not be retried away.
		if attempt == 2 || ctx.Err() != nil || !isTransport(cerr) {
			return 0, 0, false, errors.Join(firstErr, cerr)
		}
		firstErr = cerr
		s.Set(tgt.Name+".retry", cerr.Error())
	}
	return 0, 0, false, firstErr
}

func isTransport(err error) bool {
	var te *pg.TransportError
	return errors.As(err, &te)
}

const consistencyRelationsSQL = `
SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'm') AND c.relpersistence = 'p'
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
ORDER BY c.oid`

const consistencyIndexesSQL = `
SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname)
FROM pg_index i
JOIN pg_class c ON c.oid = i.indexrelid
JOIN pg_class tb ON tb.oid = i.indrelid
JOIN pg_am am ON am.oid = c.relam
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE am.amname = 'btree' AND i.indisvalid AND i.indisready
  AND tb.relkind IN ('r', 'm') AND c.relpersistence = 'p'
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
ORDER BY c.oid`

const consistencyPrimaryKeyTablesSQL = `
SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname)
FROM pg_index i
JOIN pg_class c ON c.oid = i.indrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE i.indisprimary AND i.indisvalid AND c.relkind = 'r' AND c.relpersistence = 'p'
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
ORDER BY c.oid`

func checkEndpoint(ctx context.Context, s *core.StepHandle, conn *pg.Client, who string, params consistencyParams) (int, int, error) {
	if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS amcheck"); err != nil {
		return 0, 0, fmt.Errorf("%s: create amcheck: %w", who, err)
	}
	rels, err := queryNames(ctx, conn, consistencyRelationsSQL)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: list user relations: %w", who, err)
	}
	idxs, err := queryNames(ctx, conn, consistencyIndexesSQL)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: list btree indexes: %w", who, err)
	}
	rels = capRelations(s, who, "relations", rels, params.MaxRelations)
	idxs = capRelations(s, who, "indexes", idxs, params.MaxRelations)

	for _, idx := range idxs {
		if params.ProbeSQL != "" {
			if _, err := conn.Exec(ctx, params.ProbeSQL); err != nil {
				return 0, 0, fmt.Errorf("%s: stress.consistency.probe_sql failed on index %s: %w", who, idx, err)
			}
			continue
		}
		// heapallindexed: the index is held to the heap, not just to itself.
		if _, err := conn.Exec(ctx, "SELECT bt_index_check($1::regclass, true)", idx); err != nil {
			return 0, 0, fmt.Errorf("%s: amcheck bt_index_check over %s: the index and the heap disagree, so a page reconstructed after the fault is not the page that was written: %w", who, idx, err)
		}
	}

	heapam, err := hasVerifyHeapam(ctx, conn)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: look up verify_heapam: %w", who, err)
	}
	checkedRels := 0
	if !heapam {
		s.Set(who+".verify_heapam", "absent: this amcheck predates it, no heap page was read")
	}
	if heapam {
		for _, rel := range rels {
			if err := verifyHeap(ctx, conn, rel); err != nil {
				return 0, 0, fmt.Errorf("%s: %w", who, err)
			}
			checkedRels++
		}
	}

	pkTables, err := queryNames(ctx, conn, consistencyPrimaryKeyTablesSQL)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: list primary-key tables: %w", who, err)
	}
	pkTables = capRelations(s, who, "pk_tables", pkTables, params.MaxRelations)
	for _, tbl := range pkTables {
		if err := reconcileHeapAndIndex(ctx, conn, tbl); err != nil {
			return 0, 0, fmt.Errorf("%s: %w", who, err)
		}
	}

	for _, cat := range []string{"pg_catalog.pg_class", "pg_catalog.pg_attribute", "pg_catalog.pg_index"} {
		if _, err := conn.QueryInt(ctx, "SELECT count(*) FROM "+cat); err != nil {
			return 0, 0, fmt.Errorf("%s: %s cannot be read back after the fault: %w", who, cat, err)
		}
	}
	return checkedRels, len(idxs), nil
}

func capRelations(s *core.StepHandle, who, kind string, names []string, max int) []string {
	if max <= 0 || len(names) <= max {
		return names
	}
	s.Set(fmt.Sprintf("%s.%s_skipped", who, kind),
		fmt.Sprintf("%d of %d over stress.consistency.max_relations=%d", len(names)-max, len(names), max))
	return names[:max]
}

func hasVerifyHeapam(ctx context.Context, conn *pg.Client) (bool, error) {
	n, err := conn.QueryInt(ctx, "SELECT count(*) FROM pg_proc WHERE proname = 'verify_heapam'")
	return n > 0, err
}

// verifyHeap reads every page of rel. verify_heapam reports corruption as ROWS,
// not as an error, so an unchecked call passes on a corrupt heap.
func verifyHeap(ctx context.Context, conn *pg.Client, rel string) error {
	var found []string
	err := conn.Query(ctx, func(rows pgx.Rows) error {
		if len(found) >= 5 {
			return nil
		}
		var blkno, offnum *int64
		var msg string
		if err := rows.Scan(&blkno, &offnum, &msg); err != nil {
			return err
		}
		found = append(found, fmt.Sprintf("block %s offset %s: %s", nullableInt(blkno), nullableInt(offnum), msg))
		return nil
	}, "SELECT blkno, offnum, msg FROM verify_heapam($1::regclass)", rel)
	if err != nil {
		return fmt.Errorf("amcheck verify_heapam over %s: %w", rel, err)
	}
	if len(found) > 0 {
		return fmt.Errorf("amcheck verify_heapam reports %s as corrupt: %s", rel, strings.Join(found, "; "))
	}
	return nil
}

func nullableInt(v *int64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *v)
}

// One repeatable-read transaction: a concurrent writer would otherwise make the
// two counts differ legitimately.
func reconcileHeapAndIndex(ctx context.Context, conn *pg.Client, tbl string) error {
	sess, err := conn.Pin(ctx)
	if err != nil {
		return fmt.Errorf("pin a session to reconcile %s: %w", tbl, err)
	}
	defer sess.Close()
	if _, err := sess.Exec(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ"); err != nil {
		return fmt.Errorf("open the reconciliation snapshot for %s: %w", tbl, err)
	}
	defer func() { _, _ = sess.Exec(context.WithoutCancel(ctx), "ROLLBACK") }()

	if err := setLocal(ctx, sess, map[string]string{
		"enable_indexscan": "off", "enable_indexonlyscan": "off", "enable_bitmapscan": "off", "enable_seqscan": "on",
	}); err != nil {
		return fmt.Errorf("force the sequential plan for %s: %w", tbl, err)
	}
	heap, err := sess.QueryInt(ctx, "SELECT count(*) FROM "+tbl)
	if err != nil {
		return fmt.Errorf("sequential count of %s: %w", tbl, err)
	}
	if err := setLocal(ctx, sess, map[string]string{
		"enable_indexscan": "on", "enable_indexonlyscan": "on", "enable_bitmapscan": "on", "enable_seqscan": "off",
	}); err != nil {
		return fmt.Errorf("force the index plan for %s: %w", tbl, err)
	}
	countSQL := "SELECT count(*) FROM " + tbl
	// enable_seqscan = off is a cost penalty, not a prohibition: without this
	// the leg can take the same sequential path and agree by construction.
	if err := requireSessionPlanUsesAnIndex(ctx, sess, countSQL); err != nil {
		return err
	}
	viaIndex, err := sess.QueryInt(ctx, countSQL)
	if err != nil {
		return fmt.Errorf("index count of %s: %w", tbl, err)
	}
	if heap != viaIndex {
		return fmt.Errorf("%s holds %d row(s) by a sequential scan and %d through an index in one snapshot: the heap and the index disagree about what is stored", tbl, heap, viaIndex)
	}
	return nil
}

// Any index will do; which one the planner picks is its business, and pinning
// the primary key fails the moment the workload adds a second index the
// index-only scan prefers (result 4303c440).
func requireSessionPlanUsesAnIndex(ctx context.Context, sess pg.Session, sql string) error {
	plan, err := sess.QueryString(ctx, "EXPLAIN (FORMAT JSON) "+sql)
	if err != nil {
		return fmt.Errorf("explain %q: %w", sql, err)
	}
	indexed, seq, err := planScanKinds(plan)
	if err != nil {
		return fmt.Errorf("read the plan of %q: %w: %s", sql, err, plan)
	}
	if !indexed || seq {
		return fmt.Errorf("the count meant to read an index does not use one, so this leg would agree with the heap without touching the index:\nquery: %s\nplan:\n%s", sql, plan)
	}
	return nil
}

// planScanKinds walks an EXPLAIN (FORMAT JSON) tree for the two facts the leg
// turns on. Decoded rather than matched as text: nothing makes EXPLAIN's
// spacing part of the contract.
func planScanKinds(plan string) (indexed, seq bool, err error) {
	var tree any
	if err := json.Unmarshal([]byte(plan), &tree); err != nil {
		return false, false, err
	}
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if s, ok := v["Index Name"].(string); ok && s != "" {
				indexed = true
			}
			if s, ok := v["Node Type"].(string); ok && s == "Seq Scan" {
				seq = true
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(tree)
	return indexed, seq, nil
}

func setLocal(ctx context.Context, sess pg.Session, gucs map[string]string) error {
	for guc, val := range gucs {
		if _, err := sess.Exec(ctx, "SET LOCAL "+guc+" = "+val); err != nil {
			return err
		}
	}
	return nil
}

func queryNames(ctx context.Context, conn *pg.Client, sql string) ([]string, error) {
	var out []string
	err := conn.Query(ctx, func(rows pgx.Rows) error {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		out = append(out, name)
		return nil
	}, sql)
	return out, err
}
