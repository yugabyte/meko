package perf

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// Every knob the scale tests read must be declared in default.yaml, and the
// declared values must be the ones that resolve — the same contract the
// multitenant knobs carry.
func TestScaleParamsDeclaredInDefaults(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load shipped defaults: %v", err)
	}
	for _, key := range []string{
		"perf.scale.max_drivers",
		"perf.scale.budget",
		"perf.scale.min_tps",
		"perf.scale.collapse_pct",
		"perf.scale.stage_duration",
		"perf.scale.floor_pct",
		"perf.scale.read_timeout",
		"perf.scale.endpoint_cpu",
		"perf.scale.endpoint_memory",
	} {
		if !cfg.Has(key) {
			t.Errorf("%s is read by the test but not declared in configs/default.yaml", key)
		}
	}
	p := scaleParamsFromConfig(cfg)
	// Six tenants scaled cleanly on the first ladder (tpcc at 0.99 efficiency),
	// so a ladder that stops there cannot find what six already cleared.
	if p.MaxDrivers <= 6 {
		t.Errorf("max_drivers = %d: too short to find a ceiling", p.MaxDrivers)
	}
	if p.StageDuration < 5*time.Minute {
		t.Errorf("stage_duration = %s: too short for a saturation that only appears once caches warm", p.StageDuration)
	}
	if p.FloorPct <= 0 || p.FloorPct >= 100 {
		t.Errorf("floor_pct = %v, want inside (0,100)", p.FloorPct)
	}
	if p.ReadTimeout <= 0 {
		t.Error("read_timeout must be positive: it is the wedge detector, not a nicety")
	}
	// The ladder is open-ended by design, so what must fit the registered 4h
	// timeout is the wall-clock budget, not max_drivers × stage_duration.
	if p.Budget <= 0 || p.Budget > 3*time.Hour {
		t.Errorf("budget = %s, want positive and inside the 4h timeout with room to provision", p.Budget)
	}
	if p.MinTPS <= 0 {
		t.Error("min_tps must be positive: it is the no-throughput stop")
	}
	if p.CollapsePct <= 0 || p.CollapsePct >= 100 {
		t.Errorf("collapse_pct = %v, want inside (0,100)", p.CollapsePct)
	}
}

// Every ladder escalates load against ONE endpoint, and each must name drivers
// the mix can actually build.
func TestScaleLaddersAreBuildable(t *testing.T) {
	if len(scaleLadders) < 4 {
		t.Errorf("only %d ladders registered; tpcc, ycsb, pgbench and the sample-app mix are the intended set", len(scaleLadders))
	}
	for name, mix := range scaleLadders {
		if len(mix) == 0 {
			t.Errorf("ladder %s has no drivers", name)
		}
		for _, w := range mix {
			if !knownMixWorkload(w) {
				t.Errorf("ladder %s names %q, which the mix cannot build", name, w)
			}
		}
	}
	// The sample-app ladder must stay a MIX: identical clients find one wall.
	if len(appLadderMix) < 2 {
		t.Errorf("app ladder mix %v is not a mix", appLadderMix)
	}
	for _, name := range appLadderMix {
		if !isSampleAppName(name) {
			t.Errorf("%q is not a sample-app driver", name)
		}
	}
}

// The escalation registers one test per workload, and only drivers the mix can
// actually build.
func TestScaleTestsRegisterKnownWorkloads(t *testing.T) {
	for _, wl := range []string{mixTPCC, mixYCSB, mixPgbench} {
		if !knownMixWorkload(wl) {
			t.Errorf("perf/scale_%s registered for a workload the mix cannot build", wl)
		}
	}
}

// The depth ladder is the other axis — one endpoint, a growing dataset — and it
// is what reaches the scale meko#739 needed. Its knobs must be declared, its
// two lists must pair index-for-index, and the top of the ladder must exceed
// the SF 150 / 75 terminals that reproduced the wedge: a ladder stopping below
// it cannot find that class of wall.
func TestScaleDepthLadderReachesTheKnownWedgeScale(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load shipped defaults: %v", err)
	}
	for _, key := range []string{
		"perf.scale_depth.warehouses",
		"perf.scale_depth.terminals",
		"perf.scale_depth.stage_duration",
		"perf.scale_depth.read_timeout",
		"perf.scale_depth.load_timeout",
		"perf.scale_depth.endpoint_cpu",
		"perf.scale_depth.endpoint_memory",
	} {
		if !cfg.Has(key) {
			t.Errorf("%s is read by the test but not declared in configs/default.yaml", key)
		}
	}
	p := scaleDepthParamsFromConfig(cfg)
	if len(p.Warehouses) != len(p.Terminals) || len(p.Warehouses) < 2 {
		t.Fatalf("ladder = %v warehouses / %v terminals: must pair index-for-index and escalate", p.Warehouses, p.Terminals)
	}
	for i := 1; i < len(p.Warehouses); i++ {
		if p.Warehouses[i] <= p.Warehouses[i-1] || p.Terminals[i] <= p.Terminals[i-1] {
			t.Errorf("ladder does not escalate at stage %d: %v / %v", i+1, p.Warehouses, p.Terminals)
		}
	}
	top, topT := p.Warehouses[len(p.Warehouses)-1], p.Terminals[len(p.Terminals)-1]
	if top < 150 || topT < 75 {
		t.Errorf("ladder tops out at %d warehouses / %d terminals — below the SF 150 / 75 that reproduced meko#739", top, topT)
	}
	// meko#739 wedged at 80 of 150 warehouses, i.e. DURING the load, so a stage
	// below that point must exist for the ladder to walk into it gradually.
	if p.Warehouses[0] > 80 {
		t.Errorf("the ladder starts at %d warehouses, past the ~80 where the known wedge appeared", p.Warehouses[0])
	}
	if p.LoadTimeout <= 0 || p.ReadTimeout <= 0 {
		t.Error("load_timeout and read_timeout must be positive: an unbounded load ate a whole run once already")
	}
}

// The wedge markers are the product's own words. If they drift, the detector
// silently stops detecting, so they are pinned here.
func TestWedgeMarkersCoverTheReportedSignature(t *testing.T) {
	reported := "YB WAL fetch failed for segno 4410 offset 7337520: WAL chunk fetch for segno 4410 " +
		"offset 7337520 returned 64 empty pages in a row on the covering arm of bucket 5 " +
		"while still continuing — the continuation token is not advancing"
	hit := false
	for _, m := range wedgeMarkers {
		if strings.Contains(reported, m) {
			hit = true
		}
	}
	if !hit {
		t.Errorf("no marker in %v matches the signature meko#739 reported", wedgeMarkers)
	}
	healthy := "INFO WAL acceptor: start receiving WAL since 0/1516AA0"
	for _, m := range wedgeMarkers {
		if strings.Contains(healthy, m) {
			t.Errorf("marker %q matches a healthy log line", m)
		}
	}
}

// The ladder must keep climbing past its configured rungs — "more and more
// until it breaks" has no fixed top — and its load budget must grow with the
// dataset, or a big stage dies on the clock rather than on a real limit.
func TestScaleDepthLadderIsOpenEnded(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load shipped defaults: %v", err)
	}
	p := scaleDepthParamsFromConfig(cfg)
	last := len(p.Warehouses) - 1
	wh, term, ok := p.rung(last)
	if !ok || wh != p.Warehouses[last] || term != p.Terminals[last] {
		t.Fatalf("rung(%d) = %d/%d,%v; want the configured last rung", last, wh, term, ok)
	}
	prev := wh
	for i := last + 1; i <= last+4; i++ {
		wh, _, ok := p.rung(i)
		if !ok {
			t.Fatalf("ladder stopped at stage %d; it must keep growing until the product fails", i)
		}
		if wh <= prev {
			t.Errorf("stage %d = %d warehouses, not larger than %d", i, wh, prev)
		}
		prev = wh
	}
	if got := p.loadBudget(p.Warehouses[0]); got != p.LoadTimeout {
		t.Errorf("small stage budget = %s, want the flat floor %s", got, p.LoadTimeout)
	}
	if big := p.loadBudget(2000); big <= p.LoadTimeout {
		t.Errorf("big stage budget = %s, want more than the floor", big)
	}
	if p.MinTPS <= 0 {
		t.Error("min_tps must be positive: it is the no-throughput stop")
	}
}

// A refused dial on the test's own loopback forward is the harness, not the
// endpoint. Misreading it turned two healthy runs into "ceiling = 1 driver".
func TestProbeTransportErrorsAreNotEndpointFailures(t *testing.T) {
	transport := []string{
		"failed to connect to `user=cloud_admin database=postgres`: 127.0.0.1:44071: connect: connection refused",
		"write tcp 127.0.0.1:5433: broken pipe",
		"read: connection reset by peer",
	}
	for _, msg := range transport {
		if !isTransportErr(errors.New(msg)) {
			t.Errorf("not treated as a transport error, so the probe would blame the endpoint: %q", msg)
		}
	}
	// A real product failure must NOT be excused as a dead forward.
	product := []string{
		"ERROR: relation \"amp_scale_probe\" does not exist (SQLSTATE 42P01)",
		"canceling statement due to statement timeout",
	}
	for _, msg := range product {
		if isTransportErr(errors.New(msg)) {
			t.Errorf("excused as a transport error, so a real failure would be retried away: %q", msg)
		}
	}
}

// Every stage loads a fresh dataset, so the previous one must be dropped: a
// reload over benchbase's fixed table names left rows its own workers could not
// find ("C_D_ID=4 C_ID=639 not found!").
func TestTPCCTablesCoverWhatBenchbaseCreates(t *testing.T) {
	for _, want := range []string{"warehouse", "district", "customer", "oorder", "order_line", "new_order", "stock", "item", "history"} {
		found := false
		for _, got := range tpccTables {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is not dropped between stages; its rows would survive into the next dataset", want)
		}
	}
}

// A load that stops being served IS the ceiling — that is what a stress ladder
// is for. A harness failure is NOT: a dead port-forward at stage 2 once came
// back as "ceiling = 1 driver" in a green run with a failed step inside it, and
// the wallError split is what keeps those apart.
func TestOnlyLoadFailuresCountAsTheWall(t *testing.T) {
	var wall error = wallError{errors.New("driver 2 stopped serving")}
	var w wallError
	if !errors.As(wall, &w) {
		t.Fatal("a load failure must be recognisable as the wall")
	}
	if !errors.As(fmt.Errorf("stage: %w", wall), &w) {
		t.Error("wrapping must not hide the wall")
	}
	if errors.As(errors.New("client pod never started"), &w) {
		t.Error("a harness failure must NOT read as the wall")
	}

	src, err := os.ReadFile("scale.go")
	if err != nil {
		t.Fatalf("read scale.go: %v", err)
	}
	body := string(src)
	// A transport failure reaching a driver is the harness, never the product.
	if !strings.Contains(body, "isTransportErr(errs[i])") {
		t.Error("a lost exec transport is being counted as a driver breaking")
	}
	// Stage 1 has no ceiling to report, so it must still fail the test.
	if !strings.Contains(body, "a single driver could not be measured") {
		t.Error("stage 1 failing must fail the ladder, not report a ceiling of zero")
	}
}

// Diminishing returns are a finding, not a stop: the ladder that quit at 2
// drivers with the endpoint healthy answered a capacity question, not a stress
// one.
func TestSoftSignalsDoNotStopTheLadder(t *testing.T) {
	src, err := os.ReadFile("scale.go")
	if err != nil {
		t.Fatalf("read scale.go: %v", err)
	}
	body := string(src)
	for _, marker := range []string{"firstDropAt = n", "firstStarvedAt = n"} {
		i := strings.Index(body, marker)
		if i < 0 {
			t.Fatalf("%s no longer recorded", marker)
		}
		if strings.Contains(body[i:i+220], "break") {
			t.Errorf("%s still ends the ladder; it must be recorded and climbed past", marker)
		}
	}
	for _, hard := range []string{"throughput collapsed to", "aggregate collapsed to", "stopped committing"} {
		if !strings.Contains(body, hard) {
			t.Errorf("hard stop %q is missing; the ladder would never end on its own", hard)
		}
	}
}

func TestLadderCurveReportsWhatWasMeasured(t *testing.T) {
	if got := ladderCurve(nil); got != "no stage completed" {
		t.Errorf("empty curve = %q", got)
	}
	got := ladderCurve([]core.ChartPoint{{X: 1, Y: 251.9}, {X: 2, Y: 261.4}})
	if !strings.Contains(got, "1:251.9tps") || !strings.Contains(got, "2:261.4tps") {
		t.Errorf("curve = %q, want both stages", got)
	}
}

// Every knob perf/scale_tables reads must be declared, and the ladder must end
// on the product rather than on the config.
func TestScaleTablesParamsDeclaredInDefaults(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	for _, key := range []string{
		"perf.scale_tables.batch_size", "perf.scale_tables.max_tables",
		"perf.scale_tables.rows_per_table", "perf.scale_tables.budget",
		"perf.scale_tables.batch_timeout", "perf.scale_tables.catalog_timeout",
		"perf.scale_tables.endpoint_cpu", "perf.scale_tables.endpoint_memory",
	} {
		if !cfg.Has(key) {
			t.Errorf("%s is read by the test but not declared in configs/default.yaml", key)
		}
	}
	p := scaleTablesParamsFromConfig(cfg)
	if p.BatchSize <= 0 {
		t.Errorf("batch_size = %d", p.BatchSize)
	}
	// A backstop that a real endpoint hits first would make the config the
	// answer instead of the product.
	if p.MaxTables < 10000 {
		t.Errorf("max_tables = %d: too low to be a backstop rather than the ceiling", p.MaxTables)
	}
	if p.RowsPerTable <= 0 || p.RowsPerTable > 100 {
		t.Errorf("rows_per_table = %d: this ladder measures relations, not data", p.RowsPerTable)
	}
	if p.Budget <= 0 || p.Budget > 6*time.Hour {
		t.Errorf("budget = %s, want positive and inside the registered 6h timeout", p.Budget)
	}
	if p.CatalogTimeout <= 0 || p.CatalogTimeout >= p.BatchTimeout {
		t.Errorf("catalog_timeout = %s must be positive and tighter than batch_timeout %s", p.CatalogTimeout, p.BatchTimeout)
	}
}

// The count must survive a batch that fails halfway, or the reported ceiling is
// wrong in the one case that matters.
func TestScaleTablesReportsWhatItActuallyCreated(t *testing.T) {
	src, err := os.ReadFile("scale_tables.go")
	if err != nil {
		t.Fatalf("read scale_tables.go: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, "return made, fmt.Errorf(\"create table") {
		t.Error("a failed create no longer reports how many tables were made before it")
	}
	if !strings.Contains(body, "created += made") {
		t.Error("the running total no longer counts a partial batch")
	}
	for _, wall := range []string{"pg_class did not answer", "probeWriteRead"} {
		if !strings.Contains(body, wall) {
			t.Errorf("the ladder no longer checks %q; creates succeeding is not the same as the endpoint working", wall)
		}
	}
}

// A drive window that recorded zero transactions is a measurement that did not
// happen, not a product at its limit. It reached the portal once as "25
// warehouses: 0.00 tps — no throughput left" on a rung two earlier runs had
// driven at 161 and 229 tps, because the driver skipped its execute phase.
func TestZeroThroughputIsNotACeiling(t *testing.T) {
	src, err := os.ReadFile("scale_depth.go")
	if err != nil {
		t.Fatalf("read scale_depth.go: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, "sample.TPS <= 0") {
		t.Error("a stage that drove no transactions at all is no longer rejected; it would be reported as the wall")
	}
	if !strings.Contains(body, "params.MinTPS && i == 0") {
		t.Error("the smallest rung falling under the floor must fail: there is nothing to scale from")
	}
}

// An endpoint that stops serving must take its own evidence with it. Three
// consecutive depth runs failed at 50 warehouses with "connection refused" and
// none could say whether the pod was evicted, OOM-killed or crashed, because
// the env only collects the endpoint IT provisioned and these ladders create
// their own.
func TestLaddersCaptureTheEndpointThatDied(t *testing.T) {
	for _, f := range []string{"scale_depth.go", "scale.go", "scale_tables.go",
		"scale_endpoints.go", "scale_branches.go", "scale_transaction.go", "scale_wal_ingest.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !strings.Contains(string(src), "captureEndpointDiag(") {
			t.Errorf("%s does not capture the endpoint's state when it stops serving", f)
		}
	}
	diag, err := os.ReadFile("endpoint_diag.go")
	if err != nil {
		t.Fatalf("read endpoint_diag.go: %v", err)
	}
	body := string(diag)
	// Eviction, OOM and a clean crash each report in a different place.
	for _, want := range []string{"Reason", "LastState", "RestartCount", "LogsPrevious("} {
		if !strings.Contains(body, want) {
			t.Errorf("the endpoint verdict ignores %s, so one of evicted/OOMKilled/crashed would go unexplained", want)
		}
	}
}

// A ladder tagged "perf" would rejoin the nightly schedule silently.
func TestScaleLaddersAreNotOnThePerfSchedule(t *testing.T) {
	for _, tc := range core.Default().List() {
		if !strings.HasPrefix(tc.Name, "perf/scale") {
			continue
		}
		var tags []string
		tags = append(tags, tc.Tags...)
		for _, tag := range tags {
			if tag == "perf" || tag == "nightly" {
				t.Errorf("%s carries tag %q; scale ladders run on demand (--tag %s), not from a schedule", tc.Name, tag, scaleTier)
			}
		}
		if len(tags) == 0 {
			t.Errorf("%s carries no tag at all, so nothing can select it", tc.Name)
		}
	}
}

func TestLimitsLaddersParamsDeclaredInDefaults(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	for _, key := range []string{
		"perf.scale_endpoints.start", "perf.scale_endpoints.factor", "perf.scale_endpoints.max_stages",
		"perf.scale_endpoints.stop_max_create_time", "perf.scale_endpoints.budget",
		"perf.scale_endpoints.probe_timeout",
		"perf.scale_branches.start", "perf.scale_branches.factor", "perf.scale_branches.max_stages",
		"perf.scale_branches.stop_max_fork_time", "perf.scale_branches.max_parent_read_ratio",
		"perf.scale_branches.seed_rows", "perf.scale_branches.budget", "perf.scale_branches.read_timeout",
		"perf.scale_transaction.start", "perf.scale_transaction.factor", "perf.scale_transaction.max_stages",
		"perf.scale_transaction.stop_statement_timeout", "perf.scale_transaction.payload_bytes",
		"perf.scale_transaction.recover_timeout", "perf.scale_transaction.budget",
		"perf.scale_wal_ingest.start", "perf.scale_wal_ingest.factor", "perf.scale_wal_ingest.max_stages",
		"perf.scale_wal_ingest.stop_max_p99", "perf.scale_wal_ingest.stop_throughput_drop_pct",
		"perf.scale_wal_ingest.batch_rows", "perf.scale_wal_ingest.payload_bytes",
		"perf.scale_wal_ingest.statement_timeout",
	} {
		if !cfg.Has(key) {
			t.Errorf("%s is read by the test but not declared in configs/default.yaml", key)
		}
	}
	wal := walIngestParamsFromConfig(cfg)
	if wal.StatementTimeout <= wal.StageDuration {
		t.Errorf("scale_wal_ingest statement_timeout = %s, must exceed stage_duration %s", wal.StatementTimeout, wal.StageDuration)
	}
}
