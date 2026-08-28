// This file carries the functional test for pausing and resuming an endpoint's
// compute: stop paying for an idle compute without destroying the endpoint
// record, its branch, or any storage, then bring it back on the same branch. It
// is the missing middle between "running" and DELETE.
package features

import (
	"context"
	"fmt"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	// pauseTable is the dataset written before the pause and re-read after the
	// resume.
	pauseTable = "amp_pause"
	// pauseRows is the row count seeded before the pause.
	pauseRows = 50
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_endpoint_pause_resume",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Pause an endpoint and assert its compute stops while the record, branch and data survive, then resume it on the same branch and assert it serves the prior data and new writes.",
		Skip:        "blocked on yugabyte/meko#518: the controller has no pause/resume routes, so the pause POST 404s. meko#524 carried the implementation and was closed unmerged, leaving the surface unowned",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpEndpointPauseResume,
	})
}

func runAmpEndpointPauseResume(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"endpoint_pause_resume", a.endpointPauseResume}}
	})
}

// endpointPauseResume asserts what pause/resume promises: pause is a COMPUTE
// operation only (the endpoint record, its branch and its storage are untouched,
// and the branch's single-writer slot stays taken), and resume brings the same
// branch back — re-resolving the address, since a resumed endpoint may return on
// a different pod IP — serving the data written before the pause plus new writes.
func (a *ampFeatureRun) endpointPauseResume(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	ep, err := createEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID)
	if err != nil {
		return fmt.Errorf("create endpoint: %w", err)
	}

	conn, err := a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	if _, err := conn.Exec(ctx, "CREATE TABLE "+pauseTable+" (id int primary key)"); err != nil {
		conn.Close()
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := conn.Exec(ctx, "INSERT INTO "+pauseTable+" (id) SELECT generate_series(1, $1)", pauseRows); err != nil {
		conn.Close()
		return fmt.Errorf("seed rows: %w", err)
	}
	conn.Close()

	if _, err := cl.PauseEndpointAndWait(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, ep.ID); err != nil {
		return fmt.Errorf("pause endpoint %s: %w", ep.ID, err)
	}

	// The compute is gone...
	if err := a.f.endpointConnectRefused(ctx, ep); err == nil {
		return fmt.Errorf("a connection to paused endpoint %s succeeded, want it refused — pause must stop the compute", ep.ID)
	}
	// ...but the record, the branch and the single-writer slot are not.
	if _, err := cl.GetEndpoint(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, ep.ID); err != nil {
		return fmt.Errorf("paused endpoint %s is not readable, want its record intact (pause is not a soft delete): %w", ep.ID, err)
	}
	eps, err := cl.ListAllEndpoints(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID)
	if err != nil {
		return fmt.Errorf("list endpoints while paused: %w", err)
	}
	if !containsEndpoint(eps, ep.ID) {
		return fmt.Errorf("paused endpoint %s is no longer listed on its branch, want it listed", ep.ID)
	}
	branches, err := cl.ListBranches(ctx, a.proj.poolID, a.proj.projID, 100, 0)
	if err != nil {
		return fmt.Errorf("list branches while paused: %w", err)
	}
	if !containsBranch(branches.Branches, a.proj.branchID) {
		return fmt.Errorf("the branch of paused endpoint %s is gone, want the branch and its storage untouched", ep.ID)
	}
	if _, err := cl.CreateEndpointAndWait(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID,
		controller.CreateEndpointRequest{Role: roleReadWrite}); err == nil {
		return fmt.Errorf("a second read_write endpoint was created on the branch while endpoint %s was paused, want it rejected — the single-writer slot must stay taken or resume becomes impossible", ep.ID)
	}

	if _, err := cl.ResumeEndpointAndWait(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, ep.ID); err != nil {
		return fmt.Errorf("resume endpoint %s: %w", ep.ID, err)
	}

	// A resumed endpoint may come back on a different address, so re-read it.
	resumed, err := cl.GetEndpoint(ctx, a.proj.poolID, a.proj.projID, a.proj.branchID, ep.ID)
	if err != nil {
		return fmt.Errorf("read resumed endpoint %s: %w", ep.ID, err)
	}
	if resumed.BranchID != ep.BranchID {
		return fmt.Errorf("resumed endpoint %s reports branch %s, want the branch it was paused on (%s)", ep.ID, resumed.BranchID, ep.BranchID)
	}
	back, err := a.f.endpointSQL(ctx, resumed)
	if err != nil {
		return fmt.Errorf("connect resumed endpoint %s: %w", ep.ID, err)
	}
	defer back.Close()

	n, err := back.QueryInt(ctx, "SELECT count(*) FROM "+pauseTable)
	if err != nil {
		return fmt.Errorf("read the pre-pause dataset after resume: %w", err)
	}
	if n != pauseRows {
		return fmt.Errorf("after resume the endpoint serves %d rows of %s, want the %d written before the pause — pause must not touch storage", n, pauseTable, pauseRows)
	}
	if _, err := back.Exec(ctx, "INSERT INTO "+pauseTable+" (id) VALUES ($1)", pauseRows+1); err != nil {
		return fmt.Errorf("a resumed endpoint must accept writes on its branch: %w", err)
	}
	if n, err := back.QueryInt(ctx, "SELECT count(*) FROM "+pauseTable); err != nil {
		return fmt.Errorf("re-read after the post-resume write: %w", err)
	} else if n != pauseRows+1 {
		return fmt.Errorf("after the post-resume write %s has %d rows, want %d", pauseTable, n, pauseRows+1)
	}
	return nil
}

func containsEndpoint(eps []controller.Endpoint, id string) bool {
	for _, e := range eps {
		if e.ID == id {
			return true
		}
	}
	return false
}

func containsBranch(branches []controller.Branch, id string) bool {
	for _, b := range branches {
		if b.BranchID == id {
			return true
		}
	}
	return false
}
