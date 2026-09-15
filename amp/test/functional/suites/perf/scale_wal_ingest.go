// The writers run inside the endpoint pod and time their own commits: the
// coordinator's tunnel adds ~29ms per round trip, which is most of a healthy commit.
package perf

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func init() {
	core.Register(core.Test{
		Name:        "perf/scale_wal_ingest",
		Suite:       "perf",
		Category:    core.Perf,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:        []string{scaleTier},
		OwningTeam:  core.TeamCompute,
		Description: "Double the number of in-pod writers committing batches against ONE endpoint every stage until commit p99 crosses the bound or WAL throughput falls stage over stage, and publish the highest sustained ingest rate as a datapoint. The failure at the ceiling must be clean: no compute restart, no pageserver or safekeeper respawn, every committed row still present, and every writer answered with success or a timeout rather than a severed connection.",
		Params:      []string{"perf.scale_wal_ingest", "perf.scale"},
		Timeout:     4 * time.Hour,
		Run:         runScaleWalIngest,
	})
}

type walIngestParams struct {
	ladderSteps
	StageDuration    time.Duration
	MaxP99           time.Duration
	DropPct          float64
	BatchRows        int
	PayloadBytes     int
	StatementTimeout time.Duration
	EndpointCPU      string
	EndpointMem      string
}

func walIngestParamsFromConfig(c *core.Config) walIngestParams {
	return walIngestParams{
		ladderSteps:      ladderStepsFromConfig(c, "perf.scale_wal_ingest", 2, 2, 6),
		StageDuration:    c.GetDuration("perf.scale.stage_duration", 6*time.Minute),
		MaxP99:           c.GetDuration("perf.scale_wal_ingest.stop_max_p99", 2*time.Second),
		DropPct:          c.GetFloat("perf.scale_wal_ingest.stop_throughput_drop_pct", 5),
		BatchRows:        c.GetInt("perf.scale_wal_ingest.batch_rows", 100),
		PayloadBytes:     c.GetInt("perf.scale_wal_ingest.payload_bytes", 64),
		StatementTimeout: c.GetDuration("perf.scale_wal_ingest.statement_timeout", 10*time.Minute),
		EndpointCPU:      c.GetString("perf.scale_wal_ingest.endpoint_cpu", "4"),
		EndpointMem:      c.GetString("perf.scale_wal_ingest.endpoint_memory", "8Gi"),
	}
}

// Each iteration carries the previous one's elapsed into its own transaction, so
// timing costs no commit of its own.
const walIngestSchema = `
CREATE TABLE IF NOT EXISTS wal_ingest_data (writer int, seq bigint, pad text, PRIMARY KEY (writer, seq));
CREATE TABLE IF NOT EXISTS wal_ingest_lat (stage int, writer int, ms double precision);
CREATE OR REPLACE PROCEDURE wal_ingest_run(p_stage int, p_writer int, p_secs int, p_batch int, p_pad int)
LANGUAGE plpgsql AS $proc$
DECLARE
  deadline timestamptz := clock_timestamp() + make_interval(secs => p_secs);
  t0 timestamptz;
  n bigint := 0;
  prev double precision;
BEGIN
  WHILE clock_timestamp() < deadline LOOP
    t0 := clock_timestamp();
    INSERT INTO wal_ingest_data (writer, seq, pad)
      SELECT p_writer, n + g - 1, repeat('x', p_pad) FROM generate_series(1, p_batch) g;
    IF prev IS NOT NULL THEN
      INSERT INTO wal_ingest_lat (stage, writer, ms) VALUES (p_stage, p_writer, prev);
    END IF;
    COMMIT;
    prev := extract(epoch from clock_timestamp() - t0) * 1000;
    n := n + p_batch;
  END LOOP;
  IF prev IS NOT NULL THEN
    INSERT INTO wal_ingest_lat (stage, writer, ms) VALUES (p_stage, p_writer, prev);
    COMMIT;
  END IF;
END $proc$;
`

func walIngestScript(p walIngestParams, stage, writers int) string {
	return fmt.Sprintf(`rm -f /tmp/wi_*.out /tmp/wi_*.rc
for i in $(seq 1 %d); do
  (
    PGAPPNAME=walingest psql -U cloud_admin -h 127.0.0.1 -p 55433 -d postgres -At -v ON_ERROR_STOP=1 \
      -c "SET statement_timeout = '%ds'" \
      -c "CALL wal_ingest_run(%d, $i, %d, %d, %d)" >/tmp/wi_$i.out 2>&1
    echo $? >/tmp/wi_$i.rc
  ) &
done
wait
for i in $(seq 1 %d); do echo "writer $i rc=$(cat /tmp/wi_$i.rc 2>/dev/null) $(tr '\n' ' ' </tmp/wi_$i.out 2>/dev/null)"; done
`, writers, int(p.StatementTimeout.Seconds()), stage, int(p.StageDuration.Seconds()),
		p.BatchRows, p.PayloadBytes, writers)
}

func runScaleWalIngest(t *core.T) error {
	env, ok := t.Env.(ampMultitenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, ok := t.Env.(interface{ AmpKube() *k8s.Kubectl })
	if !ok || kubeEnv.AmpKube() == nil {
		t.Skip("the writers run inside the endpoint pod, which needs a kube context")
		return nil
	}
	params := walIngestParamsFromConfig(t.Options)
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
				logf("scale_wal_ingest: could not delete cluster %s: %v", tenants[i].ClusterID, err)
			}
		}
	}()

	var pod string
	var baseHealth endpointHealth
	if err := t.Step("create_endpoint", func(s *core.StepHandle) error {
		ten, _, err := createTenant(t, cl, env, poolID, 0, multitenantParams{
			SeedRows:       1,
			EndpointCPU:    params.EndpointCPU,
			EndpointMemory: params.EndpointMem,
		})
		if err != nil {
			return fmt.Errorf("%w%s", err, schedulingHint(t, logf))
		}
		tenants = append(tenants, ten)
		if _, err := ten.Conn.Exec(t.Context, walIngestSchema); err != nil {
			return fmt.Errorf("install the writer procedure: %w", err)
		}
		if pod, err = endpointPodByID(t.Context, kube, ten.Endpoint.ID); err != nil {
			return err
		}
		if baseHealth, err = snapshotEndpointHealth(t.Context, kube, pod); err != nil {
			return fmt.Errorf("baseline endpoint health: %w", err)
		}
		s.Set("endpoint", ten.Endpoint.ID)
		s.Set("endpoint_size", params.EndpointCPU+" / "+params.EndpointMem)
		s.Set("pod", pod)
		s.Set("batch_rows", params.BatchRows)
		return nil
	}); err != nil {
		return err
	}
	ten := tenants[0]
	ex := kube.ForPod(pod, "endpoint")

	var (
		mbpsAt     []core.ChartPoint
		p99At      []core.ChartPoint
		lastGood   float64
		goodWriter int
		brokeAt    int
		writerOut  string
		stopReason string
	)

	for i := 0; i < params.MaxStages; i++ {
		writers := params.rung(i)
		stage := fmt.Sprintf("stage_%02d_writers%d", i+1, writers)
		var mbps, p99 float64
		err := t.Step(stage, func(s *core.StepHandle) error {
			if _, err := ten.Conn.Exec(t.Context, "TRUNCATE wal_ingest_data"); err != nil {
				return fmt.Errorf("clear the previous stage's rows: %w", err)
			}
			before, err := ten.Conn.QueryString(t.Context, "SELECT pg_current_wal_lsn()::text")
			if err != nil {
				return fmt.Errorf("read the WAL position: %w", err)
			}
			script := walIngestScript(params, i+1, writers)
			if _, err := ex.RunStdin(t.Context, strings.NewReader(script), "cat > /tmp/wal_ingest.sh"); err != nil {
				return fmt.Errorf("deliver the writer script: %w", err)
			}
			runCtx, cancel := context.WithTimeout(t.Context, params.StageDuration+10*time.Minute)
			defer cancel()
			start := time.Now()
			res, err := ex.Run(runCtx, "sh /tmp/wal_ingest.sh")
			elapsed := time.Since(start)
			if err != nil {
				return fmt.Errorf("run %d writers: %w", writers, err)
			}
			writerOut = strings.TrimSpace(res.Stdout)
			t.AttachBytes(fmt.Sprintf("writers-stage%02d.txt", i+1), "text/plain", []byte(writerOut+"\n"+res.Stderr))

			bytes, err := walBytesSince(t.Context, ten.Conn, before)
			if err != nil {
				return err
			}
			mbps = bytes / (1 << 20) / elapsed.Seconds()
			p99, err = commitP99MS(t.Context, ten.Conn, i+1)
			if err != nil {
				return err
			}
			s.Set("writers", writers)
			s.Set("wal_mb", fmt.Sprintf("%.0f", bytes/(1<<20)))
			s.Set("wal_mbps", fmt.Sprintf("%.2f", mbps))
			s.Set("commit_p99_ms", fmt.Sprintf("%.0f", p99))
			if failed := failedWriters(writerOut); failed != "" {
				s.Set("writer_failures", failed)
				if diag := captureEndpointDiag(t, kube, ten, fmt.Sprintf("writers%d", writers), logf); diag != "" {
					s.Set("endpoint_state", strings.ReplaceAll(diag, "\n", " | "))
				}
				return wallError{fmt.Errorf("%d writers: a writer stopped committing: %s", writers, failed)}
			}
			if p99 <= 0 {
				return fmt.Errorf("%d writers: no commit was timed, so the stage measured nothing", writers)
			}
			return nil
		})
		if err != nil {
			var wall wallError
			if !errors.As(err, &wall) {
				return fmt.Errorf("%d writers (ceiling so far %d writers at %.2f MB/s): %w", writers, goodWriter, lastGood, err)
			}
			if i == 0 {
				return fmt.Errorf("the first stage could not be measured: %w", err)
			}
			stopReason = fmt.Sprintf("%d writers: %v", writers, err)
			brokeAt = writers
			break
		}
		mbpsAt = append(mbpsAt, core.ChartPoint{X: float64(writers), Y: mbps})
		p99At = append(p99At, core.ChartPoint{X: float64(writers), Y: p99})
		logf("scale_wal_ingest: %d writers: %.2f MB/s of WAL, commit p99 %.0fms", writers, mbps, p99)

		if p99 > float64(params.MaxP99.Milliseconds()) {
			stopReason = fmt.Sprintf("%d writers: commit p99 reached %.0fms, past the %s bound", writers, p99, params.MaxP99)
			brokeAt = writers
			break
		}
		if i > 0 && mbps < lastGood*(1-params.DropPct/100) {
			stopReason = fmt.Sprintf("%d writers: WAL throughput fell to %.2f MB/s from %.2f", writers, mbps, lastGood)
			brokeAt = writers
			break
		}
		lastGood, goodWriter = mbps, writers
	}

	return t.Step("report_ceiling", func(s *core.StepHandle) error {
		if goodWriter == 0 {
			return fmt.Errorf("no stage sustained any ingest")
		}
		s.Set("stop_reason", stopReason)
		if brokeAt == 0 {
			return noCeilingReached(params.MaxStages, fmt.Sprintf("%d writers still sustained %.2f MB/s", goodWriter, lastGood), "")
		}
		s.Set("max_sustained_mbps", fmt.Sprintf("%.2f", lastGood))
		s.Set("max_sustained_writers", goodWriter)
		s.Set("broke_at_writers", brokeAt)
		gaps, gapErr := walIngestGaps(t.Context, ten.Conn)
		if gapErr == nil && gaps > 0 {
			gapErr = fmt.Errorf("%d writer(s) are missing rows they committed", gaps)
		}
		clean := assertCleanCeiling(t.Context, s, kube, pod, baseHealth,
			cleanCheck{"acknowledged rows", gapErr},
			cleanCheck{"how the writers were answered", writerAnswerVerdict(writerOut)})
		t.Chart(core.Chart{
			Title: "scale_wal_ingest: WAL throughput vs writers", Kind: core.ChartLine,
			XLabel: "writers", YLabel: "MB/s",
			Series: []core.ChartSeries{{Name: "wal", Points: mbpsAt}},
		})
		t.Chart(core.Chart{
			Title: "scale_wal_ingest: commit p99 vs writers", Kind: core.ChartLine,
			XLabel: "writers", YLabel: "ms",
			Series: []core.ChartSeries{{Name: "p99", Points: p99At}},
		})
		gated := []core.Datapoint{
			{Name: "wal_ingest.max_sustained_mbps", Value: lastGood, Unit: "MB/s", Direction: core.HigherIsBetter},
		}
		info := []core.Datapoint{
			{Name: "wal_ingest.max_sustained_writers", Value: float64(goodWriter), Unit: "writers", Direction: core.HigherIsBetter},
			{Name: "wal_ingest.commit_p99_at_ceiling", Value: p99At[len(p99At)-1].Y, Unit: "ms", Direction: core.LowerIsBetter},
		}
		id := identityFromConfig(t.Options, "", frameworkCommit(),
			LoadFields{Script: "scale_wal_ingest", Clients: params.Start, Rows: params.BatchRows,
				MeasurementMS: params.StageDuration.Milliseconds()})
		t.SetPerfIdentity(id.core())
		if gateErr := emitAndGateAuto(t, perfFromConfig(t.Options), scaleVersion(t), "perf/scale_wal_ingest", gated, info, id, logf); gateErr != nil {
			return gateErr
		}
		return clean
	})
}

func walBytesSince(ctx context.Context, conn *pg.Client, lsn string) (float64, error) {
	got, err := conn.QueryString(ctx,
		"SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), $1::pg_lsn)::text", lsn)
	if err != nil {
		return 0, fmt.Errorf("measure the WAL the stage produced: %w", err)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(got), 64)
	if err != nil {
		return 0, fmt.Errorf("WAL delta %q: %w", got, err)
	}
	return v, nil
}

func commitP99MS(ctx context.Context, conn *pg.Client, stage int) (float64, error) {
	got, err := conn.QueryString(ctx,
		"SELECT coalesce(percentile_disc(0.99) WITHIN GROUP (ORDER BY ms), 0)::text FROM wal_ingest_lat WHERE stage = $1", stage)
	if err != nil {
		return 0, fmt.Errorf("read the stage's commit latencies: %w", err)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(got), 64)
	if err != nil {
		return 0, fmt.Errorf("commit p99 %q: %w", got, err)
	}
	return v, nil
}

// Writers number rows from zero without a gap, so a gap is an acknowledged commit that was lost.
func walIngestGaps(ctx context.Context, conn *pg.Client) (int64, error) {
	return conn.QueryInt(ctx,
		`SELECT count(*) FROM (SELECT writer FROM wal_ingest_data GROUP BY writer
		 HAVING count(*) <> max(seq) + 1 OR min(seq) <> 0) g`)
}

func failedWriters(out string) string {
	var bad []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "rc=0") || strings.TrimSpace(line) == "" {
			continue
		}
		bad = append(bad, strings.TrimSpace(line))
	}
	return strings.Join(bad, " | ")
}

var severedMarkers = []string{
	"server closed the connection unexpectedly",
	"connection reset by peer",
	"could not receive data from server",
	"terminating connection because of crash",
	"SSL connection has been closed unexpectedly",
}

func writerAnswerVerdict(out string) error {
	low := strings.ToLower(out)
	for _, m := range severedMarkers {
		if strings.Contains(low, strings.ToLower(m)) {
			return fmt.Errorf("a writer's connection was severed rather than refused or timed out (%q)", m)
		}
	}
	return nil
}
