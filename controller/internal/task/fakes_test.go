package task

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/yb-amp/controller/internal/deploy"
)

// Shared in-memory fakes for the task-package tests. They live in one file so
// the per-unit test files (steps_test.go, plan_test.go, worker_test.go,
// registry_test.go) share one definition each — matching the no-database test
// convention used across the controller (core.MemBackend, api's fakeTaskStore).

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

	errEnsureProject  error
	errEnsureBranch   error
	errEnsureEndpoint error
	errSetAddress     error
	errDeleteEndpoint error
}

func newFakeStateStore() *fakeStateStore {
	return &fakeStateStore{
		createdBranches:      map[uuid.UUID]struct{}{},
		createdEndpoints:     map[uuid.UUID]string{},
		endpointAddresses:    map[uuid.UUID]string{},
		deletedEndpoints:     map[uuid.UUID]struct{}{},
		existingWriterBranch: map[uuid.UUID]uuid.UUID{},
	}
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

func (f *fakeStateStore) SetEndpointAddress(_ context.Context, endpointID uuid.UUID, address string) error {
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

// --- fakeHandler: a configurable task.Handler for worker tests --------------

// fakeHandler records each invocation (and the step ordinal it saw) and can be
// told to fail. It lets the worker tests assert which steps ran, in what order,
// and that already-succeeded steps are skipped on resume.
type fakeHandler struct {
	kind     string
	mu       sync.Mutex
	calls    int
	ordinals []int
	failWith error
}

func (h *fakeHandler) Kind() string { return h.kind }

func (h *fakeHandler) Run(_ context.Context, in StepInput) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	h.ordinals = append(h.ordinals, in.Step.Ordinal)
	if h.failWith != nil {
		return nil, h.failWith
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (h *fakeHandler) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// --- fakeStore: an in-memory task.Store for worker tests --------------------

// fakeStore is a faithful in-memory model of the write-ahead task store: tasks
// and their ordered steps, an atomic claim under a lease (pending, or running
// with an expired lease, becomes claimable), and the step/task status
// transitions the worker drives. It lets the worker's resume-safe execution
// loop (§8.1) be tested without a database; the real PgStore SQL is exercised
// by the e2e suite. The clock is injectable so lease expiry is deterministic.
type fakeStore struct {
	mu    sync.Mutex
	order []uuid.UUID
	tasks map[uuid.UUID]*Task
	steps map[uuid.UUID][]*Step
	now   func() time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		tasks: map[uuid.UUID]*Task{},
		steps: map[uuid.UUID][]*Step{},
		now:   time.Now,
	}
}

// seed inserts a task and its steps directly, for preloading resume/failure
// scenarios (e.g. a running task with an expired lease and some steps already
// succeeded).
func (s *fakeStore) seed(t *Task, steps []*Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, t.ID)
	s.tasks[t.ID] = t
	s.steps[t.ID] = steps
}

func (s *fakeStore) Plan(ctx context.Context, kind string, params json.RawMessage, steps []StepPlan) (*Task, error) {
	return s.PlanFrom(ctx, Plan{Kind: kind, Params: params, Steps: steps})
}

func (s *fakeStore) PlanFrom(_ context.Context, p Plan) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := uuid.New()
	now := s.now()
	t := &Task{
		ID:        id,
		Kind:      p.Kind,
		Status:    StatusPending,
		ProjectID: p.ProjectID,
		TargetRef: p.TargetRef,
		Params:    p.Params,
		CreatedAt: now,
		UpdatedAt: now,
	}
	var steps []*Step
	for i, sp := range p.Steps {
		steps = append(steps, &Step{TaskID: id, Ordinal: i, Kind: sp.Kind, Status: StatusPending, Args: sp.Args, UpdatedAt: now})
	}
	s.order = append(s.order, id)
	s.tasks[id] = t
	s.steps[id] = steps
	return cloneTask(t), nil
}

func (s *fakeStore) ClaimNext(_ context.Context, owner string, lease time.Duration) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, id := range s.order {
		t := s.tasks[id]
		claimable := t.Status == StatusPending ||
			(t.Status == StatusRunning && t.LeaseExpiry != nil && t.LeaseExpiry.Before(now))
		if !claimable {
			continue
		}
		t.Status = StatusRunning
		t.Owner = &owner
		exp := now.Add(lease)
		t.LeaseExpiry = &exp
		t.UpdatedAt = now
		return cloneTask(t), nil
	}
	return nil, nil
}

func (s *fakeStore) Steps(_ context.Context, taskID uuid.UUID) ([]Step, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.steps[taskID]
	out := make([]Step, 0, len(src))
	for _, st := range src {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	return out, nil
}

func (s *fakeStore) StartStep(_ context.Context, taskID uuid.UUID, ordinal int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.step(taskID, ordinal); st != nil {
		st.Status = StatusRunning
		st.UpdatedAt = s.now()
	}
	return nil
}

func (s *fakeStore) CompleteStep(_ context.Context, taskID uuid.UUID, ordinal int, result json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.step(taskID, ordinal); st != nil {
		st.Status = StatusSucceeded
		st.Result = result
		st.Error = nil
		st.UpdatedAt = s.now()
	}
	return nil
}

func (s *fakeStore) FailStep(_ context.Context, taskID uuid.UUID, ordinal int, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := cause.Error()
	if st := s.step(taskID, ordinal); st != nil {
		st.Status = StatusFailed
		st.Error = &msg
		st.UpdatedAt = s.now()
	}
	if t := s.tasks[taskID]; t != nil {
		t.Status = StatusFailed
		t.Error = &msg
		t.UpdatedAt = s.now()
	}
	return nil
}

func (s *fakeStore) CompleteTask(_ context.Context, taskID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.tasks[taskID]; t != nil {
		t.Status = StatusSucceeded
		t.Error = nil
		t.UpdatedAt = s.now()
	}
	return nil
}

func (s *fakeStore) Get(_ context.Context, id uuid.UUID) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[id]
	if t == nil {
		return nil, nil
	}
	return cloneTask(t), nil
}

// step returns the step pointer for (taskID, ordinal). Caller holds s.mu.
func (s *fakeStore) step(taskID uuid.UUID, ordinal int) *Step {
	for _, st := range s.steps[taskID] {
		if st.Ordinal == ordinal {
			return st
		}
	}
	return nil
}

func cloneTask(t *Task) *Task {
	c := *t
	return &c
}
