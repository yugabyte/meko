package features

import (
	"context"
	"fmt"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const singleWriterTable = "amp_single_writer"

func init() {
	core.Register(core.Test{
		Name:        "features/amp_single_writer",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Assert one_writer_per_branch: a second read_write endpoint on the same branch must fail to create, and the first keeps serving reads+writes intact.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpSingleWriter,
	})
}

func runAmpSingleWriter(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"single_writer", a.singleWriter}}
	})
}

func (a *ampFeatureRun) singleWriter(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	return singleWriterSteps(ctx, cl, a.proj, a.f.endpointSQL)
}

func singleWriterSteps(ctx context.Context, cl ampCtl, proj ampProject, connect endpointConnector) error {
	ep, err := createRoleEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID, roleReadWrite)
	if err != nil {
		return fmt.Errorf("create first read_write endpoint: %w", err)
	}
	c, err := connect(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect first endpoint: %w", err)
	}
	defer c.Close()

	if _, err := c.Exec(ctx, "CREATE TABLE "+singleWriterTable+" (id int primary key, val text)"); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO "+singleWriterTable+" (id, val) VALUES (1, 'marker')"); err != nil {
		return fmt.Errorf("write marker: %w", err)
	}

	// One writer per branch: the second read_write create must fail (no retry —
	// a retry would mask the guardrail).
	if _, err := cl.CreateEndpointAndWait(ctx, proj.poolID, proj.projID, proj.branchID, controller.CreateEndpointRequest{Role: roleReadWrite}); err == nil {
		return fmt.Errorf("creating a second read_write endpoint on branch %s succeeded, want it rejected (the one_writer_per_branch guardrail must reject a second writer)", proj.branchID)
	}

	// The first endpoint still serves reads and writes; the marker is intact.
	if n, err := c.QueryInt(ctx, "SELECT count(*) FROM "+singleWriterTable+" WHERE val = 'marker'"); err != nil {
		return fmt.Errorf("re-read marker: %w", err)
	} else if n != 1 {
		return fmt.Errorf("marker row on %s is damaged after the rejected second-writer create (count=%d, want 1) — the first endpoint's reads should be unaffected by the rejected create", singleWriterTable, n)
	}
	if _, err := c.Exec(ctx, "INSERT INTO "+singleWriterTable+" (id, val) VALUES (2, 'post-reject')"); err != nil {
		return fmt.Errorf("write after rejected create: %w", err)
	}
	if n, err := c.QueryInt(ctx, "SELECT count(*) FROM "+singleWriterTable); err != nil {
		return fmt.Errorf("final count: %w", err)
	} else if n != 2 {
		return fmt.Errorf("%s has %d rows after the marker write and the post-reject write, want 2 — the first endpoint should keep serving writes intact after the second-writer create was rejected", singleWriterTable, n)
	}
	return nil
}
