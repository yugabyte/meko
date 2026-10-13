// stream_compression measures what tserver RPC compression costs amp on the
// paths meko#1857 and meko#1856 found it hurts: bulk loads of compressible
// rows, ANALYZE, and reads that miss the endpoint's caches. Each mode in
// perf.stream_compression.modes gets its own RF3 backing created with that
// mode, its own control plane and endpoint, and runs the same phases in step
// with the other arms. Every stack is pinned to one zone (perf.pin_zone), so
// this measures the CPU and latency compression adds, not the cross-zone bytes
// it saves.
package perf

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/pgbench"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybgflags"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/diag"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	testNameStreamCompression = "perf/stream_compression"
	scPrefix                  = "stream_compression."
	// Bumped when a change alters what the numbers mean.
	scHarnessRev = "sc1"
)

type streamCompressionParams struct {
	Modes              []string
	RowBytes           int
	RandomPct          int
	Batch              int
	SeedRows           int
	SeedClients        int
	RampClients        []int
	RampStep           time.Duration
	AnalyzeTargets     []int
	AnalyzeReps        int
	ReadClients        int
	ReadDuration       time.Duration
	SharedBuffers      string
	FileCache          string
	BackingStorageSize string
	EndpointCPU        string
	EndpointMem        string

	listErr error
}

func streamCompressionParamsFromConfig(c *core.Config) streamCompressionParams {
	p := streamCompressionParams{
		Modes:              c.GetStringSlice("perf.stream_compression.modes", []string{ybgflags.CompressionGzip, ybgflags.CompressionNone}),
		RowBytes:           c.GetInt("perf.stream_compression.row_bytes", 1000),
		RandomPct:          c.GetInt("perf.stream_compression.random_pct", 25),
		Batch:              max(c.GetInt("perf.stream_compression.batch_rows", 100), 1),
		SeedRows:           c.GetInt("perf.stream_compression.seed_rows", 2_000_000),
		SeedClients:        max(c.GetInt("perf.stream_compression.seed_clients", 4), 1),
		RampStep:           c.GetDuration("perf.stream_compression.ramp_step", 60*time.Second),
		AnalyzeReps:        max(c.GetInt("perf.stream_compression.analyze_reps", 2), 1),
		ReadClients:        max(c.GetInt("perf.stream_compression.read_clients", 8), 1),
		ReadDuration:       c.GetDuration("perf.stream_compression.read_duration", 60*time.Second),
		SharedBuffers:      c.GetString("perf.stream_compression.shared_buffers", "128MB"),
		FileCache:          c.GetString("perf.stream_compression.file_cache_size_limit", "128MB"),
		BackingStorageSize: c.GetString("perf.stream_compression.backing_storage_size", "50Gi"),
		EndpointCPU:        c.GetString("perf.stream_compression.endpoint_cpu", "2"),
		EndpointMem:        c.GetString("perf.stream_compression.endpoint_memory", "8Gi"),
	}
	var rampErr, targetErr error
	p.RampClients, rampErr = intList("perf.stream_compression.ramp_clients", c.GetStringSlice("perf.stream_compression.ramp_clients", []string{"2", "4", "8", "16"}))
	p.AnalyzeTargets, targetErr = intList("perf.stream_compression.analyze_targets", c.GetStringSlice("perf.stream_compression.analyze_targets", []string{"100", "1000"}))
	p.listErr = errors.Join(rampErr, targetErr)
	return p
}

func intList(key string, ss []string) ([]int, error) {
	var out []int
	for _, s := range ss {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%s: %q is not a positive integer", key, s)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s is empty", key)
	}
	return out, nil
}

func (p streamCompressionParams) validate() error {
	if len(p.Modes) == 0 {
		return errors.New("perf.stream_compression.modes is empty")
	}
	seen := map[string]bool{}
	for _, m := range p.Modes {
		if _, _, err := ybgflags.StreamCompression(m); err != nil || m == "" {
			return fmt.Errorf("perf.stream_compression.modes: %q is not gzip or none", m)
		}
		if seen[m] {
			return fmt.Errorf("perf.stream_compression.modes lists %q twice", m)
		}
		seen[m] = true
	}
	if p.RandomPct < 0 || p.RandomPct > 100 {
		return fmt.Errorf("perf.stream_compression.random_pct = %d, want 0-100", p.RandomPct)
	}
	if p.listErr != nil {
		return p.listErr
	}
	// read.sql draws ids from 1..seed_rows, so every one of them has to exist.
	if per := p.Batch * p.SeedClients; p.SeedRows < per || p.SeedRows%per != 0 {
		return fmt.Errorf("perf.stream_compression.seed_rows = %d must be a positive multiple of batch_rows x seed_clients (%d)", p.SeedRows, per)
	}
	return nil
}

func (p streamCompressionParams) pgSettings() map[string]string {
	return map[string]string{"shared_buffers": p.SharedBuffers, "yb_amp.file_cache_size_limit": p.FileCache}
}

func (p streamCompressionParams) endpointSpec() perfEndpointSpec {
	return perfEndpointSpec{PgSettings: p.pgSettings(), Resources: &controller.EndpointResources{CPU: p.EndpointCPU, Memory: p.EndpointMem}}
}

func loadFieldsStreamCompression(p streamCompressionParams, cfg perfConfig) LoadFields {
	return LoadFields{
		Rows:          p.SeedRows,
		Clients:       p.SeedClients,
		RF:            cfg.RF,
		MeasurementMS: p.RampStep.Milliseconds(),
		Script: fmt.Sprintf("row%d/rand%d/batch%d/ramp%v/analyze%v/read%dx%s/cache%s+%s/ep%s+%s",
			p.RowBytes, p.RandomPct, p.Batch, p.RampClients, p.AnalyzeTargets, p.ReadClients, p.ReadDuration, p.SharedBuffers, p.FileCache,
			p.EndpointCPU, p.EndpointMem),
		Harness: scHarnessRev + cfg.isolationSuffix(),
	}
}

func init() {
	core.Register(core.Test{
		Name:     testNameStreamCompression,
		Suite:    "perf",
		Category: core.Perf,
		Env:      core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe, ClusterSlots: armClusterSlots},
		Tags:     []string{"perf"},
		// The cost is on the compute's read and write paths (meko#1856, #1857).
		OwningTeam: core.TeamCompute,
		Description: "Measure what tserver RPC compression costs amp: one RF3 backing per perf.stream_compression.modes entry (gzip and none by default), each created with that mode, its own control plane and a cache-capped endpoint, all running the same phases in step. " +
			"Seed perf.stream_compression.seed_rows compressible rows, ANALYZE them at each target, read them by random primary key through the pageserver, then ramp batched inserts over perf.stream_compression.ramp_clients. " +
			"Every arm's rates and times are gated on history, and gzip's ratio to none is reported beside them.",
		Params: []string{"perf.stream_compression", "perf.rf", "perf.isolation", "perf.pin_zone", "apps.ybk8s", "env.ampstack"},
		Run:    runStreamCompression,
	})
}

func runStreamCompression(t *core.T) error {
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
	params := streamCompressionParamsFromConfig(t.Options)
	if err := params.validate(); err != nil {
		return err
	}
	logf := t.Masker().Logf(log.Printf)
	cfg.BackingStorageSize = params.BackingStorageSize
	// The arms set their own mode, so the run-level one must not split the series.
	id := identityFromConfig(t.Options.Derive(map[string]string{ybgflags.StreamCompressionKey: ""}), "", frameworkCommit(),
		loadFieldsStreamCompression(params, cfg))
	t.SetPerfIdentity(id.core())

	zcfg := cfg.inZone(zoneFor(armZones(t.Context, t, cfg, logf), 0))
	pair := newModeBarrier(len(params.Modes))
	results := make([]scArmResult, len(params.Modes))
	errs := make([]error, len(params.Modes))
	var wg sync.WaitGroup
	for i, mode := range params.Modes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer pair.leave()
			arm := &scArm{t: t, cfg: zcfg, params: params, mode: mode, pair: pair, td: &teardownStack{},
				logf: func(f string, a ...any) { logf("["+mode+"] "+f, a...) }}
			arm.sink = attachSink{t: t, logf: arm.logf}
			results[i], errs[i] = arm.run()
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	t.AttachBytes("stream_compression.txt", "log", []byte(formatSCResults(results)))
	for _, c := range scCharts(results) {
		t.Chart(c)
	}
	return t.Step("gate_perf", func(*core.StepHandle) error {
		gated, info := scDatapoints(results)
		return emitAndGateHistory(t, cfg, chartVersion(t.Options), testNameStreamCompression, gated, info, id, true, logf)
	})
}

type scArmResult struct {
	Mode           string
	SeedRowsPerSec float64
	// RampRowsPerSec is keyed by client count.
	RampRowsPerSec map[int]float64
	// AnalyzeMS holds every repetition per statistics target.
	AnalyzeMS      map[int][]float64
	AnalyzePages   map[int]int
	ReadTPS        float64
	ReadLatencyAvg float64
	ReadLatencyP95 float64
	ReadLatencyP99 float64
	ReadLFCHitPct  float64
	ReadLFCHitOK   bool
}

type scArm struct {
	t      *core.T
	cfg    perfConfig
	params streamCompressionParams
	mode   string
	pair   *modeBarrier
	td     *teardownStack
	logf   func(string, ...any)
	sink   apps.ArtifactSink

	kube    *k8s.Kubectl
	ampKube *k8s.Kubectl
	ampNS   string
	conn    pg.ConnInfo
	podExec exec.RemoteExecutor
	res     scArmResult
}

const (
	scSeedTable = "sc_seed"
	scRampTable = "sc_ramp"
	scDir       = "/tmp/ampqa-sc"
)

func (a *scArm) run() (_ scArmResult, err error) {
	a.res = scArmResult{Mode: a.mode, RampRowsPerSec: map[int]float64{}, AnalyzeMS: map[int][]float64{}, AnalyzePages: map[int]int{}}
	defer func() {
		tctx, cancel := context.WithTimeout(context.Background(), perfTeardownBudget)
		defer cancel()
		if tdErr := a.td.run(tctx, a.logf); tdErr != nil && err == nil {
			err = fmt.Errorf("%s: teardown: %w", a.mode, tdErr)
		}
	}()
	for _, ph := range []struct {
		name string
		fn   func(context.Context) error
	}{
		{"provision", a.provision},
		{"verify_compression", a.verifyCompression},
		{"seed", a.seed},
		{"analyze", a.analyze},
		{"cold_reads", a.coldReads},
		{"insert_ramp", a.insertRamp},
	} {
		a.pair.sync(ph.name)
		if err := a.t.Step(ph.name+"@"+a.mode, func(s *core.StepHandle) error {
			s.Set("stream_compression", a.mode)
			return ph.fn(a.t.Context)
		}); err != nil {
			a.collectDiag()
			return a.res, fmt.Errorf("%s: %w", a.mode, err)
		}
	}
	return a.res, nil
}

// collectDiag attaches the arm's own control plane and backing. The env collector
// gathers neither, so run 558579ca lost both endpoints mid-ramp with no evidence.
func (a *scArm) collectDiag() {
	ctx, cancel := context.WithTimeout(context.Background(), perfTeardownBudget)
	defer cancel()
	sink := attachSink{t: a.t, logf: a.logf}
	if a.ampKube != nil {
		diag.CollectK8sDiagnostics(ctx, a.ampKube, a.ampNS, sink, a.logf)
	}
	if a.kube != nil {
		diag.CollectUniverseK8sDiagnostics(ctx, a.kube, a.kube.Namespace(), sink, a.logf)
	}
}

func (a *scArm) provision(ctx context.Context) error {
	sfx := a.mode + "-" + a.t.ResourceSuffix()
	ybCfg := clampBackingRF(ybk8s.FromConfig(a.t.Options), a.cfg.RF)
	ybCfg.StreamCompression = a.mode
	ybCfg = a.cfg.isolateBacking(ybCfg)
	info, kube, err := deployYBBackingCfg(ctx, a.t, ybCfg, sfx, a.td, a.logf)
	if err != nil {
		return err
	}
	a.kube = kube
	if err := waitBackingYSQL(ctx, kube, info, a.cfg.RF, a.cfg.ReadyTimeout, a.sink, a.logf); err != nil {
		return err
	}
	amp := a.cfg.isolateEndpoints(ampstack.FromConfig(a.t.Options))
	ep, err := provisionAmpEndpointOn(ctx, a.t, amp, ybCfg.KubeContext, ybCfg.Kubeconfig, ampBacking(info), sfx, a.params.endpointSpec(), a.sink, a.td, a.logf)
	if err != nil {
		return err
	}
	a.conn, a.ampKube, a.ampNS = ep.Conn, ep.Kube, ep.Namespace
	a.podExec, err = startShapedClientPod(ctx, kube, a.sink, a.td, "ampqa-sc-"+sfx, a.cfg.ClientImage, a.cfg.ClientPullSecret, a.cfg.clientShape())
	if err != nil {
		return err
	}
	if err := deliverPgbenchArtifact(ctx, a.t, a.cfg, a.podExec); err != nil {
		return err
	}
	if err := checkEndpointSettings(ctx, a.podExec, a.conn, a.params.pgSettings()); err != nil {
		return err
	}
	return a.writeScripts(ctx)
}

// verifyCompression reads every tserver's flags back: an arm whose backing
// ignored its mode would report the other arm's numbers under this one's name.
func (a *scArm) verifyCompression(ctx context.Context) error {
	_, want, _ := ybgflags.StreamCompression(a.mode)
	names, err := a.kube.GetSelector(ctx, "pod", "app=yb-tserver", "{.items[*].metadata.name}")
	if err != nil {
		return core.EnvFail("list tserver pods: %w", err)
	}
	pods := strings.Fields(names)
	if len(pods) == 0 {
		return core.EnvFail("no tserver pods in %s", a.kube.Namespace())
	}
	for _, pod := range pods {
		res, err := a.kube.ForPod(pod, "yb-tserver").Run(ctx, `curl -sf 'http://127.0.0.1:9000/varz?raw'`)
		if err != nil || res.ExitCode != 0 {
			return core.EnvFail("read /varz from %s: exit %d: %v %s", pod, res.ExitCode, err, strings.TrimSpace(res.Stderr))
		}
		if bad := gflagMismatches(res.Stdout, want); len(bad) > 0 {
			return fmt.Errorf("tserver %s was created with %s=%s but runs %s", pod, ybgflags.StreamCompressionKey, a.mode, strings.Join(bad, ", "))
		}
	}
	a.logf("perf: %d tservers run %s", len(pods), ybgflags.CSV(want))
	return nil
}

func gflagMismatches(varz string, want map[string]string) []string {
	got := map[string]string{}
	for _, line := range strings.Split(varz, "\n") {
		if k, v, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "--"), "="); ok {
			got[k] = v
		}
	}
	var bad []string
	for _, k := range slices.Sorted(maps.Keys(want)) {
		if got[k] != want[k] {
			bad = append(bad, fmt.Sprintf("%s=%q (want %q)", k, got[k], want[k]))
		}
	}
	return bad
}

func (a *scArm) writeScripts(ctx context.Context) error {
	files := map[string]string{
		"seed.sql":   scInsertScript(scSeedTable, a.params),
		"ramp.sql":   scInsertScript(scRampTable, a.params),
		"read.sql":   scReadScript(scSeedTable, a.params.SeedRows),
		"schema.sql": scSchemaSQL(),
	}
	res, err := a.podExec.Run(ctx, scWriteFilesCmd(scDir, files))
	if err != nil || res.ExitCode != 0 {
		return core.EnvFail("write the workload scripts into the client pod: exit %d: %v %s", res.ExitCode, err, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// scWriteFilesCmd writes each file through its own heredoc line; a heredoc
// body ends the command line, so nothing can be chained after its opener.
func scWriteFilesCmd(dir string, files map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "set -e\nmkdir -p %s\n", dir)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		fmt.Fprintf(&b, "cat > %s/%s <<'AMPQA_SC_EOF'\n%s\nAMPQA_SC_EOF\n", dir, name, files[name])
	}
	return b.String()
}

func scSchemaSQL() string {
	var b strings.Builder
	for _, tbl := range []string{scSeedTable, scRampTable} {
		fmt.Fprintf(&b, "DROP TABLE IF EXISTS %[1]s; CREATE TABLE %[1]s (id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY, v text NOT NULL);\n", tbl)
	}
	return b.String()
}

// scInsertScript inserts one batch of rows of row_bytes each, random_pct of
// which is md5 hex distinct per row and the rest repeated text, so gzip has
// both to work on. The md5 subquery is correlated on the row so no two rows
// of a batch share it.
func scInsertScript(table string, p streamCompressionParams) string {
	random := p.RowBytes * p.RandomPct / 100
	md5s := (random + 31) / 32
	fill := p.RowBytes - random
	const phrase = "the quick brown fox jumps over the lazy dog "
	reps := fill/len(phrase) + 1
	return fmt.Sprintf(`\set r random(1, 2000000000)
INSERT INTO %s (v) SELECT left((SELECT string_agg(md5((:r + g * 7919 + s)::text), '') FROM generate_series(1, %d) s), %d) || left(repeat('%s', %d), %d) FROM generate_series(1, %d) g;`,
		table, max(md5s, 1), random, phrase, reps, fill, p.Batch)
}

func scReadScript(table string, rows int) string {
	return fmt.Sprintf("\\set id random(1, %d)\nSELECT length(v) FROM %s WHERE id = :id;", rows, table)
}

func (a *scArm) seed(ctx context.Context) error {
	if _, err := a.sql(ctx, "-f "+scDir+"/schema.sql"); err != nil {
		return err
	}
	perClient := a.params.SeedRows / a.params.Batch / a.params.SeedClients
	res, err := pgbench.Run(ctx, a.podExec, pgbench.RunOptions{Conn: a.conn, Clients: a.params.SeedClients, Jobs: a.params.SeedClients,
		ExtraArgs: []string{"-n", "-f", scDir + "/seed.sql", "-t", strconv.Itoa(perClient)}})
	a.attach("seed", res.Raw)
	if err != nil {
		return fmt.Errorf("seed %d rows: %w", a.params.SeedRows, err)
	}
	a.res.SeedRowsPerSec = res.TPS * float64(a.params.Batch)
	a.logf("perf: seeded %d rows at %.0f rows/s", perClient*a.params.SeedClients*a.params.Batch, a.res.SeedRowsPerSec)
	return nil
}

func (a *scArm) analyze(ctx context.Context) error {
	for _, target := range a.params.AnalyzeTargets {
		for rep := 1; rep <= a.params.AnalyzeReps; rep++ {
			out, err := a.sql(ctx, fmt.Sprintf("-c %s -c %s -c %s",
				shellQuote(fmt.Sprintf("SET default_statistics_target = %d", target)), shellQuote(`\timing on`), shellQuote("ANALYZE VERBOSE "+scSeedTable)))
			a.attach(fmt.Sprintf("analyze-t%d-r%d", target, rep), out)
			if err != nil {
				return fmt.Errorf("ANALYZE at target %d: %w", target, err)
			}
			ms, pages, err := parseAnalyze(out)
			if err != nil {
				return fmt.Errorf("ANALYZE at target %d: %w", target, err)
			}
			a.res.AnalyzeMS[target] = append(a.res.AnalyzeMS[target], ms)
			a.res.AnalyzePages[target] = pages
			a.logf("perf: ANALYZE target %d rep %d: %.0f ms over %d pages", target, rep, ms, pages)
		}
	}
	return nil
}

var (
	psqlTimingRe    = regexp.MustCompile(`Time: ([0-9.]+) ms`)
	analyzeScanRe   = regexp.MustCompile(`scanned (\d+) of (\d+) pages`)
	errNoAnalyzeOut = errors.New("no ANALYZE timing in the ysqlsh output")
)

// parseAnalyze reads the ANALYZE's own psql timing (the last one, after the
// SET's) and the pages it sampled.
func parseAnalyze(out string) (ms float64, pages int, err error) {
	times := psqlTimingRe.FindAllStringSubmatch(out, -1)
	if len(times) == 0 {
		return 0, 0, errNoAnalyzeOut
	}
	ms, err = strconv.ParseFloat(times[len(times)-1][1], 64)
	if err != nil {
		return 0, 0, err
	}
	if m := analyzeScanRe.FindStringSubmatch(out); m != nil {
		pages, _ = strconv.Atoi(m[1])
	}
	return ms, pages, nil
}

func (a *scArm) coldReads(ctx context.Context) error {
	h0, m0, cerr := lfcCounters(ctx, a.podExec, a.conn)
	logPrefix := fmt.Sprintf("%s/read-%d", scDir, time.Now().UnixNano())
	res, err := pgbench.Run(ctx, a.podExec, pgbench.RunOptions{Conn: a.conn, Clients: a.params.ReadClients, Jobs: a.params.ReadClients,
		Duration: a.params.ReadDuration, ExtraArgs: []string{"-n", "-f", scDir + "/read.sql", "-l", "--log-prefix=" + logPrefix}})
	a.attach("cold-reads", res.Raw)
	if err != nil {
		return fmt.Errorf("cold reads: %w", err)
	}
	p95, p99, err := pgbenchLogPercentiles(ctx, a.podExec, logPrefix)
	if err != nil {
		return err
	}
	a.res.ReadTPS, a.res.ReadLatencyAvg, a.res.ReadLatencyP95, a.res.ReadLatencyP99 = res.TPS, res.LatencyAvgMs, p95, p99
	if h1, m1, err := lfcCounters(ctx, a.podExec, a.conn); err == nil && cerr == nil {
		a.res.ReadLFCHitPct, a.res.ReadLFCHitOK = lfcHitPct(h0, m0, h1, m1)
	}
	return nil
}

func (a *scArm) insertRamp(ctx context.Context) error {
	for _, clients := range a.params.RampClients {
		a.pair.sync(fmt.Sprintf("ramp/%d", clients))
		res, err := pgbench.Run(ctx, a.podExec, pgbench.RunOptions{Conn: a.conn, Clients: clients, Jobs: min(clients, 4),
			Duration: a.params.RampStep, ExtraArgs: []string{"-n", "-f", scDir + "/ramp.sql"}})
		a.attach(fmt.Sprintf("ramp-c%d", clients), res.Raw)
		if err != nil {
			return fmt.Errorf("insert ramp at %d clients: %w", clients, err)
		}
		a.res.RampRowsPerSec[clients] = res.TPS * float64(a.params.Batch)
		a.logf("perf: %d clients inserted %.0f rows/s", clients, a.res.RampRowsPerSec[clients])
	}
	return nil
}

func (a *scArm) sql(ctx context.Context, args string) (string, error) {
	ci := a.conn
	cmd := fmt.Sprintf("PGPASSWORD=%s %s -h %s -p %d -U %s -d %s -v ON_ERROR_STOP=1 %s 2>&1",
		shellQuote(ci.Password), ysqlshShimPath, shellQuote(ci.Host), ci.Port, shellQuote(ci.User), shellQuote(ci.Database), args)
	res, err := a.podExec.Run(ctx, cmd)
	if err != nil {
		return res.Stdout, err
	}
	if res.ExitCode != 0 {
		return res.Stdout, fmt.Errorf("ysqlsh exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stdout))
	}
	return res.Stdout, nil
}

func (a *scArm) attach(name, raw string) {
	if raw != "" {
		a.t.AttachBytes(fmt.Sprintf("sc-%s-%s.out", a.mode, name), "log", []byte(raw))
	}
}

// modeBarrier holds every arm at a phase until all arms reach it, so the arms
// contend for the cluster equally. An arm that leaves stops being waited for.
type modeBarrier struct {
	mu      sync.Mutex
	live    int
	arrived map[string]int
	release map[string]chan struct{}
}

func newModeBarrier(n int) *modeBarrier {
	return &modeBarrier{live: n, arrived: map[string]int{}, release: map[string]chan struct{}{}}
}

func (b *modeBarrier) sync(phase string) {
	b.mu.Lock()
	ch := b.chanLocked(phase)
	b.arrived[phase]++
	b.releaseReadyLocked()
	b.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(pairWaitCap):
	}
}

func (b *modeBarrier) leave() {
	b.mu.Lock()
	b.live--
	b.releaseReadyLocked()
	b.mu.Unlock()
}

func (b *modeBarrier) chanLocked(phase string) chan struct{} {
	ch, ok := b.release[phase]
	if !ok {
		ch = make(chan struct{})
		b.release[phase] = ch
	}
	return ch
}

func (b *modeBarrier) releaseReadyLocked() {
	for phase, n := range b.arrived {
		ch := b.chanLocked(phase)
		select {
		case <-ch:
		default:
			if n >= b.live {
				close(ch)
			}
		}
	}
}

// scDatapoints gates every arm's own numbers and reports gzip's ratio to none
// for each, where both arms ran.
func scDatapoints(results []scArmResult) (gated, info []core.Datapoint) {
	byMode := map[string][]core.Datapoint{}
	for _, r := range results {
		var dps []core.Datapoint
		add := func(name string, v float64, unit string, dir core.Direction) {
			if v > 0 {
				dps = append(dps, core.Datapoint{Name: scPrefix + r.Mode + "." + name, Value: v, Unit: unit, Direction: dir})
			}
		}
		add("seed_rows_per_s", r.SeedRowsPerSec, "rows/s", core.HigherIsBetter)
		for _, c := range slices.Sorted(maps.Keys(r.RampRowsPerSec)) {
			add(fmt.Sprintf("insert_rows_per_s@c%d", c), r.RampRowsPerSec[c], "rows/s", core.HigherIsBetter)
		}
		for _, target := range slices.Sorted(maps.Keys(r.AnalyzeMS)) {
			dp := core.Datapoint{Name: fmt.Sprintf("%s%s.analyze_ms@target%d", scPrefix, r.Mode, target), Value: median(r.AnalyzeMS[target]),
				Unit: "ms", Direction: core.LowerIsBetter, N: len(r.AnalyzeMS[target]), Samples: r.AnalyzeMS[target]}
			if dp.Value > 0 {
				dps = append(dps, dp)
			}
		}
		add("read_tps", r.ReadTPS, "tps", core.HigherIsBetter)
		add("read_latency_avg_ms", r.ReadLatencyAvg, "ms", core.LowerIsBetter)
		gated = append(gated, dps...)
		byMode[r.Mode] = dps

		for target, pages := range r.AnalyzePages {
			if pages > 0 {
				info = append(info, core.Datapoint{Name: fmt.Sprintf("%s%s.analyze_pages@target%d", scPrefix, r.Mode, target), Value: float64(pages), Unit: "pages", Direction: core.LowerIsBetter})
			}
		}
		if r.ReadLatencyP95 > 0 {
			info = append(info, core.Datapoint{Name: scPrefix + r.Mode + ".read_latency_p95_ms", Value: r.ReadLatencyP95, Unit: "ms", Direction: core.LowerIsBetter})
		}
		if r.ReadLatencyP99 > 0 {
			info = append(info, core.Datapoint{Name: scPrefix + r.Mode + ".read_latency_p99_ms", Value: r.ReadLatencyP99, Unit: "ms", Direction: core.LowerIsBetter})
		}
		if r.ReadLFCHitOK {
			info = append(info, core.Datapoint{Name: scPrefix + r.Mode + ".read_lfc_hit_pct", Value: r.ReadLFCHitPct, Unit: "%", Direction: core.LowerIsBetter})
		}
	}
	on, off := byMode[ybgflags.CompressionGzip], byMode[ybgflags.CompressionNone]
	ref := map[string]float64{}
	for _, dp := range off {
		ref[strings.TrimPrefix(dp.Name, scPrefix+ybgflags.CompressionNone+".")] = dp.Value
	}
	for _, dp := range on {
		metric := strings.TrimPrefix(dp.Name, scPrefix+ybgflags.CompressionGzip+".")
		if base := ref[metric]; base > 0 {
			info = append(info, core.Datapoint{Name: scPrefix + "gzip_over_none." + metric, Value: dp.Value / base, Unit: "x", Direction: dp.Direction})
		}
	}
	return gated, info
}

func formatSCResults(results []scArmResult) string {
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "%s\n  seed: %.0f rows/s\n", r.Mode, r.SeedRowsPerSec)
		for _, target := range slices.Sorted(maps.Keys(r.AnalyzeMS)) {
			fmt.Fprintf(&b, "  ANALYZE target %d: %v ms over %d pages\n", target, r.AnalyzeMS[target], r.AnalyzePages[target])
		}
		fmt.Fprintf(&b, "  cold reads: %.0f tps, avg %.2f ms, p95 %.2f ms, p99 %.2f ms", r.ReadTPS, r.ReadLatencyAvg, r.ReadLatencyP95, r.ReadLatencyP99)
		if r.ReadLFCHitOK {
			fmt.Fprintf(&b, ", LFC hit %.1f%%", r.ReadLFCHitPct)
		}
		b.WriteString("\n")
		for _, c := range slices.Sorted(maps.Keys(r.RampRowsPerSec)) {
			fmt.Fprintf(&b, "  insert ramp %d clients: %.0f rows/s\n", c, r.RampRowsPerSec[c])
		}
	}
	return b.String()
}

func scCharts(results []scArmResult) []core.Chart {
	ramp := core.Chart{Title: "Insert rate by client count", Kind: core.ChartLine, XLabel: "clients", YLabel: "rows/s"}
	for _, r := range results {
		s := core.ChartSeries{Name: r.Mode}
		for _, c := range slices.Sorted(maps.Keys(r.RampRowsPerSec)) {
			s.Points = append(s.Points, core.ChartPoint{X: float64(c), Y: r.RampRowsPerSec[c]})
		}
		ramp.Series = append(ramp.Series, s)
	}
	analyze := core.Chart{Title: "ANALYZE time by statistics target", Kind: core.ChartBar, XLabel: "default_statistics_target", YLabel: "ms"}
	for _, r := range results {
		s := core.ChartSeries{Name: r.Mode}
		for _, target := range slices.Sorted(maps.Keys(r.AnalyzeMS)) {
			s.Points = append(s.Points, core.ChartPoint{X: float64(target), Y: median(r.AnalyzeMS[target])})
		}
		analyze.Series = append(analyze.Series, s)
	}
	return []core.Chart{ramp, analyze}
}
