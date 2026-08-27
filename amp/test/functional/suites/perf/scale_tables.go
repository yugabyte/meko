// Escalation by TABLE COUNT on one endpoint — catalog pressure rather than the
// client count (scale.go) or dataset size (scale_depth.go). Each table carries
// only a few rows: the question is relations survived, not data.
package perf

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func init() {
	core.Register(core.Test{
		Name:     "perf/scale_tables",
		Suite:    "perf",
		Category: core.Perf,
		Env:      core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:     []string{scaleTier},
		// Catalog growth is a storage-layer question as much as a Postgres one:
		// every relation is state the pageserver tracks.
		OwningTeam:  core.TeamCompute,
		Description: "Create tables on ONE endpoint batch after batch, each with a few rows, until the endpoint stops serving: creates fail, a batch stops finishing, a catalog query stops answering, or the storage layer stops advancing. Reports how many relations one compute carried and what the create rate did on the way.",
		Params:      []string{"perf.scale_tables"},
		Timeout:     6 * time.Hour,
		Run:         runScaleTables,
	})
}

type scaleTablesParams struct {
	// BatchSize is how many tables one stage adds.
	BatchSize int
	// MaxTables is the backstop; the ladder is meant to end on the product.
	MaxTables int
	// RowsPerTable keeps each relation small — this measures relations, not data.
	RowsPerTable int
	// Budget caps the ladder in wall-clock.
	Budget time.Duration
	// BatchTimeout bounds one batch: a create that hangs must end the stage
	// rather than the run.
	BatchTimeout time.Duration
	// CatalogTimeout bounds the post-batch catalog query. A catalog that stops
	// answering is a wall even when creates still succeed.
	CatalogTimeout time.Duration
	EndpointCPU    string
	EndpointMem    string
}

func scaleTablesParamsFromConfig(c *core.Config) scaleTablesParams {
	return scaleTablesParams{
		BatchSize:      c.GetInt("perf.scale_tables.batch_size", 500),
		MaxTables:      c.GetInt("perf.scale_tables.max_tables", 100000),
		RowsPerTable:   c.GetInt("perf.scale_tables.rows_per_table", 3),
		Budget:         c.GetDuration("perf.scale_tables.budget", 4*time.Hour),
		BatchTimeout:   c.GetDuration("perf.scale_tables.batch_timeout", 30*time.Minute),
		CatalogTimeout: c.GetDuration("perf.scale_tables.catalog_timeout", 2*time.Minute),
		EndpointCPU:    c.GetString("perf.scale_tables.endpoint_cpu", "4"),
		EndpointMem:    c.GetString("perf.scale_tables.endpoint_memory", "8Gi"),
	}
}

func runScaleTables(t *core.T) error {
	env, ok := t.Env.(ampMultitenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, ok := t.Env.(interface{ AmpKube() *k8s.Kubectl })
	if !ok || kubeEnv.AmpKube() == nil {
		t.Skip("needs a kube context for the control-plane namespace")
		return nil
	}
	params := scaleTablesParamsFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)
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
				logf("scale_tables: could not delete cluster %s: %v", tenants[i].ClusterID, err)
			}
		}
	}()

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
		if err := ensureProbeTable(t.Context, ten.Conn); err != nil {
			return fmt.Errorf("create the probe table: %w", err)
		}
		s.Set("endpoint", ten.Endpoint.ID)
		s.Set("endpoint_size", params.EndpointCPU+" / "+params.EndpointMem)
		s.Set("batch_size", params.BatchSize)
		s.Set("rows_per_table", params.RowsPerTable)
		return nil
	}); err != nil {
		return err
	}
	ten := tenants[0]

	var (
		created   int
		rateAt    []core.ChartPoint
		catalogAt []core.ChartPoint
		peakRate  float64
		brokeAt   int
	)
	stopReason := fmt.Sprintf("carried all %d tables without breaking", params.MaxTables)
	deadline := time.Now().Add(params.Budget)

	for batch := 1; created < params.MaxTables; batch++ {
		if batch > 1 && time.Now().After(deadline) {
			stopReason = fmt.Sprintf("%d tables: ran out of the %s ladder budget before anything broke", created, params.Budget)
			break
		}
		stage := fmt.Sprintf("batch_%02d_tables%d", batch, created+params.BatchSize)
		var rate, catalogMS float64
		err := t.Step(stage, func(s *core.StepHandle) error {
			start := time.Now()
			ctx, cancel := context.WithTimeout(t.Context, params.BatchTimeout)
			defer cancel()
			made, err := createTableBatch(ctx, ten.Conn, created, params)
			created += made
			elapsed := time.Since(start)
			s.Set("tables_total", created)
			s.Set("tables_added", made)
			s.Set("batch_seconds", fmt.Sprintf("%.1f", elapsed.Seconds()))
			if elapsed > 0 {
				rate = float64(made) / elapsed.Seconds()
				s.Set("creates_per_second", fmt.Sprintf("%.2f", rate))
			}
			if err != nil {
				return err
			}
			// A catalog that stops answering is a wall even while creates still
			// succeed, and it is what a customer notices first.
			cctx, ccancel := context.WithTimeout(t.Context, params.CatalogTimeout)
			defer ccancel()
			cstart := time.Now()
			n, err := ten.Conn.QueryInt(cctx, "SELECT count(*) FROM pg_class WHERE relkind = 'r'")
			if err != nil {
				return fmt.Errorf("pg_class did not answer within %s at %d tables: %w", params.CatalogTimeout, created, err)
			}
			catalogMS = float64(time.Since(cstart).Milliseconds())
			s.Set("catalog_count", n)
			s.Set("catalog_query_ms", fmt.Sprintf("%.0f", catalogMS))
			// The storage layer has to still be advancing under all of it.
			return probeWriteRead(cctx, ten, batch)
		})
		if err != nil {
			if diag := captureEndpointDiag(t, kubeEnv.AmpKube(), ten, fmt.Sprintf("tables%d", created), logf); diag != "" {
				logf("scale_tables: endpoint at the wall: %s", strings.ReplaceAll(diag, "\n", " | "))
				err = fmt.Errorf("%w [endpoint: %s]", err, strings.ReplaceAll(diag, "\n", " | "))
			}
			stopReason = fmt.Sprintf("%d tables: broke — %v", created, err)
			brokeAt = created
			if created <= params.BatchSize {
				return fmt.Errorf("the first batch of %d tables did not complete, so nothing was measured: %w", params.BatchSize, err)
			}
			break
		}
		rateAt = append(rateAt, core.ChartPoint{X: float64(created), Y: rate})
		catalogAt = append(catalogAt, core.ChartPoint{X: float64(created), Y: catalogMS})
		if rate > peakRate {
			peakRate = rate
		}
		logf("scale_tables: %d tables, %.2f creates/s, catalog count in %.0fms", created, rate, catalogMS)
	}

	return t.Step("report_ceiling", func(s *core.StepHandle) error {
		if created == 0 {
			return fmt.Errorf("no table was created")
		}
		s.Set("max_tables", created)
		s.Set("broke_at_tables", brokeAt)
		s.Set("stop_reason", stopReason)
		s.Set("peak_creates_per_second", fmt.Sprintf("%.2f", peakRate))
		if n := len(rateAt); n > 0 {
			s.Set("final_creates_per_second", fmt.Sprintf("%.2f", rateAt[n-1].Y))
			s.Set("final_catalog_query_ms", fmt.Sprintf("%.0f", catalogAt[n-1].Y))
		}
		gated := []core.Datapoint{
			{Name: "scale_tables.max_tables", Value: float64(created), Unit: "tables", Direction: core.HigherIsBetter},
		}
		info := []core.Datapoint{
			{Name: "scale_tables.peak_creates_per_second", Value: peakRate, Unit: "tables/s", Direction: core.HigherIsBetter},
		}
		if n := len(catalogAt); n > 0 {
			info = append(info, core.Datapoint{Name: "scale_tables.final_catalog_query_ms",
				Value: catalogAt[n-1].Y, Unit: "ms", Direction: core.LowerIsBetter})
		}
		id := identityFromConfig(t.Options, "", frameworkCommit(), loadFieldsScaleTables(params))
		t.SetPerfIdentity(id.core())
		gateErr := emitAndGateAuto(t, perfFromConfig(t.Options), scaleVersion(t), "perf/scale_tables", gated, info, id, logf)
		t.Chart(core.Chart{
			Title: "scale_tables: create rate vs table count", Kind: core.ChartLine,
			XLabel: "tables", YLabel: "creates/s",
			Series: []core.ChartSeries{{Name: "creates/s", Points: rateAt}},
		})
		t.Chart(core.Chart{
			Title: "scale_tables: catalog query latency vs table count", Kind: core.ChartLine,
			XLabel: "tables", YLabel: "ms",
			Series: []core.ChartSeries{{Name: "count(pg_class)", Points: catalogAt}},
		})
		return gateErr
	})
}

// createTableBatch creates the next batch, each table carrying RowsPerTable
// rows. Returns how many it actually made, so a failure mid-batch still reports
// the true count.
func createTableBatch(ctx context.Context, conn *pg.Client, from int, p scaleTablesParams) (int, error) {
	made := 0
	for i := from; i < from+p.BatchSize; i++ {
		name := fmt.Sprintf("t_%06d", i)
		if _, err := conn.Exec(ctx, fmt.Sprintf(
			"CREATE TABLE %s (id int PRIMARY KEY, v bigint, pad text)", name)); err != nil {
			return made, fmt.Errorf("create table %s (number %d): %w", name, i+1, err)
		}
		var vals []string
		for r := 0; r < p.RowsPerTable; r++ {
			vals = append(vals, fmt.Sprintf("(%d, %d, 'r%d')", r, int64(i)*100+int64(r), r))
		}
		if _, err := conn.Exec(ctx, fmt.Sprintf("INSERT INTO %s (id, v, pad) VALUES %s",
			name, strings.Join(vals, ","))); err != nil {
			return made, fmt.Errorf("seed table %s: %w", name, err)
		}
		made++
	}
	return made, nil
}

// loadFieldsScaleTables is what must match for two runs to be comparable.
func loadFieldsScaleTables(p scaleTablesParams) LoadFields {
	return LoadFields{
		Script: "scale_tables",
		Tables: p.BatchSize,
		Rows:   p.RowsPerTable,
	}
}
