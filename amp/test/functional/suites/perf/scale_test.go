package perf

import (
	"errors"
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
	// The whole ladder must fit the registered timeout.
	if budget := time.Duration(p.MaxDrivers) * p.StageDuration; budget > 3*time.Hour {
		t.Errorf("defaults spend %s on measured windows alone, leaving no room for provisioning in the 4h timeout", budget)
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

// A stage that ERRORED measured nothing, so the ladder must not report it as a
// ceiling and pass. The first version did: a dead port-forward at stage 2 came
// back as "ceiling = 1 driver" with a green result and a failed step inside it.
// Ceilings come from measurements (floor_pct, aggregate drop, min_tps); an
// error is a failure to explain.
func TestStageErrorIsNotACeiling(t *testing.T) {
	src, err := os.ReadFile("scale.go")
	if err != nil {
		t.Fatalf("read scale.go: %v", err)
	}
	body := string(src)
	// The error branch must return, not set a stop reason and break.
	i := strings.Index(body, "if err != nil {\n\t\t\t// A stage that ERRORED")
	if i < 0 {
		t.Fatal("the stage-error branch no longer explains why an error is not a ceiling")
	}
	tail := body[i : i+700]
	if !strings.Contains(tail, "return fmt.Errorf(") {
		t.Error("a stage error no longer fails the ladder")
	}
	if strings.Contains(tail, "stopReason =") {
		t.Error("a stage error is being turned into a stop reason again")
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
