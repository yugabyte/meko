package features

import (
	"context"
	"fmt"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	// cronTable receives one row per fired job, which is the observable proof the
	// scheduler's background worker is alive on an amp endpoint.
	cronTable = "amp_cron_fired"
	// cronFireTimeout bounds waiting for the every-minute job's first execution:
	// one full minute of granularity, plus scheduling slack.
	cronFireTimeout = 3 * time.Minute
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_extension_pg_cron",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "pg_cron end to end on an opted-in endpoint: the library is genuinely in shared_preload_libraries, a scheduled every-minute job actually fires (a row lands and job_run_details records success), and unschedule removes it.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     25 * time.Minute,
		Run:         runAmpExtensionPgCron,
	})
}

func runAmpExtensionPgCron(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"extension_pg_cron", func(ctx context.Context) error {
			cl, err := a.f.controllerClient(ctx)
			if err != nil {
				return err
			}
			return extensionPgCronSteps(ctx, cl, a.proj, a.f.endpointSQL, a.f.t.Skip)
		}}}
	})
}

func extensionPgCronSteps(ctx context.Context, cl ampCtl, proj ampCluster, connect endpointConnector, skip func(string)) error {
	c, ok, err := optInExtensionEndpoint(ctx, cl, proj, connect, skip, "pg_cron", "pg_cron")
	if err != nil || !ok {
		return err
	}
	defer c.Close()

	// CREATE EXTENSION succeeds from the SQL scripts alone; the scheduler only
	// exists if the library was preloaded, so prove that first and cheaply.
	if err := assertPreloaded(ctx, c, "pg_cron"); err != nil {
		return err
	}
	if n, err := c.QueryInt(ctx, "SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'pg_cron launcher'"); err != nil {
		return fmt.Errorf("look for the pg_cron launcher worker: %w", err)
	} else if n == 0 {
		return fmt.Errorf("pg_cron is in shared_preload_libraries but no 'pg_cron launcher' backend is running — the scheduler's background worker never started, so no job will ever fire")
	}

	// The scheduler executes jobs in cron.database_name, and the fired-marker
	// table lives in the CONNECTED database — if the endpoint connection is ever
	// configured onto a different database, the job would fire invisibly and the
	// timeout below would blame the product. Make that mismatch its own error.
	cronDB, err := c.QueryString(ctx, "SELECT current_setting('cron.database_name')")
	if err != nil {
		return fmt.Errorf("read cron.database_name: %w", err)
	}
	connectedDB, err := c.QueryString(ctx, "SELECT current_database()")
	if err != nil {
		return fmt.Errorf("read current_database: %w", err)
	}
	if cronDB != connectedDB {
		return fmt.Errorf("this connection is to database %q but pg_cron executes jobs in cron.database_name = %q — align the endpoint connection config (env.ampstack endpoint db) with the scheduler's database before this test can observe a fired job", connectedDB, cronDB)
	}

	if _, err := c.Exec(ctx, "CREATE TABLE "+cronTable+" (fired_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		return fmt.Errorf("create the fired-marker table: %w", err)
	}
	jobID, err := c.QueryInt(ctx, "SELECT cron.schedule('amp-canary', '* * * * *', $$INSERT INTO "+cronTable+" DEFAULT VALUES$$)")
	if err != nil {
		return fmt.Errorf("cron.schedule an every-minute job: %w", err)
	}

	// The job fired iff its row landed. job_run_details is checked as well so a
	// job that ran and FAILED is reported as that, not as "never fired".
	deadline := time.Now().Add(cronFireTimeout)
	var fired int64
	for {
		if fired, err = c.QueryInt(ctx, "SELECT count(*) FROM "+cronTable); err != nil {
			return fmt.Errorf("count fired rows: %w", err)
		}
		if fired > 0 {
			break
		}
		if failedRuns, ferr := c.QueryInt(ctx,
			"SELECT count(*) FROM cron.job_run_details WHERE jobid = $1 AND status = 'failed'", jobID); ferr == nil && failedRuns > 0 {
			msg, _ := c.QueryString(ctx,
				"SELECT coalesce(max(return_message), '') FROM cron.job_run_details WHERE jobid = $1 AND status = 'failed'", jobID)
			return fmt.Errorf("the scheduled job ran and failed (%d failed run(s); last message %q) — the scheduler works but its executor cannot run SQL on this endpoint", failedRuns, msg)
		}
		if time.Now().After(deadline) {
			runs, _ := c.QueryInt(ctx, "SELECT count(*) FROM cron.job_run_details WHERE jobid = $1", jobID)
			return fmt.Errorf("the every-minute job never fired within %s (job_run_details rows: %d) — the pg_cron launcher is running but jobs do not execute on this endpoint", cronFireTimeout, runs)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}

	// unschedule is overloaded (bigint job id / text job name), so the parameter
	// must say which.
	if _, err := c.Exec(ctx, "SELECT cron.unschedule($1::bigint)", jobID); err != nil {
		return fmt.Errorf("cron.unschedule job %d: %w", jobID, err)
	}
	if n, err := c.QueryInt(ctx, "SELECT count(*) FROM cron.job WHERE jobid = $1", jobID); err != nil {
		return fmt.Errorf("re-read cron.job after unschedule: %w", err)
	} else if n != 0 {
		return fmt.Errorf("job %d is still in cron.job after cron.unschedule returned success", jobID)
	}
	return nil
}
