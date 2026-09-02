// meko#651: a deployed endpoint never throttles writers when the safekeeper
// falls behind, because the walproposer lag thresholds and the WAL rate limiter
// all default to -1 in the k8s spec while the local launcher sets them.
package features

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// backpressureGUCs are the three gates; any one above zero means writers can be
// held back when storage lags.
var backpressureGUCs = []string{"max_replication_write_lag", "max_replication_flush_lag", "yb_amp.max_wal_mb_per_second"}

func init() {
	core.Register(core.Test{
		Name:        "features/pg_backpressure",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "Assert a deployed endpoint has walproposer backpressure armed: at least one of max_replication_write_lag, max_replication_flush_lag or yb_amp.max_wal_mb_per_second is above zero, so a compute cannot outrun its safekeeper without bound.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runPGBackpressure,
	})
}

func runPGBackpressure(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"backpressure_armed", a.backpressureArmed}}
	})
}

func (a *ampFeatureRun) backpressureArmed(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	ep, err := createEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID)
	if err != nil {
		return err
	}
	c, err := a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	defer c.Close()
	var seen []string
	armed := false
	for _, guc := range backpressureGUCs {
		known, err := c.QueryInt(ctx, "SELECT count(*) FROM pg_settings WHERE name = $1", guc)
		if err != nil {
			return fmt.Errorf("look up %s: %w", guc, err)
		}
		if known == 0 {
			seen = append(seen, guc+"=<unknown>")
			continue
		}
		val, err := c.QueryString(ctx, "SELECT setting FROM pg_settings WHERE name = $1", guc)
		if err != nil {
			return fmt.Errorf("read %s: %w", guc, err)
		}
		seen = append(seen, guc+"="+val)
		if n, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil && n > 0 {
			armed = true
		}
	}
	a.f.logf("backpressure: %s", strings.Join(seen, " "))
	if !armed {
		return fmt.Errorf("every walproposer backpressure gate is off on the deployed endpoint (%s): the compute can run ahead of its safekeeper without bound, which the local launcher never allows (meko#651)", strings.Join(seen, " "))
	}
	return nil
}
