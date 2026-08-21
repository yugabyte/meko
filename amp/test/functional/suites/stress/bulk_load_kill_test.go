package stress

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func TestBulkPlanGivesEveryLoadingRoundItsOwnBlock(t *testing.T) {
	p := bulkLoadKillParams{
		BaselineRows: 1000, BatchRows: 250, Rounds: 4,
		Ops:       []string{opCopy, opInsertSelect, opTxn},
		KillModes: []string{killGraceful, killForce},
	}
	prevHi := p.BaselineRows
	for round, r := range bulkPlan(p) {
		if r.Lo != prevHi+1 {
			t.Fatalf("round %d starts at %d, want %d: a gap or an overlap makes the all-or-nothing count meaningless", round, r.Lo, prevHi+1)
		}
		if got := r.Hi - r.Lo + 1; got != p.BatchRows {
			t.Fatalf("round %d covers %d ids, want %d", round, got, p.BatchRows)
		}
		prevHi = r.Hi
	}
	if got := bulkTotalRows(p, bulkPlan(p)); got != 1000+4*250 {
		t.Errorf("bulkTotalRows = %d, want %d", got, 1000+4*250)
	}
}

// An update round rewrites the block the round before it loaded, and must not
// consume a block of its own: the next load would leave a hole, and every total
// after it would be wrong.
func TestBulkPlanUpdateRoundsRewriteThePreviousBlock(t *testing.T) {
	p := bulkLoadKillParams{
		BaselineRows: 1000, BatchRows: 250, UpdateRows: 250, Rounds: 4,
		Ops:       []string{opCopy, opUpdate, opTxn, opUpdate},
		KillModes: []string{killGraceful},
	}
	plan := bulkPlan(p)
	if plan[1].Op != opUpdate || plan[1].Lo != plan[0].Lo || plan[1].Hi != plan[0].Hi {
		t.Fatalf("round 1 is %+v, want an update over round 0's block %d-%d", plan[1], plan[0].Lo, plan[0].Hi)
	}
	if plan[2].Lo != plan[0].Hi+1 {
		t.Fatalf("round 2 loads %d-%d, want it to continue at %d: the update round consumed a block", plan[2].Lo, plan[2].Hi, plan[0].Hi+1)
	}
	if plan[3].Lo != plan[2].Lo || plan[3].Hi != plan[2].Hi {
		t.Fatalf("round 3 is %+v, want an update over round 2's block %d-%d", plan[3], plan[2].Lo, plan[2].Hi)
	}
	// Two of the four rounds load; the table grows by two blocks, not four.
	if got, want := bulkTotalRows(p, plan), int64(1000+2*250); got != want {
		t.Errorf("bulkTotalRows = %d, want %d", got, want)
	}
}

// The default plan: every op meets every kill mode, and no update round is
// first (it would have nothing to rewrite).
func TestDefaultBulkPlanCoversEveryOpAndMode(t *testing.T) {
	p := bulkLoadKillParams{
		BaselineRows: 1000, BatchRows: 250, Rounds: 10,
		Ops:       []string{opCopy, opInsertSelect, opBatchInsert, opTxn, opUpdate},
		KillModes: []string{killGraceful, killForce},
	}
	seen := map[string]bool{}
	plan := bulkPlan(p)
	for _, r := range plan {
		seen[r.Op+"/"+r.Mode] = true
	}
	if len(seen) != 10 {
		t.Errorf("the default ten rounds cover %d op/mode pair(s), want all 10: %v", len(seen), seen)
	}
	if plan[0].Op == opUpdate {
		t.Error("the first round is an update, which has no loaded block to rewrite")
	}
}

func TestBulkExpectedSumMatchesTheRowsWritten(t *testing.T) {
	const lo, hi = 41, 4137
	var want int64
	for id := int64(lo); id <= hi; id++ {
		want += id * 7
	}
	if got := bulkExpectedSum(lo, hi); got != want {
		t.Errorf("bulkExpectedSum(%d, %d) = %d, want %d", lo, hi, got, want)
	}
	if got := bulkExpectedSum(5, 5); got != 35 {
		t.Errorf("bulkExpectedSum(5, 5) = %d, want 35", got)
	}
}

func TestSplitBulkRangeCoversEveryIdOnce(t *testing.T) {
	for _, tc := range []struct {
		lo, hi, n int64
		wantParts int
	}{
		{1, 100, 4, 4},
		{1, 100, 1, 1},
		{1, 3, 10, 3}, // more statements than ids
		{10, 10, 4, 1},
		{1, 0, 4, 0}, // empty range
	} {
		parts := splitBulkRange(tc.lo, tc.hi, tc.n)
		if len(parts) != tc.wantParts {
			t.Fatalf("splitBulkRange(%d, %d, %d) = %v, want %d part(s)", tc.lo, tc.hi, tc.n, parts, tc.wantParts)
		}
		if tc.wantParts == 0 {
			continue
		}
		if parts[0][0] != tc.lo || parts[len(parts)-1][1] != tc.hi {
			t.Errorf("splitBulkRange(%d, %d, %d) = %v, does not cover the range ends", tc.lo, tc.hi, tc.n, parts)
		}
		for i := 1; i < len(parts); i++ {
			if parts[i][0] != parts[i-1][1]+1 {
				t.Errorf("splitBulkRange(%d, %d, %d) = %v, part %d is not contiguous with %d", tc.lo, tc.hi, tc.n, parts, i, i-1)
			}
		}
	}
}

func TestBulkSourceFeedsEveryRowAndTriggersAtTheKillPoint(t *testing.T) {
	const lo, hi, payload = 100, 109, 4
	src := newBulkSource(lo, hi, payload, 4)

	rows := 0
	for src.Next() {
		vals, err := src.Values()
		if err != nil {
			t.Fatalf("Values: %v", err)
		}
		rows++
		id, ok := vals[0].(int64)
		if !ok || id != int64(lo+rows-1) {
			t.Fatalf("row %d has id %v, want %d", rows, vals[0], lo+rows-1)
		}
		if v := vals[1].(int64); v != id*7 {
			t.Fatalf("row id %d has v %d, want %d", id, v, id*7)
		}
		if pad := vals[2].(string); len(pad) != payload || strings.Trim(pad, "x") != "" {
			t.Fatalf("row id %d has pad %q, want %d x's", id, pad, payload)
		}
		// The trigger must not fire before the kill point: a pod deleted at row
		// one is not a kill in the middle of anything.
		if rows < 4 {
			select {
			case <-src.trigger:
				t.Fatalf("the trigger fired after %d row(s), want it at row 4", rows)
			default:
			}
		}
	}
	if rows != hi-lo+1 {
		t.Fatalf("the source fed %d rows, want %d", rows, hi-lo+1)
	}
	select {
	case <-src.trigger:
	case <-time.After(time.Second):
		t.Fatal("the trigger never fired, so a caller waiting to kill the load would block until the load ended")
	}
}

// A COPY that ends before the kill point must still release the trigger, or the
// round blocks on a channel nothing will ever close.
func TestBulkSourceReleaseUnblocksAnUnreachedTrigger(t *testing.T) {
	src := newBulkSource(1, 10, 8, 1_000_000)
	for src.Next() {
		if _, err := src.Values(); err != nil {
			t.Fatalf("Values: %v", err)
		}
	}
	select {
	case <-src.trigger:
		t.Fatal("the trigger fired though the kill point was never reached")
	default:
	}
	src.release()
	src.release() // idempotent: the load path calls it after every COPY
	select {
	case <-src.trigger:
	default:
		t.Fatal("release did not unblock the trigger")
	}
}

func TestValidateBulkParamsRejectsUnusableConfigs(t *testing.T) {
	ok := bulkLoadKillParams{
		BaselineRows: 10, BatchRows: 5, Rounds: 1, Ops: []string{opCopy},
		PayloadBytes: 8, LoadChunkRows: 5, KillFraction: 0.5,
		UpdateRows: 2, BaselineProbeRows: 5, RowsPerStatement: 2,
		KillDelayMin: time.Second, KillDelayMax: time.Minute,
		KillModes:      []string{killGraceful, killForce},
		VerifyParallel: 2, VerifyChunkRows: 5,
		VisibilityWindowRows: 5,
	}
	if err := validateBulkParams(ok); err != nil {
		t.Fatalf("a usable config was rejected: %v", err)
	}
	for name, mutate := range map[string]func(*bulkLoadKillParams){
		"no rows":            func(p *bulkLoadKillParams) { p.BatchRows = 0 },
		"no rounds":          func(p *bulkLoadKillParams) { p.Rounds = 0 },
		"no payload":         func(p *bulkLoadKillParams) { p.PayloadBytes = 0 },
		"no chunk":           func(p *bulkLoadKillParams) { p.LoadChunkRows = 0 },
		"no update rows":     func(p *bulkLoadKillParams) { p.UpdateRows = 0 },
		"no rows per stmt":   func(p *bulkLoadKillParams) { p.RowsPerStatement = 0 },
		"no baseline probe":  func(p *bulkLoadKillParams) { p.BaselineProbeRows = 0 },
		"kill after the end": func(p *bulkLoadKillParams) { p.KillFraction = 1 },
		"kill before start":  func(p *bulkLoadKillParams) { p.KillFraction = 0 },
		"unknown op":         func(p *bulkLoadKillParams) { p.Ops = []string{"truncate"} },
		"no op":              func(p *bulkLoadKillParams) { p.Ops = nil },
		"update first":       func(p *bulkLoadKillParams) { p.Ops = []string{opUpdate, opCopy} },
		"unknown kill mode":  func(p *bulkLoadKillParams) { p.KillModes = []string{"sigsegv"} },
		"no kill mode":       func(p *bulkLoadKillParams) { p.KillModes = nil },
		"no kill delay":      func(p *bulkLoadKillParams) { p.KillDelayMin = 0 },
		"no verify workers":  func(p *bulkLoadKillParams) { p.VerifyParallel = 0 },
		"probe with no period": func(p *bulkLoadKillParams) {
			p.VisibilityProbe, p.VisibilityPeriod = true, 0
		},
		"probe with no window": func(p *bulkLoadKillParams) {
			p.VisibilityProbe, p.VisibilityWindowRows = true, 0
		},
		"inverted delays": func(p *bulkLoadKillParams) { p.KillDelayMax = time.Millisecond },
	} {
		p := ok
		mutate(&p)
		if err := validateBulkParams(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBulkInsertSQLGeneratesTheRowsServerSide(t *testing.T) {
	got := bulkInsertSQL("mt_0", 5, 9, 64)
	for _, want := range []string{"INSERT INTO mt_0 (id, v, pad)", "g * 7", "repeat('x', 64)", "generate_series(5::bigint, 9::bigint)"} {
		if !strings.Contains(got, want) {
			t.Errorf("bulkInsertSQL is missing %q: %s", want, got)
		}
	}
}

// The derived delay is what puts the kill in the same place in an unobservable
// write on a fast cluster and a slow one.
func TestBulkKillDelayScalesWithTheMeasuredRate(t *testing.T) {
	p := bulkLoadKillParams{
		KillFraction: 0.4,
		KillDelayMin: time.Second, KillDelayMax: 2 * time.Minute,
	}
	const rows = 300_000
	// 10k rows/s: 40% of 300k rows is 12s of writing.
	p.baselineRowsPerSecond = 10_000
	if got := bulkKillDelay(p, rows); got != 12*time.Second {
		t.Errorf("bulkKillDelay at 10k rows/s = %s, want 12s", got)
	}
	// A rate high enough that 40% of the batch is under the floor.
	p.baselineRowsPerSecond = 10_000_000
	if got := bulkKillDelay(p, rows); got != p.KillDelayMin {
		t.Errorf("bulkKillDelay at 10M rows/s = %s, want the floor %s", got, p.KillDelayMin)
	}
	// A crawl, where the estimate would be longer than the test can wait.
	p.baselineRowsPerSecond = 10
	if got := bulkKillDelay(p, rows); got != p.KillDelayMax {
		t.Errorf("bulkKillDelay at 10 rows/s = %s, want the cap %s", got, p.KillDelayMax)
	}
	// No baseline measured yet (a config-only run): the floor, not zero.
	p.baselineRowsPerSecond = 0
	if got := bulkKillDelay(p, rows); got != p.KillDelayMin {
		t.Errorf("bulkKillDelay with no measured rate = %s, want the floor %s", got, p.KillDelayMin)
	}
}

func TestBulkUpdateSQLRewritesTheRangeInPlace(t *testing.T) {
	got := bulkUpdateSQL("mt_0", 5, 9, 64)
	for _, want := range []string{"UPDATE mt_0 SET pad = repeat('y', 64)", "WHERE id BETWEEN 5 AND 9"} {
		if !strings.Contains(got, want) {
			t.Errorf("bulkUpdateSQL is missing %q: %s", want, got)
		}
	}
	// It must not touch id or v: the checksum and the corruption predicate are
	// the only things holding the range's contents down.
	for _, forbidden := range []string{"v =", "id ="} {
		if strings.Contains(got, forbidden) {
			t.Errorf("bulkUpdateSQL writes %q, which the verification uses as its oracle: %s", forbidden, got)
		}
	}
}

func TestBulkRoundCompleteAsksTheRightQuestionPerOp(t *testing.T) {
	const want = 100
	// A load is done when every row is there, whatever their pad says.
	if !bulkRoundComplete(opCopy, bulkStats{Rows: want}, want) {
		t.Error("a fully loaded range is not reported complete")
	}
	if bulkRoundComplete(opCopy, bulkStats{Rows: want - 1}, want) {
		t.Error("a range missing a row is reported complete")
	}
	// An update is done when every row carries the new pad — the row count is
	// the same either way, so asking about it would call an aborted update
	// finished.
	if bulkRoundComplete(opUpdate, bulkStats{Rows: want, Updated: 0}, want) {
		t.Error("an aborted update is reported complete because the rows are still there")
	}
	if bulkRoundComplete(opUpdate, bulkStats{Rows: want, Updated: want - 1}, want) {
		t.Error("a partly applied update is reported complete")
	}
	if !bulkRoundComplete(opUpdate, bulkStats{Rows: want, Updated: want}, want) {
		t.Error("a fully applied update is not reported complete")
	}
}

// An update round rewrites a SLICE off the front of the previous block, so its
// range is UpdateRows wide, not a whole block.
func TestBulkPlanUpdateRoundsRewriteASliceOfThePreviousBlock(t *testing.T) {
	p := bulkLoadKillParams{
		BaselineRows: 1000, BatchRows: 400, UpdateRows: 100, Rounds: 2,
		Ops:       []string{opCopy, opUpdate},
		KillModes: []string{killGraceful},
	}
	plan := bulkPlan(p)
	if plan[1].Lo != plan[0].Lo || plan[1].Hi != plan[0].Lo+p.UpdateRows-1 {
		t.Errorf("the update round covers %d-%d, want %d-%d", plan[1].Lo, plan[1].Hi, plan[0].Lo, plan[0].Lo+p.UpdateRows-1)
	}
	// An update slice wider than the block it rewrites is clamped to it.
	p.UpdateRows = 10_000
	plan = bulkPlan(p)
	if plan[1].Hi != plan[0].Hi {
		t.Errorf("an update wider than the block covers %d-%d, want it clamped to %d", plan[1].Lo, plan[1].Hi, plan[0].Hi)
	}
}

// The baseline probe rotates, so no round pays for the whole baseline and the
// windows together cover it.
func TestBaselineProbeRotatesOverTheWholeBaseline(t *testing.T) {
	p := bulkLoadKillParams{BaselineRows: 1000, BaselineProbeRows: 250}
	seen := map[int64]bool{}
	for round := 0; round < 4; round++ {
		lo, hi := bulkBaselineProbe(p, round)
		if hi-lo+1 != p.BaselineProbeRows {
			t.Fatalf("round %d probes %d-%d, want %d rows", round, lo, hi, p.BaselineProbeRows)
		}
		for id := lo; id <= hi; id++ {
			seen[id] = true
		}
	}
	if int64(len(seen)) != p.BaselineRows {
		t.Errorf("four rounds probe %d of %d baseline row(s)", len(seen), p.BaselineRows)
	}
	// It wraps rather than running off the end.
	if lo, _ := bulkBaselineProbe(p, 4); lo != 1 {
		t.Errorf("round 4 probes from %d, want it back at 1", lo)
	}
	// A probe as wide as the baseline (or unset) reads all of it.
	p.BaselineProbeRows = 0
	if lo, hi := bulkBaselineProbe(p, 3); lo != 1 || hi != p.BaselineRows {
		t.Errorf("an unset probe reads %d-%d, want the whole baseline", lo, hi)
	}
}

// The commit-kill block comes after everything the plan loaded, and its rows
// count towards the total the final verification expects.
func TestCommitKillBlockFollowsThePlan(t *testing.T) {
	p := bulkLoadKillParams{
		BaselineRows: 1000, BatchRows: 250, UpdateRows: 100, Rounds: 4,
		Ops:        []string{opCopy, opInsertSelect, opTxn, opUpdate},
		KillModes:  []string{killGraceful},
		CommitKill: true,
	}
	plan := bulkPlan(p)
	lo, hi := bulkCommitKillRange(p, plan)
	var lastLoaded int64
	for _, r := range plan {
		if r.Op != opUpdate {
			lastLoaded = max(lastLoaded, r.Hi)
		}
	}
	if lo != lastLoaded+1 || hi != lastLoaded+p.BatchRows {
		t.Errorf("the commit-kill block is %d-%d, want %d-%d", lo, hi, lastLoaded+1, lastLoaded+p.BatchRows)
	}
	// Three loading rounds plus the commit-kill block.
	if got, want := bulkTotalRows(p, plan), int64(1000+4*250); got != want {
		t.Errorf("bulkTotalRows with commit_kill = %d, want %d", got, want)
	}
	p.CommitKill = false
	if got, want := bulkTotalRows(p, plan), int64(1000+3*250); got != want {
		t.Errorf("bulkTotalRows without commit_kill = %d, want %d", got, want)
	}
}

func TestVisibilityProbeIsOffWhenDisabled(t *testing.T) {
	if pr := startVisibilityProbe(context.Background(), &tenant{}, bulkRound{}, bulkLoadKillParams{}); pr != nil {
		t.Error("the probe started though visibility_probe is off")
	}
}

func TestSecondaryIndexNameIsPerTable(t *testing.T) {
	if a, b := secondaryIndexName("mt_0"), secondaryIndexName("mt_1"); a == b {
		t.Errorf("two tables share the index name %q", a)
	}
}

// batch_insert is the application-shaped write: real parameters, one placeholder
// per column per row, so the rows travel from the client rather than being
// generated by the server.
func TestBatchInsertSQLCarriesAPlaceholderPerValue(t *testing.T) {
	got := batchInsertSQL("mt_0", 3)
	if !strings.HasPrefix(got, "INSERT INTO mt_0 (id, v, pad) VALUES ") {
		t.Fatalf("batchInsertSQL = %s", got)
	}
	for _, want := range []string{"($1, $2, $3)", "($4, $5, $6)", "($7, $8, $9)"} {
		if !strings.Contains(got, want) {
			t.Errorf("batchInsertSQL is missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "generate_series") || strings.Contains(got, "repeat(") {
		t.Errorf("batchInsertSQL generates rows server-side, which is the shape it exists to contrast with: %s", got)
	}
	if n := strings.Count(got, "$"); n != 9 {
		t.Errorf("batchInsertSQL has %d placeholder(s) for 3 rows, want 9", n)
	}
}

func TestFlattenRowsKeepsColumnOrder(t *testing.T) {
	got := flattenRows([][]any{{int64(1), int64(7), "x"}, {int64(2), int64(14), "y"}})
	want := []any{int64(1), int64(7), "x", int64(2), int64(14), "y"}
	if len(got) != len(want) {
		t.Fatalf("flattenRows gave %d value(s), want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("value %d is %v, want %v", i, got[i], want[i])
		}
	}
}

// The trigger has to fire at the kill point and, whatever happens, eventually —
// a round waiting on it must not block on a write that ended early.
func TestStmtProgressTriggersAtTheKillPoint(t *testing.T) {
	pr := newStmtProgress(2500)
	pr.add(1000)
	pr.add(1000)
	select {
	case <-pr.trigger:
		t.Fatal("the trigger fired after 2000 of 2500 rows")
	default:
	}
	if got := pr.handedOver(); got != 2000 {
		t.Errorf("handedOver = %d, want 2000", got)
	}
	pr.add(1000)
	select {
	case <-pr.trigger:
	default:
		t.Fatal("the trigger did not fire at 3000 of 2500 rows")
	}
	pr.release() // idempotent
}

// The params struct is built from config, so a field that is declared, validated
// and used but never READ from the config silently arrives as zero — which is
// how rows_per_statement shipped and failed a run 36 seconds in (b7a898b8). This
// builds the params the way a run does, from the embedded defaults, and holds
// them to being usable.
func TestParamsFromEmbeddedDefaultsAreUsable(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load the embedded defaults: %v", err)
	}
	p := bulkLoadKillParamsFromConfig(cfg)
	if err := validateBulkParams(p); err != nil {
		t.Fatalf("the embedded defaults do not produce a usable config: %v", err)
	}

	// Every numeric knob has to arrive non-zero: zero is what a missing read
	// looks like, and validate only covers the ones it happens to name.
	for name, got := range map[string]int64{
		"BaselineRows":         p.BaselineRows,
		"BatchRows":            p.BatchRows,
		"LoadChunkRows":        p.LoadChunkRows,
		"UpdateRows":           p.UpdateRows,
		"RowsPerStatement":     p.RowsPerStatement,
		"BaselineProbeRows":    p.BaselineProbeRows,
		"VerifyChunkRows":      p.VerifyChunkRows,
		"VisibilityWindowRows": p.VisibilityWindowRows,
	} {
		if got <= 0 {
			t.Errorf("%s = %d from the embedded defaults, want positive: its config key is not being read", name, got)
		}
	}
	if p.Rounds <= 0 || p.PayloadBytes <= 0 || p.VerifyParallel <= 0 {
		t.Errorf("rounds/payload_bytes/verify_parallel = %d/%d/%d, want all positive", p.Rounds, p.PayloadBytes, p.VerifyParallel)
	}
	if p.EndpointCPU == "" || p.EndpointMemory == "" {
		t.Errorf("endpoint sizing = %q/%q, want both set", p.EndpointCPU, p.EndpointMemory)
	}
	if p.LoadTimeout <= 0 || p.SettleTimeout <= 0 || p.KillDelayMin <= 0 || p.KillDelayMax <= 0 || p.VisibilityPeriod <= 0 {
		t.Error("a duration knob arrived zero from the embedded defaults")
	}
	// The rounds have to cover every op/mode pair, or the defaults promise
	// coverage the run does not deliver.
	if want := len(p.Ops) * len(p.KillModes); p.Rounds != want {
		t.Errorf("rounds = %d for %d op(s) and %d kill mode(s), want %d so every pair runs once",
			p.Rounds, len(p.Ops), len(p.KillModes), want)
	}
}

// The inline fallback in bulkLoadKillParamsFromConfig is the documented default
// as far as a reader of the code is concerned, and default.yaml is the one the
// run actually uses. They drifted apart for several commits — baseline_rows 500k
// against 4M, rounds 6 against 10, two ops missing — because the yaml wins at
// runtime, so nothing failed. This pins them together.
func TestInlineFallbacksMatchTheEmbeddedDefaults(t *testing.T) {
	embedded, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load the embedded defaults: %v", err)
	}
	bare, err := core.LoadConfigNoDefaults(nil, nil, nil)
	if err != nil {
		t.Fatalf("load without the embedded defaults: %v", err)
	}
	// With no config at all, every value comes from the inline fallbacks.
	want, got := bulkLoadKillParamsFromConfig(embedded), bulkLoadKillParamsFromConfig(bare)
	if !reflect.DeepEqual(want, got) {
		t.Errorf("the inline fallbacks disagree with configs/default.yaml.\n embedded: %+v\n inline:   %+v", want, got)
	}
}
