package perf

import (
	"context"
	"errors"
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
		Name:        "perf/scale_transaction",
		Suite:       "perf",
		Category:    core.Perf,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:        []string{scaleTier},
		OwningTeam:  core.TeamCompute,
		Description: "Commit ONE transaction whose row count doubles every stage until the commit fails, and publish the largest transaction the endpoint committed. The failure must be clean: a Postgres error carrying a SQLSTATE rather than a severed connection, and the endpoint serving the next transaction straight after.",
		Params:      []string{"perf.scale_transaction"},
		Timeout:     4 * time.Hour,
		Run:         runScaleTransaction,
	})
}

type scaleTxParams struct {
	ladderSteps
	StatementTimeout time.Duration
	RecoverTimeout   time.Duration
	Budget           time.Duration
	PayloadBytes     int
	EndpointCPU      string
	EndpointMem      string
}

func scaleTxParamsFromConfig(c *core.Config) scaleTxParams {
	return scaleTxParams{
		ladderSteps:      ladderStepsFromConfig(c, "perf.scale_transaction", 100000, 2, 12),
		StatementTimeout: c.GetDuration("perf.scale_transaction.stop_statement_timeout", 30*time.Minute),
		RecoverTimeout:   c.GetDuration("perf.scale_transaction.recover_timeout", 60*time.Second),
		Budget:           c.GetDuration("perf.scale_transaction.budget", 3*time.Hour),
		PayloadBytes:     c.GetInt("perf.scale_transaction.payload_bytes", 64),
		EndpointCPU:      c.GetString("perf.scale_transaction.endpoint_cpu", "4"),
		EndpointMem:      c.GetString("perf.scale_transaction.endpoint_memory", "8Gi"),
	}
}

const scaleTxTable = "scale_tx_rows"

func runScaleTransaction(t *core.T) error {
	env, ok := t.Env.(ampMultitenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, ok := t.Env.(interface{ AmpKube() *k8s.Kubectl })
	if !ok || kubeEnv.AmpKube() == nil {
		t.Skip("the clean-failure verdict reads the endpoint pod, which needs a kube context")
		return nil
	}
	kube := kubeEnv.AmpKube()
	params := scaleTxParamsFromConfig(t.Options)
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
				logf("scale_transaction: could not delete cluster %s: %v", tenants[i].ClusterID, err)
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
		if _, err := ten.Conn.Exec(t.Context, fmt.Sprintf(
			"CREATE TABLE %s (id bigint, v bigint, pad text)", scaleTxTable)); err != nil {
			return fmt.Errorf("create the target table: %w", err)
		}
		if kube != nil {
			if pod, err = endpointPodByID(t.Context, kube, ten.Endpoint.ID); err != nil {
				return err
			}
			if baseHealth, err = snapshotEndpointHealth(t.Context, kube, pod); err != nil {
				return fmt.Errorf("baseline endpoint health: %w", err)
			}
		}
		s.Set("endpoint", ten.Endpoint.ID)
		s.Set("endpoint_size", params.EndpointCPU+" / "+params.EndpointMem)
		return nil
	}); err != nil {
		return err
	}
	ten := tenants[0]

	var (
		commitAt   []core.ChartPoint
		committed  int
		brokeAt    int
		failure    error
		stopReason string
	)
	deadline := time.Now().Add(params.Budget)

	for i := 0; i < params.MaxStages; i++ {
		rows := params.rung(i)
		if i > 0 && time.Now().After(deadline) {
			stopReason = fmt.Sprintf("%d rows: out of the %s budget before a commit failed", committed, params.Budget)
			break
		}
		stage := fmt.Sprintf("stage_%02d_rows%d", i+1, rows)
		var took float64
		err := t.Step(stage, func(s *core.StepHandle) error {
			if _, err := ten.Conn.Exec(t.Context, "TRUNCATE "+scaleTxTable); err != nil {
				return fmt.Errorf("clear the previous stage's rows: %w", err)
			}
			s.Set("rows", rows)
			start := time.Now()
			err := commitOneTransaction(t.Context, ten.Conn, rows, params)
			took = msSince(start)
			s.Set("commit_ms", fmt.Sprintf("%.0f", took))
			if err != nil {
				failure = err
				s.Set("sqlstate", sqlstateOf(err))
				if diag := captureEndpointDiag(t, kube, ten, fmt.Sprintf("tx%drows", rows), logf); diag != "" {
					s.Set("endpoint_state", strings.ReplaceAll(diag, "\n", " | "))
				}
				return wallError{fmt.Errorf("the transaction of %d rows did not commit after %.0fs: %w",
					rows, took/1000, err)}
			}
			n, err := ten.Conn.QueryInt(t.Context, "SELECT count(*) FROM "+scaleTxTable)
			if err != nil {
				return fmt.Errorf("count what the transaction committed: %w", err)
			}
			if n != int64(rows) {
				return fmt.Errorf("the transaction committed but the table holds %d of %d rows", n, rows)
			}
			return nil
		})
		if err != nil {
			var wall wallError
			if !errors.As(err, &wall) {
				return fmt.Errorf("%d rows (largest committed %d): %w", rows, committed, err)
			}
			if i == 0 {
				return fmt.Errorf("the smallest transaction (%d rows) could not be committed, so nothing was measured: %w", rows, err)
			}
			stopReason = fmt.Sprintf("%d rows: %v", rows, err)
			brokeAt = rows
			break
		}
		committed = rows
		commitAt = append(commitAt, core.ChartPoint{X: float64(rows), Y: took})
		logf("scale_transaction: %d rows committed in %.1fs", rows, took/1000)
	}

	return t.Step("report_ceiling", func(s *core.StepHandle) error {
		if committed == 0 {
			return fmt.Errorf("no transaction committed")
		}
		s.Set("stop_reason", stopReason)
		if brokeAt == 0 {
			return noCeilingReached(params.MaxStages, fmt.Sprintf("a transaction of %d rows still committed", committed), stopReason)
		}
		s.Set("max_rows_committed", committed)
		s.Set("broke_at_rows", brokeAt)
		if failure != nil && isTransportErr(failure) {
			return fmt.Errorf("the ladder ended when the connection to the endpoint died, which is the coordinator's tunnel and not a commit the product refused: %w", failure)
		}
		clean := assertCleanCeiling(t.Context, s, kube, pod, baseHealth,
			cleanCheck{"the way the commit failed", postgresErrorVerdict(failure)},
			cleanCheck{"the transaction after it", servesNextTransaction(t, ten, params.RecoverTimeout)})
		t.Chart(core.Chart{
			Title: "scale_transaction: commit time vs rows in one transaction", Kind: core.ChartLine,
			XLabel: "rows", YLabel: "ms",
			Series: []core.ChartSeries{{Name: "commit", Points: commitAt}},
		})
		gated := []core.Datapoint{
			{Name: "transaction.max_rows_committed", Value: float64(committed), Unit: "rows", Direction: core.HigherIsBetter},
		}
		info := []core.Datapoint{
			{Name: "transaction.commit_ms_at_ceiling", Value: commitAt[len(commitAt)-1].Y, Unit: "ms", Direction: core.LowerIsBetter},
		}
		id := identityFromConfig(t.Options, "", frameworkCommit(),
			LoadFields{Script: "scale_transaction", Rows: params.Start})
		t.SetPerfIdentity(id.core())
		if gateErr := emitAndGateAuto(t, perfFromConfig(t.Options), scaleVersion(t), "perf/scale_transaction", gated, info, id, logf); gateErr != nil {
			return gateErr
		}
		return clean
	})
}

// Pinned, not pooled: through the pool a compute that went away is silently
// replaced, and the question is whether this transaction's connection survived.
func commitOneTransaction(ctx context.Context, conn *pg.Client, rows int, p scaleTxParams) error {
	sess, err := conn.Pin(ctx)
	if err != nil {
		return fmt.Errorf("pin a backend: %w", err)
	}
	defer sess.Close()
	if _, err := sess.Exec(ctx, "BEGIN"); err != nil {
		return err
	}
	// SET LOCAL, not SET: a pinned session goes back to the pool carrying
	// whatever it was set to, and the next pool query inherits it.
	if _, err := sess.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%ds'", int(p.StatementTimeout.Seconds()))); err != nil {
		_, _ = sess.Exec(ctx, "ROLLBACK")
		return fmt.Errorf("set the statement timeout: %w", err)
	}
	if _, err := sess.Exec(ctx, fmt.Sprintf(
		"INSERT INTO %s SELECT g, g * 7, repeat('x', %d) FROM generate_series(1, %d) g",
		scaleTxTable, p.PayloadBytes, rows)); err != nil {
		_, _ = sess.Exec(ctx, "ROLLBACK")
		return err
	}
	_, err = sess.Exec(ctx, "COMMIT")
	return err
}

func postgresErrorVerdict(err error) error {
	if err == nil {
		return nil
	}
	if errors.As(err, new(*pg.TransportError)) && !isTransportErr(err) {
		return fmt.Errorf("the connection was severed instead of the server refusing: %v", err)
	}
	if sqlstateOf(err) == "" {
		return fmt.Errorf("the failure carries no SQLSTATE, so Postgres never answered: %v", err)
	}
	return nil
}

func servesNextTransaction(t *core.T, ten tenant, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(t.Context, timeout)
	defer cancel()
	if _, err := ten.Conn.Exec(ctx, fmt.Sprintf(
		"INSERT INTO %s VALUES (-1, -1, 'after-the-ceiling')", scaleTxTable)); err != nil {
		return fmt.Errorf("the endpoint did not take the next transaction within %s: %w", timeout, err)
	}
	return nil
}
