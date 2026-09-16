package task

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/extensions"
)

// TestAddBranchExtensionsStepUnionsAndRedeploys covers the happy path: the step
// unions the requested names into the branch's set and redeploys the endpoints that are behind it
// on the branch with the resolved (defaults ∪ opted-in) mount set.
func TestAddBranchExtensionsStepUnionsAndRedeploys(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	deps := endpointDeps(state, nil, dep)
	deps.Extensions = extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)

	clusterID, branchID := uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID, "pgvector")
	ep1, ep2 := uuid.New(), uuid.New()
	state.seedEndpoint(ep1, clusterID, branchID)
	state.seedEndpoint(ep2, clusterID, branchID)

	step := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pg_cron"}}, deps)
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}

	// The branch set is the union of the existing and the added names, deduped.
	got, _ := state.BranchExtensions(t.Context(), clusterID, branchID)
	set := map[string]bool{}
	for _, n := range got {
		set[n] = true
	}
	if len(got) != 2 || !set["pg_cron"] || !set["pgvector"] {
		t.Fatalf("branch extensions = %v, want {pg_cron, pgvector}", got)
	}
	// Both endpoints were redeployed, each mounting both extensions.
	if len(dep.created) != 2 {
		t.Fatalf("deploy calls = %d, want 2 (one per endpoint)", len(dep.created))
	}
	for _, spec := range dep.created {
		if len(spec.Extensions) != 2 {
			t.Fatalf("redeploy spec extensions = %+v, want both pg_cron and pgvector", spec.Extensions)
		}
	}
	// The audit set was recorded on each endpoint.
	if len(state.endpointExts[ep1]) != 2 || len(state.endpointExts[ep2]) != 2 {
		t.Fatalf("recorded endpoint extensions = %v / %v, want 2 each", state.endpointExts[ep1], state.endpointExts[ep2])
	}
}

// A redeploy re-renders each endpoint's whole spec, so every value it does not
// carry across from that endpoint's row is silently replaced by a default: the
// pod comes back on the deployer's default major version or resized to the
// chart's 2 cores/4Gi. The two endpoints here differ in both, so a spec assembled
// from anything but its own record fails.
func TestAddBranchExtensionsStepRedeploysEachEndpointAsItIs(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	deps := endpointDeps(state, nil, dep)
	deps.Extensions = extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)

	clusterID, branchID := uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID)
	big, small := uuid.New(), uuid.New()
	state.seedEndpointDeploy(clusterID, core.EndpointDeploy{ID: big, BranchID: branchID, PgVersion: 15, CPU: "8", Memory: "16Gi"})
	state.seedEndpointDeploy(clusterID, core.EndpointDeploy{ID: small, BranchID: branchID, PgVersion: 16, CPU: "1", Memory: "2Gi"})

	step := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pgvector"}}, deps)
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}

	deployed := map[string]deploy.EndpointSpec{}
	for _, spec := range dep.created {
		deployed[spec.EndpointID] = spec
	}
	if len(deployed) != 2 {
		t.Fatalf("distinct endpoints redeployed = %d, want 2", len(deployed))
	}
	for _, want := range []deploy.EndpointSpec{
		{EndpointID: big.String(), PgVersion: 15, CPU: "8", Memory: "16Gi"},
		{EndpointID: small.String(), PgVersion: 16, CPU: "1", Memory: "2Gi"},
	} {
		got := deployed[want.EndpointID]
		if got.PgVersion != want.PgVersion || got.CPU != want.CPU || got.Memory != want.Memory {
			t.Errorf("endpoint %s redeployed as pg%d %s/%s, want pg%d %s/%s (its own row's desired state)",
				want.EndpointID, got.PgVersion, got.CPU, got.Memory, want.PgVersion, want.CPU, want.Memory)
		}
		// The newly added extension is mounted, which the read only returns when the
		// endpoint's branch id came through on its record.
		if len(got.Extensions) != 1 || got.Extensions[0].Name != "pgvector" {
			t.Errorf("endpoint %s redeployed with extensions %+v, want the branch's pgvector", want.EndpointID, got.Extensions)
		}
	}
}

// One read of the branch's set for the whole roll. Reading per endpoint instead
// would let a concurrent add give the later endpoints a larger set than the
// earlier ones, and would let a read that fails partway abort with endpoints
// already redeployed — which the store here forces by failing every read after
// the first.
func TestAddBranchExtensionsStepReadsTheBranchSetOnce(t *testing.T) {
	state := &recordingExtStore{fakeStateStore: newFakeStateStore(), err: errors.New("boom"), okReads: 1}
	dep := &fakeDeployer{}
	deps := endpointDeps(state, nil, dep)
	deps.Extensions = extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)

	clusterID, branchID := uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID, "pgvector")
	for range 3 {
		state.seedEndpoint(uuid.New(), clusterID, branchID)
	}

	step := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pg_cron"}}, deps)
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if state.calls != 1 {
		t.Errorf("BranchExtensions reads = %d for a 3-endpoint branch, want 1", state.calls)
	}
	if len(dep.created) != 3 {
		t.Fatalf("endpoints redeployed = %d, want 3", len(dep.created))
	}
	for _, spec := range dep.created {
		if len(spec.Extensions) != 2 {
			t.Errorf("endpoint %s redeployed with %+v, want the same 2-extension snapshot as every other", spec.EndpointID, spec.Extensions)
		}
	}
}

// TestAddBranchExtensionsStepRecordsTheDeployedVersion: the redeploy is a real
// deploy, so the deployer answers with the tag it ran — and a NULL version means
// unknown (§2). Recording it is what makes one pass move an endpoint from unknown
// to known instead of leaving it unknown through every extension change. Guarded:
// an empty report is no information (the local driver places nothing) and must
// not overwrite the row.
func TestAddBranchExtensionsStepRecordsTheDeployedVersion(t *testing.T) {
	state := newFakeStateStore()
	dep := &fakeDeployer{reportVersion: "ep-2026.08.1"}
	deps := endpointDeps(state, nil, dep)
	deps.Extensions = extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)

	clusterID, branchID := uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID)
	unknown := uuid.New()
	state.seedEndpointDeploy(clusterID, core.EndpointDeploy{ID: unknown, BranchID: branchID, PgVersion: 18})

	step := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pgvector"}}, deps)
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if got := state.endpointVersions[unknown]; got != "ep-2026.08.1" {
		t.Errorf("recorded version = %q, want the tag the deployer reported", got)
	}

	// A deployer that reports nothing leaves the column as it was: the fake echoes
	// the spec's version back, and this endpoint asks for none.
	silent := newFakeStateStore()
	quiet := endpointDeps(silent, nil, &fakeDeployer{})
	quiet.Extensions = extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)
	silent.seedBranchExtensions(clusterID, branchID)
	ep := uuid.New()
	silent.seedEndpointDeploy(clusterID, core.EndpointDeploy{ID: ep, BranchID: branchID, PgVersion: 18})

	step = NewAddBranchExtensionsStep(AddBranchExtensionsArgs{ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pgvector"}}, quiet)
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute (silent deployer): %v", res.Err)
	}
	if got, ok := silent.endpointVersions[ep]; ok {
		t.Errorf("recorded version = %q, want the column untouched by an empty report", got)
	}
}

// A branch set that cannot be read aborts the roll before the first endpoint is
// touched, rather than leaving the branch half redeployed.
func TestAddBranchExtensionsStepReadFailureDeploysNothing(t *testing.T) {
	state := &recordingExtStore{fakeStateStore: newFakeStateStore(), err: errors.New("boom")}
	dep := &fakeDeployer{}
	deps := endpointDeps(state, nil, dep)
	deps.Extensions = extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)

	clusterID, branchID := uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID, "pgvector")
	state.seedEndpoint(uuid.New(), clusterID, branchID)
	state.seedEndpoint(uuid.New(), clusterID, branchID)

	step := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pg_cron"}}, deps)
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with an unreadable branch set, want a failure")
	}
	if len(dep.created) != 0 {
		t.Errorf("endpoints redeployed = %d before the failed read aborted, want 0", len(dep.created))
	}
}

// TestAddBranchExtensionsStepRequiresDeployerAndCatalog: the step fails cleanly
// when either the deployer or the catalog is absent (no task is planned without
// them, but a replay must not run half-wired).
func TestAddBranchExtensionsStepRequiresDeployerAndCatalog(t *testing.T) {
	clusterID, branchID := uuid.New(), uuid.New()

	noDeployer := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pgvector"}},
		Deps{State: newFakeStateStore(), Extensions: extensions.NewService(extensions.NewCatalog("r"), nil)})
	if res := noDeployer.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with no deployer, want a failure")
	}

	noCatalog := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pgvector"}},
		Deps{State: newFakeStateStore(), Deployer: &fakeDeployer{}})
	if res := noCatalog.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with no catalog, want a failure")
	}
}

// The redeploy renders the release from core.EndpointDeploy alone, so it must
// carry the endpoint's exposure. Without it service.connectivity renders empty,
// service-lb.yaml's gate is false, helm prunes the -lb Service and the load
// balancer is deleted — silently reverting an nlb_* endpoint to cluster_ip and
// invalidating the address every client holds.
func TestAddBranchExtensionsStepPreservesConnectivity(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	deps := endpointDeps(state, nil, dep)
	deps.Extensions = extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)

	clusterID, branchID := uuid.New(), uuid.New()
	nlb, plain := uuid.New(), uuid.New()
	cidrs := []string{"10.0.0.0/16"}
	state.seedEndpointWithConnectivity(nlb, clusterID, branchID, "nlb_internal", cidrs)
	state.seedEndpoint(plain, clusterID, branchID)

	step := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{
		ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pg_cron"},
	}, deps)
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}

	var sawNLB bool
	for _, spec := range dep.created {
		switch spec.EndpointID {
		case nlb.String():
			sawNLB = true
			if spec.Connectivity != "nlb_internal" {
				t.Errorf("redeploy Connectivity = %q, want nlb_internal (an empty value deletes the load balancer)", spec.Connectivity)
			}
			if len(spec.AllowedCIDRs) != 1 || spec.AllowedCIDRs[0] != cidrs[0] {
				t.Errorf("redeploy AllowedCIDRs = %v, want %v", spec.AllowedCIDRs, cidrs)
			}
		case plain.String():
			// A cluster_ip endpoint must stay unexposed, not inherit its neighbour's.
			if spec.Connectivity != "" {
				t.Errorf("cluster_ip endpoint redeployed with Connectivity = %q, want empty", spec.Connectivity)
			}
		}
	}
	if !sawNLB {
		t.Fatalf("the nlb_internal endpoint was never redeployed; specs = %+v", dep.created)
	}
}

// An endpoint already mounting the set is left alone. The step used to redeploy
// every endpoint on the branch unconditionally, because AddBranchExtensions
// reports only an error and never whether the union changed the row — so a task
// whose set was already applied still ran a helm upgrade per endpoint, rendering
// an identical manifest each time.
//
// The signal was already there: the step records what each endpoint mounts, and
// only after that endpoint's deploy returned. That makes it "last successfully
// deployed with", which is exactly the question here.
func TestAddBranchExtensionsStepSkipsEndpointsAlreadyCurrent(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	deps := endpointDeps(state, nil, dep)
	deps.Extensions = extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)

	clusterID, branchID := uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID, "pgvector")

	// current already mounts what this task will resolve to; behind does not.
	current, behind := uuid.New(), uuid.New()
	state.seedEndpointDeploy(clusterID, core.EndpointDeploy{
		ID: current, BranchID: branchID, Extensions: []string{"pg_cron", "pgvector"}})
	state.seedEndpointDeploy(clusterID, core.EndpointDeploy{
		ID: behind, BranchID: branchID, Extensions: []string{"pgvector"}})

	step := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{
		ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pg_cron"}}, deps)
	res := step.Execute(t.Context())
	if res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}

	if len(dep.created) != 1 {
		t.Fatalf("deploy calls = %d, want 1 — only the endpoint that is behind", len(dep.created))
	}
	if dep.created[0].EndpointID != behind.String() {
		t.Errorf("redeployed %s, want the endpoint that was behind (%s)", dep.created[0].EndpointID, behind)
	}
}

// Order is not membership. The recorded set is written through marshalExtensions,
// which sorts, while the resolved set comes out of the catalog in catalog order —
// so comparing the two as sequences would redeploy every endpoint on every task
// and quietly undo the skip above.
func TestAddBranchExtensionsStepIgnoresRecordedOrder(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	deps := endpointDeps(state, nil, dep)
	deps.Extensions = extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)

	clusterID, branchID := uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID, "pgvector")
	ep := uuid.New()
	state.seedEndpointDeploy(clusterID, core.EndpointDeploy{
		ID: ep, BranchID: branchID, Extensions: []string{"pgvector", "pg_cron"}})

	step := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{
		ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pg_cron"}}, deps)
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if len(dep.created) != 0 {
		t.Errorf("deploy calls = %d, want 0 — the same set in a different order is current", len(dep.created))
	}
}

// The recovery the agent used to force by re-sending the whole set, now the
// task's own: a run that unions the row and then fails partway leaves the
// endpoints it never reached unrecorded, so a replay redeploys exactly those.
//
// It matters because the union lands first. Once the row says the branch has the
// extension, nothing downstream would ever revisit an endpoint that never got it.
func TestAddBranchExtensionsStepReplayRedeploysOnlyWhatItMissed(t *testing.T) {
	state := newFakeStateStore()
	clusterID, branchID := uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID, "pgvector")
	reached, missed := uuid.New(), uuid.New()
	state.seedEndpointDeploy(clusterID, core.EndpointDeploy{ID: reached, BranchID: branchID})
	state.seedEndpointDeploy(clusterID, core.EndpointDeploy{ID: missed, BranchID: branchID})

	// First run: the deployer fails on the second endpoint it is handed.
	failing := &fakeDeployer{}
	failing.createErrAfter = 1
	deps := endpointDeps(state, nil, failing)
	deps.Extensions = extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)
	step := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{
		ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pg_cron"}}, deps)
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute: want the first run to fail partway")
	}
	if len(failing.created) != 2 {
		t.Fatalf("first run deploy attempts = %d, want 2 (one succeeded, one failed)", len(failing.created))
	}
	done := failing.created[0].EndpointID

	// Replay. The row already carries the union, so nothing else would re-drive it.
	replay := &fakeDeployer{}
	deps2 := endpointDeps(state, nil, replay)
	deps2.Extensions = extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)
	step2 := NewAddBranchExtensionsStep(AddBranchExtensionsArgs{
		ClusterID: clusterID, BranchID: branchID, Extensions: []string{"pg_cron"}}, deps2)
	if res := step2.Execute(t.Context()); res.Failed() {
		t.Fatalf("replay: %v", res.Err)
	}
	if len(replay.created) != 1 {
		t.Fatalf("replay deploy calls = %d, want 1 — only the endpoint the first run never reached", len(replay.created))
	}
	if replay.created[0].EndpointID == done {
		t.Errorf("replay redeployed %s, which the first run already did", done)
	}
}
