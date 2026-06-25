package core

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemBackend is an in-memory Backend that mirrors PgBackend's semantics
// (owner existence checks, globally-unique pool names, top-level pools,
// paginated lists, the single-writer rule, and endpoints whose address is set
// after deploy). It lets the API layer be tested end-to-end — through the
// generated client and the real controllers — without a database. It is safe
// for concurrent use.
type MemBackend struct {
	mu           sync.Mutex
	users        map[uuid.UUID]User
	pools        map[uuid.UUID]StoragePool
	names        map[string]struct{} // pool names are unique across the placement group
	projects     map[uuid.UUID]Project
	branches     map[uuid.UUID]Branch
	projectNames map[projectNameKey]struct{} // project names are unique within their pool
	branchNames  map[branchNameKey]struct{}  // branch names are unique within their project
	endpoints    map[uuid.UUID]Endpoint
	writers      map[branchKey]struct{} // at most one read_write endpoint per branch
	now          func() time.Time
}

// projectNameKey makes a project name unique within its pool (the schema's
// UNIQUE (storage_pool_id, name)).
type projectNameKey struct {
	poolID uuid.UUID
	name   string
}

// branchNameKey makes a branch name unique within its project (the schema's
// branch UNIQUE (project_id, name)): a project's default branch plus its
// branches all share one name space.
type branchNameKey struct {
	projectID uuid.UUID
	name      string
}

// branchKey identifies a branch (the schema's (project_id, branch_id) PK).
type branchKey struct {
	projectID uuid.UUID
	branchID  uuid.UUID
}

// NewMemBackend returns an empty in-memory backend.
func NewMemBackend() *MemBackend {
	return &MemBackend{
		users:        make(map[uuid.UUID]User),
		pools:        make(map[uuid.UUID]StoragePool),
		names:        make(map[string]struct{}),
		projects:     make(map[uuid.UUID]Project),
		branches:     make(map[uuid.UUID]Branch),
		projectNames: make(map[projectNameKey]struct{}),
		branchNames:  make(map[branchNameKey]struct{}),
		endpoints:    make(map[uuid.UUID]Endpoint),
		writers:      make(map[branchKey]struct{}),
		now:          time.Now,
	}
}

// SeedBranch registers a branch (and a stub project for it, if absent) under a
// pool. Endpoint tests (#108) seed a branch directly when they want to exercise
// the endpoint lifecycle without going through CreateProject/CreateBranch,
// mirroring how a real branch is provisioned out-of-band.
func (m *MemBackend) SeedBranch(poolID uuid.UUID, b Branch) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[b.ProjectID]; !ok {
		m.projects[b.ProjectID] = Project{ID: b.ProjectID, StoragePoolID: poolID, Name: b.Name, CreatedAt: b.CreatedAt}
		m.projectNames[projectNameKey{poolID: poolID, name: b.Name}] = struct{}{}
	}
	m.branches[b.BranchID] = b
	m.branchNames[branchNameKey{projectID: b.ProjectID, name: b.Name}] = struct{}{}
}

func (m *MemBackend) Healthy(context.Context) error { return nil }

func (m *MemBackend) CreateUser(_ context.Context, isAdmin bool) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := User{ID: uuid.New(), IsAdmin: isAdmin, CreatedAt: m.now()}
	m.users[u.ID] = u
	return u, nil
}

func (m *MemBackend) GetUser(_ context.Context, id uuid.UUID) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}

func (m *MemBackend) CreateStoragePool(_ context.Context, owner uuid.UUID, name string) (StoragePool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[owner]; !ok {
		return StoragePool{}, ErrNotFound
	}
	if _, taken := m.names[name]; taken {
		return StoragePool{}, ErrConflict
	}
	p := StoragePool{ID: uuid.New(), Name: name, OwnerUserID: owner, CreatedAt: m.now()}
	m.pools[p.ID] = p
	m.names[name] = struct{}{}
	return p, nil
}

func (m *MemBackend) ListUsers(_ context.Context, limit, offset int) ([]User, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	users := make([]User, 0, len(m.users))
	for _, u := range m.users {
		users = append(users, u)
	}
	sort.SliceStable(users, func(i, j int) bool {
		return createdBefore(users[i].CreatedAt, users[j].CreatedAt, users[i].ID, users[j].ID)
	})
	return page(users, limit, offset), len(users), nil
}

func (m *MemBackend) ListStoragePools(_ context.Context, limit, offset int) ([]StoragePool, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pools := make([]StoragePool, 0, len(m.pools))
	for _, p := range m.pools {
		pools = append(pools, p)
	}
	sort.SliceStable(pools, func(i, j int) bool {
		return createdBefore(pools[i].CreatedAt, pools[j].CreatedAt, pools[i].ID, pools[j].ID)
	})
	return page(pools, limit, offset), len(pools), nil
}

func (m *MemBackend) GetStoragePool(_ context.Context, poolID uuid.UUID) (StoragePool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pools[poolID]
	if !ok {
		return StoragePool{}, ErrNotFound
	}
	return p, nil
}

func (m *MemBackend) CreateProject(_ context.Context, poolID uuid.UUID, name string, pgVersion int) (Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pools[poolID]; !ok {
		return Project{}, ErrNotFound
	}
	key := projectNameKey{poolID: poolID, name: name}
	if _, taken := m.projectNames[key]; taken {
		return Project{}, ErrConflict
	}
	now := m.now()
	// The project and its default branch are created together: a project with no
	// branch is not a valid resource.
	p := Project{ID: uuid.New(), StoragePoolID: poolID, Name: name, CreatedAt: now}
	def := Branch{
		ProjectID: p.ID,
		BranchID:  uuid.New(),
		Name:      DefaultBranchName,
		PgVersion: pgVersion,
		IsDefault: true,
		CreatedAt: now,
	}
	// Populate DefaultBranch before storing so the map entry is self-consistent
	// (the read paths re-derive it from m.branches, but a partially-initialized
	// stored struct is a footgun for any future direct access to m.projects).
	p.DefaultBranch = def
	m.projects[p.ID] = p
	m.branches[def.BranchID] = def
	m.projectNames[key] = struct{}{}
	m.branchNames[branchNameKey{projectID: p.ID, name: def.Name}] = struct{}{}
	return p, nil
}

func (m *MemBackend) ListProjects(_ context.Context, poolID uuid.UUID, limit, offset int) ([]Project, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pools[poolID]; !ok {
		return nil, 0, ErrNotFound
	}
	var projects []Project
	for _, p := range m.projects {
		if p.StoragePoolID == poolID {
			p.DefaultBranch = m.defaultBranchOf(p.ID)
			projects = append(projects, p)
		}
	}
	sort.SliceStable(projects, func(i, j int) bool {
		return createdBefore(projects[i].CreatedAt, projects[j].CreatedAt, projects[i].ID, projects[j].ID)
	})
	return page(projects, limit, offset), len(projects), nil
}

func (m *MemBackend) GetProject(_ context.Context, poolID, projectID uuid.UUID) (Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.projectInPool(poolID, projectID)
	if !ok {
		return Project{}, ErrNotFound
	}
	p.DefaultBranch = m.defaultBranchOf(p.ID)
	return p, nil
}

func (m *MemBackend) CreateBranch(_ context.Context, poolID, projectID, parentBranchID uuid.UUID, name, forkLSN string) (Branch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projectInPool(poolID, projectID); !ok {
		return Branch{}, ErrNotFound
	}
	parent, ok := m.branches[parentBranchID]
	if !ok || parent.ProjectID != projectID {
		return Branch{}, ErrNotFound
	}
	nk := branchNameKey{projectID: projectID, name: name}
	if _, taken := m.branchNames[nk]; taken {
		return Branch{}, ErrConflict
	}
	parentID := parent.BranchID
	// A branch stays in its project and inherits the parent's Postgres version.
	b := Branch{
		ProjectID:      projectID,
		BranchID:       uuid.New(),
		Name:           name,
		PgVersion:      parent.PgVersion,
		IsDefault:      false,
		ParentBranchID: &parentID,
		ForkLSN:        forkLSN,
		CreatedAt:      m.now(),
	}
	m.branches[b.BranchID] = b
	m.branchNames[nk] = struct{}{}
	return b, nil
}

func (m *MemBackend) ListBranches(_ context.Context, poolID, projectID uuid.UUID, limit, offset int) ([]Branch, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projectInPool(poolID, projectID); !ok {
		return nil, 0, ErrNotFound
	}
	var branches []Branch
	for _, b := range m.branches {
		if b.ProjectID == projectID {
			branches = append(branches, b)
		}
	}
	sort.SliceStable(branches, func(i, j int) bool {
		return createdBefore(branches[i].CreatedAt, branches[j].CreatedAt, branches[i].BranchID, branches[j].BranchID)
	})
	return page(branches, limit, offset), len(branches), nil
}

func (m *MemBackend) GetBranch(_ context.Context, poolID, projectID, branchID uuid.UUID) (Branch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.branchInProject(poolID, projectID, branchID)
	if !ok {
		return Branch{}, ErrNotFound
	}
	return b, nil
}

// projectInPool returns the project addressed by (poolID, projectID) if the
// project belongs to the pool. Caller holds m.mu.
func (m *MemBackend) projectInPool(poolID, projectID uuid.UUID) (Project, bool) {
	if _, ok := m.pools[poolID]; !ok {
		return Project{}, false
	}
	p, ok := m.projects[projectID]
	if !ok || p.StoragePoolID != poolID {
		return Project{}, false
	}
	return p, true
}

// branchInProject returns the branch addressed by (poolID, projectID, branchID)
// if the pool->project->branch chain holds. Caller holds m.mu.
func (m *MemBackend) branchInProject(poolID, projectID, branchID uuid.UUID) (Branch, bool) {
	if _, ok := m.projectInPool(poolID, projectID); !ok {
		return Branch{}, false
	}
	b, ok := m.branches[branchID]
	if !ok || b.ProjectID != projectID {
		return Branch{}, false
	}
	return b, true
}

// defaultBranchOf returns the project's default branch (the one with IsDefault),
// or a zero Branch if absent. Caller holds m.mu.
func (m *MemBackend) defaultBranchOf(projectID uuid.UUID) Branch {
	for _, b := range m.branches {
		if b.ProjectID == projectID && b.IsDefault {
			return b
		}
	}
	return Branch{}
}

func (m *MemBackend) CreateEndpoint(_ context.Context, projectID, branchID uuid.UUID, role string, _ []byte) (Endpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.branches[branchID]
	if !ok || b.ProjectID != projectID {
		return Endpoint{}, ErrNotFound
	}
	if role == RoleReadWrite {
		k := branchKey{projectID: projectID, branchID: branchID}
		if _, taken := m.writers[k]; taken {
			return Endpoint{}, ErrConflict
		}
		m.writers[k] = struct{}{}
	}
	// Address is set later via SetEndpointAddress, once the endpoint is deployed.
	e := Endpoint{
		ID:        uuid.New(),
		ProjectID: projectID,
		BranchID:  branchID,
		Role:      role,
		CreatedAt: m.now(),
	}
	m.endpoints[e.ID] = e
	return e, nil
}

func (m *MemBackend) GetEndpoint(_ context.Context, poolID, projectID, branchID, endpointID uuid.UUID) (Endpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.branchInProject(poolID, projectID, branchID); !ok {
		return Endpoint{}, ErrNotFound
	}
	ep, ok := m.endpoints[endpointID]
	// Match PgBackend, which joins on (project_id, branch_id): the endpoint must
	// belong to this branch, not just share its branch id.
	if !ok || ep.BranchID != branchID || ep.ProjectID != projectID {
		return Endpoint{}, ErrNotFound
	}
	return ep, nil
}

func (m *MemBackend) ListEndpoints(_ context.Context, poolID, projectID, branchID uuid.UUID, limit, offset int) ([]Endpoint, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.branchInProject(poolID, projectID, branchID); !ok {
		return nil, 0, ErrNotFound
	}
	var endpoints []Endpoint
	for _, e := range m.endpoints {
		if e.BranchID == branchID && e.ProjectID == projectID {
			endpoints = append(endpoints, e)
		}
	}
	sort.SliceStable(endpoints, func(i, j int) bool {
		return createdBefore(endpoints[i].CreatedAt, endpoints[j].CreatedAt, endpoints[i].ID, endpoints[j].ID)
	})
	return page(endpoints, limit, offset), len(endpoints), nil
}

func (m *MemBackend) SetEndpointAddress(_ context.Context, endpointID uuid.UUID, address string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ep, ok := m.endpoints[endpointID]; ok {
		ep.Address = address
		m.endpoints[endpointID] = ep
	}
	return nil
}

func (m *MemBackend) DeleteEndpoint(_ context.Context, endpointID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ep, ok := m.endpoints[endpointID]; ok {
		if ep.Role == RoleReadWrite {
			delete(m.writers, branchKey{projectID: ep.ProjectID, branchID: ep.BranchID})
		}
		delete(m.endpoints, endpointID)
	}
	return nil
}

// createdBefore orders items by (created_at, id), matching the pg backend's
// ORDER BY created_at, <id> so the two backends paginate identically.
func createdBefore(ti, tj time.Time, idi, idj uuid.UUID) bool {
	if ti.Equal(tj) {
		return idi.String() < idj.String()
	}
	return ti.Before(tj)
}

// page returns the half-open window items[offset : offset+limit], clamped to the
// slice bounds. The API layer guarantees limit >= 1 and offset >= 0.
func page[T any](items []T, limit, offset int) []T {
	start := min(offset, len(items))
	end := min(start+limit, len(items))
	return items[start:end]
}
