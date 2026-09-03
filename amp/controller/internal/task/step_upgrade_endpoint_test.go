package task

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/extensions"
)

// upgradeDeps is the deps an upgrade_endpoint test wants: the state store, the
// deployer, and a real extension catalog — the step re-renders the whole spec, so
// the mounts have to resolve the way they do in production.
func upgradeDeps(state StateStore, dep *fakeDeployer) Deps {
	d := endpointDeps(state, nil, dep)
	d.Extensions = extensions.NewCatalog("quay.io/yugabyte")
	return d
}

// The happy path, asserted on the spec the deployer *received*: a field missing
// from the assembled record is silent, so the fixture makes every one
// distinguishable from a zero value.
func TestUpgradeEndpointStepUpgradesAndRedeploys(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	epID, clusterID, branchID := uuid.New(), uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID, "pgvector")
	state.seedEndpointDeploy(clusterID, core.EndpointDeploy{
		ID: epID, BranchID: branchID,
		PgVersion: 15, CPU: "8", Memory: "16Gi", Version: "ep-v1",
	})
	// The address the endpoint is recorded at before the upgrade. The pod is
	// replaced, so this one is dead afterwards.
	state.endpointAddresses[epID] = "yb-ep-old.endpoints.svc:55433"

	step := NewUpgradeEndpointStep(UpgradeEndpointArgs{EndpointID: epID, Version: "ep-v2"}, upgradeDeps(state, dep))
	res := step.Execute(t.Context())
	if res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}

	// Converged: both columns at the target.
	if got := state.endpointTargetVersions[epID]; got != "ep-v2" {
		t.Errorf("recorded target version = %q, want ep-v2", got)
	}
	if got := state.endpointVersions[epID]; got != "ep-v2" {
		t.Errorf("recorded running version = %q, want ep-v2", got)
	}
	if len(dep.created) != 1 {
		t.Fatalf("deploy calls = %d, want 1", len(dep.created))
	}
	spec := dep.created[0]
	if spec.Version != "ep-v2" {
		t.Errorf("deployed spec version = %q, want ep-v2", spec.Version)
	}
	if spec.Name != EndpointDeployName(epID) || spec.EndpointID != epID.String() {
		t.Errorf("deployed spec identity = %q/%q, want %q/%s", spec.Name, spec.EndpointID, EndpointDeployName(epID), epID)
	}
	if spec.PgVersion != 15 || spec.CPU != "8" || spec.Memory != "16Gi" {
		t.Errorf("deployed spec = pg%d %s/%s, want pg15 8/16Gi (the endpoint's own desired state)", spec.PgVersion, spec.CPU, spec.Memory)
	}
	if len(spec.Extensions) != 1 || spec.Extensions[0].Name != "pgvector" {
		t.Errorf("deployed spec extensions = %+v, want the branch's pgvector", spec.Extensions)
	}
	wantAddr := EndpointDeployName(epID) + ".endpoints.svc:55433"
	if got := state.endpointAddresses[epID]; got != wantAddr {
		t.Errorf("recorded address = %q, want %q (a replaced pod leaves the old address dead)", got, wantAddr)
	}
	out := decodeResult(t, res.Output)
	if out["endpoint_id"] != epID.String() || out["version"] != "ep-v2" || out["address"] != wantAddr {
		t.Errorf("step result = %v, want the endpoint, ep-v2 and %s", out, wantAddr)
	}

	// Idempotent replay: the same state, not a double-apply or an error.
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("replay Execute: %v", res.Err)
	}
	if len(dep.created) != 2 || !reflect.DeepEqual(dep.created[1], spec) {
		t.Errorf("replay deployed %+v, want the same spec re-applied", dep.created[1:])
	}
	if got := state.endpointVersions[epID]; got != "ep-v2" {
		t.Errorf("recorded version after replay = %q, want ep-v2", got)
	}
	if got := state.endpointAddresses[epID]; got != wantAddr {
		t.Errorf("recorded address after replay = %q, want %q", got, wantAddr)
	}
}

// A failed deploy leaves the target recorded and the running version untouched:
// that divergence is what says the endpoint has not converged, and what makes a
// replay pick it up.
func TestUpgradeEndpointStepRecordsTheTargetBeforeDeploying(t *testing.T) {
	state := newFakeStateStore()
	dep := &fakeDeployer{createErr: errors.New("helm upgrade failed")}
	epID, clusterID, branchID := uuid.New(), uuid.New(), uuid.New()
	state.seedBranchExtensions(clusterID, branchID)
	state.seedEndpointDeploy(clusterID, core.EndpointDeploy{ID: epID, BranchID: branchID, PgVersion: 18, Version: "ep-v1"})
	state.endpointVersions = map[uuid.UUID]string{epID: "ep-v1"}

	step := NewUpgradeEndpointStep(UpgradeEndpointArgs{EndpointID: epID, Version: "ep-v2"}, upgradeDeps(state, dep))
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded, want the deploy error propagated")
	}
	if got := state.endpointTargetVersions[epID]; got != "ep-v2" {
		t.Errorf("recorded target version = %q, want ep-v2 before the deploy was attempted", got)
	}
	if got := state.endpointVersions[epID]; got != "ep-v1" {
		t.Errorf("recorded running version = %q, want ep-v1 — nothing deployed, so nothing runs the target", got)
	}
}

// The spec carries the version the step was ASKED for, not the one the record
// happens to carry: the write and the read are separate statements, modeled here
// by a store whose write does not feed back into the record it returns.
func TestUpgradeEndpointStepRendersTheTargetVersion(t *testing.T) {
	inner, dep := newFakeStateStore(), &fakeDeployer{}
	epID, clusterID, branchID := uuid.New(), uuid.New(), uuid.New()
	inner.seedBranchExtensions(clusterID, branchID)
	inner.seedEndpointDeploy(clusterID, core.EndpointDeploy{ID: epID, BranchID: branchID, PgVersion: 18, Version: "ep-v1"})

	step := NewUpgradeEndpointStep(UpgradeEndpointArgs{EndpointID: epID, Version: "ep-v2"}, upgradeDeps(staleVersionReadStore{inner}, dep))
	if res := step.Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if len(dep.created) != 1 {
		t.Fatalf("deploy calls = %d, want 1", len(dep.created))
	}
	if got := dep.created[0].Version; got != "ep-v2" {
		t.Errorf("deployed spec version = %q, want ep-v2 (the target, not the version on the record read back)", got)
	}
}

// staleVersionReadStore is a StateStore whose version write does not reach the
// record EndpointDeploy returns: a read that has not observed the write (a
// follower read, or a snapshot taken before it committed).
type staleVersionReadStore struct{ *fakeStateStore }

func (s staleVersionReadStore) SetEndpointVersion(_ context.Context, endpointID uuid.UUID, version string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.endpointVersions == nil {
		s.endpointVersions = map[uuid.UUID]string{}
	}
	s.endpointVersions[endpointID] = version
	return nil
}

func TestUpgradeEndpointStepRequiresDeployer(t *testing.T) {
	step := NewUpgradeEndpointStep(
		UpgradeEndpointArgs{EndpointID: uuid.New(), Version: "ep-v2"},
		Deps{State: newFakeStateStore()}, // no deployer
	)
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with no deployer configured, want a failure")
	}
}

// An endpoint that is gone has no desired state to render, so the step fails
// rather than deploying a default-everything pod under its id.
func TestUpgradeEndpointStepMissingEndpoint(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	step := NewUpgradeEndpointStep(UpgradeEndpointArgs{EndpointID: uuid.New(), Version: "ep-v2"}, upgradeDeps(state, dep))
	res := step.Execute(t.Context())
	if !res.Failed() {
		t.Fatal("Execute succeeded for an endpoint that does not exist, want a failure")
	}
	if !errors.Is(res.Err, core.ErrNotFound) {
		t.Errorf("error = %v, want core.ErrNotFound", res.Err)
	}
	if len(dep.created) != 0 {
		t.Errorf("deployed %+v for a missing endpoint, want nothing", dep.created)
	}
}

// An upgrade with no target version would clear the pinned desired state and hand
// the endpoint back to the controller's configured default, so it is refused
// rather than applied.
func TestUpgradeEndpointStepRequiresATargetVersion(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	epID, clusterID, branchID := uuid.New(), uuid.New(), uuid.New()
	state.seedEndpointDeploy(clusterID, core.EndpointDeploy{ID: epID, BranchID: branchID, PgVersion: 18, Version: "ep-v1"})

	step := NewUpgradeEndpointStep(UpgradeEndpointArgs{EndpointID: epID}, upgradeDeps(state, dep))
	if res := step.Execute(t.Context()); !res.Failed() {
		t.Fatal("Execute succeeded with no target version, want a failure")
	}
	if got, ok := state.endpointVersions[epID]; ok {
		t.Errorf("recorded version = %q, want the pinned version left alone", got)
	}
	if len(dep.created) != 0 {
		t.Errorf("deployed %+v with no target version, want nothing", dep.created)
	}
}

// The deployer, not the request, is the authority on what a version resolved to.
// Needs a deployer that reports something other than what it was asked for —
// every other fixture echoes the request back, which would make this
// indistinguishable from recording the target.
func TestUpgradeEndpointStepRecordsWhatTheDeployerReported(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{reportVersion: "ep-v2-resolved"}
	epID, clusterID, branchID := uuid.New(), uuid.New(), uuid.New()
	state.seedEndpointDeploy(clusterID, core.EndpointDeploy{
		ID: epID, BranchID: branchID,
		PgVersion: 15, CPU: "8", Memory: "16Gi", Version: "ep-v1",
	})

	step := NewUpgradeEndpointStep(UpgradeEndpointArgs{EndpointID: epID, Version: "ep-v2"}, upgradeDeps(state, dep))
	res := step.Execute(t.Context())
	if res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if got := state.endpointVersions[epID]; got != "ep-v2-resolved" {
		t.Errorf("recorded running version = %q, want ep-v2-resolved (what the deployer reported)", got)
	}
	if got := state.endpointTargetVersions[epID]; got != "ep-v2" {
		t.Errorf("recorded target version = %q, want ep-v2 (what was asked for)", got)
	}
	// The spec still carried the requested version: resolving it is the deployer's job.
	if got := dep.created[0].Version; got != "ep-v2" {
		t.Errorf("deployed spec version = %q, want ep-v2 (the target as asked)", got)
	}
}
