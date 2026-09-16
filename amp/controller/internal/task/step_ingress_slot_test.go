package task

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/ingress"
)

// fakeIngress records what the step asked for. Allocate is idempotent, as the
// real allocator is — that contract is what the step relies on for replay, so a
// fake that handed out a second slot would hide the bug it exists to catch.
type fakeIngress struct {
	held      map[uuid.UUID]ingress.Allocation
	allocs    int
	released  []uuid.UUID
	allocErr  error
	nextPort  int
	lastFamly ingress.Family
}

func newFakeIngress() *fakeIngress {
	return &fakeIngress{held: map[uuid.UUID]ingress.Allocation{}, nextPort: 20000}
}

func (f *fakeIngress) Allocate(_ context.Context, id uuid.UUID, family ingress.Family) (ingress.Allocation, error) {
	f.lastFamly = family
	if f.allocErr != nil {
		return ingress.Allocation{}, f.allocErr
	}
	if a, ok := f.held[id]; ok {
		return a, nil
	}
	f.allocs++
	a := ingress.Allocation{
		EndpointID: id, Shard: "pgin-0", Port: f.nextPort,
		DNSName:   "ep-" + id.String()[:8] + ".pg.example",
		ShardHost: "lb.example", RecordType: "CNAME",
	}
	f.nextPort++
	f.held[id] = a
	return a, nil
}

func (f *fakeIngress) Release(_ context.Context, id uuid.UUID) error {
	f.released = append(f.released, id)
	delete(f.held, id)
	return nil
}

// lastCreated is the spec the deployer was last asked to create.
func lastCreated(t *testing.T, d *fakeDeployer) deploy.EndpointSpec {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.created) == 0 {
		t.Fatal("the deployer was never asked to create an endpoint")
	}
	return d.created[len(d.created)-1]
}

func ingressDeps(state StateStore, recipes *fakeRecipeStore, dep *fakeDeployer, alloc IngressAllocator) Deps {
	d := endpointDeps(state, recipes, dep)
	d.Ingress = alloc
	return d
}

// The slot has to reach the deployer: without it the release renders no route
// and the endpoint answers nowhere, while the API has already reported the mode.
func TestCreateEndpointStepPutsTheSlotOnTheSpec(t *testing.T) {
	state, recipes, dep := newFakeStateStore(), newFakeRecipeStore(), &fakeDeployer{}
	alloc := newFakeIngress()
	epID, clusterID, branchID := uuid.New(), uuid.New(), uuid.New()
	args := CreateEndpointArgs{
		EndpointID: epID, ClusterID: clusterID, BranchID: branchID,
		Role: "read_write", PgVersion: 18,
		Connectivity: deploy.ConnectivityIngressInternal,
	}
	if res := NewCreateEndpointStep(args, ingressDeps(state, recipes, dep, alloc)).Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if lastCreated(t, dep).Ingress.Port != 20000 || lastCreated(t, dep).Ingress.Shard != "pgin-0" {
		t.Errorf("spec.Ingress = %+v, want the allocated slot", lastCreated(t, dep).Ingress)
	}
	if lastCreated(t, dep).Ingress.ShardHost != "lb.example" {
		t.Errorf("spec.Ingress.ShardHost = %q, want the shard's address carried through",
			lastCreated(t, dep).Ingress.ShardHost)
	}
	if alloc.lastFamly != ingress.FamilyInternal {
		t.Errorf("family = %q, want internal for ingress_internal", alloc.lastFamly)
	}
}

// ingress_external must not land on an internal shard: the families are separate
// load balancers, so the wrong one is the wrong exposure.
func TestCreateEndpointStepPicksFamilyFromMode(t *testing.T) {
	state, recipes, dep := newFakeStateStore(), newFakeRecipeStore(), &fakeDeployer{}
	alloc := newFakeIngress()
	args := CreateEndpointArgs{
		EndpointID: uuid.New(), ClusterID: uuid.New(), BranchID: uuid.New(),
		Role: "read_write", PgVersion: 18,
		Connectivity: deploy.ConnectivityIngressExternal,
	}
	if res := NewCreateEndpointStep(args, ingressDeps(state, recipes, dep, alloc)).Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if alloc.lastFamly != ingress.FamilyExternal {
		t.Errorf("family = %q, want external for ingress_external", alloc.lastFamly)
	}
}

// A step is the unit of re-run, so the second execution must redeploy the SAME
// address. A second slot would leave the first held until its quarantine expired
// and change an address a client may already be using.
func TestCreateEndpointStepReplayKeepsTheSameSlot(t *testing.T) {
	state, recipes, dep := newFakeStateStore(), newFakeRecipeStore(), &fakeDeployer{}
	alloc := newFakeIngress()
	epID := uuid.New()
	args := CreateEndpointArgs{
		EndpointID: epID, ClusterID: uuid.New(), BranchID: uuid.New(),
		Role: "read_write", PgVersion: 18,
		Connectivity: deploy.ConnectivityIngressInternal,
	}
	deps := ingressDeps(state, recipes, dep, alloc)
	if res := NewCreateEndpointStep(args, deps).Execute(t.Context()); res.Failed() {
		t.Fatalf("first Execute: %v", res.Err)
	}
	first := lastCreated(t, dep).Ingress
	if res := NewCreateEndpointStep(args, deps).Execute(t.Context()); res.Failed() {
		t.Fatalf("replay Execute: %v", res.Err)
	}
	if lastCreated(t, dep).Ingress != first {
		t.Errorf("replay deployed %+v, want the original %+v", lastCreated(t, dep).Ingress, first)
	}
	if alloc.allocs != 1 {
		t.Errorf("allocations = %d, want 1: the replay issued a second address", alloc.allocs)
	}
}

// The ingress modes are refused at the API, but a controller reconfigured after
// a task was queued can still reach the step with no allocator. Failing beats
// deploying an endpoint that answers nowhere while reporting an ingress mode.
func TestCreateEndpointStepFailsWithoutAnAllocator(t *testing.T) {
	state, recipes, dep := newFakeStateStore(), newFakeRecipeStore(), &fakeDeployer{}
	args := CreateEndpointArgs{
		EndpointID: uuid.New(), ClusterID: uuid.New(), BranchID: uuid.New(),
		Role: "read_write", PgVersion: 18,
		Connectivity: deploy.ConnectivityIngressInternal,
	}
	res := NewCreateEndpointStep(args, endpointDeps(state, recipes, dep)).Execute(t.Context())
	if !res.Failed() {
		t.Fatal("a step with no allocator must fail rather than deploy an unreachable endpoint")
	}
	// A non-ingress mode on the same controller is unaffected.
	plain := CreateEndpointArgs{
		EndpointID: uuid.New(), ClusterID: uuid.New(), BranchID: uuid.New(),
		Role: "read_write", PgVersion: 18,
		Connectivity: deploy.ConnectivityClusterIP,
	}
	if res := NewCreateEndpointStep(plain, endpointDeps(state, recipes, dep)).Execute(t.Context()); res.Failed() {
		t.Errorf("cluster_ip must not need an allocator, got %v", res.Err)
	}
}

// An allocation failure must fail the step. Deploying anyway would give the
// endpoint the in-cluster address while its recorded mode says otherwise.
func TestCreateEndpointStepFailsWhenAllocationFails(t *testing.T) {
	state, recipes, dep := newFakeStateStore(), newFakeRecipeStore(), &fakeDeployer{}
	alloc := newFakeIngress()
	alloc.allocErr = ingress.ErrNoCapacity
	args := CreateEndpointArgs{
		EndpointID: uuid.New(), ClusterID: uuid.New(), BranchID: uuid.New(),
		Role: "read_write", PgVersion: 18,
		Connectivity: deploy.ConnectivityIngressInternal,
	}
	res := NewCreateEndpointStep(args, ingressDeps(state, recipes, dep, alloc)).Execute(t.Context())
	if !res.Failed() {
		t.Fatal("a failed allocation must fail the step")
	}
	if !errors.Is(res.Err, ingress.ErrNoCapacity) {
		t.Errorf("err = %v, want it to carry ErrNoCapacity so the cause survives", res.Err)
	}
}

// Release is unconditional and idempotent, so retiring an endpoint that never
// had a slot — a cluster_ip endpoint, or a row predating ingress — is fine, and
// nothing can strand a slot when a mode was changed.
func TestDeleteEndpointStepReleasesTheSlot(t *testing.T) {
	state, dep := newFakeStateStore(), &fakeDeployer{}
	alloc := newFakeIngress()
	epID := uuid.New()
	alloc.held[epID] = ingress.Allocation{EndpointID: epID, Shard: "pgin-0", Port: 20000}

	deps := Deps{State: state, Deployer: dep, Ingress: alloc}
	if res := NewDeleteEndpointStep(DeleteEndpointArgs{EndpointID: epID}, deps).Execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if len(alloc.released) != 1 || alloc.released[0] != epID {
		t.Errorf("released = %v, want exactly %s", alloc.released, epID)
	}
	// An endpoint with no allocation releases cleanly too.
	other := uuid.New()
	if res := NewDeleteEndpointStep(DeleteEndpointArgs{EndpointID: other}, deps).Execute(t.Context()); res.Failed() {
		t.Errorf("releasing an endpoint with no slot must be a no-op, got %v", res.Err)
	}
}

// A cluster or branch teardown retires endpoints too, and each one may hold a
// slot. Skipping the release there strands it for good: endpoints are
// hard-deleted, the allocation has no foreign key to follow, and the reclaim
// sweep only ages rows that were released — so the (shard, port) and the name
// are held with nothing left to find them by.
func TestDeleteCascadesReleaseEveryEndpointSlot(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(deps Deps, ids []uuid.UUID) StepResult
	}{
		{"delete_cluster", func(deps Deps, _ []uuid.UUID) StepResult {
			return NewDeleteClusterStep(DeleteClusterArgs{ClusterID: clusterForCascade}, deps).Execute(context.Background())
		}},
		{"delete_branch", func(deps Deps, _ []uuid.UUID) StepResult {
			return NewDeleteBranchStep(DeleteBranchArgs{ClusterID: clusterForCascade, BranchID: branchForCascade}, deps).Execute(context.Background())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, dep := newFakeStateStore(), &fakeDeployer{}
			alloc := newFakeIngress()
			ids := seedCascade(t, state, alloc)
			res := tc.run(Deps{State: state, Deployer: dep, Ingress: alloc}, ids)
			if res.Failed() {
				t.Fatalf("Execute: %v", res.Err)
			}
			for _, id := range ids {
				if _, held := alloc.held[id]; held {
					t.Errorf("endpoint %s still holds a slot after %s; it is stranded for good "+
						"because the endpoint row is gone and the sweep only sees released rows", id, tc.name)
				}
			}
			if len(alloc.released) != len(ids) {
				t.Errorf("released %d slots, want %d — one per retired endpoint", len(alloc.released), len(ids))
			}
		})
	}
}

var (
	clusterForCascade = uuid.New()
	branchForCascade  = uuid.New()
)

// seedCascade puts two endpoints on one branch of one cluster, both holding a
// slot, so either cascade enumerates them.
func seedCascade(t *testing.T, f *fakeStateStore, alloc *fakeIngress) []uuid.UUID {
	t.Helper()
	var ids []uuid.UUID
	for i := 0; i < 2; i++ {
		id := uuid.New()
		f.mu.Lock()
		f.endpointBranch[id] = core.EndpointDeploy{ID: id, BranchID: branchForCascade}
		f.endpointCluster[id] = clusterForCascade
		f.mu.Unlock()
		alloc.held[id] = ingress.Allocation{EndpointID: id, Shard: "pgin-0", Port: 20000 + i}
		ids = append(ids, id)
	}
	return ids
}
