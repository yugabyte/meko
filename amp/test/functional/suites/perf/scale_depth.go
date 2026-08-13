// Escalation by DATA on one endpoint — the axis that finds storage walls.
// meko#739 wedged a WAL fetch at TPC-C SF 150 / 75 terminals against a SINGLE
// endpoint, a scale no lightly-loaded ladder reaches. Each stage reloads the
// same endpoint at a larger scale factor, drives it, then checks the storage
// layer: a write and a read of it on a bounded deadline (a wedge answers
// nothing, so the deadline is the detector) and the endpoint log scanned for
// the product's wedge markers. Finding the wall is the point; only a stage-1
// failure fails the test.
package perf

import (
	"context"
	"fmt"
	"log"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/benchbase"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// The product's own words for a replay that stopped advancing.
var wedgeMarkers = []string{
	"empty pages in a row",
	"continuation token is not advancing",
	"WAL fetch failed",
	"WAL chunk fetch for segno",
}

// The backing filling up looks exactly like a wedge from the client: the WAL
// acceptor cannot write, the walproposer handshake never completes, and every
// COMMIT parks in SyncRep. It is a different diagnosis and a different fix
// (a bigger apps.yba.k8s.volume_size_gb), so it gets its own verdict.
var backingFullMarkers = []string{
	"insufficient disk space",
	"Not enough disk space",
}

func init() {
	core.Register(core.Test{
		Name:        "perf/scale_depth_tpcc",
		Suite:       "perf",
		Category:    core.Perf,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"perf"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Grow ONE endpoint's TPC-C dataset and concurrency stage by stage (warehouses x terminals), driving each for a full window, and after every stage prove the storage layer still advances: a read above the freshly written LSN on a bounded deadline, plus a scan of the endpoint log for WAL-wedge markers. Reports the largest scale that stayed healthy.",
		Params:      []string{"perf.scale_depth"},
		Timeout:     4 * time.Hour,
		Run:         runScaleDepth,
	})
}

type scaleDepthParams struct {
	// Warehouses and Terminals pair index for index.
	Warehouses    []int
	Terminals     []int
	StageDuration time.Duration
	ReadTimeout   time.Duration
	// LoadTimeout bounds a stage's load, and scales with the dataset: a fixed
	// budget kills a big stage that is loading perfectly well.
	LoadTimeout    time.Duration
	LoadPerWarehse time.Duration
	// Growth continues the ladder past the configured list, multiplying the last
	// rung until something fails — "more and more" has no fixed top.
	Growth float64
	// MinTPS is the no-throughput floor: a stage that drives this slowly has
	// found the wall even without an error.
	MinTPS      float64
	EndpointCPU string
	EndpointMem string
}

func scaleDepthParamsFromConfig(c *core.Config) scaleDepthParams {
	return scaleDepthParams{
		Warehouses:     intSlice(c.GetStringSlice("perf.scale_depth.warehouses", []string{"25", "50", "100", "150", "250"})),
		Terminals:      intSlice(c.GetStringSlice("perf.scale_depth.terminals", []string{"16", "32", "50", "75", "100"})),
		StageDuration:  c.GetDuration("perf.scale_depth.stage_duration", 15*time.Minute),
		ReadTimeout:    c.GetDuration("perf.scale_depth.read_timeout", 2*time.Minute),
		LoadTimeout:    c.GetDuration("perf.scale_depth.load_timeout", 45*time.Minute),
		LoadPerWarehse: c.GetDuration("perf.scale_depth.load_timeout_per_warehouse", 90*time.Second),
		Growth:         c.GetFloat("perf.scale_depth.growth", 1.6),
		MinTPS:         c.GetFloat("perf.scale_depth.min_tps", 1),
		EndpointCPU:    c.GetString("perf.scale_depth.endpoint_cpu", "4"),
		EndpointMem:    c.GetString("perf.scale_depth.endpoint_memory", "8Gi"),
	}
}

func runScaleDepth(t *core.T) error {
	env, ok := t.Env.(ampMultitenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, ok := t.Env.(interface{ AmpKube() *k8s.Kubectl })
	if !ok || kubeEnv.AmpKube() == nil {
		t.Skip("driving a real benchmark client needs a kube context for the control-plane namespace")
		return nil
	}
	params := scaleDepthParamsFromConfig(t.Options)
	if len(params.Warehouses) == 0 || len(params.Terminals) != len(params.Warehouses) {
		return fmt.Errorf("perf.scale_depth.warehouses and .terminals must be non-empty and the same length (got %d and %d)",
			len(params.Warehouses), len(params.Terminals))
	}
	cfg := perfFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)
	kube := kubeEnv.AmpKube()
	cl, err := env.AmpController(t.Context)
	if err != nil {
		return err
	}
	poolID := env.AmpPoolID()

	var tenants []tenant
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		for i := range tenants {
			if tenants[i].Conn != nil {
				tenants[i].Conn.Close()
			}
			if _, err := cl.DeleteClusterAndWait(ctx, poolID, tenants[i].ClusterID); err != nil {
				logf("scale_depth: could not delete cluster %s: %v", tenants[i].ClusterID, err)
			}
		}
	}()

	td := &teardownStack{}
	defer func() {
		tctx, cancel := context.WithTimeout(context.Background(), perfTeardownBudget)
		defer cancel()
		_ = td.run(tctx, logf)
	}()

	var client tenantClient
	if err := t.Step("create_tenant", func(s *core.StepHandle) error {
		ten, _, err := createTenant(t, cl, env, poolID, 0, multitenantParams{
			SeedRows:       1,
			EndpointCPU:    params.EndpointCPU,
			EndpointMemory: params.EndpointMem,
		})
		if err != nil {
			return fmt.Errorf("%w%s", err, schedulingHint(t, logf))
		}
		tenants = append(tenants, ten)
		clients, err := startTenantClients(t.Context, t, cfg, kube, attachSink{t: t, logf: logf},
			td, tenants, []string{mixTPCC})
		if err != nil {
			return err
		}
		client = clients[0]
		if err := ensureProbeTable(t.Context, ten.Conn); err != nil {
			return fmt.Errorf("create the probe table: %w", err)
		}
		s.Set("endpoint", ten.Endpoint.ID)
		s.Set("endpoint_size", params.EndpointCPU+" / "+params.EndpointMem)
		return nil
	}); err != nil {
		return err
	}

	var reached, reachedTerminals int
	var tpsAt []core.ChartPoint
	stopReason := "completed every configured stage without wedging"

	for i := 0; ; i++ {
		wh, term, ok := params.rung(i)
		if !ok {
			break
		}
		stage := fmt.Sprintf("stage_%02d_wh%d_t%d", i+1, wh, term)
		var tps float64
		err := t.Step(stage, func(s *core.StepHandle) error {
			// Growing the wals table is the variable under test.
			w := &tpccWorkload{
				runDir:     path.Join(cfg.RemoteDir, benchbase.DefaultSubdir),
				conn:       endpointConn(kube.Namespace(), tenants[0].Endpoint.ID, tenants[0].Endpoint.Address),
				benchmark:  benchbase.TPCC,
				warehouses: wh,
				terminals:  term,
				maxHeap:    cfg.MaxHeap,
			}
			// Bounded and attached either way: an unlogged load that never
			// progressed cost a whole run once.
			// benchbase always creates the same table names, so reloading over
			// the previous stage's dataset leaves rows its own workers then
			// cannot find ("C_D_ID=4 C_ID=639 not found!").
			if i > 0 {
				if err := dropTPCCSchema(t.Context, tenants[0]); err != nil {
					return fmt.Errorf("drop the previous stage's dataset: %w", err)
				}
			}
			loadCtx, cancelLoad := context.WithTimeout(t.Context, params.loadBudget(wh))
			loadStart := time.Now()
			loadErr := w.Prepare(loadCtx, client.Exec)
			cancelLoad()
			s.Set("load_ms", fmt.Sprintf("%.0f", msSince(loadStart)))
			if out := readRemoteFile(t.Context, client.Exec,
				path.Join(cfg.RemoteDir, benchbase.DefaultSubdir, "benchbase.out")); out != "" {
				t.AttachBytes(fmt.Sprintf("load-wh%d.log", wh), "text/plain", []byte(out))
			}
			if loadErr != nil {
				// meko#739 wedged mid-load, at 80 of 150 warehouses.
				if m, line, full, _ := scanEndpointLogFor(t, kube, tenants[0], backingFullMarkers); full {
					s.Set("stop_reason", "backing out of disk")
					t.AttachBytes(fmt.Sprintf("backing-full-wh%d.txt", wh), "text/plain", []byte(line))
					return fmt.Errorf("the backing ran out of disk loading %d warehouses (%q): raise apps.yba.k8s.volume_size_gb — see backing-full-wh%d.txt: %w",
						wh, m, wh, loadErr)
				}
				marker, line, found, scanErr := scanEndpointLog(t, kube, tenants[0])
				if scanErr != nil {
					s.Set("wedge_scan", "inconclusive: "+scanErr.Error())
					logf("scale_depth: wedge scan inconclusive: %v", scanErr)
				}
				if found {
					s.Set("wedge_marker", marker)
					t.AttachBytes(fmt.Sprintf("wedge-evidence-wh%d.txt", wh), "text/plain", []byte(line))
					return fmt.Errorf("the WAL path stopped advancing while loading %d warehouses (%q) — see wedge-evidence-wh%d.txt: %w",
						wh, marker, wh, loadErr)
				}
				if backends, syncRep := stuckBackends(t, env, tenants[0], logf); backends != "" {
					t.AttachBytes(fmt.Sprintf("stuck-backends-wh%d.txt", wh), "text/plain", []byte(backends))
					if syncRep {
						s.Set("wedge_marker", "IPC/SyncRep")
						return fmt.Errorf("loading %d warehouses stalled with every backend parked in IPC/SyncRep, so no commit is being acknowledged — see stuck-backends-wh%d.txt and the endpoint log for which of the several causes it was: %w",
							wh, wh, loadErr)
					}
				}
				return fmt.Errorf("load %d warehouses after %.0fs: %w", wh, msSince(loadStart)/1000, loadErr)
			}

			sample, err := w.RunOnce(t.Context, client.Exec,
				RunParams{Duration: params.StageDuration, SampleInterval: time.Second})
			recordDriverRun(t, logf, fmt.Sprintf("tpcc-wh%d", wh), stage, sample, params.StageDuration)
			if err != nil {
				return fmt.Errorf("drive %d warehouses / %d terminals: %w", wh, term, err)
			}
			tps = sample.TPS
			s.Set("tps", fmt.Sprintf("%.1f", sample.TPS))
			s.Set("latency_ms", fmt.Sprintf("%.1f", sample.LatencyAvgMS))

			// The storage-layer check the throughput number cannot make.
			return assertStorageAdvances(t, s, kube, env, tenants[0], i+1, params.ReadTimeout, logf)
		})
		if err != nil {
			// An errored stage measured nothing, so it is not a ceiling. A wedge
			// here is a product bug and a load that will not load is a finding;
			// both must fail rather than be reported as the wall.
			return fmt.Errorf("%d warehouses / %d terminals (reached %d, %s): %w",
				wh, term, reached, ladderCurve(tpsAt), err)
		}
		reached, reachedTerminals = wh, term
		tpsAt = append(tpsAt, core.ChartPoint{X: float64(wh), Y: tps})
		if tps <= params.MinTPS {
			stopReason = fmt.Sprintf("%d warehouses / %d terminals: %.2f tps — no throughput left", wh, term, tps)
			break
		}
	}

	return t.Step("report_ceiling", func(s *core.StepHandle) error {
		s.Set("stop_reason", stopReason)
		s.Set("max_warehouses", reached)
		s.Set("max_terminals", reachedTerminals)
		t.Datapoint(core.Datapoint{Name: "scale_depth.tpcc.max_warehouses",
			Value: float64(reached), Unit: "warehouses", Direction: core.HigherIsBetter})
		t.Datapoint(core.Datapoint{Name: "scale_depth.tpcc.max_terminals",
			Value: float64(reachedTerminals), Unit: "terminals", Direction: core.HigherIsBetter})
		if len(tpsAt) > 0 {
			t.Datapoint(core.Datapoint{Name: "scale_depth.tpcc.tps_at_ceiling",
				Value: tpsAt[len(tpsAt)-1].Y, Unit: "tps", Direction: core.HigherIsBetter})
			t.Chart(core.Chart{
				Title: "TPC-C throughput vs dataset size", Kind: core.ChartLine,
				XLabel: "warehouses", YLabel: "tps",
				Series: []core.ChartSeries{{Name: "tps", Points: tpsAt}},
			})
		}
		return nil
	})
}

// The test's own table: the ladders drive different workloads, so a probe
// rung returns stage i's (warehouses, terminals). Past the configured list it
// keeps multiplying by Growth, so the ladder ends where the product does, not
// where the config ran out.
func (p scaleDepthParams) rung(i int) (int, int, bool) {
	if i < len(p.Warehouses) {
		return p.Warehouses[i], p.Terminals[i], true
	}
	if p.Growth <= 1 || len(p.Warehouses) == 0 {
		return 0, 0, false
	}
	wh, term := p.Warehouses[len(p.Warehouses)-1], p.Terminals[len(p.Terminals)-1]
	for n := len(p.Warehouses); n <= i; n++ {
		wh = int(float64(wh) * p.Growth)
		term = int(float64(term) * p.Growth)
	}
	return wh, term, true
}

// loadBudget scales with the dataset; the flat LoadTimeout is the floor.
func (p scaleDepthParams) loadBudget(warehouses int) time.Duration {
	scaled := time.Duration(warehouses) * p.LoadPerWarehse
	if scaled < p.LoadTimeout {
		return p.LoadTimeout
	}
	return scaled
}

// naming one driver's table cannot check the others.
const probeTable = "amp_scale_probe"

// ensureProbeTable creates the probe table on the endpoint. Idempotent.
func ensureProbeTable(ctx context.Context, conn *pg.Client) error {
	_, err := conn.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s (stage int PRIMARY KEY, at timestamptz DEFAULT now())", probeTable))
	return err
}

// assertStorageAdvances writes and reads it back on a bounded deadline, and
// scans the endpoint log for wedge markers. The write is what makes this a WAL
// check rather than a cache check.
func assertStorageAdvances(t *core.T, s *core.StepHandle, kube *k8s.Kubectl, env ampMultitenantEnv, ten tenant,
	stage int, timeout time.Duration, logf func(string, ...any)) error {

	marker, line, found, scanErr := scanEndpointLog(t, kube, ten)
	if scanErr != nil {
		s.Set("wedge_scan", "inconclusive: "+scanErr.Error())
		logf("scale_depth: wedge scan inconclusive: %v", scanErr)
	}
	if found {
		s.Set("wedge_marker", marker)
		t.AttachBytes("wedge-evidence.txt", "text/plain", []byte(line))
		return fmt.Errorf("the endpoint's WAL path reports it is not advancing (%q) — see wedge-evidence.txt", marker)
	}

	ctx, cancel := context.WithTimeout(t.Context, timeout)
	defer cancel()
	start := time.Now()
	// The test's own connection rides a kubectl port-forward that dies on its
	// own schedule; a refused dial is that, not a wedged endpoint. Reconnect
	// once and judge the retry, or the probe reports the harness as the product.
	err := probeWriteRead(ctx, ten, stage)
	if err != nil && isTransportErr(err) {
		logf("scale_depth: probe transport died (%v); reconnecting", err)
		conn, rerr := env.ConnectEndpoint(ctx, ten.Endpoint)
		if rerr != nil {
			return fmt.Errorf("post-stage probe could not reconnect to the endpoint: %w (first attempt: %v)", rerr, err)
		}
		ten.Conn.Close()
		ten.Conn = conn
		err = probeWriteRead(ctx, ten, stage)
	}
	if err != nil {
		if ctx.Err() != nil {
			if backends, syncRep := stuckBackends(t, env, ten, logf); backends != "" {
				t.AttachBytes("stuck-backends.txt", "text/plain", []byte(backends))
				if syncRep {
					s.Set("wedge_marker", "IPC/SyncRep")
				}
			}
			return fmt.Errorf("the probe did not complete within %s after the stage, so the WAL path is not advancing — see the endpoint log for the cause: %w", timeout, err)
		}
		return err
	}
	s.Set("readback_ms", fmt.Sprintf("%.0f", msSince(start)))
	return nil
}

// readRemoteFile tails a file out of a client pod, best-effort.
func readRemoteFile(ctx context.Context, ex exec.RemoteExecutor, remotePath string) string {
	res, err := ex.Run(ctx, "tail -c 200000 "+remotePath)
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return res.Stdout
}

// tpccTables are what benchbase's TPC-C creates, in dependency order.
var tpccTables = []string{"order_line", "new_order", "oorder", "history", "stock", "item", "customer", "district", "warehouse"}

// dropTPCCSchema clears the dataset so the next stage loads a clean one.
func dropTPCCSchema(ctx context.Context, ten tenant) error {
	for _, tbl := range tpccTables {
		if _, err := ten.Conn.Exec(ctx, "DROP TABLE IF EXISTS "+tbl+" CASCADE"); err != nil {
			return fmt.Errorf("drop %s: %w", tbl, err)
		}
	}
	return nil
}

// probeWriteRead writes one row and reads it back: the write must reach the
// safekeeper and come back through replay.
func probeWriteRead(ctx context.Context, ten tenant, stage int) error {
	if _, err := ten.Conn.Exec(ctx, fmt.Sprintf(
		"INSERT INTO %s (stage) VALUES ($1) ON CONFLICT (stage) DO UPDATE SET at = now()", probeTable), stage); err != nil {
		return fmt.Errorf("post-stage write failed: %w", err)
	}
	if _, err := ten.Conn.QueryInt(ctx, fmt.Sprintf("SELECT count(*) FROM %s", probeTable)); err != nil {
		return fmt.Errorf("post-stage read failed: %w", err)
	}
	return nil
}

// stuckBackends renders the endpoint's long-running backends and their wait
// events, on a connection of its own so a wedged session cannot hide them.
// meko#727 is invisible in the endpoint log and in CPU: the only witness is
// every backend parked in IPC/SyncRep. Best-effort, and "" is not a verdict.
func stuckBackends(t *core.T, env ampMultitenantEnv, ten tenant, logf func(string, ...any)) (string, bool) {
	ctx, cancel := context.WithTimeout(t.Context, 30*time.Second)
	defer cancel()
	conn, err := env.ConnectEndpoint(ctx, ten.Endpoint)
	if err != nil {
		logf("scale_depth: could not open a diagnostic connection: %v", err)
		return "", false
	}
	defer conn.Close()
	const q = `SELECT coalesce(string_agg(
	format('pid=%s %s/%s %ss %s', pid, coalesce(wait_event_type, '-'), coalesce(wait_event, '-'),
	       round(extract(epoch FROM now() - query_start)), left(query, 70)), chr(10)), '')
	FROM pg_stat_activity
	WHERE backend_type = 'client backend' AND state = 'active'
	  AND query_start < now() - interval '60 seconds'`
	out, err := conn.QueryString(ctx, q)
	if err != nil {
		logf("scale_depth: could not sample pg_stat_activity: %v", err)
		return "", false
	}
	return out, strings.Contains(out, "IPC/SyncRep")
}

// isTransportErr reports a dead local forward rather than an unhealthy endpoint.
func isTransportErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "127.0.0.1")
}

// scanEndpointLog keeps the read failure separate from the verdict: an
// unreadable log must report as "could not scan", never as "no wedge", or a
// broken kube context silently clears the product.
func scanEndpointLog(t *core.T, kube *k8s.Kubectl, ten tenant) (string, string, bool, error) {
	return scanEndpointLogFor(t, kube, ten, wedgeMarkers)
}

// scanEndpointLogFor scans the endpoint's log for any of markers.
func scanEndpointLogFor(t *core.T, kube *k8s.Kubectl, ten tenant, markers []string) (string, string, bool, error) {
	pods, err := kube.ListPods(t.Context)
	if err != nil {
		return "", "", false, fmt.Errorf("list pods: %w", err)
	}
	want := "yb-ep-" + ten.Endpoint.ID
	for _, pod := range pods {
		if !strings.HasPrefix(pod, want) {
			continue
		}
		out, err := kube.Logs(t.Context, pod, "endpoint", 4000)
		if err != nil {
			return "", "", false, fmt.Errorf("read %s logs: %w", pod, err)
		}
		for _, line := range strings.Split(out, "\n") {
			for _, m := range markers {
				if strings.Contains(line, m) {
					return m, line, true, nil
				}
			}
		}
		return "", "", false, nil
	}
	return "", "", false, fmt.Errorf("no pod matches endpoint %s", ten.Endpoint.ID)
}

// intSlice parses a config list of numbers, dropping anything unparseable —
// the ladder is validated for length by the caller, and a typo there must not
// silently shift every stage's pairing.
func intSlice(vals []string) []int {
	out := make([]int, 0, len(vals))
	for _, v := range vals {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n <= 0 {
			continue
		}
		out = append(out, n)
	}
	return out
}
