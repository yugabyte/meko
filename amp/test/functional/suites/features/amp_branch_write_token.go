// The branch write lease (write_token_settings: ttl_ms, renew_seconds) decides
// whether a compute may keep committing. A bad value here is a data-plane
// outage, and nothing in the suite ever set it. Two halves: the 400s the
// controller promises, and a writer that keeps committing across many lease
// windows on a short, valid lease.
package features

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	// leaseTTLMS / leaseRenewSeconds is a short but legal lease: renewals every
	// second on a four-second token, so leaseWriteWindow spans several expiries.
	leaseTTLMS        = 4000
	leaseRenewSeconds = 1
	leaseWriteWindow  = 30 * time.Second
	leaseTable        = "amp_lease_rows"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_branch_write_token",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "gapcoverage"},
		OwningTeam:  core.TeamCompute,
		Description: "The branch write lease: renew_seconds at or over ttl_ms, a zero or fractional ttl, a non-numeric value and an unknown key are each refused 400 leaving the stored lease untouched; a valid short lease (4s ttl, 1s renew) is stored and echoed on the branch, a writer created under it commits every row for 30s without a single failure, a fork at that point reads them all, and tightening the lease under the running writer is acknowledged and leaves it writing.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     30 * time.Minute,
		Run:         runAmpBranchWriteToken,
	})
}

func runAmpBranchWriteToken(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{
			{"lease_refusals", a.leaseRefusals},
			{"lease_writer", a.leaseWriter},
		}
	})
}

func leaseReq(kv map[string]any) controller.UpdateSettingsRequest {
	return controller.UpdateSettingsRequest{WriteTokenSettings: &kv}
}

func (a *ampFeatureRun) leaseRefusals(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	before, err := findBranchByID(ctx, cl, a.proj)
	if err != nil {
		return err
	}
	cases := []struct {
		what string
		kv   map[string]any
		want string
	}{
		{"renew_seconds equal to ttl_ms", map[string]any{"ttl_ms": 10000, "renew_seconds": 10}, "must be under"},
		{"renew_seconds over ttl_ms", map[string]any{"ttl_ms": 10000, "renew_seconds": 11}, "must be under"},
		{"a zero ttl_ms", map[string]any{"ttl_ms": 0, "renew_seconds": 5}, "positive whole number"},
		{"a fractional ttl_ms", map[string]any{"ttl_ms": 1.5}, "positive whole number"},
		{"a string ttl_ms", map[string]any{"ttl_ms": "15000"}, "must be a number"},
		{"an unknown key", map[string]any{"ttl_ms": 15000, "lease_owner": "x"}, "unknown key"},
	}
	for _, tc := range cases {
		_, err := cl.UpdateBranchSettings(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, leaseReq(tc.kv))
		if err == nil {
			return fmt.Errorf("write_token_settings with %s was accepted, want 400: a lease a compute cannot renew in time stops every write on the branch", tc.what)
		}
		if status := apiErrorStatus(err); status != http.StatusBadRequest {
			if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
				a.f.t.Skip("this controller has no branch settings PATCH: " + err.Error())
				return nil
			}
			return fmt.Errorf("write_token_settings with %s failed with HTTP %d, want 400: %w", tc.what, status, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			return fmt.Errorf("the 400 for %s does not say %q: %v", tc.what, tc.want, err)
		}
	}
	after, err := findBranchByID(ctx, cl, a.proj)
	if err != nil {
		return err
	}
	if fmt.Sprint(after.WriteTokenSettings) != fmt.Sprint(before.WriteTokenSettings) {
		return fmt.Errorf("a refused write moved the stored lease from %v to %v", before.WriteTokenSettings, after.WriteTokenSettings)
	}
	return nil
}

func (a *ampFeatureRun) leaseWriter(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	proj := a.proj
	reply, err := cl.UpdateBranchSettings(ctx, proj.poolID, proj.projID, proj.branchID, leaseReq(map[string]any{"ttl_ms": leaseTTLMS, "renew_seconds": leaseRenewSeconds}))
	if err != nil {
		return fmt.Errorf("set a %dms/%ds lease: %w", leaseTTLMS, leaseRenewSeconds, err)
	}
	if !reply.Applied {
		return fmt.Errorf("the lease write answered applied=false")
	}
	br, err := findBranchByID(ctx, cl, proj)
	if err != nil {
		return err
	}
	if got := numberOf(br.WriteTokenSettings["ttl_ms"]); got != leaseTTLMS {
		return fmt.Errorf("the branch echoes write_token_settings.ttl_ms=%v after storing %d: %v", br.WriteTokenSettings["ttl_ms"], leaseTTLMS, br.WriteTokenSettings)
	}
	if got := numberOf(br.WriteTokenSettings["renew_seconds"]); got != leaseRenewSeconds {
		return fmt.Errorf("the branch echoes write_token_settings.renew_seconds=%v after storing %d", br.WriteTokenSettings["renew_seconds"], leaseRenewSeconds)
	}

	ep, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create a writer under the short lease: %w", err)
	}
	c, err := a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect the writer: %w", err)
	}
	defer c.Close()
	if _, err := c.Exec(ctx, "CREATE TABLE "+leaseTable+" (id serial primary key, at timestamptz default now())"); err != nil {
		return fmt.Errorf("create %s: %w", leaseTable, err)
	}
	written, err := writeAcross(ctx, c, leaseWriteWindow)
	if err != nil {
		return err
	}
	n, err := c.QueryInt(ctx, "SELECT count(*) FROM "+leaseTable)
	if err != nil {
		return fmt.Errorf("count %s: %w", leaseTable, err)
	}
	if n != written {
		return fmt.Errorf("%d rows committed over %s but %d are present", written, leaseWriteWindow, n)
	}
	a.f.t.Metric("lease_commits", float64(written), "rows")
	a.f.logf("%d commits over %s on a %dms lease renewed every %ds", written, leaseWriteWindow, leaseTTLMS, leaseRenewSeconds)

	lsn, err := currentLSN(ctx, c)
	if err != nil {
		return err
	}
	child, err := forkChildAt(ctx, cl, proj, proj.branchID, lsn, "lease-fork-"+a.f.suffix(), a.f.endpointSQL)
	if err != nil {
		return err
	}
	defer child.Close()
	if m, err := child.QueryInt(ctx, "SELECT count(*) FROM "+leaseTable); err != nil || m != n {
		return fmt.Errorf("the fork at %s sees %d rows of %s, want %d (err %v)", lsn, m, leaseTable, n, err)
	}

	// Tighten under the running writer. Whether that costs a restart is the
	// controller's call; that the writer writes afterwards is not.
	reply, err = cl.UpdateBranchSettings(ctx, proj.poolID, proj.projID, proj.branchID, leaseReq(map[string]any{"ttl_ms": leaseTTLMS - 1000, "renew_seconds": leaseRenewSeconds}))
	if err != nil {
		return fmt.Errorf("tighten the lease under a running writer: %w", err)
	}
	if !reply.Applied {
		return fmt.Errorf("tightening the lease answered applied=false")
	}
	for _, o := range reply.Endpoints {
		if o.Unreachable != "" {
			return fmt.Errorf("the writer %s could not be told about the new lease: %s", o.EndpointID, o.Unreachable)
		}
	}
	c.Close()
	fresh, err := reconnect(ctx, a.f.endpointSQL, ep)
	if err != nil {
		return fmt.Errorf("after the lease change: %w", err)
	}
	c = fresh
	if _, err := c.Exec(ctx, "INSERT INTO "+leaseTable+" DEFAULT VALUES"); err != nil {
		return fmt.Errorf("the writer cannot commit after the lease was tightened: %w", err)
	}
	return nil
}

// writeAcross inserts one row at a time for window and returns how many
// committed; the first failure is the verdict, since a lease lapse surfaces as
// a refused write.
func writeAcross(ctx context.Context, c sqlConn, window time.Duration) (int64, error) {
	deadline := time.Now().Add(window)
	var n int64
	for time.Now().Before(deadline) {
		if _, err := c.Exec(ctx, "INSERT INTO "+leaseTable+" DEFAULT VALUES"); err != nil {
			return n, fmt.Errorf("insert %d failed %s into the window: the writer lost its lease: %w", n+1, window-time.Until(deadline), err)
		}
		n++
		select {
		case <-ctx.Done():
			return n, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return n, nil
}

func numberOf(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	}
	return -1
}

// findBranchByID reads the cluster's default branch back through the list.
func findBranchByID(ctx context.Context, cl ampCtl, proj ampCluster) (controller.Branch, error) {
	const page = 100
	for offset := 0; ; offset += page {
		list, err := cl.ListBranches(ctx, proj.poolID, proj.projID, page, offset)
		if err != nil {
			return controller.Branch{}, fmt.Errorf("list branches: %w", err)
		}
		for _, b := range list.Branches {
			if b.BranchID == proj.branchID {
				return b, nil
			}
		}
		if list.Count < page || offset+len(list.Branches) >= list.Total {
			return controller.Branch{}, fmt.Errorf("branch %s not in the list", proj.branchID)
		}
	}
}
