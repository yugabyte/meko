// Branch depth versus performance: does a workload on a fork run like one on
// the root, and does it matter how many forks deep it sits? A page miss on a
// branch is looked up in the branch, then in its parent, and so on up to the
// root, and those lookups are not pushed down today, so the cost is on the READ
// path on a cache miss and grows with depth. The writes pay it too, but only
// because they must read the rows they change: update_ms and delete_ms grow
// with depth for the same reason the reads do, not because the WAL cares which
// branch it belongs to. insert_ms writes rows nobody has, so it is the control
// on that side.
//
// One loaded root, then a chain root -> child -> grandchild -> ... to
// perf.branch_depth.max_depth. Every level gets a FRESH endpoint (the root's is
// rolled after a flush), so its first reads are cache misses at every layer,
// and is measured the same way: cold random point reads timed inside the
// server, the same reads warm, a cold full scan, then a timed INSERT, UPDATE
// and DELETE of its own slice of the rows. Each level writes DIFFERENT rows
// before the next fork, so a deeper branch has pages whose latest version lives
// at every ancestor: the lookup chain Kannan described, not a fork of an
// untouched parent.
//
// The id space is banded so every level's writes are its own and the reads can
// still be checked. Ids rise through one update band per level, then one delete
// band per level, then a pristine region no level ever writes; inserts go above
// the loaded rows entirely. Reads sample the update bands and the pristine
// region, never a delete band, so a sampled row always exists and its expected
// value is known. Each level inserts exactly as many rows as it deletes, so
// count(*) is the loaded row count at every depth.
//
// A level's endpoint is deleted once its measurements are flushed, so exactly
// one compute is alive at a time. The branches all survive — they are what the
// chain is made of — but a 20-rung ladder holding 20 endpoints at 2 cores and
// 40Gi of ephemeral storage each would never schedule on a test cluster. The
// cost is that nodes fall idle as the ladder climbs and the autoscaler reclaims
// them, sometimes evicting the pod still under measurement; readLevel measures
// again on the replacement rather than publish the eviction as latency.
//
// Every depth reports its numbers as branch.<metric>@depth<N>. Only cold p50,
// the cold scan and the update on the perf.branch_depth.gate_depths rungs are
// gated against portal history; p99, warm reads, insert, delete, fork, endpoint
// and ratio figures ride along as informational.
// Cold p50 at perf.branch_depth.ratio_depth over cold p50 at the root is
// asserted in-run against perf.branch_depth.max_ratio.
package perf

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pageserver"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	testNameBranchDepth = "perf/branch_depth"
	branchDepthPrefix   = "branch."
	branchDepthTable    = "bd_rows"
	branchDepthTimings  = "bd_timings"
	// "3" = insert and delete joined the update, and the id space is banded for
	// them; the depth-N series from the earlier harnesses measured less work.
	// "4" = each write and the cold scan is the median of perf.branch_depth.repeats
	// statements over disjoint slices, not one statement over the whole band.
	// "5" = a gated rung's cold scan is the median of perf.branch_depth.scan_passes
	// passes on rolled pods, and its writes run on the last rolled pod.
	// "6" = a gated rung's cold point-read p50 is the median of the per-pass
	// p50s over the same rolled pods.
	branchDepthHarnessRev = "6"

	DefaultBranchDepthMaxDepth     = 20
	DefaultBranchDepthRows         = 2000000
	DefaultBranchDepthBatchRows    = 200000
	DefaultBranchDepthPayloadBytes = 64
	DefaultBranchDepthSamples      = 300
	DefaultBranchDepthTouchRows    = 25000
	DefaultBranchDepthMaxRatio     = 2.0
	DefaultBranchDepthRatioDepth   = 1
	DefaultBranchDepthEndpointCPU  = "2"
	DefaultBranchDepthEndpointMem  = "4Gi"
	DefaultBranchDepthReadyTimeout = 10 * time.Minute
	DefaultBranchDepthRepeats      = 5
	DefaultBranchDepthScanPasses   = 3

	// Absolute floors on the gated datapoints: a history delta smaller than these
	// is run-to-run noise at this dataset size, whatever percent it comes to.
	branchDepthColdFloorMS  = 1.0
	branchDepthScanFloorMS  = 200.0
	branchDepthWriteFloorMS = 100.0
	// Cold reads pay one backing round trip per ancestor, so the whole ladder
	// moves with the shared cluster's load: 0.261002.2 and 0.261003.0 carry the
	// same amp code and measured 73.5 ms at depth 20 against a 54.9 ms median.
	// A percent of the baseline, see coldFloorMS.
	branchDepthColdFloorPct = 40.0
)

// Every depth is measured and charted; these are the rungs whose numbers become
// GATED datapoints. A 21-rung ladder gating three metrics each would put 63
// series on the portal, most of them saying the same thing as their neighbour.
var DefaultBranchDepthGateDepths = []string{"0", "1", "5", "10", "20"}

type branchDepthParams struct {
	MaxDepth     int
	Rows         int
	BatchRows    int
	PayloadBytes int
	Samples      int
	TouchRows    int
	MaxRatio     float64
	RatioDepth   int
	GateDepths   []int
	EndpointCPU  string
	EndpointMem  string
	ReadyTimeout time.Duration
	Repeats      int
	ScanPasses   int
}

func branchDepthParamsFromConfig(c *core.Config) (branchDepthParams, error) {
	p := branchDepthParams{
		MaxDepth:     c.GetInt("perf.branch_depth.max_depth", DefaultBranchDepthMaxDepth),
		Rows:         c.GetInt("perf.branch_depth.rows", DefaultBranchDepthRows),
		BatchRows:    max(c.GetInt("perf.branch_depth.batch_rows", DefaultBranchDepthBatchRows), 1),
		PayloadBytes: c.GetInt("perf.branch_depth.payload_bytes", DefaultBranchDepthPayloadBytes),
		Samples:      c.GetInt("perf.branch_depth.samples", DefaultBranchDepthSamples),
		TouchRows:    c.GetInt("perf.branch_depth.touch_rows", DefaultBranchDepthTouchRows),
		MaxRatio:     c.GetFloat("perf.branch_depth.max_ratio", DefaultBranchDepthMaxRatio),
		RatioDepth:   c.GetInt("perf.branch_depth.ratio_depth", DefaultBranchDepthRatioDepth),
		EndpointCPU:  c.GetString("perf.branch_depth.endpoint_cpu", DefaultBranchDepthEndpointCPU),
		EndpointMem:  c.GetString("perf.branch_depth.endpoint_memory", DefaultBranchDepthEndpointMem),
		ReadyTimeout: c.GetDuration("perf.branch_depth.ready_timeout", DefaultBranchDepthReadyTimeout),
		Repeats:      max(c.GetInt("perf.branch_depth.repeats", DefaultBranchDepthRepeats), 1),
		ScanPasses:   max(c.GetInt("perf.branch_depth.scan_passes", DefaultBranchDepthScanPasses), 1),
	}
	if p.MaxDepth < 1 {
		return p, fmt.Errorf("perf.branch_depth.max_depth is %d; the test compares at least one fork with the root", p.MaxDepth)
	}
	if p.Rows < 1000 || p.Samples < 1 {
		return p, fmt.Errorf("perf.branch_depth.rows (%d) and samples (%d) must be positive, rows at least 1000", p.Rows, p.Samples)
	}
	// Each level needs an update band and a delete band of its own, and rows
	// left over that no level ever writes for the reads to land on.
	if p.TouchRows < 1 || 2*p.TouchRows*(p.MaxDepth+1) >= p.Rows {
		return p, fmt.Errorf("perf.branch_depth.touch_rows %d needs an update and a delete band per level (2 x %d x %d) plus rows left pristine, which does not fit in the %d rows",
			p.TouchRows, p.TouchRows, p.MaxDepth+1, p.Rows)
	}
	if p.TouchRows < p.Repeats {
		return p, fmt.Errorf("perf.branch_depth.touch_rows %d cannot be split into %d write slices", p.TouchRows, p.Repeats)
	}
	if p.MaxRatio > 0 && (p.RatioDepth < 1 || p.RatioDepth > p.MaxDepth) {
		return p, fmt.Errorf("perf.branch_depth.ratio_depth %d is not in 1..%d, so the max_ratio assertion would never run", p.RatioDepth, p.MaxDepth)
	}
	// The same floor chain_depth_read enforces: below this a p99 is just the
	// maximum, and the ratio compares one read per side.
	if p.MaxRatio > 0 && p.Samples < minChainRatioSamples {
		return p, fmt.Errorf("perf.branch_depth.samples is %d, too few for a p99: below %d the ratio assertion compares two maxima, so set max_ratio to 0 or raise samples", p.Samples, minChainRatioSamples)
	}
	gates, err := parseGateDepths(c.GetStringSlice("perf.branch_depth.gate_depths", DefaultBranchDepthGateDepths))
	if err != nil {
		return p, fmt.Errorf("perf.branch_depth.gate_depths: %w", err)
	}
	// Lowering max_depth must not turn a default list into an error, so rungs
	// past the end are dropped rather than rejected.
	for _, d := range gates {
		if d <= p.MaxDepth {
			p.GateDepths = append(p.GateDepths, d)
		}
	}
	// Only gated rungs are read on several pods, and a ratio of a single pod
	// over a median of pods is the noise the passes exist to remove.
	if p.MaxRatio > 0 && (!slices.Contains(p.GateDepths, 0) || !slices.Contains(p.GateDepths, p.RatioDepth)) {
		return p, fmt.Errorf("perf.branch_depth.gate_depths %v must include 0 and ratio_depth %d for the max_ratio assertion", p.GateDepths, p.RatioDepth)
	}
	return p, nil
}

func init() {
	core.Register(core.Test{
		Name:       testNameBranchDepth,
		Suite:      "perf",
		Category:   core.Perf,
		Env:        core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:       []string{"perf"},
		OwningTeam: core.TeamCompute,
		Description: "Read and write latency at each branch depth: load perf.branch_depth.rows rows into a root, then fork a chain root -> child -> grandchild to perf.branch_depth.max_depth, " +
			"each level on a fresh endpoint. Every level is measured the same way, cold random point reads timed in the server, the same reads warm, a cold full scan, then a timed INSERT, UPDATE and " +
			"DELETE of its own perf.branch_depth.touch_rows band before the next fork, so a deeper branch has pages whose newest version lives at every ancestor. One endpoint is alive at a time. " +
			"Every depth is reported as branch.<metric>@depth<N>, the perf.branch_depth.gate_depths rungs gated on history, and cold p50 at perf.branch_depth.ratio_depth is asserted within " +
			"perf.branch_depth.max_ratio of the root's.",
		Params: []string{"perf.branch_depth", "perf.gate"},
		// 21 rungs by default, each a fork, an endpoint, a cold read set, a
		// full scan and an update — and the scan and update both grow with depth.
		Timeout: 4 * time.Hour,
		Run:     runBranchDepth,
	})
}

func runBranchDepth(t *core.T) error {
	env, ok := t.Env.(ampMultitenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, ok := t.Env.(interface{ AmpKube() *k8s.Kubectl })
	if !ok || kubeEnv.AmpKube() == nil {
		t.Skip("the root's cold read needs its compute rolled and every branch flushed, which needs a kube context")
		return nil
	}
	params, err := branchDepthParamsFromConfig(t.Options)
	if err != nil {
		return err
	}
	logf := t.Masker().Logf(log.Printf)
	cl, err := env.AmpController(t.Context)
	if err != nil {
		return err
	}
	id := identityFromConfig(t.Options, "", frameworkCommit(), loadFieldsBranchDepth(params))
	t.SetPerfIdentity(id.core())

	r := &branchDepthRun{t: t, cl: cl, env: env, kube: kubeEnv.AmpKube(), poolID: env.AmpPoolID(), params: params, logf: logf}
	defer r.cleanup()
	if err := r.standUp(); err != nil {
		return err
	}
	if err := r.load(); err != nil {
		return err
	}
	for depth := 0; depth <= params.MaxDepth; depth++ {
		if err := r.level(depth); err != nil {
			return fmt.Errorf("depth %d: %w", depth, err)
		}
	}
	return r.gate(id)
}

// branchLevel is one rung of the chain: the branch, its fresh endpoint, and
// what was measured on it.
type branchLevel struct {
	depth      int
	ten        tenant
	forkMS     float64
	endpointMS float64
	// flushLSN is what the next fork forks AT. Without it the controller
	// derives the fork point from a live endpoint on the parent, and this test
	// has already given that endpoint back.
	flushLSN string
	cold     []float64
	// One cold p50 per pass on a rolled pod, set on gated rungs only.
	coldPasses []float64
	warm       []float64
	// The *MS fields are the medians of the matching per-slice timings.
	scanMS   float64
	insertMS float64
	updateMS float64
	deleteMS float64
	scanRuns []float64
	// One per-slice median per cold scan pass, set on gated rungs only.
	scanPasses []float64
	insertRuns []float64
	updateRuns []float64
	deleteRuns []float64
}

type branchDepthRun struct {
	t      *core.T
	cl     *controller.Client
	env    ampMultitenantEnv
	kube   *k8s.Kubectl
	poolID string
	params branchDepthParams
	logf   func(string, ...any)

	levels  []*branchLevel
	tableMB float64
}

func (r *branchDepthRun) root() *branchLevel { return r.levels[0] }

func (r *branchDepthRun) cleanup() {
	for _, l := range r.levels {
		if l.ten.Conn != nil {
			l.ten.Conn.Close()
			l.ten.Conn = nil
		}
	}
	if len(r.levels) == 0 || r.root().ten.ClusterID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if _, err := r.cl.DeleteClusterAndWait(ctx, r.poolID, r.root().ten.ClusterID); err != nil {
		r.logf("branch_depth: could not delete cluster %s: %v", r.root().ten.ClusterID, err)
	}
}

func (r *branchDepthRun) standUp() error {
	return r.t.Step("create_tenant", func(s *core.StepHandle) error {
		ten, tm, err := createTenant(r.t, r.cl, r.env, r.poolID, 0, multitenantParams{
			EndpointCPU: r.params.EndpointCPU, EndpointMemory: r.params.EndpointMem})
		if err != nil {
			return fmt.Errorf("%w%s", err, schedulingHint(r.t, r.logf))
		}
		r.levels = []*branchLevel{{ten: ten, endpointMS: tm.endpointMS}}
		s.Set("cluster", ten.ClusterID)
		s.Set("endpoint", ten.Endpoint.ID)
		s.Set("endpoint_size", r.params.EndpointCPU+" / "+r.params.EndpointMem)
		return nil
	})
}

func (r *branchDepthRun) load() error {
	ctx := r.t.Context
	p := r.params
	conn := r.root().ten.Conn
	return r.t.Step("load", func(s *core.StepHandle) error {
		start := time.Now()
		for _, stmt := range []string{
			"CREATE TABLE IF NOT EXISTS " + branchDepthTable + " (id bigint PRIMARY KEY, v bigint NOT NULL, pad text)",
			"CREATE TABLE IF NOT EXISTS " + branchDepthTimings + " (depth int NOT NULL, kind text NOT NULL, ms double precision NOT NULL)",
		} {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		for lo := int64(1); lo <= int64(p.Rows); lo += int64(p.BatchRows) {
			hi := min(lo+int64(p.BatchRows)-1, int64(p.Rows))
			if _, err := conn.Exec(ctx, fmt.Sprintf(
				"INSERT INTO %s (id, v, pad) SELECT g, g * 7, repeat('x', %d) FROM generate_series($1::bigint, $2::bigint) g ON CONFLICT (id) DO NOTHING",
				branchDepthTable, p.PayloadBytes), lo, hi); err != nil {
				return fmt.Errorf("rows %d..%d: %w", lo, hi, err)
			}
		}
		bytes, err := conn.QueryInt(ctx, "SELECT pg_total_relation_size('"+branchDepthTable+"')")
		if err != nil {
			return err
		}
		r.tableMB = float64(bytes) / (1024 * 1024)
		s.Set("rows", p.Rows)
		s.Set("table_mb", fmt.Sprintf("%.1f", r.tableMB))
		s.Set("ms", fmt.Sprintf("%.0f", msSince(start)))
		return nil
	})
}

// level brings depth up on a fresh endpoint, measures it, touches its slice,
// flushes so the next fork reads materialised pages rather than WAL, and gives
// the compute back.
func (r *branchDepthRun) level(depth int) error {
	if depth == 0 {
		if err := r.flushAndRestartRoot(); err != nil {
			return err
		}
	} else if err := r.fork(depth); err != nil {
		return err
	}
	l := r.levels[depth]
	if err := r.readLevel(l); err != nil {
		return err
	}
	if slices.Contains(r.params.GateDepths, depth) {
		if err := r.rescanCold(l); err != nil {
			return err
		}
	}
	if err := r.mutate(l); err != nil {
		if lostTheEndpoint(err) {
			return fmt.Errorf("the endpoint went away during the writes at depth %d, which leaves the band half-written and the rungs above it unmeasurable: %w", l.depth, err)
		}
		return err
	}
	if depth == r.params.MaxDepth {
		return nil
	}
	if err := r.flush(l, "flush_depth"+strconv.Itoa(depth)); err != nil {
		return err
	}
	return r.retireEndpoint(l)
}

// The branch stays; only its compute goes. The next level forks from the branch
// id, which needs nothing running.
func (r *branchDepthRun) retireEndpoint(l *branchLevel) error {
	return r.t.Step("retire_endpoint_depth"+strconv.Itoa(l.depth), func(s *core.StepHandle) error {
		if l.ten.Conn != nil {
			l.ten.Conn.Close()
			l.ten.Conn = nil
		}
		_, err := r.cl.DeleteEndpointAndWait(r.t.Context, r.poolID, l.ten.ClusterID, l.ten.BranchID, l.ten.Endpoint.ID)
		if err != nil {
			return err
		}
		s.Set("endpoint", l.ten.Endpoint.ID)
		return nil
	})
}

func (r *branchDepthRun) flushAndRestartRoot() error {
	root := r.root()
	if err := r.flush(root, "flush_root"); err != nil {
		return err
	}
	return r.restartEndpoint(root, "restart_root_endpoint")
}

// A pod roll drops the compute's caches and nothing on the branch.
func (r *branchDepthRun) restartEndpoint(l *branchLevel, step string) error {
	return r.t.Step(step, func(s *core.StepHandle) error {
		l.ten.Conn.Close()
		l.ten.Conn = nil
		start := time.Now()
		pod, err := ampstack.EndpointPodName(r.t.Context, r.kube, l.ten.Endpoint.ID)
		if err != nil {
			return err
		}
		if err := r.kube.Delete(r.t.Context, "pod", pod); err != nil {
			return fmt.Errorf("delete endpoint %s compute pod %s: %w", l.ten.Endpoint.ID, pod, err)
		}
		if err := ampstack.WaitEndpointWorkloadReady(r.t.Context, r.kube, l.ten.Endpoint.ID, r.params.ReadyTimeout); err != nil {
			return fmt.Errorf("%w%s", err, captureEndpointDiag(r.t, r.kube, l.ten, "after-restart", r.logf))
		}
		conn, err := r.env.ConnectEndpoint(r.t.Context, l.ten.Endpoint)
		if err != nil {
			return fmt.Errorf("%w%s", err, captureEndpointDiag(r.t, r.kube, l.ten, "after-restart", r.logf))
		}
		if _, err := conn.QueryInt(r.t.Context, "SELECT 1"); err != nil {
			conn.Close()
			return err
		}
		l.ten.Conn = conn
		// endpointMS stays the endpoint CREATE. A pod roll is a different
		// operation, so it is a step timing only and never joins that series.
		s.Set("roll_ms", fmt.Sprintf("%.0f", msSince(start)))
		return nil
	})
}

// One cold scan is one pod on one node at one moment, so a busy neighbour moves
// all of its slices together and their spread cannot tell: 0.261003.0 scanned
// depth 1 in 698, 717 and 721 ms and then 1256 ms. Cold point reads have the
// same problem: the root's p50 ranged 3.2-4.7 ms across runs, and a 3.17 ms root
// put depth 1 at 2.20x on 0.261007.0 where it is otherwise 1.4-1.7x. A gated
// rung is read and scanned again on a rolled pod and gates the median pass.
func (r *branchDepthRun) rescanCold(l *branchLevel) error {
	l.scanPasses = []float64{l.scanMS}
	l.coldPasses = []float64{median(l.cold)}
	ids := r.sampleReadIDs(r.t.Seed.Fork("branch_depth").Rand())
	for pass := 2; pass <= r.params.ScanPasses; pass++ {
		suffix := fmt.Sprintf("_pass%d", pass)
		if err := r.restartEndpoint(l, fmt.Sprintf("restart_endpoint_depth%d%s", l.depth, suffix)); err != nil {
			return err
		}
		cold, ms, err := r.coldPass(l, ids, suffix)
		if lostTheEndpoint(err) {
			r.logf("branch_depth: depth %d lost its endpoint mid-pass (%v); measuring again on the replacement", l.depth, err)
			if err := r.reconnect(l); err != nil {
				return err
			}
			cold, ms, err = r.coldPass(l, ids, suffix+"_retry")
		}
		if err != nil {
			return err
		}
		l.coldPasses = append(l.coldPasses, median(cold))
		l.scanPasses = append(l.scanPasses, ms)
	}
	l.scanMS = median(l.scanPasses)
	return nil
}

// The reads go first: the scan would warm the cache they need cold.
func (r *branchDepthRun) coldPass(l *branchLevel, ids []int, suffix string) (cold []float64, scanMS float64, err error) {
	if cold, err = r.timedReads(l, "cold", suffix, ids); err != nil {
		return nil, 0, err
	}
	scanMS, _, err = r.scanCold(l, suffix)
	return cold, scanMS, err
}

// coldP50 is what a rung's cold point read is gated on.
func coldP50(l *branchLevel) float64 {
	if len(l.coldPasses) > 1 {
		return median(l.coldPasses)
	}
	return median(l.cold)
}

func (r *branchDepthRun) fork(depth int) error {
	ctx := r.t.Context
	parent := r.levels[depth-1]
	l := &branchLevel{depth: depth}
	name := fmt.Sprintf("bd-d%d-%s", depth, r.t.ResourceSuffix())
	if err := r.t.Step("fork_depth"+strconv.Itoa(depth), func(s *core.StepHandle) error {
		start := time.Now()
		if _, err := r.cl.CreateBranchAndWait(ctx, r.poolID, parent.ten.ClusterID, controller.CreateBranchRequest{
			Name: name, ParentBranchID: parent.ten.BranchID, ForkPointLSN: parent.flushLSN}); err != nil {
			return err
		}
		l.forkMS = msSince(start)
		br, err := findBranchByName(ctx, r.cl, r.poolID, parent.ten.ClusterID, name)
		if err != nil {
			return err
		}
		l.ten = tenant{Index: depth, ClusterID: parent.ten.ClusterID, BranchID: br.BranchID, Table: branchDepthTable}
		s.Set("branch", br.BranchID)
		s.Set("parent", parent.ten.BranchID)
		s.Set("fork_point_lsn", parent.flushLSN)
		s.Set("ms", fmt.Sprintf("%.0f", l.forkMS))
		return nil
	}); err != nil {
		return err
	}
	// Appended only once the endpoint answers: a half-built level in r.levels
	// would be one the gate and the charts have to know to skip.
	if err := r.endpointFor(l); err != nil {
		return err
	}
	r.levels = append(r.levels, l)
	return nil
}

func (r *branchDepthRun) endpointFor(l *branchLevel) error {
	ctx := r.t.Context
	depth := l.depth
	return r.t.Step("endpoint_depth"+strconv.Itoa(depth), func(s *core.StepHandle) error {
		start := time.Now()
		if _, err := r.cl.CreateEndpointAndWait(ctx, r.poolID, l.ten.ClusterID, l.ten.BranchID, controller.CreateEndpointRequest{
			Role:      "read_write",
			Resources: &controller.EndpointResources{CPU: r.params.EndpointCPU, Memory: r.params.EndpointMem},
		}); err != nil {
			return fmt.Errorf("%w%s", err, schedulingHint(r.t, r.logf))
		}
		eps, err := r.cl.ListAllEndpoints(ctx, r.poolID, l.ten.ClusterID, l.ten.BranchID)
		if err != nil {
			return err
		}
		if len(eps) == 0 {
			return errors.New("the fork lists no endpoint after a successful create")
		}
		l.ten.Endpoint = eps[len(eps)-1]
		conn, err := r.env.ConnectEndpoint(ctx, l.ten.Endpoint)
		if err != nil {
			return fmt.Errorf("%w%s", err, captureEndpointDiag(r.t, r.kube, l.ten, "fork-endpoint", r.logf))
		}
		if _, err := conn.QueryInt(ctx, "SELECT 1"); err != nil {
			conn.Close()
			return err
		}
		l.ten.Conn = conn
		l.endpointMS = msSince(start)
		s.Set("endpoint", l.ten.Endpoint.ID)
		s.Set("ms", fmt.Sprintf("%.0f", l.endpointMS))
		return nil
	})
}

// Timed inside the server, like chain_depth_read: the tunnel's ~29 ms would
// hide a few-millisecond page miss.
// Retiring an endpoint every rung leaves nodes idle, and GKE's autoscaler
// reclaims one the moment it can — taking the pod still being measured with it
// ("deleting pod for node scale down" ended the depth-20 scan in run 60948b31).
// Reads change nothing on the branch, so the honest response is to come back on
// the replacement pod and measure the level again rather than report the
// eviction as the product's latency.
func (r *branchDepthRun) readLevel(l *branchLevel) error {
	err := r.measureLevel(l, "")
	if err == nil || !lostTheEndpoint(err) {
		return err
	}
	r.logf("branch_depth: depth %d lost its endpoint mid-measurement (%v); measuring again on the replacement", l.depth, err)
	if rerr := r.reconnect(l); rerr != nil {
		return errors.Join(err, rerr)
	}
	return r.measureLevel(l, "_retry")
}

// A pod that went away under a query, as opposed to an answer the product gave.
func lostTheEndpoint(err error) bool {
	return err != nil && (isTransportErr(err) || strings.Contains(err.Error(), "EOF"))
}

func (r *branchDepthRun) reconnect(l *branchLevel) error {
	return r.t.Step(fmt.Sprintf("reconnect_depth%d", l.depth), func(s *core.StepHandle) error {
		if l.ten.Conn != nil {
			l.ten.Conn.Close()
			l.ten.Conn = nil
		}
		if err := ampstack.WaitEndpointWorkloadReady(r.t.Context, r.kube, l.ten.Endpoint.ID, r.params.ReadyTimeout); err != nil {
			return fmt.Errorf("%w%s", err, captureEndpointDiag(r.t, r.kube, l.ten, "after-eviction", r.logf))
		}
		conn, err := r.env.ConnectEndpoint(r.t.Context, l.ten.Endpoint)
		if err != nil {
			return err
		}
		if _, err := conn.QueryInt(r.t.Context, "SELECT 1"); err != nil {
			conn.Close()
			return err
		}
		l.ten.Conn = conn
		return nil
	})
}

func (r *branchDepthRun) measureLevel(l *branchLevel, attempt string) error {
	rnd := r.t.Seed.Fork("branch_depth").Rand()
	ids := r.sampleReadIDs(rnd)
	var err error
	if l.cold, err = r.timedReads(l, "cold", attempt, ids); err != nil {
		return err
	}
	if l.warm, err = r.timedReads(l, "warm", attempt, ids); err != nil {
		return err
	}
	l.scanMS, l.scanRuns, err = r.scanCold(l, attempt)
	return err
}

func (r *branchDepthRun) scanCold(l *branchLevel, attempt string) (ms float64, runs []float64, err error) {
	err = r.t.Step(fmt.Sprintf("scan_cold_depth%d%s", l.depth, attempt), func(s *core.StepHandle) error {
		blocks, err := l.ten.Conn.QueryInt(r.t.Context,
			fmt.Sprintf("SELECT pg_relation_size('%s') / current_setting('block_size')::bigint", branchDepthTable))
		if err != nil {
			return err
		}
		slices := branchScanSlices(blocks, r.params.Repeats)
		// A seq scan with a ctid filter would read the whole table per slice,
		// every slice after the first warm.
		var plan []string
		if err := l.ten.Conn.Query(r.t.Context, func(rows pgx.Rows) error {
			var line string
			if err := rows.Scan(&line); err != nil {
				return err
			}
			plan = append(plan, strings.TrimSpace(line))
			return nil
		}, "EXPLAIN SELECT count(*) FROM "+branchDepthTable+" WHERE "+slices[0]); err != nil {
			return err
		}
		if len(slices) > 1 && !strings.Contains(strings.Join(plan, " "), "Tid Range Scan") {
			return fmt.Errorf("the planner does not scan a ctid slice by TID range (%q), so the slices are not disjoint reads", plan)
		}
		runs = nil
		var total int64
		for _, q := range slices {
			start := time.Now()
			n, err := l.ten.Conn.QueryInt(r.t.Context, "SELECT count(*) FROM "+branchDepthTable+" WHERE "+q)
			if err != nil {
				return err
			}
			runs = append(runs, msSince(start))
			total += n
		}
		ms = median(runs)
		if total != int64(r.params.Rows) {
			return fmt.Errorf("count(*) over the %d block slices = %d on depth %d, want %d", len(runs), total, l.depth, r.params.Rows)
		}
		s.Set("slices", len(runs))
		s.Set("ms", fmt.Sprintf("%.0f", ms))
		return nil
	})
	return ms, runs, err
}

// branchScanSlices cuts the table into n disjoint ctid block ranges, so each
// slice is as cold as the whole scan was. The last is open-ended.
func branchScanSlices(blocks int64, n int) []string {
	n = max(n, 1)
	if blocks < int64(n) {
		n = 1
	}
	step := blocks / int64(n)
	out := make([]string, n)
	for i := range out {
		lo := int64(i) * step
		if i == n-1 {
			out[i] = fmt.Sprintf("ctid >= '(%d,0)'::tid", lo)
			continue
		}
		out[i] = fmt.Sprintf("ctid >= '(%d,0)'::tid AND ctid < '(%d,0)'::tid", lo, lo+step)
	}
	return out
}

func (r *branchDepthRun) timedReads(l *branchLevel, kind, attempt string, ids []int) ([]float64, error) {
	ctx := r.t.Context
	var vals []float64
	err := r.t.Step(fmt.Sprintf("read_%s_depth%d%s", kind, l.depth, attempt), func(s *core.StepHandle) error {
		if _, err := l.ten.Conn.Exec(ctx, branchReadBlock(l.depth, kind, ids, r.sliceRows())); err != nil {
			return err
		}
		if err := l.ten.Conn.Query(ctx, func(rows pgx.Rows) error {
			var ms float64
			if err := rows.Scan(&ms); err != nil {
				return err
			}
			vals = append(vals, ms)
			return nil
		}, "SELECT ms FROM "+branchDepthTimings+" WHERE depth = $1 AND kind = $2", l.depth, kind); err != nil {
			return err
		}
		if len(vals) != len(ids) {
			return fmt.Errorf("%d reads timed, want %d", len(vals), len(ids))
		}
		s.Set("samples", len(vals))
		s.Set("p50_ms", fmt.Sprintf("%.3f", median(vals)))
		s.Set("p99_ms", fmt.Sprintf("%.3f", percentile(vals, 99)))
		return nil
	})
	return vals, err
}

// The bands, in rising id order. Level L updates (L*slice, (L+1)*slice],
// deletes deleteBase + the same offsets, and inserts above the loaded rows;
// everything past readBase is pristine.
func (r *branchDepthRun) sliceRows() int  { return r.params.TouchRows }
func (r *branchDepthRun) deleteBase() int { return (r.params.MaxDepth + 1) * r.params.TouchRows }
func (r *branchDepthRun) readBase() int   { return 2 * (r.params.MaxDepth + 1) * r.params.TouchRows }

// branchReadBlock times one point read per id and checks the value: at depth d
// the levels 0..d-1 have each rewritten their slice to -(level+1), everything
// else still holds the load's id*7. A wrong value means a branch served a
// stale or foreign version, which is worse than a slow one.
func branchReadBlock(depth int, kind string, ids []int, sliceRows int) string {
	idList := make([]string, len(ids))
	for i, id := range ids {
		idList[i] = strconv.Itoa(id)
	}
	touchedHi := depth * sliceRows
	return fmt.Sprintf(`DO $ampqa$
DECLARE
  ids bigint[] := ARRAY[%s];
  i bigint; t0 timestamptz; got bigint; want bigint; acc double precision[] := '{}';
BEGIN
  FOREACH i IN ARRAY ids LOOP
    t0 := clock_timestamp();
    SELECT v INTO got FROM %s WHERE id = i;
    acc := array_append(acc, (EXTRACT(epoch FROM (clock_timestamp() - t0)) * 1000)::double precision);
    want := CASE WHEN i <= %d THEN -(((i - 1) / %d) + 1) ELSE i * 7 END;
    IF got IS DISTINCT FROM want THEN
      RAISE EXCEPTION 'row %% at depth %d reads v = %%, want %%', i, got, want;
    END IF;
  END LOOP;
  DELETE FROM %s WHERE depth = %d AND kind = '%s';
  INSERT INTO %s SELECT %d, '%s', unnest(acc);
END
$ampqa$`, strings.Join(idList, ","), branchDepthTable, touchedHi, max(sliceRows, 1), depth,
		branchDepthTimings, depth, kind, branchDepthTimings, depth, kind)
}

// sampleReadIDs draws from the update bands and the pristine region only. A
// delete band would hand the read block a row an ancestor removed, which is a
// different assertion than this one.
func (r *branchDepthRun) sampleReadIDs(rnd *rand.Rand) []int {
	updates, pristineLo := r.deleteBase(), r.readBase()
	pool := updates + (r.params.Rows - pristineLo)
	want := min(r.params.Samples, pool)
	seen := make(map[int]bool, want)
	out := make([]int, 0, want)
	for len(out) < want {
		k := rnd.IntN(pool)
		id := k + 1
		if k >= updates {
			id = pristineLo + (k - updates) + 1
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// mutate runs the level's own writes: an INSERT above the loaded rows, an
// UPDATE of its update band, a DELETE of its delete band, each once per
// disjoint slice of the band. Insert and delete are the same size, so count(*)
// is unchanged for the next level's scan.
func (r *branchDepthRun) mutate(l *branchLevel) error {
	p := r.params
	base := int64(l.depth) * int64(r.sliceRows())
	l.insertRuns, l.updateRuns, l.deleteRuns = nil, nil, nil
	for i, sl := range branchWriteSlices(r.sliceRows(), p.Repeats) {
		lo, hi := base+sl[0], base+sl[1]
		ops := []struct {
			name string
			stmt string
			at   *[]float64
		}{
			{"insert", fmt.Sprintf("INSERT INTO %s (id, v, pad) SELECT g, g * 7, repeat('x', %d) FROM generate_series(%d, %d) g",
				branchDepthTable, p.PayloadBytes, int64(p.Rows)+lo, int64(p.Rows)+hi), &l.insertRuns},
			{"update", fmt.Sprintf("UPDATE %s SET v = %d WHERE id BETWEEN %d AND %d",
				branchDepthTable, -(l.depth + 1), lo, hi), &l.updateRuns},
			{"delete", fmt.Sprintf("DELETE FROM %s WHERE id BETWEEN %d AND %d",
				branchDepthTable, int64(r.deleteBase())+lo, int64(r.deleteBase())+hi), &l.deleteRuns},
		}
		for _, op := range ops {
			if err := r.t.Step(fmt.Sprintf("%s_depth%d", op.name, l.depth), func(s *core.StepHandle) error {
				s.Set("slice", i+1)
				start := time.Now()
				n, err := l.ten.Conn.Exec(r.t.Context, op.stmt)
				if err != nil {
					return err
				}
				ms := msSince(start)
				*op.at = append(*op.at, ms)
				if want := hi - lo + 1; n != want {
					return fmt.Errorf("%s hit %d rows, want %d", op.name, n, want)
				}
				s.Set("rows", hi-lo+1)
				s.Set("ms", fmt.Sprintf("%.0f", ms))
				return nil
			}); err != nil {
				return err
			}
		}
	}
	l.insertMS, l.updateMS, l.deleteMS = median(l.insertRuns), median(l.updateRuns), median(l.deleteRuns)
	return nil
}

// branchWriteSlices is n inclusive, 1-based ranges covering rows ids; the last
// takes the remainder.
func branchWriteSlices(rows, n int) [][2]int64 {
	n = min(max(n, 1), max(rows, 1))
	step := int64(rows / n)
	out := make([][2]int64, n)
	for i := range out {
		lo := int64(i)*step + 1
		hi := lo + step - 1
		if i == n-1 {
			hi = int64(rows)
		}
		out[i] = [2]int64{lo, hi}
	}
	return out
}

func (r *branchDepthRun) flush(l *branchLevel, step string) error {
	return r.t.Step(step, func(s *core.StepHandle) error {
		lsn, err := l.ten.Conn.QueryString(r.t.Context, "SELECT pg_current_wal_flush_lsn()::text")
		if err != nil {
			return err
		}
		// The pageserver addresses a branch by the id its own endpoint reports.
		branchID, err := l.ten.Conn.QueryString(r.t.Context, "SELECT current_setting('yb_amp.branch_id')")
		if err != nil {
			return err
		}
		ps, err := endpointPageserver(r.t.Context, r.kube, l.ten.Endpoint.ID)
		if err != nil {
			return err
		}
		if err := ps.FlushBranch(r.t.Context, branchID, lsn); err != nil {
			return err
		}
		l.flushLSN = lsn
		s.Set("flush_lsn", lsn)
		return nil
	})
}

// endpointPageserver reaches the pageserver's management port from inside the
// endpoint's own pod, the only place it answers.
func endpointPageserver(ctx context.Context, kube *k8s.Kubectl, endpointID string) (*pageserver.Client, error) {
	pod, err := ampstack.EndpointPodName(ctx, kube, endpointID)
	if err != nil {
		return nil, err
	}
	return pageserver.New(func(ctx context.Context, script string) (string, error) {
		res, err := kube.ForPod(pod, chainEndpointContainer).Run(ctx, script)
		if err != nil {
			return "", err
		}
		switch res.ExitCode {
		case 0:
			return res.Stdout, nil
		case pageserver.ConnectRefusedExit:
			return "", fmt.Errorf("nothing answers on the pageserver's management port in pod %s", pod)
		default:
			return "", fmt.Errorf("pageserver request in pod %s exited %d: %s", pod, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
	}, pageserver.DefaultHTTPPort), nil
}

func (r *branchDepthRun) gate(id Identity) error {
	return r.t.Step("gate_branch_depth", func(s *core.StepHandle) error {
		gated, info, ratioErr := branchDepthDatapoints(r.levels, r.tableMB, r.params)
		for _, d := range r.params.GateDepths {
			if ratio, ok := branchColdRatio(r.levels, d); ok {
				s.Set(fmt.Sprintf("cold_p50_ratio@depth%d", d), fmt.Sprintf("%.2fx", ratio))
			}
		}
		s.Set("max_depth", r.params.MaxDepth)
		s.Set("gate_depths", fmt.Sprint(r.params.GateDepths))
		s.Set("table_mb", fmt.Sprintf("%.1f", r.tableMB))
		for _, c := range branchDepthCharts(r.levels) {
			r.t.Chart(c)
		}
		gateErr := emitAndGateAuto(r.t, perfFromConfig(r.t.Options), scaleVersion(r.t),
			testNameBranchDepth, gated, info, id, r.logf)
		return errors.Join(ratioErr, gateErr)
	})
}

// branchColdRatio is cold p50 at depth over cold p50 at the root. p50, not
// p99: two fresh pods differ in their tails by more than a page miss costs.
func branchColdRatio(levels []*branchLevel, depth int) (float64, bool) {
	if depth < 1 || depth >= len(levels) || len(levels[0].cold) == 0 || len(levels[depth].cold) == 0 {
		return 0, false
	}
	// Like for like: the pod medians only when both sides were read on several pods.
	root, l := levels[0], levels[depth]
	base, v := median(root.cold), median(l.cold)
	if len(root.coldPasses) > 1 && len(l.coldPasses) > 1 {
		base, v = median(root.coldPasses), median(l.coldPasses)
	}
	if base <= 0 {
		return 0, false
	}
	return v / base, true
}

func branchDepthDatapoints(levels []*branchLevel, tableMB float64, p branchDepthParams) (gated, info []core.Datapoint, err error) {
	for _, l := range levels {
		suffix := fmt.Sprintf("@depth%d", l.depth)
		// An ungated rung still reports its headline, so the ladder is complete
		// on the result page and in history even where nothing gates it.
		bucket := &info
		if slices.Contains(p.GateDepths, l.depth) {
			bucket = &gated
		}
		if len(l.cold) > 0 {
			cold := coldP50(l)
			*bucket = append(*bucket, core.Datapoint{Name: branchDepthPrefix + "read_cold_p50_ms" + suffix, Value: cold, Unit: "ms", Direction: core.LowerIsBetter,
				MinDelta: coldFloorMS(cold)})
			// p99 of the sample set is ~3 reads, so one slow read triples it.
			info = append(info, core.Datapoint{Name: branchDepthPrefix + "read_cold_p99_ms" + suffix, Value: percentile(l.cold, 99), Unit: "ms", Direction: core.LowerIsBetter})
		}
		if len(l.warm) > 0 {
			// A warm read is a few microseconds, below what the timer resolves as a change.
			info = append(info, core.Datapoint{Name: branchDepthPrefix + "read_warm_p50_ms" + suffix, Value: median(l.warm), Unit: "ms", Direction: core.LowerIsBetter})
		}
		if l.scanMS > 0 {
			runs := l.scanRuns
			if len(l.scanPasses) > 1 {
				runs = l.scanPasses
			}
			// Spread across rolled pods is the history band's job, as in splitDatapoints.
			*bucket = append(*bucket, core.Datapoint{Name: branchDepthPrefix + "scan_cold_ms" + suffix, Value: l.scanMS, Unit: "ms", Direction: core.LowerIsBetter, MinDelta: branchDepthScanFloorMS,
				N: len(runs), Samples: runs, SpreadPct: spreadPct(aggregate(l.scanRuns))})
		}
		if l.updateMS > 0 {
			*bucket = append(*bucket, core.Datapoint{Name: branchDepthPrefix + "update_ms" + suffix, Value: l.updateMS, Unit: "ms", Direction: core.LowerIsBetter, MinDelta: branchDepthWriteFloorMS,
				N: len(l.updateRuns), Samples: l.updateRuns, SpreadPct: spreadPct(aggregate(l.updateRuns))})
		}
		for _, w := range []struct {
			name string
			v    float64
			runs []float64
		}{{"insert_ms", l.insertMS, l.insertRuns}, {"delete_ms", l.deleteMS, l.deleteRuns}} {
			if w.v > 0 {
				info = append(info, core.Datapoint{Name: branchDepthPrefix + w.name + suffix, Value: w.v, Unit: "ms", Direction: core.LowerIsBetter,
					N: len(w.runs), Samples: w.runs})
			}
		}
		if l.endpointMS > 0 {
			info = append(info, core.Datapoint{Name: branchDepthPrefix + "endpoint_ms" + suffix, Value: l.endpointMS, Unit: "ms", Direction: core.LowerIsBetter})
		}
		if l.depth > 0 && l.forkMS > 0 {
			info = append(info, core.Datapoint{Name: branchDepthPrefix + "fork_ms" + suffix, Value: l.forkMS, Unit: "ms", Direction: core.LowerIsBetter})
		}
		if ratio, ok := branchColdRatio(levels, l.depth); ok {
			info = append(info, core.Datapoint{Name: branchDepthPrefix + "cold_p50_ratio" + suffix, Value: ratio, Unit: "x", Direction: core.LowerIsBetter})
		}
	}
	if tableMB > 0 {
		info = append(info, core.Datapoint{Name: branchDepthPrefix + "table_mb", Value: tableMB, Unit: "MiB", Direction: core.LowerIsBetter})
	}
	if ratio, ok := branchColdRatio(levels, p.RatioDepth); ok && p.MaxRatio > 0 && ratio > p.MaxRatio {
		err = fmt.Errorf("cold point-read p50 on the depth-%d branch is %.2fx the root's (limit %.2fx, %d samples per pod, %d pod(s) per side): a page miss on the fork costs more than one extra lookup should",
			p.RatioDepth, ratio, p.MaxRatio, len(levels[p.RatioDepth].cold), max(len(levels[p.RatioDepth].coldPasses), 1))
	}
	return gated, info, err
}

// coldFloorMS is the delta a cold read v must clear to be flagged. Both gates
// test |v - baseline| against it, so v*p/(100+p) flags exactly v >= baseline*(1+p/100).
func coldFloorMS(v float64) float64 {
	return max(branchDepthColdFloorMS, v*branchDepthColdFloorPct/(100+branchDepthColdFloorPct))
}

func branchDepthCharts(levels []*branchLevel) []core.Chart {
	var cold50, cold99, warm50, scan, ins, upd, del []core.ChartPoint
	for _, l := range levels {
		x := float64(l.depth)
		if len(l.cold) > 0 {
			cold50 = append(cold50, core.ChartPoint{X: x, Y: median(l.cold)})
			cold99 = append(cold99, core.ChartPoint{X: x, Y: percentile(l.cold, 99)})
		}
		if len(l.warm) > 0 {
			warm50 = append(warm50, core.ChartPoint{X: x, Y: median(l.warm)})
		}
		if l.scanMS > 0 {
			scan = append(scan, core.ChartPoint{X: x, Y: l.scanMS})
		}
		if l.insertMS > 0 {
			ins = append(ins, core.ChartPoint{X: x, Y: l.insertMS})
		}
		if l.updateMS > 0 {
			upd = append(upd, core.ChartPoint{X: x, Y: l.updateMS})
		}
		if l.deleteMS > 0 {
			del = append(del, core.ChartPoint{X: x, Y: l.deleteMS})
		}
	}
	return []core.Chart{
		{
			Title: "Point read latency by branch depth", Kind: core.ChartLine,
			XLabel: "branch depth (0 = root)", YLabel: "latency (ms)",
			Series: []core.ChartSeries{{Name: "cold p50", Points: cold50}, {Name: "cold p99", Points: cold99}, {Name: "warm p50", Points: warm50}},
		},
		{
			Title: "Cold scan and writes by branch depth", Kind: core.ChartLine,
			XLabel: "branch depth (0 = root)", YLabel: "ms",
			Series: []core.ChartSeries{{Name: "cold count(*)", Points: scan}, {Name: "insert", Points: ins},
				{Name: "update", Points: upd}, {Name: "delete", Points: del}},
		},
	}
}

func loadFieldsBranchDepth(p branchDepthParams) LoadFields {
	return LoadFields{
		Script:  fmt.Sprintf("branch_depth@%s/%s/d%d/t%d/r%d/p%d", p.EndpointCPU, p.EndpointMem, p.MaxDepth, p.TouchRows, p.Repeats, p.ScanPasses),
		Rows:    p.Rows,
		Clients: p.Samples,
		Scale:   p.PayloadBytes,
		Harness: branchDepthHarnessRev,
	}
}

// parseGateDepths is parseDepths for a list that MAY name the root: depth 0 is
// the baseline every ratio is taken against, so it is a legal rung here.
func parseGateDepths(raw []string) ([]int, error) {
	out := make([]int, 0, len(raw))
	for _, str := range raw {
		d, err := strconv.Atoi(strings.TrimSpace(str))
		if err != nil || d < 0 {
			return nil, fmt.Errorf("%q is not a non-negative integer", str)
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, errors.New("no depths listed")
	}
	sort.Ints(out)
	return out, nil
}
