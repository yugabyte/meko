// A Temporal-visibility-shaped workload over a multi-column GIN index mixing
// btree_gin scalar opclasses with a pg_trgm text opclass. Every answer is
// compared against a Go-built oracle over the seeded dataset, so a wrong or
// stale index shows up as wrong rows, not just a wrong count.
package features

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// Knob defaults for the seeded dataset (the second batch lands after the index
// exists, which is what exercises the GIN pending-list merge).
const (
	DefaultGinSearchRows      = 5000
	DefaultGinSearchExtraRows = 1000
)

// pg_stat_user_indexes counters are flushed when the backend that ran the query
// next goes idle, so a scan shows up shortly after the query returns.
const (
	idxScanPollTimeout  = 20 * time.Second
	idxScanPollInterval = 250 * time.Millisecond
)

// errVisIndexUnused marks the index-usage failure specifically, so the
// self-check can tell it apart from a wrong-rows failure.
var errVisIndexUnused = errors.New("the GIN index was not used")

// ginMode is one planner configuration a predicate runs under. sets is applied
// as SET LOCAL statements inside the plpgsql helpers.
type ginMode struct {
	how       string
	sets      string
	wantIndex bool
}

var (
	ginPlannerChoice = ginMode{how: "the planner's own choice"}
	ginForcedIndex   = ginMode{how: "enable_seqscan off", sets: "SET LOCAL enable_seqscan = off", wantIndex: true}
	// ginNoIndexProbe is the self-check: the index-usage assertion must fail here.
	ginNoIndexProbe = ginMode{
		how:       "index scans disabled",
		sets:      "SET LOCAL enable_indexscan = off;SET LOCAL enable_bitmapscan = off;SET LOCAL enable_indexonlyscan = off",
		wantIndex: true,
	}
)

var (
	visNamespaces = buildVisValues("namespace-%02d", 10)
	visWorkflows  = buildVisValues("wf-type-%02d", 20)
	visStatuses   = []int{0, 1, 2, 3, 4, 5}
	// Memo words are long enough to carry several trigrams each, so LIKE
	// '%word%' is answerable from the trigram opclass.
	visMemoWords = []string{
		"provisioning", "reconciliation", "shipmentbatch", "settlement",
		"inventorysync", "checkoutflow", "teardownsweep", "migrationstep",
	}
)

func init() {
	core.Register(core.Test{
		Name:        "features/pg_gin_search",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "Seed a visibility-shaped table, build a multi-column GIN index over btree_gin scalars plus a pg_trgm memo column, and assert four predicates return exactly the ids a Go oracle computes, through the index, after a post-index insert batch and across a branch fork.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     25 * time.Minute,
		Run:         runPgGinSearch,
	})
}

// visRow is one seeded visibility record; the slice of them is the oracle.
type visRow struct {
	id      int64
	ns      string
	wfType  string
	status  int
	startTS string
	memo    string
}

// visPredicate is one query shape: its SQL WHERE clause and the Go filter that
// must select exactly the same rows.
type visPredicate struct {
	name  string
	where string
	match func(visRow) bool
}

func runPgGinSearch(t *core.T) error {
	rowCount := t.Options.GetInt("functional.gin_search.rows", DefaultGinSearchRows)
	extraRows := t.Options.GetInt("functional.gin_search.extra_rows", DefaultGinSearchExtraRows)
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"pg_gin_search", func(ctx context.Context) error {
			cl, err := a.f.controllerClient(ctx)
			if err != nil {
				return err
			}
			// The index name must not be a substring of the table name: the plan text
			// of a sequential scan names the table.
			table := "vis_" + a.f.suffix()
			return ginSearchSteps(ctx, cl, a.proj, table, table+"_trgm_gin", "gin_h_"+a.f.suffix(),
				"gin-fork-"+a.f.suffix(), rowCount, extraRows, t.Seed.Fork("gin_search"), a.f.endpointSQL, a.f.logf)
		}}}
	})
}

// ginSearchSteps drives the whole exercise over the ampCtl and
// endpoint-connector seams. Nothing here is an environment condition: both
// extensions are baked into the compute image, so a failure is the product's.
func ginSearchSteps(ctx context.Context, cl ampCtl, proj ampCluster, table, index, fnPrefix, childName string,
	rowCount, extraRows int, seed *core.Seed, connect endpointConnector, logf func(string, ...any)) error {
	ep, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create endpoint: %w", err)
	}
	base, err := connect(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	defer base.Close()

	rows := buildVisRows(seed, 1, rowCount)
	if _, err := base.Exec(ctx, "CREATE TABLE "+table+
		" (id bigint primary key, ns text, wf_type text, status int, start_ts timestamptz, memo text)"); err != nil {
		return fmt.Errorf("create the visibility table: %w", err)
	}
	if err := insertVisRows(ctx, base, table, rows); err != nil {
		return err
	}

	for _, ext := range []string{"btree_gin", "pg_trgm"} {
		if _, err := base.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+ext); err != nil {
			return fmt.Errorf("CREATE EXTENSION %s: the compute image ships it as PG contrib and a multi-column GIN index over scalars plus trigrams needs it: %w", ext, err)
		}
	}
	if _, err := base.Exec(ctx, "CREATE INDEX "+index+" ON "+table+
		" USING gin (ns, wf_type, status, memo gin_trgm_ops)"); err != nil {
		return fmt.Errorf("CREATE INDEX %s over (ns, wf_type, status, memo gin_trgm_ops) failed: the text and int columns need btree_gin's default GIN operator classes and memo needs pg_trgm's gin_trgm_ops: %w", index, err)
	}
	if err := createGinHelpers(ctx, base, fnPrefix); err != nil {
		return err
	}

	preds := pickVisPredicates(rows)
	for _, p := range preds {
		if len(expectVisIDs(rows, p)) == 0 {
			return fmt.Errorf("the %s predicate matches no seeded row, so it would assert nothing: WHERE %s over %d rows",
				p.name, p.where, len(rows))
		}
	}
	if err := analyzeVis(ctx, base, table); err != nil {
		return err
	}
	if err := assertVisQueries(ctx, base, table, index, fnPrefix, "the seeded dataset", preds, rows, logf); err != nil {
		return err
	}
	if err := assertVisIndexCheckCanFail(ctx, base, table, index, fnPrefix, preds[0], rows, logf); err != nil {
		return err
	}

	extra := buildVisRows(seed, int64(rowCount)+1, extraRows)
	if err := insertVisRows(ctx, base, table, extra); err != nil {
		return err
	}
	rows = append(rows, extra...)
	if err := analyzeVis(ctx, base, table); err != nil {
		return err
	}
	if err := assertVisQueries(ctx, base, table, index, fnPrefix, "a second insert batch applied after the index existed", preds, rows, logf); err != nil {
		return err
	}

	return assertVisOnFork(ctx, cl, proj, base, connect, childName, table, index, fnPrefix, preds, rows, logf)
}

// assertVisIndexCheckCanFail proves the index-usage assertion has detection
// power: with index scans disabled the same check must report the index unused.
// One predicate, so the proof costs one query plus one idle idx_scan poll.
func assertVisIndexCheckCanFail(ctx context.Context, c sqlConn, table, index, fnPrefix string,
	p visPredicate, rows []visRow, logf func(string, ...any)) error {
	err := runVisPredicate(ctx, c, table, index, fnPrefix, "the index-usage self-check", p, ginNoIndexProbe, rows, logf)
	if err == nil {
		return fmt.Errorf("the index-usage assertion has no detection power: with enable_indexscan, enable_bitmapscan and enable_indexonlyscan off, the %s predicate still reported index %s as used, so every other round of this test proves nothing about the GIN index",
			p.name, index)
	}
	if !errors.Is(err, errVisIndexUnused) {
		return fmt.Errorf("the index-usage self-check failed for the wrong reason (expected the index-unused failure, got a different one): %w", err)
	}
	logf("pg_gin_search: index-usage self-check has detection power: with index scans disabled the assertion failed: %v", err)
	return nil
}

// assertVisOnFork forks the branch at a captured LSN and re-runs the combined
// predicate on the fork's endpoint against the same oracle, through the index,
// which is what proves the index pages replay correctly through the fork path.
func assertVisOnFork(ctx context.Context, cl ampCtl, proj ampCluster, base sqlConn, connect endpointConnector,
	childName, table, index, fnPrefix string, preds []visPredicate, rows []visRow, logf func(string, ...any)) error {
	forkLSN, err := currentLSN(ctx, base)
	if err != nil {
		return err
	}
	if _, err := cl.CreateBranchAndWait(ctx, proj.poolID, proj.projID, controller.CreateBranchRequest{
		Name:           childName,
		ParentBranchID: proj.branchID,
		ForkPointLSN:   forkLSN,
	}); err != nil {
		return fmt.Errorf("fork branch at LSN %s: %w", forkLSN, err)
	}
	child, err := findBranchCtl(ctx, cl, proj.poolID, proj.projID, childName)
	if err != nil {
		return err
	}
	childEP, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, child.BranchID)
	if err != nil {
		return fmt.Errorf("create endpoint on the fork: %w", err)
	}
	kid, err := connect(ctx, childEP)
	if err != nil {
		return fmt.Errorf("connect the fork's endpoint: %w", err)
	}
	defer kid.Close()

	combined := preds[len(preds)-1]
	return runVisPredicate(ctx, kid, table, index, fnPrefix, "the fork of the branch", combined, ginForcedIndex, rows, logf)
}

// assertVisQueries runs every predicate twice: once as the planner chooses it,
// once with sequential scans disabled. Both must return exactly the ids the
// oracle computes; the forced round must additionally prove the index was
// scanned. The planner's own choice is recorded, not asserted: once the second
// batch sits in the GIN pending list, gincostestimate charges every scan for the
// pending pages, so a sequential scan is a legitimate plan for the least
// selective predicates on a table this small.
func assertVisQueries(ctx context.Context, c sqlConn, table, index, fnPrefix, stage string,
	preds []visPredicate, rows []visRow, logf func(string, ...any)) error {
	for _, m := range []ginMode{ginPlannerChoice, ginForcedIndex} {
		for _, p := range preds {
			if err := runVisPredicate(ctx, c, table, index, fnPrefix, stage, p, m, rows, logf); err != nil {
				return err
			}
		}
	}
	return nil
}

// runVisPredicate runs one predicate under one planner mode. The ids must always
// match the oracle exactly. Where the mode expects the index, the proof is
// pg_stat_user_indexes.idx_scan increasing for that index, which no sequential
// scan can do; the plan node naming the index is the secondary check.
func runVisPredicate(ctx context.Context, c sqlConn, table, index, fnPrefix, stage string,
	p visPredicate, m ginMode, rows []visRow, logf func(string, ...any)) error {
	q := visIDsQuery(table, p.where)
	before, err := readIdxScan(ctx, c, index)
	if err != nil {
		return err
	}
	plan, err := c.QueryString(ctx, "SELECT "+fnPrefix+"_plan($1, $2)", q, m.sets)
	if err != nil {
		return fmt.Errorf("EXPLAIN the %s predicate (%s): %w", p.name, m.how, err)
	}
	got, err := runVisIDsIn(ctx, c, fnPrefix, q, m.sets)
	if err != nil {
		return fmt.Errorf("run the %s predicate (%s): %w", p.name, m.how, err)
	}
	if err := compareVisIDs(p, stage+" under "+m.how, expectVisIDs(rows, p), got); err != nil {
		return err
	}
	after, err := waitIdxScanAbove(ctx, c, index, before)
	if err != nil {
		return err
	}
	node := planIndexNode(plan, index)
	if !m.wantIndex {
		logf("pg_gin_search: %s predicate on %s under %s: index %s idx_scan %d to %d, index plan node %q",
			p.name, stage, m.how, index, before, after, node)
		return nil
	}
	if after <= before {
		return fmt.Errorf("%w: the %s predicate on %s under %s did not scan index %s: pg_stat_user_indexes.idx_scan stayed at %d over %s, so the rows were served without the multi-column GIN index. WHERE %s\nplan:\n%s",
			errVisIndexUnused, p.name, stage, m.how, index, before, idxScanPollTimeout, p.where, plan)
	}
	if node == "" {
		return fmt.Errorf("%w: the %s predicate on %s under %s has no plan node reading index %s (no \"Index Scan using %s\", \"Index Only Scan using %s\" or \"Bitmap Index Scan on %s\" line), while idx_scan moved %d to %d. WHERE %s\nplan:\n%s",
			errVisIndexUnused, p.name, stage, m.how, index, index, index, index, before, after, p.where, plan)
	}
	return nil
}

// readIdxScan reads the index's scan counter. Summed because the name is matched
// across schemas.
func readIdxScan(ctx context.Context, c sqlConn, index string) (int64, error) {
	n, err := c.QueryInt(ctx,
		"SELECT coalesce(sum(idx_scan), 0) FROM pg_stat_user_indexes WHERE indexrelname = $1", index)
	if err != nil {
		return 0, fmt.Errorf("read pg_stat_user_indexes.idx_scan for index %s: %w", index, err)
	}
	return n, nil
}

// waitIdxScanAbove polls the scan counter until it moves past before, returning
// the last value read. A counter that never moves is the caller's assertion to
// make, not an error here.
func waitIdxScanAbove(ctx context.Context, c sqlConn, index string, before int64) (int64, error) {
	deadline := time.Now().Add(idxScanPollTimeout)
	for {
		n, err := readIdxScan(ctx, c, index)
		if err != nil {
			return 0, err
		}
		if n > before || !time.Now().Before(deadline) {
			return n, nil
		}
		select {
		case <-ctx.Done():
			return n, ctx.Err()
		case <-time.After(idxScanPollInterval):
		}
	}
}

// planIndexNode returns the plan line whose node reads the index, matching the
// node type and the index name together so no other text can satisfy it.
func planIndexNode(plan, index string) string {
	nodes := []string{
		"Index Scan using " + index,
		"Index Only Scan using " + index,
		"Bitmap Index Scan on " + index,
	}
	for _, line := range strings.Split(plan, "\n") {
		l := strings.TrimSpace(line)
		for _, n := range nodes {
			if strings.Contains(l, n) {
				return l
			}
		}
	}
	return ""
}

// createGinHelpers installs the two plpgsql helpers every assertion runs through.
// SET LOCAL inside the function body is what makes the planner settings apply to
// the query: a plain SET would land on one connection of a pool and the next
// statement could be executed on another.
func createGinHelpers(ctx context.Context, c sqlConn, fnPrefix string) error {
	applySets := " FOREACH s IN ARRAY string_to_array(sets, ';') LOOP" +
		" IF btrim(s) <> '' THEN EXECUTE s; END IF; END LOOP;"
	stmts := []string{
		"CREATE FUNCTION " + fnPrefix + "_plan(q text, sets text) RETURNS text LANGUAGE plpgsql AS $fn$" +
			" DECLARE line text; s text; out text := ''; BEGIN" + applySets +
			" FOR line IN EXECUTE 'EXPLAIN ' || q LOOP out := out || line || E'\\n'; END LOOP;" +
			" RETURN out; END $fn$",
		"CREATE FUNCTION " + fnPrefix + "_run(q text, sets text) RETURNS text LANGUAGE plpgsql AS $fn$" +
			" DECLARE r text; s text; BEGIN" + applySets +
			" EXECUTE q INTO r; RETURN coalesce(r, ''); END $fn$",
	}
	for _, s := range stmts {
		if _, err := c.Exec(ctx, s); err != nil {
			return fmt.Errorf("create the plan/run helper functions: %w", err)
		}
	}
	return nil
}

func analyzeVis(ctx context.Context, c sqlConn, table string) error {
	if _, err := c.Exec(ctx, "ANALYZE "+table); err != nil {
		return fmt.Errorf("ANALYZE %s: %w", table, err)
	}
	return nil
}

// visIDsQuery renders the predicate as a single aggregated id list, ordered by
// id so the comparison against the oracle is positional.
func visIDsQuery(table, where string) string {
	return "SELECT coalesce(string_agg(id::text, ',' ORDER BY id), '') FROM " + table + " WHERE " + where
}

func runVisIDsIn(ctx context.Context, c sqlConn, fnPrefix, q, sets string) ([]int64, error) {
	agg, err := c.QueryString(ctx, "SELECT "+fnPrefix+"_run($1, $2)", q, sets)
	if err != nil {
		return nil, err
	}
	return parseVisIDs(agg)
}

func parseVisIDs(agg string) ([]int64, error) {
	if agg == "" {
		return nil, nil
	}
	parts := strings.Split(agg, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("unparseable id %q in the returned id list: %w", p, err)
		}
		out = append(out, v)
	}
	return out, nil
}

func expectVisIDs(rows []visRow, p visPredicate) []int64 {
	var out []int64
	for _, r := range rows {
		if p.match(r) {
			out = append(out, r.id)
		}
	}
	return out
}

// compareVisIDs compares the returned ids to the oracle position by position.
func compareVisIDs(p visPredicate, where string, want, got []int64) error {
	n := len(want)
	if len(got) < n {
		n = len(got)
	}
	for i := 0; i < n; i++ {
		if want[i] != got[i] {
			return fmt.Errorf("the %s predicate on %s returned the wrong rows: at position %d it returned id %d, want %d (%d ids returned, %d expected). WHERE %s",
				p.name, where, i, got[i], want[i], len(got), len(want), p.where)
		}
	}
	if len(want) != len(got) {
		return fmt.Errorf("the %s predicate on %s returned %d ids, want %d: the lists agree up to position %d and then one of them ends. WHERE %s",
			p.name, where, len(got), len(want), n, p.where)
	}
	return nil
}

// buildVisRows seeds count rows deterministically, ids starting at firstID.
func buildVisRows(seed *core.Seed, firstID int64, count int) []visRow {
	r := seed.Rand()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]visRow, 0, count)
	for i := 0; i < count; i++ {
		memo := visMemoWords[r.IntN(len(visMemoWords))] + " " +
			visMemoWords[r.IntN(len(visMemoWords))] + " " +
			visMemoWords[r.IntN(len(visMemoWords))]
		out = append(out, visRow{
			id:      firstID + int64(i),
			ns:      visNamespaces[r.IntN(len(visNamespaces))],
			wfType:  visWorkflows[r.IntN(len(visWorkflows))],
			status:  visStatuses[r.IntN(len(visStatuses))],
			startTS: base.Add(time.Duration(i) * 17 * time.Second).Format("2006-01-02 15:04:05Z07:00"),
			memo:    memo,
		})
	}
	return out
}

func buildVisValues(format string, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf(format, i))
	}
	return out
}

func insertVisRows(ctx context.Context, c sqlConn, table string, rows []visRow) error {
	const batch = 250
	for start := 0; start < len(rows); start += batch {
		end := start + batch
		if end > len(rows) {
			end = len(rows)
		}
		var b strings.Builder
		b.WriteString("INSERT INTO " + table + " (id, ns, wf_type, status, start_ts, memo) VALUES ")
		for i, r := range rows[start:end] {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "(%d,%s,%s,%d,%s::timestamptz,%s)",
				r.id, sqlText(r.ns), sqlText(r.wfType), r.status, sqlText(r.startTS), sqlText(r.memo))
		}
		if _, err := c.Exec(ctx, b.String()); err != nil {
			return fmt.Errorf("insert visibility rows %d..%d: %w", rows[start].id, rows[end-1].id, err)
		}
	}
	return nil
}

func sqlText(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// pickVisPredicates derives the four query shapes from the seeded rows, picking
// the values with the most matches so no predicate is vacuous. The combined one
// is built from a real row, so its expected set always holds at least that row.
func pickVisPredicates(rows []visRow) []visPredicate {
	nsWf := mostCommon(rows, func(r visRow) string { return r.ns + "\x00" + r.wfType })
	ns, wf := splitKey(nsWf)
	nsStatus := mostCommon(rows, func(r visRow) string { return r.ns + "\x00" + strconv.Itoa(r.status) })
	nsForStatus, statusStr := splitKey(nsStatus)
	status, _ := strconv.Atoi(statusStr)
	bestNS := mostCommon(rows, func(r visRow) string { return r.ns })
	token := mostCommonToken(rows, bestNS)

	var seedRow visRow
	for _, r := range rows {
		if r.ns == ns && r.wfType == wf {
			seedRow = r
			break
		}
	}
	comboToken := strings.Fields(seedRow.memo)[0]

	return []visPredicate{
		{
			name:  "namespace and workflow type",
			where: fmt.Sprintf("ns = %s AND wf_type = %s", sqlText(ns), sqlText(wf)),
			match: func(r visRow) bool { return r.ns == ns && r.wfType == wf },
		},
		{
			name:  "namespace and status",
			where: fmt.Sprintf("ns = %s AND status = %d", sqlText(nsForStatus), status),
			match: func(r visRow) bool { return r.ns == nsForStatus && r.status == status },
		},
		{
			name:  "namespace and a memo trigram match",
			where: fmt.Sprintf("ns = %s AND memo LIKE %s", sqlText(bestNS), sqlText("%"+token+"%")),
			match: func(r visRow) bool { return r.ns == bestNS && strings.Contains(r.memo, token) },
		},
		{
			name: "namespace, workflow type, status and a memo trigram match",
			where: fmt.Sprintf("ns = %s AND wf_type = %s AND status = %d AND memo LIKE %s",
				sqlText(seedRow.ns), sqlText(seedRow.wfType), seedRow.status, sqlText("%"+comboToken+"%")),
			match: func(r visRow) bool {
				return r.ns == seedRow.ns && r.wfType == seedRow.wfType &&
					r.status == seedRow.status && strings.Contains(r.memo, comboToken)
			},
		},
	}
}

// mostCommon returns the key with the most rows, scanning in slice order so ties
// resolve deterministically.
func mostCommon(rows []visRow, key func(visRow) string) string {
	counts := map[string]int{}
	for _, r := range rows {
		counts[key(r)]++
	}
	best, bestN := "", 0
	for _, r := range rows {
		k := key(r)
		if counts[k] > bestN {
			best, bestN = k, counts[k]
		}
	}
	return best
}

// mostCommonToken returns the memo word carried by the most rows in ns.
func mostCommonToken(rows []visRow, ns string) string {
	counts := map[string]int{}
	for _, r := range rows {
		if r.ns != ns {
			continue
		}
		for _, w := range uniqueFields(r.memo) {
			counts[w]++
		}
	}
	best, bestN := "", 0
	for _, w := range visMemoWords {
		if counts[w] > bestN {
			best, bestN = w, counts[w]
		}
	}
	return best
}

func uniqueFields(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range strings.Fields(s) {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

func splitKey(k string) (string, string) {
	a, b, _ := strings.Cut(k, "\x00")
	return a, b
}
