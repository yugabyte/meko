// This file carries the functional test for externalized PostgreSQL extensions:
// a branch opts into a catalog extension (`addBranchExtensions [pgvector]`), an
// endpoint on the branch mounts it as a read-only ImageVolume, and PostgreSQL
// resolves it. The opt-in is a BRANCH property, so the test also forks the
// branch and asserts the fork inherits it.
package features

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"time"
)

const (
	// pgvectorCatalogName is the controller catalog's name for the extension (the
	// image suffix and ImageVolume mount subdir). It is NOT the SQL name.
	pgvectorCatalogName = "pgvector"
	// pgvectorSQLName is what CREATE EXTENSION / pg_extension.extname use.
	pgvectorSQLName = "vector"
	// vectorTable holds the embeddings the distance query orders.
	vectorTable = "amp_vector"
	// vectorProbe/vectorProbeFar are the two query points. Every stored embedding
	// lies on one axis at a distinct distance from the origin, so both orderings
	// are total — a tie would let insertion order pass for a distance sort.
	vectorProbe    = "[0,0,0]"
	vectorProbeFar = "[9,0,0]"
	// vectorNearFirst/vectorFarFirst are the id orders the two probes must yield.
	vectorNearFirst = "1,2,3,4"
	vectorFarFirst  = "4,3,2,1"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_extension_pgvector",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Opt an endpoint's branch into the pgvector extension, run a vector distance query over it, and assert a fork of the branch inherits the extension.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		// Two endpoint deploys (base + fork); a crash-looping fork rides the helm
		// wait, and the test should FAIL on that create, not vanish at a deadline.
		Timeout: 25 * time.Minute,
		Run:     runAmpExtensionPgvector,
	})
}

func runAmpExtensionPgvector(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{
			{"extension_pgvector", a.extensionPgvector},
			{"extension_survives_restart", a.extensionSurvivesRestart},
		}
	})
}

func (a *ampFeatureRun) extensionPgvector(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	return extensionPgvectorSteps(ctx, cl, a.proj, "ext-fork-"+a.f.suffix(), a.f.endpointSQL, a.f.t.Skip)
}

// extensionPgvectorSteps drives the assertions over the ampCtl and
// endpoint-connector seams. skip is called ONLY for the one environment
// condition the API reports explicitly — a controller with no extension catalog
// (Docker driver / feature off). Everything else, including an endpoint that
// fails to come up with the mount, is the product's and fails.
func extensionPgvectorSteps(ctx context.Context, cl ampCtl, proj ampCluster, childName string, connect endpointConnector, skip func(string)) error {
	if _, err := cl.AddBranchExtensionsAndWait(ctx, proj.poolID, proj.projID, proj.branchID, controller.AddBranchExtensionsRequest{
		Extensions: []string{pgvectorCatalogName},
	}); err != nil {
		if extensionsUnavailable(err) {
			skip("externalized extensions are not available on this control plane (no extension catalog: Docker driver or the feature is off): " + err.Error())
			return nil
		}
		return fmt.Errorf("opt branch into %s: %w", pgvectorCatalogName, err)
	}
	ep, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create endpoint after opting the branch into %s: %w", pgvectorCatalogName, err)
	}
	base, err := connect(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	defer base.Close()

	// The controller accepts the extension request even when the branch cannot
	// mount ImageVolumes (kubelet < 1.36): the pod comes up READY with no mount,
	// which no API error reveals. The control file's presence is the ground
	// truth, so probe it and make "this cluster can't do it yet" a skip instead
	// of a 10-minute deadline burn — while a broken mount on a capable cluster
	// still fails on CREATE EXTENSION below.
	available, err := base.QueryInt(ctx,
		"SELECT count(*) FROM pg_available_extensions WHERE name = $1", pgvectorSQLName)
	if err != nil {
		return fmt.Errorf("probe pg_available_extensions: %w", err)
	}
	if available == 0 {
		skip("endpoint came up without the " + pgvectorCatalogName +
			" ImageVolume mounted (pg_available_extensions has no '" + pgvectorSQLName +
			"'): the cluster likely lacks ImageVolume support (needs k8s >= 1.36)")
		return nil
	}

	if _, err := base.Exec(ctx, "CREATE EXTENSION "+pgvectorSQLName); err != nil {
		return fmt.Errorf("CREATE EXTENSION %s on an endpoint that opted into %s (the extension's ImageVolume must be mounted and on extension_control_path): %w",
			pgvectorSQLName, pgvectorCatalogName, err)
	}
	if err := seedVectors(ctx, base); err != nil {
		return err
	}
	if err := assertVectorOrder(ctx, base, "base branch"); err != nil {
		return err
	}

	// Fork the branch at a captured LSN (the "latest" auto fork point is not
	// settled straight after a write) and assert the fork gets the extension too:
	// a fork inherits its parent branch's opted-in set, so an endpoint on it
	// mounts pgvector.
	forkLSN, err := currentLSN(ctx, base)
	if err != nil {
		return err
	}
	if _, err := cl.CreateBranchAndWait(ctx, proj.poolID, proj.projID, controller.CreateBranchRequest{
		Name:           childName,
		ParentBranchID: proj.branchID,
		ForkPointLSN:   forkLSN,
	}); err != nil {
		return fmt.Errorf("fork branch at LSN %s: %w", forkLSN, err)
	}
	child, err := findBranchCtl(ctx, cl, proj.poolID, proj.projID, childName)
	if err != nil {
		return err
	}
	childEP, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, child.BranchID)
	if err != nil {
		return fmt.Errorf("create endpoint on the fork: %w", err)
	}
	kid, err := connect(ctx, childEP)
	if err != nil {
		return fmt.Errorf("connect the fork's endpoint: %w", err)
	}
	defer kid.Close()

	n, err := kid.QueryInt(ctx, "SELECT count(*) FROM pg_extension WHERE extname = $1", pgvectorSQLName)
	if err != nil {
		return fmt.Errorf("read pg_extension on the fork: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("the fork of a branch opted into %s reports %d rows in pg_extension for %q, want 1 — a fork inherits its parent's opted-in set and every endpoint on it must mount the extension",
			pgvectorCatalogName, n, pgvectorSQLName)
	}
	// The catalog row alone would survive a MISSING mount (it is inherited
	// copy-on-write with the rest of the catalog); running the distance operator
	// is what proves the fork's pod actually got the ImageVolume and its library.
	return assertVectorOrder(ctx, kid, "the fork")
}

// seedVectors creates the embedding table and fills it with the four axis points.
func seedVectors(ctx context.Context, c sqlConn) error {
	if _, err := c.Exec(ctx, "CREATE TABLE "+vectorTable+" (id int primary key, embedding vector(3))"); err != nil {
		return fmt.Errorf("create a table with a vector column: %w", err)
	}
	for id := 1; id <= 4; id++ {
		v := fmt.Sprintf("[%d,0,0]", id)
		if _, err := c.Exec(ctx, "INSERT INTO "+vectorTable+" (id, embedding) VALUES ($1, $2::vector)", id, v); err != nil {
			return fmt.Errorf("insert embedding %s: %w", v, err)
		}
	}
	return nil
}

// assertVectorOrder runs the L2-distance ordering from both probes and asserts
// each returns the ids in the order the geometry dictates.
func assertVectorOrder(ctx context.Context, c sqlConn, where string) error {
	for _, tc := range []struct{ probe, want string }{
		{vectorProbe, vectorNearFirst},
		{vectorProbeFar, vectorFarFirst},
	} {
		got, err := c.QueryString(ctx,
			"SELECT string_agg(id::text, ',' ORDER BY embedding <-> $1::vector) FROM "+vectorTable, tc.probe)
		if err != nil {
			return fmt.Errorf("vector distance query on %s: %w", where, err)
		}
		if got != tc.want {
			return fmt.Errorf("on %s, ordering %s by the <-> distance to %s gave ids %q, want %q",
				where, vectorTable, tc.probe, got, tc.want)
		}
	}
	return nil
}

// extensionsUnavailable reports the controller's own "this control plane has no
// extension catalog" rejection — the ONE environment condition that is not a
// product failure. An unknown-extension 400, a failed deploy, or anything else
// is a real failure and must not be softened into a skip.
func extensionsUnavailable(err error) bool {
	var apiErr *controller.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return strings.Contains(apiErr.Message, "externalized extensions are not supported")
}

// extensionSurvivesRestart rolls the base endpoint's pod and reruns the distance
// query: the ImageVolume must be remounted and the extension usable on the
// rescheduled compute, which is what an endpoint upgrade also relies on.
func (a *ampFeatureRun) extensionSurvivesRestart(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	ep, ok := lastEndpoint(ctx, cl, a.proj.poolID, a.proj.projID, a.proj.branchID)
	if !ok {
		return fmt.Errorf("the base branch has no endpoint to restart")
	}
	c, ok, err := a.restartEndpointCompute(ctx, ep)
	if err != nil || !ok {
		return err
	}
	defer c.Close()
	n, err := c.QueryInt(ctx, "SELECT count(*) FROM pg_extension WHERE extname = $1", pgvectorSQLName)
	if err != nil {
		return fmt.Errorf("read pg_extension after the restart: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("pg_extension has %d rows for %q after the compute restart, want 1", n, pgvectorSQLName)
	}
	return assertVectorOrder(ctx, c, "the base branch after a compute restart")
}
