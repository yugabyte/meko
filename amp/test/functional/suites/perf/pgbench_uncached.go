// pgbench_uncached is perf/pgbench with the working set far past the endpoint's
// caches. perf/pgbench's dataset fits in shared_buffers after warmup, so its
// steady state never reaches the pageserver, which is where amp's own read path
// lives. Here shared_buffers and the LFC are capped well under the dataset and
// the LFC hit ratio is reported beside the numbers, as the proof the cap bit.
package perf

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

const testNamePgbenchUncached = "perf/pgbench_uncached"

// Scale 50 is ~750 MiB of accounts plus indexes, six times the capped caches.
const (
	DefaultPgbenchUncachedScale         = 50
	DefaultPgbenchUncachedSharedBuffers = "128MB"
	DefaultPgbenchUncachedFileCache     = "128MB"
)

type pgbenchUncachedParams struct {
	pgbenchParams
	SharedBuffers string
	FileCache     string
}

func pgbenchUncachedParamsFromConfig(c *core.Config) pgbenchUncachedParams {
	return pgbenchUncachedParams{
		pgbenchParams: pgbenchParams{
			Scale:   c.GetInt("perf.pgbench_uncached.scale", DefaultPgbenchUncachedScale),
			Clients: c.GetInt("perf.pgbench_uncached.clients", DefaultPgbenchClients),
			Jobs:    c.GetInt("perf.pgbench_uncached.jobs", DefaultPgbenchJobs),
		},
		SharedBuffers: c.GetString("perf.pgbench_uncached.shared_buffers", DefaultPgbenchUncachedSharedBuffers),
		FileCache:     c.GetString("perf.pgbench_uncached.file_cache_size_limit", DefaultPgbenchUncachedFileCache),
	}
}

func (p pgbenchUncachedParams) pgSettings() map[string]string {
	return map[string]string{"shared_buffers": p.SharedBuffers, "yb_amp.file_cache_size_limit": p.FileCache}
}

func init() {
	core.Register(core.Test{
		Name:        testNamePgbenchUncached,
		Suite:       "perf",
		Category:    core.Perf,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe, ClusterSlots: benchClusterSlots},
		Tags:        []string{"perf"},
		OwningTeam:  core.TeamCompute,
		Description: "Run pgbench through an amp compute endpoint whose shared_buffers and LFC are capped far under the dataset, so reads go through the pageserver; gate tps and fixed-rate latency, and report the LFC hit ratio.",
		Params:      []string{"perf", "apps.ybk8s", "env.ampstack"},
		Run:         runPgbenchUncached,
	})
}

func runPgbenchUncached(t *core.T) error {
	cfg := perfFromConfig(t.Options)
	if reason, ok := perfGate(t.Options, cfg); !ok {
		t.Skip(reason)
		return nil
	}
	if reason, ok := gateNoDoubleProvision(t.Env); !ok {
		t.Skip(reason)
		return nil
	}
	if reason, ok := ampEndpointGate(t.Options); !ok {
		t.Skip(reason)
		return nil
	}
	bc := benchConfigFromConfig(t.Options)
	params := pgbenchUncachedParamsFromConfig(t.Options)
	load := loadFieldsPgbench(bc, cfg, params.pgbenchParams)
	load.Script = "uncached@" + params.SharedBuffers + "/" + params.FileCache
	return runBenchProvisioned(t, benchWorkloadSpec{
		testName:           testNamePgbenchUncached,
		runStep:            "run_pgbench",
		podPrefix:          "ampqa-pgbu",
		attachPrefix:       "pgbench-uncached",
		noTPSMsg:           "pgbench produced no tps — parse of pgbench output failed (see attached pgbench-uncached-iter-*.out)",
		deliver:            deliverPgbenchArtifact,
		endpointPgSettings: params.pgSettings(),
		mkWorkload: func(conn pg.ConnInfo) Workload {
			return &uncachedWorkload{pgbenchWorkload: pgbenchWorkload{conn: conn, scale: params.Scale, clients: params.Clients, jobs: params.Jobs},
				want: params.pgSettings()}
		},
		specs: pgbenchUncachedSpecs,
		datapoints: func(res BenchResult) (gated, info []core.Datapoint) {
			return splitDatapoints(pgbenchUncachedSpecs(res))
		},
	}, cfg, bc, identityFromConfig(t.Options, "", frameworkCommit(), load))
}

func pgbenchUncachedSpecs(res BenchResult) []metricSpec {
	specs := renamePrefix(pgbenchMetricSpecs(res), "pgbench.", "pgbench_uncached.")
	if res.CacheHit.Median > 0 {
		// A fraction of a percent by design, so first-to-last moves by multiples
		// (0.11% -> 0.29%) with nothing wrong.
		specs = append(specs, metricSpec{Name: "pgbench_uncached.lfc_hit_pct", Unit: "%", Dir: core.LowerIsBetter, Stats: res.CacheHit, Info: true, NoDrift: true})
	}
	return specs
}

func renamePrefix(specs []metricSpec, from, to string) []metricSpec {
	for i := range specs {
		specs[i].Name = to + strings.TrimPrefix(specs[i].Name, from)
	}
	return specs
}

// uncachedWorkload is pgbench with the endpoint's LFC counters read around
// every run, so each Sample carries the hit ratio it ran at.
type uncachedWorkload struct {
	pgbenchWorkload
	want map[string]string
}

// Prepare also checks the caps took: the first run read a 0.1% LFC hit ratio
// at pgbench's uncached tps, which a cap that never applied would also produce.
func (w *uncachedWorkload) Prepare(ctx context.Context, ex exec.RemoteExecutor) error {
	if err := w.pgbenchWorkload.Prepare(ctx, ex); err != nil {
		return err
	}
	return checkEndpointSettings(ctx, ex, w.conn, w.want)
}

func (w *uncachedWorkload) Name() string { return testNamePgbenchUncached }

func (w *uncachedWorkload) RunOnce(ctx context.Context, ex exec.RemoteExecutor, p RunParams) (Sample, error) {
	h0, m0, cerr := lfcCounters(ctx, ex, w.conn)
	s, err := w.pgbenchWorkload.RunOnce(ctx, ex, p)
	if err != nil || cerr != nil {
		return s, err
	}
	h1, m1, cerr := lfcCounters(ctx, ex, w.conn)
	if cerr == nil {
		s.CacheHitPct, _ = lfcHitPct(h0, m0, h1, m1)
	}
	return s, nil
}

// lfcHitPct is the hit ratio between two counter reads; ok is false when the
// run touched the cache not at all or the counters went backwards (a restart).
func lfcHitPct(h0, m0, h1, m1 int64) (float64, bool) {
	dh, dm := h1-h0, m1-m0
	if dh < 0 || dm < 0 || dh+dm == 0 {
		return 0, false
	}
	return float64(dh) / float64(dh+dm) * 100, true
}

const lfcCountersSQL = "CREATE EXTENSION IF NOT EXISTS yb_amp; " +
	"SELECT coalesce(sum(lfc_value) FILTER (WHERE lfc_key = 'file_cache_hits'), 0), " +
	"coalesce(sum(lfc_value) FILTER (WHERE lfc_key = 'file_cache_misses'), 0) FROM yb_amp_lfc_stats"

// lfcCounters reads the endpoint's cumulative LFC hits and misses. A failure is
// returned, never fatal: the counters are evidence, not the measurement.
func lfcCounters(ctx context.Context, ex exec.RemoteExecutor, ci pg.ConnInfo) (hits, misses int64, err error) {
	out, err := ysqlshQuery(ctx, ex, ci, lfcCountersSQL)
	if err != nil {
		return 0, 0, fmt.Errorf("read LFC counters: %w", err)
	}
	return parseLFCCounters(out)
}

func ysqlshQuery(ctx context.Context, ex exec.RemoteExecutor, ci pg.ConnInfo, sql string) (string, error) {
	cmd := fmt.Sprintf("PGPASSWORD=%s %s -h %s -p %d -U %s -d %s -Atq -F ' ' -c %s",
		shellQuote(ci.Password), ysqlshShimPath, shellQuote(ci.Host), ci.Port, shellQuote(ci.User), shellQuote(ci.Database), shellQuote(sql))
	res, err := ex.Run(ctx, cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("ysqlsh exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// checkEndpointSettings fails when the endpoint is not running with want: the
// test measures nothing it claims to if the caps did not apply.
func checkEndpointSettings(ctx context.Context, ex exec.RemoteExecutor, ci pg.ConnInfo, want map[string]string) error {
	names := make([]string, 0, len(want))
	for n := range want {
		names = append(names, n)
	}
	sort.Strings(names)
	var q []string
	for _, n := range names {
		q = append(q, fmt.Sprintf("current_setting('%s', true)", n))
	}
	out, err := ysqlshQuery(ctx, ex, ci, "SELECT "+strings.Join(q, ", "))
	if err != nil {
		return fmt.Errorf("read endpoint settings: %w", err)
	}
	return compareSettings(names, want, out)
}

func compareSettings(names []string, want map[string]string, out string) error {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	got := strings.Fields(lines[len(lines)-1])
	if len(got) != len(names) {
		return fmt.Errorf("endpoint settings: want %d values, got %q", len(names), strings.TrimSpace(out))
	}
	var bad []string
	for i, n := range names {
		if !strings.EqualFold(got[i], want[n]) {
			bad = append(bad, fmt.Sprintf("%s = %s, want %s", n, got[i], want[n]))
		}
	}
	if len(bad) > 0 {
		return core.EnvFail("the endpoint did not take the cache caps pgbench_uncached measures under: %s", strings.Join(bad, "; "))
	}
	return nil
}

func parseLFCCounters(out string) (hits, misses int64, err error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	f := strings.Fields(lines[len(lines)-1])
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("LFC counters: want \"<hits> <misses>\", got %q", strings.TrimSpace(out))
	}
	hits, err1 := strconv.ParseInt(f[0], 10, 64)
	misses, err2 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("LFC counters: unparsable %q", strings.TrimSpace(out))
	}
	return hits, misses, nil
}
