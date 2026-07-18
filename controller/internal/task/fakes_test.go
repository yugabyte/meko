package task

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/google/uuid"

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

	ensureProjectCalls   int
	ensureBranchCalls    int
	endpointRecordCalls  int
	setAddressCalls      int
	deleteEndpointCalls  int
	createdBranches      map[uuid.UUID]struct{} // distinct branch ids created
	createdEndpoints     map[uuid.UUID]string   // endpoint id -> role
	endpointAddresses    map[uuid.UUID]string
	deletedEndpoints     map[uuid.UUID]struct{}
	existingWriterBranch map[uuid.UUID]uuid.UUID // branch id -> the writer endpoint id already present

	// Cascade-delete bookkeeping: where each (live) endpoint and branch sits, so
	// EndpointIDsForBranch / EndpointIDsForProject can enumerate children and the
	// delete-by-id methods can prune them. Populated by seedEndpoint / seedBranch.
	endpointBranch  map[uuid.UUID]branchRef // endpoint id -> its (project, branch)
	branchProject   map[uuid.UUID]uuid.UUID // branch id -> its project
	branchParent    map[uuid.UUID]uuid.UUID // branch id -> its parent branch id (forks only)
	deletedBranches map[uuid.UUID]struct{}
	deletedProjects map[uuid.UUID]struct{}

	errEnsureProject  error
	errEnsureBranch   error
	errEnsureEndpoint error
	errSetAddress     error
	errDeleteEndpoint error
	errDeleteBranch   error
	errDeleteProject  error
}

// branchRef is an endpoint's (project, branch) placement in the fake.
type branchRef struct {
	projectID uuid.UUID
	branchID  uuid.UUID
}

func newFakeStateStore() *fakeStateStore {
	return &fakeStateStore{
		createdBranches:      map[uuid.UUID]struct{}{},
		createdEndpoints:     map[uuid.UUID]string{},
		endpointAddresses:    map[uuid.UUID]string{},
		deletedEndpoints:     map[uuid.UUID]struct{}{},
		existingWriterBranch: map[uuid.UUID]uuid.UUID{},
		endpointBranch:       map[uuid.UUID]branchRef{},
		branchProject:        map[uuid.UUID]uuid.UUID{},
		branchParent:         map[uuid.UUID]uuid.UUID{},
		deletedBranches:      map[uuid.UUID]struct{}{},
		deletedProjects:      map[uuid.UUID]struct{}{},
	}
}

// seedEndpoint registers a live endpoint on (projectID, branchID) so the cascade
// enumeration methods can find it. Test helper.
func (f *fakeStateStore) seedEndpoint(endpointID, projectID, branchID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpointBranch[endpointID] = branchRef{projectID: projectID, branchID: branchID}
	f.createdEndpoints[endpointID] = "read_only"
}

// seedBranch registers a live branch under projectID. Test helper.
func (f *fakeStateStore) seedBranch(projectID, branchID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.branchProject[branchID] = projectID
	f.createdBranches[branchID] = struct{}{}
}

// seedBranchFork registers a live branch under projectID forked from parentID,
// so ChildBranchIDs reports parentID as having a child. Test helper.
func (f *fakeStateStore) seedBranchFork(projectID, branchID, parentID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.branchProject[branchID] = projectID
	f.branchParent[branchID] = parentID
	f.createdBranches[branchID] = struct{}{}
}

func (f *fakeStateStore) ChildBranchIDs(_ context.Context, projectID, branchID uuid.UUID) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []uuid.UUID
	for id, parent := range f.branchParent {
		if parent == branchID && f.branchProject[id] == projectID {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (f *fakeStateStore) EnsureProject(_ context.Context, _, _, branchID uuid.UUID, _ string, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureProjectCalls++
	if f.errEnsureProject != nil {
		return f.errEnsureProject
	}
	f.createdBranches[branchID] = struct{}{}
	return nil
}

func (f *fakeStateStore) EnsureBranch(_ context.Context, _, _, branchID uuid.UUID, _, _ string, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureBranchCalls++
	if f.errEnsureBranch != nil {
		return f.errEnsureBranch
	}
	f.createdBranches[branchID] = struct{}{}
	return nil
}

func (f *fakeStateStore) EnsureEndpointRecord(_ context.Context, endpointID, _, branchID uuid.UUID, role string, _ []byte) error {
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

func (f *fakeStateStore) EndpointIDsForBranch(_ context.Context, projectID, branchID uuid.UUID) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []uuid.UUID
	for id, ref := range f.endpointBranch {
		if ref.projectID == projectID && ref.branchID == branchID {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (f *fakeStateStore) EndpointIDsForProject(_ context.Context, projectID uuid.UUID) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []uuid.UUID
	for id, ref := range f.endpointBranch {
		if ref.projectID == projectID {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (f *fakeStateStore) DeleteBranch(_ context.Context, projectID, branchID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errDeleteBranch != nil {
		return f.errDeleteBranch
	}
	// Mirror PgBackend's self-FK RESTRICT: a still-referencing child fork makes
	// the delete a conflict rather than a silent drop.
	for id, parent := range f.branchParent {
		if parent == branchID && f.branchProject[id] == projectID {
			return errConflict
		}
	}
	if p, ok := f.branchProject[branchID]; ok && p == projectID {
		delete(f.branchProject, branchID)
		delete(f.createdBranches, branchID)
	}
	f.deletedBranches[branchID] = struct{}{} // idempotent: tolerate-absent
	return nil
}

func (f *fakeStateStore) DeleteProject(_ context.Context, projectID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errDeleteProject != nil {
		return f.errDeleteProject
	}
	for id, ref := range f.endpointBranch {
		if ref.projectID == projectID {
			delete(f.endpointBranch, id)
			delete(f.createdEndpoints, id)
		}
	}
	for id, p := range f.branchProject {
		if p == projectID {
			delete(f.branchProject, id)
			delete(f.createdBranches, id)
		}
	}
	f.deletedProjects[projectID] = struct{}{} // idempotent: tolerate-absent
	return nil
}

// errConflict is a stand-in for a single-writer collision the fake raises.
var errConflict = stringError("conflict: a read_write endpoint already exists")

type stringError string

func (e stringError) Error() string { return string(e) }

// --- fakeRecipeStore: an in-memory task.RecipeStore -------------------------

type recordedRecipe struct {
	branchID string
	recipe   BranchRecipe
}

type fakeRecipeStore struct {
	mu            sync.Mutex
	recorded      []recordedRecipe
	cleared       []string // branchIDs cleared successfully
	clearAttempts int      // clears tried, incremented even when errClear is injected
	errRecord     error
	errClear      error
}

func newFakeRecipeStore() *fakeRecipeStore { return &fakeRecipeStore{} }

func (f *fakeRecipeStore) RecordBranchRecipe(_ context.Context, branchID string, r BranchRecipe) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errRecord != nil {
		return f.errRecord
	}
	f.recorded = append(f.recorded, recordedRecipe{branchID: branchID, recipe: r})
	return nil
}

func (f *fakeRecipeStore) ClearBranchRecipe(_ context.Context, branchID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearAttempts++ // record the attempt before any injected failure
	if f.errClear != nil {
		return f.errClear
	}
	f.cleared = append(f.cleared, branchID)
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
	createErr error
	deleteErr error
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

func (f *fakeDeployer) EndpointSafekeeperURL(id string) string {
	return "http://" + id + ":7676"
}

// --- fakeStep: a configurable task.Step for worker tests --------------------

// fakeStep records how many times it ran and can be told to fail. It surfaces a
// cancelled context as a failure — modeling a step whose DB/HTTP call returns a
// cancellation error when the worker is torn down mid-step — which lets the
// worker's cancellation-vs-failure distinction be tested. The worker rebuilds a
// step on each execution; regWith hands back this same instance from its builder,
// so call counts accumulate across rebuilds.
type fakeStep struct {
	kind     string
	mu       sync.Mutex
	calls    int
	failWith error
}

func (s *fakeStep) Kind() string { return s.kind }

func (s *fakeStep) Execute(ctx context.Context) StepResult {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Fail(err)
	}
	if s.failWith != nil {
		return Fail(s.failWith)
	}
	return Ok(json.RawMessage(`{"ok":true}`))
}

func (s *fakeStep) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}
