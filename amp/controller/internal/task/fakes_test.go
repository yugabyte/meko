package task

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
)

// Shared fakes for the task-package tests: the dependency *ports* (StateStore,
// RecipeStore, deploy.Deployer) and a configurable Step. They live in one file so
// the per-unit test files (steps_test.go, tasks_test.go, worker_test.go,
// registry_test.go) share one definition each. The in-memory Store itself is the
// real MemStore (mem.go) — usable by any package's tests — not a fake here.

// --- fakeStateStore: an in-memory task.StateStore ---------------------------

// fakeStateStore models the id-keyed idempotency of the real PgBackend Ensure*
// methods (INSERT ... ON CONFLICT (pk) DO NOTHING): a repeated id is a no-op,
// not a duplicate or an error. It counts calls separately from effective
// creates so a test can assert a replayed step converged (calls > creates).
// The err* fields inject failures for error-path tests.
type fakeStateStore struct {
	mu sync.Mutex

	ensureClusterCalls  int
	ensureBranchCalls   int
	endpointRecordCalls int
	setAddressCalls     int
	endpointNames       map[uuid.UUID]string
	branchPgSettings    map[uuid.UUID]map[string]string
	endpointPgSettings  map[uuid.UUID]string
	// The two daemon override documents, as encoded JSON, keyed by endpoint.
	endpointPageserverSettings map[uuid.UUID]string
	endpointSafekeeperSettings map[uuid.UUID]string
	deleteEndpointCalls        int
	createdBranches            map[uuid.UUID]struct{} // distinct branch ids created
	createdEndpoints           map[uuid.UUID]string   // endpoint id -> role
	endpointAddresses          map[uuid.UUID]string
	endpointResources          map[uuid.UUID][2]string // endpoint id -> {cpu, memory} last resized to
	// The applied YCP generation the worker stamped, per resource. Monotonic, like
	// the real GREATEST write, so a test can assert a late older task cannot regress it.
	endpointYCPGeneration map[uuid.UUID]int64
	branchYCPGeneration   map[uuid.UUID]int64
	deletedEndpoints      map[uuid.UUID]struct{}
	existingWriterBranch  map[uuid.UUID]uuid.UUID // branch id -> the writer endpoint id already present

	// Cascade-delete bookkeeping: where each (live) endpoint and branch sits, so
	// EndpointIDsForBranch / EndpointIDsForCluster can enumerate children and the
	// delete-by-id methods can prune them. Populated by seedEndpoint / seedBranch.
	endpointBranch  map[uuid.UUID]core.EndpointDeploy // endpoint id -> its renderable desired state
	endpointCluster map[uuid.UUID]uuid.UUID           // endpoint id -> the cluster its branch sits under
	branchCluster   map[uuid.UUID]uuid.UUID           // branch id -> its cluster
	branchParent    map[uuid.UUID]uuid.UUID           // branch id -> its parent branch id (forks only)
	branchExts      map[uuid.UUID][]string            // branch id -> opted-in extension names (per branch)
	endpointExts    map[uuid.UUID][]string            // endpoint id -> last-recorded resolved extension names
	deletedBranches map[uuid.UUID]struct{}
	deletedClusters map[uuid.UUID]struct{}

	// clusterEngine records the engine each EnsureCluster was given, so a task
	// test can assert the arg is carried rather than dropped.
	clusterEngine     map[uuid.UUID]string
	errEnsureCluster  error
	errEnsureBranch   error
	errEnsureEndpoint error
	errSetAddress     error
	errDeleteEndpoint error
	errDeleteBranch   error
	errDeleteCluster  error
}

func newFakeStateStore() *fakeStateStore {
	return &fakeStateStore{
		createdBranches:      map[uuid.UUID]struct{}{},
		createdEndpoints:     map[uuid.UUID]string{},
		endpointAddresses:    map[uuid.UUID]string{},
		deletedEndpoints:     map[uuid.UUID]struct{}{},
		existingWriterBranch: map[uuid.UUID]uuid.UUID{},
		endpointBranch:       map[uuid.UUID]core.EndpointDeploy{},
		endpointCluster:      map[uuid.UUID]uuid.UUID{},
		branchCluster:        map[uuid.UUID]uuid.UUID{},
		branchParent:         map[uuid.UUID]uuid.UUID{},
		branchExts:           map[uuid.UUID][]string{},
		clusterEngine:        map[uuid.UUID]string{},
		endpointExts:         map[uuid.UUID][]string{},
		deletedBranches:      map[uuid.UUID]struct{}{},
		deletedClusters:      map[uuid.UUID]struct{}{},
	}
}

// seedBranchExtensions places a branch under clusterID and sets its opted-in
// extension set. Test helper.
func (f *fakeStateStore) seedBranchExtensions(clusterID, branchID uuid.UUID, names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.branchCluster[branchID] = clusterID
	f.branchExts[branchID] = append([]string(nil), names...)
}

// BranchExtensions returns the branch's opted-in extension set (empty if none).
// Mirrors the backends, cluster id included: branch_id is a primary key, so the
// branch is fully addressed by it and the first argument is ignored.
func (f *fakeStateStore) BranchExtensions(_ context.Context, _, branchID uuid.UUID) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if names, ok := f.branchExts[branchID]; ok {
		return append([]string(nil), names...), nil
	}
	return nil, nil
}

// AddBranchExtensions unions names into the branch's opted-in set (existing-first
// order, deduplicated). Mirrors the backends: empty names is a no-op, and the
// cluster id is ignored.
func (f *fakeStateStore) AddBranchExtensions(_ context.Context, _, branchID uuid.UUID, names []string) error {
	if len(names) == 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := make(map[string]struct{})
	var union []string
	for _, n := range append(append([]string(nil), f.branchExts[branchID]...), names...) {
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		union = append(union, n)
	}
	f.branchExts[branchID] = union
	return nil
}

// BranchEndpointDeploys returns the renderable desired state of every seeded
// endpoint on the branch — whatever the seed carried, per endpoint, so a step
// that re-renders the branch is fed the same shape production hands it. Mirrors
// the backends: the cluster id is ignored.
func (f *fakeStateStore) BranchEndpointDeploys(_ context.Context, _, branchID uuid.UUID) ([]core.EndpointDeploy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.EndpointDeploy
	for _, d := range f.endpointBranch {
		if d.BranchID == branchID {
			out = append(out, d)
		}
	}
	return out, nil
}

// UpdateEndpointExtensions records the resolved names last written to an endpoint.
func (f *fakeStateStore) UpdateEndpointExtensions(_ context.Context, endpointID uuid.UUID, names []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpointExts[endpointID] = append([]string(nil), names...)
	return nil
}

// seedEndpoint registers a live endpoint on (clusterID, branchID) so the cascade
// enumeration methods can find it, with no pinned version or size. Test helper.
func (f *fakeStateStore) seedEndpoint(endpointID, clusterID, branchID uuid.UUID) {
	f.seedEndpointDeploy(clusterID, core.EndpointDeploy{ID: endpointID, BranchID: branchID})
}

// seedEndpointDeploy registers a live endpoint with the whole desired state a
// real row carries — its pinned pg version and its compute size, which differ per
// endpoint on a branch. clusterID is the cluster the endpoint's branch sits under:
// bookkeeping for the cluster-scoped cascade enumerations, not part of the record.
// Test helper for the steps that redeploy from it.
func (f *fakeStateStore) seedEndpointDeploy(clusterID uuid.UUID, d core.EndpointDeploy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpointBranch[d.ID] = d
	f.endpointCluster[d.ID] = clusterID
	f.createdEndpoints[d.ID] = "read_only"
}

// seedBranch registers a live branch under clusterID. Test helper.
func (f *fakeStateStore) seedBranch(clusterID, branchID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.branchCluster[branchID] = clusterID
	f.createdBranches[branchID] = struct{}{}
}

// seedBranchFork registers a live branch under clusterID forked from parentID,
// so ChildBranchIDs reports parentID as having a child. Test helper.
func (f *fakeStateStore) seedBranchFork(clusterID, branchID, parentID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.branchCluster[branchID] = clusterID
	f.branchParent[branchID] = parentID
	f.createdBranches[branchID] = struct{}{}
}

func (f *fakeStateStore) ChildBranchIDs(_ context.Context, clusterID, branchID uuid.UUID) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []uuid.UUID
	for id, parent := range f.branchParent {
		if parent == branchID && f.branchCluster[id] == clusterID {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (f *fakeStateStore) EnsureCluster(_ context.Context, _, clusterID, branchID uuid.UUID, _ string, _ int, engine string, extensions []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureClusterCalls++
	if f.errEnsureCluster != nil {
		return f.errEnsureCluster
	}
	f.createdBranches[branchID] = struct{}{}
	f.branchCluster[branchID] = clusterID
	f.clusterEngine[clusterID] = engine
	if len(extensions) > 0 {
		f.branchExts[branchID] = append([]string(nil), extensions...)
	}
	return nil
}

func (f *fakeStateStore) EnsureBranch(_ context.Context, parentBranchID, clusterID, branchID uuid.UUID, _, _ string, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureBranchCalls++
	if f.errEnsureBranch != nil {
		return f.errEnsureBranch
	}
	f.createdBranches[branchID] = struct{}{}
	f.branchCluster[branchID] = clusterID
	f.branchParent[branchID] = parentBranchID
	// The fork inherits its parent's extension set (copied onto its own row).
	if names, ok := f.branchExts[parentBranchID]; ok {
		f.branchExts[branchID] = append([]string(nil), names...)
	}
	return nil
}

func (f *fakeStateStore) BranchPgSettings(_ context.Context, _, branchID uuid.UUID) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.branchPgSettings[branchID]) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(f.branchPgSettings[branchID]))
	for k, v := range f.branchPgSettings[branchID] {
		out[k] = v
	}
	return out, nil
}

func (f *fakeStateStore) EnsureEndpointRecord(_ context.Context, endpointID, _, branchID uuid.UUID, role, name string,
	_, pgSettings, pageserverSettings, safekeeperSettings []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpointRecordCalls++
	if f.errEnsureEndpoint != nil {
		return f.errEnsureEndpoint
	}
	// Model the single-writer partial unique index: a *different* read_write
	// endpoint id on a branch that already has one is ErrConflict; the same id
	// is an idempotent no-op (ON CONFLICT (id) DO NOTHING).
	if role == "read_write" {
		if existing, ok := f.existingWriterBranch[branchID]; ok && existing != endpointID {
			return errConflict
		}
		f.existingWriterBranch[branchID] = endpointID
	}
	f.createdEndpoints[endpointID] = role
	if name != "" {
		if f.endpointNames == nil {
			f.endpointNames = map[uuid.UUID]string{}
		}
		f.endpointNames[endpointID] = name
	}
	if len(pgSettings) > 0 {
		if f.endpointPgSettings == nil {
			f.endpointPgSettings = map[uuid.UUID]string{}
		}
		f.endpointPgSettings[endpointID] = string(pgSettings)
	}
	// Kept as the raw encoded JSON so a test can catch the two documents being
	// swapped, which the same-typed parameters make possible.
	if len(pageserverSettings) > 0 {
		if f.endpointPageserverSettings == nil {
			f.endpointPageserverSettings = map[uuid.UUID]string{}
		}
		f.endpointPageserverSettings[endpointID] = string(pageserverSettings)
	}
	if len(safekeeperSettings) > 0 {
		if f.endpointSafekeeperSettings == nil {
			f.endpointSafekeeperSettings = map[uuid.UUID]string{}
		}
		f.endpointSafekeeperSettings[endpointID] = string(safekeeperSettings)
	}
	return nil
}

func (f *fakeStateStore) SetEndpointAddress(_ context.Context, endpointID uuid.UUID, address, tailnetAddress string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setAddressCalls++
	if f.errSetAddress != nil {
		return f.errSetAddress
	}
	f.endpointAddresses[endpointID] = address
	return nil
}

func (f *fakeStateStore) DeleteEndpoint(_ context.Context, endpointID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteEndpointCalls++
	if f.errDeleteEndpoint != nil {
		return f.errDeleteEndpoint
	}
	f.deletedEndpoints[endpointID] = struct{}{}
	delete(f.createdEndpoints, endpointID)
	delete(f.endpointBranch, endpointID)
	return nil
}

func (f *fakeStateStore) UpdateEndpointResources(_ context.Context, endpointID uuid.UUID, cpu, memory string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.endpointResources == nil {
		f.endpointResources = map[uuid.UUID][2]string{}
	}
	f.endpointResources[endpointID] = [2]string{cpu, memory}
	return nil
}

func (f *fakeStateStore) SetEndpointYCPGeneration(_ context.Context, endpointID uuid.UUID, gen int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.endpointYCPGeneration == nil {
		f.endpointYCPGeneration = map[uuid.UUID]int64{}
	}
	if cur, ok := f.endpointYCPGeneration[endpointID]; !ok || gen > cur {
		f.endpointYCPGeneration[endpointID] = gen
	}
	return nil
}

func (f *fakeStateStore) SetBranchYCPGeneration(_ context.Context, branchID uuid.UUID, gen int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.branchYCPGeneration == nil {
		f.branchYCPGeneration = map[uuid.UUID]int64{}
	}
	if cur, ok := f.branchYCPGeneration[branchID]; !ok || gen > cur {
		f.branchYCPGeneration[branchID] = gen
	}
	return nil
}

func (f *fakeStateStore) EndpointIDsForBranch(_ context.Context, clusterID, branchID uuid.UUID) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []uuid.UUID
	for id, d := range f.endpointBranch {
		if f.endpointCluster[id] == clusterID && d.BranchID == branchID {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (f *fakeStateStore) EndpointIDsForCluster(_ context.Context, clusterID uuid.UUID) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []uuid.UUID
	for id := range f.endpointBranch {
		if f.endpointCluster[id] == clusterID {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (f *fakeStateStore) DeleteBranch(_ context.Context, clusterID, branchID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errDeleteBranch != nil {
		return f.errDeleteBranch
	}
	// Mirror PgBackend's self-FK RESTRICT: a still-referencing child fork makes
	// the delete a conflict rather than a silent drop.
	for id, parent := range f.branchParent {
		if parent == branchID && f.branchCluster[id] == clusterID {
			return errConflict
		}
	}
	if p, ok := f.branchCluster[branchID]; ok && p == clusterID {
		delete(f.branchCluster, branchID)
		delete(f.createdBranches, branchID)
	}
	f.deletedBranches[branchID] = struct{}{} // idempotent: tolerate-absent
	return nil
}

func (f *fakeStateStore) DeleteCluster(_ context.Context, clusterID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errDeleteCluster != nil {
		return f.errDeleteCluster
	}
	for id := range f.endpointBranch {
		if f.endpointCluster[id] == clusterID {
			delete(f.endpointBranch, id)
			delete(f.endpointCluster, id)
			delete(f.createdEndpoints, id)
		}
	}
	for id, p := range f.branchCluster {
		if p == clusterID {
			delete(f.branchCluster, id)
			delete(f.createdBranches, id)
		}
	}
	f.deletedClusters[clusterID] = struct{}{} // idempotent: tolerate-absent
	return nil
}

// errConflict is a stand-in for a single-writer collision the fake raises.
var errConflict = stringError("conflict: a read_write endpoint already exists")

type stringError string

func (e stringError) Error() string { return string(e) }

// --- fakeRecipeStore: an in-memory task.RecipeStore -------------------------

type recordedRecipe struct {
	clusterID uuid.UUID
	branchID  uuid.UUID
	recipe    BranchRecipe
}

type fakeRecipeStore struct {
	mu        sync.Mutex
	recorded  []recordedRecipe
	errRecord error
}

func newFakeRecipeStore() *fakeRecipeStore { return &fakeRecipeStore{} }

func (f *fakeRecipeStore) RecordBranchRecipe(_ context.Context, clusterID, branchID uuid.UUID, r BranchRecipe) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errRecord != nil {
		return f.errRecord
	}
	f.recorded = append(f.recorded, recordedRecipe{clusterID: clusterID, branchID: branchID, recipe: r})
	return nil
}

// --- fakeDeployer: an idempotent deploy.Deployer ----------------------------

// fakeDeployer models a runtime-idempotent deployer: CreateEndpoint with the
// same name returns the same derived address, so replaying a step converges. It
// records every call so tests can assert what ran. createErr/deleteErr inject
// failures.
type fakeDeployer struct {
	mu        sync.Mutex
	created   []deploy.EndpointSpec
	deleted   []string
	resized   []deploy.ResizeSpec
	createErr error
	deleteErr error
	resizeErr error
}

func (f *fakeDeployer) CreateEndpoint(_ context.Context, spec deploy.EndpointSpec) (deploy.Endpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, spec)
	if f.createErr != nil {
		return deploy.Endpoint{}, f.createErr
	}
	return deploy.Endpoint{ID: spec.Name, Address: spec.Name + ".endpoints.svc:55433"}, nil
}

func (f *fakeDeployer) DeleteEndpoint(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

func (f *fakeDeployer) ResizeEndpoint(_ context.Context, spec deploy.ResizeSpec) (deploy.ResizeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resized = append(f.resized, spec)
	if f.resizeErr != nil {
		return deploy.ResizeResult{}, f.resizeErr
	}
	return deploy.ResizeResult{Outcome: deploy.ResizeCompleted, Message: "resized in place"}, nil
}

func (f *fakeDeployer) EndpointSafekeeperURL(id string) string {
	return "http://" + id + ":7676"
}

func (f *fakeDeployer) EndpointSupervisorURL(id string) string {
	return "http://" + id + ":9899"
}

// --- fakeStep: a configurable task.Step for worker tests --------------------

// fakeStep records how many times it ran and can be told to fail. It surfaces a
// cancelled context as a failure — modeling a step whose DB/HTTP call returns a
// cancellation error when the worker is torn down mid-step — which lets the
// worker's cancellation-vs-failure distinction be tested. The worker rebuilds a
// step on each execution; regWith hands back this same instance from its builder,
// so call counts accumulate across rebuilds.
type fakeStep struct {
	kind  string
	mu    sync.Mutex
	calls int
	// failWith fails every call. failTimes instead fails only the first N, so a
	// test can drive the worker's in-place retry to eventual success.
	failWith  error
	failTimes int
}

func (s *fakeStep) Kind() string { return s.kind }

func (s *fakeStep) Execute(ctx context.Context) StepResult {
	s.mu.Lock()
	s.calls++
	calls := s.calls
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Fail(err)
	}
	if s.failWith != nil && (s.failTimes == 0 || calls <= s.failTimes) {
		return Fail(s.failWith)
	}
	return Ok(json.RawMessage(`{"ok":true}`))
}

func (s *fakeStep) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}
