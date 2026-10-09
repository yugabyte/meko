package perf

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
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
	testNameChainDepth     = "perf/chain_depth_read"
	chainPrefix            = "read."
	chainTablePrefix       = "cd_depth"
	chainHarnessRev        = "2"
	chainTimings           = "cd_timings"
	chainEndpointContainer = "endpoint"
)

const (
	DefaultChainPages        = 1000
	DefaultChainSamples      = 300
	DefaultChainPayloadBytes = 700
	DefaultChainMaxRatio     = 4.0
	DefaultChainRatioDepth   = 20
	DefaultChainEndpointCPU  = "2"
	DefaultChainEndpointMem  = "4Gi"
	DefaultChainReadyTimeout = 10 * time.Minute

	minChainRatioSamples = 100
)

var DefaultChainDepths = []string{"1", "5", "10", "20", "40"}

type chainParams struct {
	Pages        int
	Depths       []int
	Samples      int
	PayloadBytes int
	MaxRatio     float64
	RatioDepth   int
	EndpointCPU  string
	EndpointMem  string
	ReadyTimeout time.Duration
}

func chainParamsFromConfig(c *core.Config) (chainParams, error) {
	depths, err := parseDepths(c.GetStringSlice("perf.chain_depth.depths", DefaultChainDepths))
	if err != nil {
		return chainParams{}, err
	}
	p := chainParams{
		Pages:        max(c.GetInt("perf.chain_depth.pages", DefaultChainPages), 1),
		Depths:       depths,
		Samples:      max(c.GetInt("perf.chain_depth.samples", DefaultChainSamples), 1),
		PayloadBytes: c.GetInt("perf.chain_depth.payload_bytes", DefaultChainPayloadBytes),
		MaxRatio:     c.GetFloat("perf.chain_depth.max_ratio", DefaultChainMaxRatio),
		RatioDepth:   c.GetInt("perf.chain_depth.ratio_depth", DefaultChainRatioDepth),
		EndpointCPU:  c.GetString("perf.chain_depth.endpoint_cpu", DefaultChainEndpointCPU),
		EndpointMem:  c.GetString("perf.chain_depth.endpoint_memory", DefaultChainEndpointMem),
		ReadyTimeout: c.GetDuration("perf.chain_depth.ready_timeout", DefaultChainReadyTimeout),
	}
	if p.MaxRatio > 0 {
		if !slices.Contains(p.Depths, p.RatioDepth) {
			return chainParams{}, fmt.Errorf("perf.chain_depth.ratio_depth %d is not in depths %v, so the max_ratio assertion would never run", p.RatioDepth, p.Depths)
		}
		if p.Samples < minChainRatioSamples {
			return chainParams{}, fmt.Errorf("perf.chain_depth.samples is %d, too few for a p99: below %d the ratio assertion compares two maxima, so set max_ratio to 0 or raise samples", p.Samples, minChainRatioSamples)
		}
	}
	return p, nil
}

func parseDepths(raw []string) ([]int, error) {
	out := make([]int, 0, len(raw))
	for _, s := range raw {
		d, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || d < 1 {
			return nil, fmt.Errorf("perf.chain_depth.depths: %q is not a positive integer", s)
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, errors.New("perf.chain_depth.depths is empty")
	}
	sort.Ints(out)
	return out, nil
}

func init() {
	core.Register(core.Test{
		Name:       testNameChainDepth,
		Suite:      "perf",
		Category:   core.Perf,
		Env:        core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:       []string{"perf"},
		OwningTeam: core.TeamCompute,
		Description: "Cold single-page read latency at each delta-chain depth: one table per rung of perf.chain_depth.depths, each of perf.chain_depth.pages one-row pages, with that rung's number of " +
			"one-delta-per-page update rounds applied. After a flush and a compute restart every rung's pages are read cold and reported as read.p50_ms@depth<N> / read.p99_ms@depth<N>, and p99 at " +
			"perf.chain_depth.ratio_depth is asserted within perf.chain_depth.max_ratio of p99 at the shallowest rung, the image_chain_limit contract.",
		Params:  []string{"perf.chain_depth", "perf.gate"},
		Timeout: 3 * time.Hour,
		Run:     runChainDepth,
	})
}

func runChainDepth(t *core.T) error {
	env, ok := t.Env.(ampMultitenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, ok := t.Env.(interface{ AmpKube() *k8s.Kubectl })
	if !ok || kubeEnv.AmpKube() == nil {
		t.Skip("a cold read needs the compute restarted, which needs a kube context to roll its pod")
		return nil
	}
	params, err := chainParamsFromConfig(t.Options)
	if err != nil {
		return err
	}
	logf := t.Masker().Logf(log.Printf)
	cl, err := env.AmpController(t.Context)
	if err != nil {
		return err
	}
	id := identityFromConfig(t.Options, "", frameworkCommit(), loadFieldsChainDepth(params))
	t.SetPerfIdentity(id.core())

	r := &chainRun{t: t, cl: cl, env: env, kube: kubeEnv.AmpKube(), poolID: env.AmpPoolID(), params: params, logf: logf}
	defer r.cleanup()
	if err := r.standUp(); err != nil {
		return err
	}
	if err := r.buildLadder(); err != nil {
		return err
	}
	if err := r.flushAndRestart(); err != nil {
		return err
	}
	if err := r.readLadder(); err != nil {
		return err
	}
	return r.gate(id)
}

type chainRun struct {
	t      *core.T
	cl     *controller.Client
	env    ampMultitenantEnv
	kube   *k8s.Kubectl
	poolID string
	params chainParams
	logf   func(string, ...any)

	ten       tenant
	deleted   bool
	reads     map[int][]float64
	heapPages map[int]int64
}

func (r *chainRun) cleanup() {
	if r.ten.Conn != nil {
		r.ten.Conn.Close()
		r.ten.Conn = nil
	}
	if r.ten.ClusterID == "" || r.deleted {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if _, err := r.cl.DeleteClusterAndWait(ctx, r.poolID, r.ten.ClusterID); err != nil {
		r.logf("chain_depth: could not delete cluster %s: %v", r.ten.ClusterID, err)
	}
}

func (r *chainRun) standUp() error {
	return r.t.Step("create_tenant", func(s *core.StepHandle) error {
		ten, _, err := createTenant(r.t, r.cl, r.env, r.poolID, 0, multitenantParams{
			EndpointCPU: r.params.EndpointCPU, EndpointMemory: r.params.EndpointMem})
		if err != nil {
			return fmt.Errorf("%w%s", err, schedulingHint(r.t, r.logf))
		}
		r.ten = ten
		s.Set("cluster", ten.ClusterID)
		s.Set("endpoint", ten.Endpoint.ID)
		return nil
	})
}

func (r *chainRun) buildLadder() error {
	r.heapPages = map[int]int64{}
	for _, depth := range append([]int{0}, r.params.Depths...) {
		if err := r.buildRung(depth); err != nil {
			return fmt.Errorf("depth %d: %w", depth, err)
		}
	}
	return nil
}

func (r *chainRun) buildRung(depth int) error {
	ctx := r.t.Context
	table := chainTable(depth)
	return r.t.Step("build_depth"+strconv.Itoa(depth), func(s *core.StepHandle) error {
		for _, stmt := range []string{
			fmt.Sprintf("DROP TABLE IF EXISTS %s", table),
			fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (id int PRIMARY KEY, n int NOT NULL, pad text) WITH (fillfactor = 10)", table),
			fmt.Sprintf("ALTER TABLE %s ALTER COLUMN pad SET STORAGE PLAIN", table),
			fmt.Sprintf("INSERT INTO %s (id, n, pad) SELECT g, 0, repeat('x', %d) FROM generate_series(1, %d) g ON CONFLICT (id) DO NOTHING",
				table, r.params.PayloadBytes, r.params.Pages),
		} {
			if _, err := r.ten.Conn.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		for i := 0; i < depth; i++ {
			// Absolute, not n = n + 1: a replayed round would leave n past the depth the read step asserts.
			if _, err := r.ten.Conn.Exec(ctx, fmt.Sprintf("UPDATE %s SET n = %d", table, i+1)); err != nil {
				return fmt.Errorf("update round %d: %w", i+1, err)
			}
		}
		pages, err := r.ten.Conn.QueryInt(ctx, "SELECT pg_relation_size($1) / 8192", table)
		if err != nil {
			return err
		}
		r.heapPages[depth] = pages
		s.Set("rows", r.params.Pages)
		s.Set("heap_pages", pages)
		s.Set("update_rounds", depth)
		return nil
	})
}

func (r *chainRun) flushAndRestart() error {
	return r.t.Step("flush_and_restart", func(s *core.StepHandle) error {
		lsn, err := r.ten.Conn.QueryString(r.t.Context, "SELECT pg_current_wal_flush_lsn()::text")
		if err != nil {
			return err
		}
		// The pageserver addresses a branch by the id its own endpoint reports.
		branchID, err := r.ten.Conn.QueryString(r.t.Context, "SELECT current_setting('yb_amp.branch_id')")
		if err != nil {
			return err
		}
		ps, err := r.pageserverClient()
		if err != nil {
			return err
		}
		if err := ps.FlushBranch(r.t.Context, branchID, lsn); err != nil {
			return err
		}
		s.Set("flush_lsn", lsn)
		r.ten.Conn.Close()
		r.ten.Conn = nil
		if err := r.restartCompute(); err != nil {
			return fmt.Errorf("%w%s", err, captureEndpointDiag(r.t, r.kube, r.ten, "after-restart", r.logf))
		}
		conn, err := r.env.ConnectEndpoint(r.t.Context, r.ten.Endpoint)
		if err != nil {
			return fmt.Errorf("%w%s", err, captureEndpointDiag(r.t, r.kube, r.ten, "after-restart", r.logf))
		}
		r.ten.Conn = conn
		_, err = conn.QueryInt(r.t.Context, "SELECT 1")
		return err
	})
}

func (r *chainRun) pageserverClient() (*pageserver.Client, error) {
	return endpointPageserver(r.t.Context, r.kube, r.ten.Endpoint.ID)
}

func (r *chainRun) restartCompute() error {
	pod, err := ampstack.EndpointPodName(r.t.Context, r.kube, r.ten.Endpoint.ID)
	if err != nil {
		return err
	}
	if err := r.kube.Delete(r.t.Context, "pod", pod); err != nil {
		return fmt.Errorf("delete endpoint %s compute pod %s: %w", r.ten.Endpoint.ID, pod, err)
	}
	return ampstack.WaitEndpointWorkloadReady(r.t.Context, r.kube, r.ten.Endpoint.ID, r.params.ReadyTimeout)
}

// Timed inside the server: the port-forward measured 29 ms against a 29.2 ms read.
func (r *chainRun) readLadder() error {
	r.reads = map[int][]float64{}
	if _, err := r.ten.Conn.Exec(r.t.Context,
		"CREATE TABLE IF NOT EXISTS "+chainTimings+" (depth int NOT NULL, ms double precision NOT NULL)"); err != nil {
		return err
	}
	rnd := r.t.Seed.Fork("chain_depth").Rand()
	if _, err := r.readRung(rnd, 0); err != nil {
		return fmt.Errorf("warm-up rung: %w", err)
	}
	for _, depth := range r.params.Depths {
		vals, err := r.readRung(rnd, depth)
		if err != nil {
			return fmt.Errorf("depth %d: %w", depth, err)
		}
		r.reads[depth] = vals
	}
	return nil
}

func (r *chainRun) readRung(rnd *rand.Rand, depth int) ([]float64, error) {
	ctx := r.t.Context
	table := chainTable(depth)
	ids := sampleIDs(rnd, r.params.Pages, r.params.Samples)
	var vals []float64
	err := r.t.Step("read_depth"+strconv.Itoa(depth), func(s *core.StepHandle) error {
		if _, err := r.ten.Conn.Exec(ctx, "DELETE FROM "+chainTimings+" WHERE depth = $1", depth); err != nil {
			return err
		}
		if _, err := r.ten.Conn.Exec(ctx, timedReadBlock(table, depth, ids)); err != nil {
			return err
		}
		if err := r.ten.Conn.Query(ctx, func(rows pgx.Rows) error {
			var ms float64
			if err := rows.Scan(&ms); err != nil {
				return err
			}
			vals = append(vals, ms)
			return nil
		}, "SELECT ms FROM "+chainTimings+" WHERE depth = $1", depth); err != nil {
			return err
		}
		if len(vals) != len(ids) {
			return fmt.Errorf("%d reads timed, want %d", len(vals), len(ids))
		}
		st := aggregate(vals)
		s.Set("samples", len(vals))
		s.Set("p50_ms", fmt.Sprintf("%.3f", st.Median))
		s.Set("p99_ms", fmt.Sprintf("%.3f", percentile(vals, 99)))
		return nil
	})
	return vals, err
}

func timedReadBlock(table string, depth int, ids []int) string {
	idList := make([]string, len(ids))
	for i, id := range ids {
		idList[i] = strconv.Itoa(id)
	}
	return fmt.Sprintf(`DO $ampqa$
DECLARE
  ids int[] := ARRAY[%s];
  i int; t0 timestamptz; got int; acc double precision[] := '{}';
BEGIN
  FOREACH i IN ARRAY ids LOOP
    t0 := clock_timestamp();
    SELECT n INTO got FROM %s WHERE id = i;
    acc := array_append(acc, (EXTRACT(epoch FROM (clock_timestamp() - t0)) * 1000)::double precision);
    IF got IS DISTINCT FROM %d THEN
      RAISE EXCEPTION 'row %% of %s reads n = %%, want %d', i, got;
    END IF;
  END LOOP;
  DELETE FROM %s WHERE depth = %d;
  INSERT INTO %s SELECT %d, unnest(acc);
END
$ampqa$`, strings.Join(idList, ","), table, depth, table, depth, chainTimings, depth, chainTimings, depth)
}

func (r *chainRun) gate(id Identity) error {
	return r.t.Step("gate_chain_depth", func(s *core.StepHandle) error {
		gated, info, ratioErr := chainDatapoints(r.reads, r.params)
		if ratio, ok := chainRatio(r.reads, r.params); ok {
			s.Set("p99_ratio", fmt.Sprintf("%.2fx", ratio))
		}
		s.Set("depths", fmt.Sprint(r.params.Depths))
		s.Set("heap_pages", fmt.Sprint(r.heapPages))
		r.t.Chart(chainChart(r.reads, r.params.Depths))
		gateErr := emitAndGateAuto(r.t, perfFromConfig(r.t.Options), scaleVersion(r.t),
			testNameChainDepth, gated, info, id, r.logf)
		return errors.Join(ratioErr, gateErr)
	})
}

func chainTable(depth int) string { return chainTablePrefix + strconv.Itoa(depth) }

func sampleIDs(rnd *rand.Rand, pages, n int) []int {
	perm := rnd.Perm(pages)
	if n > pages {
		n = pages
	}
	out := make([]int, n)
	for i := 0; i < n; i++ {
		out[i] = perm[i] + 1
	}
	return out
}

func percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	rank := int(math.Ceil(p/100*float64(len(s)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(s) {
		rank = len(s) - 1
	}
	return s[rank]
}

func chainRatio(reads map[int][]float64, p chainParams) (float64, bool) {
	base := reads[p.Depths[0]]
	target := reads[p.RatioDepth]
	if len(base) == 0 || len(target) == 0 {
		return 0, false
	}
	b := percentile(base, 99)
	if b <= 0 {
		return 0, false
	}
	return percentile(target, 99) / b, true
}

func chainDatapoints(reads map[int][]float64, p chainParams) (gated, info []core.Datapoint, err error) {
	// Absolute p50 follows pod placement, not depth: one image read 1.99 to 3.44 ms
	// at depth 1 on amp-dev in one day. Each rung over the shallowest one in the
	// same run cancels that, and the cost of depth is what this test is about.
	base := 0.0
	if vals := reads[p.Depths[0]]; len(vals) > 0 {
		base = median(vals)
	}
	for i, depth := range p.Depths {
		vals := reads[depth]
		if len(vals) == 0 {
			continue
		}
		suffix := fmt.Sprintf("@depth%d", depth)
		info = append(info, core.Datapoint{Name: chainPrefix + "p50_ms" + suffix, Value: median(vals), Unit: "ms", Direction: core.LowerIsBetter})
		if i > 0 && base > 0 {
			gated = append(gated, core.Datapoint{Name: chainPrefix + "p50_ratio" + suffix, Value: median(vals) / base, Unit: "x", Direction: core.LowerIsBetter})
		}
		// p99 of the sample set is ~3 reads, so one slow read sets it: 0.260923.0
		// read 3.2 ms at depth 1 in one run and 24.4 ms in the next.
		info = append(info, core.Datapoint{Name: chainPrefix + "p99_ms" + suffix, Value: percentile(vals, 99), Unit: "ms", Direction: core.LowerIsBetter})
	}
	ratio, ok := chainRatio(reads, p)
	if !ok {
		return gated, info, nil
	}
	info = append(info, core.Datapoint{Name: chainPrefix + "p99_ratio", Value: ratio, Unit: "x", Direction: core.LowerIsBetter})
	if p.MaxRatio > 0 && ratio > p.MaxRatio {
		err = fmt.Errorf("cold read p99 at depth %d is %.2fx the p99 at depth %d (limit %.2fx): the image materialiser is not bounding the chain at image_chain_limit",
			p.RatioDepth, ratio, p.Depths[0], p.MaxRatio)
	}
	return gated, info, err
}

func chainChart(reads map[int][]float64, depths []int) core.Chart {
	var p50, p99 []core.ChartPoint
	for _, d := range depths {
		vals := reads[d]
		if len(vals) == 0 {
			continue
		}
		p50 = append(p50, core.ChartPoint{X: float64(d), Y: median(vals)})
		p99 = append(p99, core.ChartPoint{X: float64(d), Y: percentile(vals, 99)})
	}
	return core.Chart{
		Title: "Cold read latency by chain depth", Kind: core.ChartLine,
		XLabel: "chain depth (update rounds)", YLabel: "latency (ms)",
		Series: []core.ChartSeries{{Name: "p50", Points: p50}, {Name: "p99", Points: p99}},
	}
}

func loadFieldsChainDepth(p chainParams) LoadFields {
	return LoadFields{
		Script:  fmt.Sprintf("chain_depth@%s/%s/%v", p.EndpointCPU, p.EndpointMem, p.Depths),
		Rows:    p.Pages,
		Clients: p.Samples,
		Scale:   p.PayloadBytes,
		Harness: chainHarnessRev,
	}
}
