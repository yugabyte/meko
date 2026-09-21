package core

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/extensions"
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
	droppedPools map[uuid.UUID]struct{} // ids of deleted pools; never reusable, as in the schema
	names        map[string]struct{}    // pool names are unique across the placement group
	clusters     map[uuid.UUID]Cluster
	branches     map[uuid.UUID]Branch
	clusterNames map[clusterNameKey]struct{} // cluster names are unique within their pool
	branchNames  map[branchNameKey]struct{}  // branch names are unique within their cluster
	endpoints    map[uuid.UUID]Endpoint
	// endpointExts is what each endpoint was last successfully deployed mounting.
	// Kept beside the endpoint rather than on it because that is its shape in
	// Postgres: it lives in the endpoint's `config` blob and surfaces only on the
	// deploy projection, never on Endpoint.
	endpointExts map[uuid.UUID][]string
	writers      map[branchKey]struct{} // at most one read_write endpoint per branch
	// extensions an operator registered (the extensions.Store half); keyed by
	// name, which is the identity in the schema too.
	extensions map[string]extensions.Extension
	now        func() time.Time
}

// clusterNameKey makes a cluster name unique within its pool (the schema's
// clusters_name_per_pool).
type clusterNameKey struct {
	poolID uuid.UUID
	name   string
}

// branchNameKey makes a branch name unique within its cluster (the schema's
// branches_name_per_cluster): a cluster's default branch plus its branches all
// share one name space.
type branchNameKey struct {
	clusterID uuid.UUID
	name      string
}

// branchKey identifies a branch. The schema keys branches on `branch_id` alone
// (it is the storage layer's globally-unique BranchId), but the cluster is
// carried here too so the fake can answer cluster-scoped questions without a
// second index.
type branchKey struct {
	clusterID uuid.UUID
	branchID  uuid.UUID
}

// NewMemBackend returns an empty in-memory backend.
func NewMemBackend() *MemBackend {
	return &MemBackend{
		users:        make(map[uuid.UUID]User),
		pools:        make(map[uuid.UUID]StoragePool),
		droppedPools: make(map[uuid.UUID]struct{}),
		names:        make(map[string]struct{}),
		clusters:     make(map[uuid.UUID]Cluster),
		branches:     make(map[uuid.UUID]Branch),
		clusterNames: make(map[clusterNameKey]struct{}),
		branchNames:  make(map[branchNameKey]struct{}),
		endpoints:    make(map[uuid.UUID]Endpoint),
		endpointExts: make(map[uuid.UUID][]string),
		writers:      make(map[branchKey]struct{}),
		now:          time.Now,
	}
}

// SeedBranch registers a branch (and a stub cluster for it, if absent) under a
// pool. Endpoint tests (#108) seed a branch directly when they want to exercise
// the endpoint lifecycle without going through CreateCluster/CreateBranch,
// mirroring how a real branch is provisioned out-of-band.
func (m *MemBackend) SeedBranch(poolID uuid.UUID, b Branch) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.clusters[b.ClusterID]; !ok {
		m.clusters[b.ClusterID] = Cluster{ID: b.ClusterID, StoragePoolID: poolID, Name: b.Name, CreatedAt: b.CreatedAt}
		m.clusterNames[clusterNameKey{poolID: poolID, name: b.Name}] = struct{}{}
	}
	m.branches[b.BranchID] = b
	m.branchNames[branchNameKey{clusterID: b.ClusterID, name: b.Name}] = struct{}{}
}

func (m *MemBackend) Healthy(context.Context) error { return nil }

func (m *MemBackend) CreateUser(ctx context.Context, isAdmin bool) (User, error) {
	return m.EnsureUser(ctx, uuid.New(), isAdmin)
}

func (m *MemBackend) EnsureUser(_ context.Context, id uuid.UUID, isAdmin bool) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[id]; ok {
		return u, nil // idempotent — first-seen wins
	}
	u := User{ID: id, IsAdmin: isAdmin, CreatedAt: m.now()}
	m.users[id] = u
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

func (m *MemBackend) CreateStoragePool(ctx context.Context, owner uuid.UUID, name string) (StoragePool, error) {
	return m.CreateStoragePoolWithID(ctx, uuid.New(), owner, name)
}

func (m *MemBackend) CreateStoragePoolWithID(_ context.Context, id, owner uuid.UUID, name string) (StoragePool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.pools[id]; ok {
		return p, nil // idempotent — the id already maps to a pool
	}
	if _, dropped := m.droppedPools[id]; dropped {
		return StoragePool{}, ErrConflict // a tombstone keeps the id; only names are reusable
	}
	if _, ok := m.users[owner]; !ok {
		return StoragePool{}, ErrNotFound
	}
	if _, taken := m.names[name]; taken {
		return StoragePool{}, ErrConflict
	}
	p := StoragePool{ID: id, Name: name, OwnerUserID: owner, CreatedAt: m.now()}
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

func (m *MemBackend) RenameStoragePool(_ context.Context, poolID uuid.UUID, name string) (StoragePool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pools[poolID]
	if !ok {
		return StoragePool{}, ErrNotFound
	}
	if name == p.Name {
		return p, nil
	}
	if _, taken := m.names[name]; taken {
		return StoragePool{}, ErrConflict
	}
	delete(m.names, p.Name)
	m.names[name] = struct{}{}
	p.Name = name
	m.pools[poolID] = p
	return p, nil
}

// PgBackend tombstones the pool; this drops the row and remembers the id, which
// reproduces what callers can observe — the pool is gone from every read, its
// name is free, and its id is not.
//
// What it cannot reproduce is the tombstoned-CLUSTER case this delete policy is
// built around: MemBackend's DeleteCluster hard-deletes, so the fake never holds
// a cluster that is dropped-but-present, and the live-only check below can never
// differ from "any cluster". That case is covered against the real schema by
// TestPgStoragePoolDeleteRename.
func (m *MemBackend) DeleteStoragePool(_ context.Context, poolID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pools[poolID]
	if !ok {
		return ErrNotFound
	}
	for _, c := range m.clusters {
		if c.StoragePoolID == poolID {
			return ErrConflict
		}
	}
	delete(m.pools, poolID)
	delete(m.names, p.Name)
	m.droppedPools[poolID] = struct{}{}
	return nil
}

func (m *MemBackend) CreateCluster(_ context.Context, poolID uuid.UUID, name string, pgVersion int) (Cluster, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pools[poolID]; !ok {
		return Cluster{}, ErrNotFound
	}
	key := clusterNameKey{poolID: poolID, name: name}
	if _, taken := m.clusterNames[key]; taken {
		return Cluster{}, ErrConflict
	}
	now := m.now()
	// The cluster and its default branch are created together: a cluster with no
	// branch is not a valid resource.
	p := Cluster{ID: uuid.New(), StoragePoolID: poolID, Name: name, CreatedAt: now}
	def := Branch{
		ClusterID: p.ID,
		BranchID:  uuid.New(),
		Name:      DefaultBranchName,
		PgVersion: pgVersion,
		IsDefault: true,
		CreatedAt: now,
	}
	// Populate DefaultBranch before storing so the map entry is self-consistent
	// (the read paths re-derive it from m.branches, but a partially-initialized
	// stored struct is a footgun for any future direct access to m.clusters).
	p.DefaultBranch = def
	m.clusters[p.ID] = p
	m.branches[def.BranchID] = def
	m.clusterNames[key] = struct{}{}
	m.branchNames[branchNameKey{clusterID: p.ID, name: def.Name}] = struct{}{}
	return p, nil
}

// includeFailed is accepted for parity with PgBackend but ignored: the in-memory
// backend has no create_cluster task history, so it has no failed-creation orphans
// to filter.
func (m *MemBackend) ListClusters(_ context.Context, poolID uuid.UUID, limit, offset int, _ bool) ([]Cluster, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pools[poolID]; !ok {
		return nil, 0, ErrNotFound
	}
	var clusters []Cluster
	for _, p := range m.clusters {
		if p.StoragePoolID == poolID {
			p.DefaultBranch = m.defaultBranchOf(p.ID)
			clusters = append(clusters, p)
		}
	}
	sort.SliceStable(clusters, func(i, j int) bool {
		return createdBefore(clusters[i].CreatedAt, clusters[j].CreatedAt, clusters[i].ID, clusters[j].ID)
	})
	return page(clusters, limit, offset), len(clusters), nil
}

func (m *MemBackend) GetCluster(_ context.Context, poolID, clusterID uuid.UUID) (Cluster, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.clusterInPool(poolID, clusterID)
	if !ok {
		return Cluster{}, ErrNotFound
	}
	p.DefaultBranch = m.defaultBranchOf(p.ID)
	return p, nil
}

func (m *MemBackend) CreateBranch(_ context.Context, poolID, clusterID, parentBranchID uuid.UUID, name, forkLSN string) (Branch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.clusterInPool(poolID, clusterID); !ok {
		return Branch{}, ErrNotFound
	}
	parent, ok := m.branches[parentBranchID]
	if !ok || parent.ClusterID != clusterID {
		return Branch{}, ErrNotFound
	}
	nk := branchNameKey{clusterID: clusterID, name: name}
	if _, taken := m.branchNames[nk]; taken {
		return Branch{}, ErrConflict
	}
	parentID := parent.BranchID
	// A branch stays in its cluster and inherits the parent's Postgres version.
	b := Branch{
		ClusterID:      clusterID,
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

func (m *MemBackend) ListBranches(_ context.Context, poolID, clusterID uuid.UUID, limit, offset int) ([]Branch, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.clusterInPool(poolID, clusterID); !ok {
		return nil, 0, ErrNotFound
	}
	var branches []Branch
	for _, b := range m.branches {
		if b.ClusterID == clusterID {
			branches = append(branches, b)
		}
	}
	sort.SliceStable(branches, func(i, j int) bool {
		return createdBefore(branches[i].CreatedAt, branches[j].CreatedAt, branches[i].BranchID, branches[j].BranchID)
	})
	return page(branches, limit, offset), len(branches), nil
}

func (m *MemBackend) GetBranch(_ context.Context, poolID, clusterID, branchID uuid.UUID) (Branch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.branchInCluster(poolID, clusterID, branchID)
	if !ok {
		return Branch{}, ErrNotFound
	}
	return b, nil
}

// clusterInPool returns the cluster addressed by (poolID, clusterID) if the
// cluster belongs to the pool. Caller holds m.mu.
func (m *MemBackend) clusterInPool(poolID, clusterID uuid.UUID) (Cluster, bool) {
	if _, ok := m.pools[poolID]; !ok {
		return Cluster{}, false
	}
	p, ok := m.clusters[clusterID]
	if !ok || p.StoragePoolID != poolID {
		return Cluster{}, false
	}
	return p, true
}

// branchInCluster returns the branch addressed by (poolID, clusterID, branchID)
// if the pool->cluster->branch chain holds. Caller holds m.mu.
func (m *MemBackend) branchInCluster(poolID, clusterID, branchID uuid.UUID) (Branch, bool) {
	if _, ok := m.clusterInPool(poolID, clusterID); !ok {
		return Branch{}, false
	}
	b, ok := m.branches[branchID]
	if !ok || b.ClusterID != clusterID {
		return Branch{}, false
	}
	return b, true
}

// defaultBranchOf returns the cluster's default branch (the one with IsDefault),
// or a zero Branch if absent. Caller holds m.mu.
func (m *MemBackend) defaultBranchOf(clusterID uuid.UUID) Branch {
	for _, b := range m.branches {
		if b.ClusterID == clusterID && b.IsDefault {
			return b
		}
	}
	return Branch{}
}

func (m *MemBackend) CreateEndpoint(_ context.Context, clusterID, branchID uuid.UUID, role string, _ []byte) (Endpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.branches[branchID]
	if !ok || b.ClusterID != clusterID {
		return Endpoint{}, ErrNotFound
	}
	if role == RoleReadWrite {
		k := branchKey{clusterID: clusterID, branchID: branchID}
		if _, taken := m.writers[k]; taken {
			return Endpoint{}, ErrConflict
		}
		m.writers[k] = struct{}{}
	}
	// Address is set later via SetEndpointAddress, once the endpoint is deployed.
	e := Endpoint{
		ID:        uuid.New(),
		ClusterID: clusterID,
		BranchID:  branchID,
		Role:      role,
		CreatedAt: m.now(),
	}
	m.endpoints[e.ID] = e
	return e, nil
}

func (m *MemBackend) GetEndpoint(_ context.Context, poolID, clusterID, branchID, endpointID uuid.UUID) (Endpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.branchInCluster(poolID, clusterID, branchID); !ok {
		return Endpoint{}, ErrNotFound
	}
	ep, ok := m.endpoints[endpointID]
	// Match PgBackend, which reaches the cluster through the branch: the endpoint must
	// belong to this branch, not just share its branch id.
	if !ok || ep.BranchID != branchID || ep.ClusterID != clusterID {
		return Endpoint{}, ErrNotFound
	}
	return ep, nil
}

func (m *MemBackend) ListEndpoints(_ context.Context, poolID, clusterID, branchID uuid.UUID, limit, offset int) ([]Endpoint, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.branchInCluster(poolID, clusterID, branchID); !ok {
		return nil, 0, ErrNotFound
	}
	var endpoints []Endpoint
	for _, e := range m.endpoints {
		if e.BranchID == branchID && e.ClusterID == clusterID {
			endpoints = append(endpoints, e)
		}
	}
	sort.SliceStable(endpoints, func(i, j int) bool {
		return createdBefore(endpoints[i].CreatedAt, endpoints[j].CreatedAt, endpoints[i].ID, endpoints[j].ID)
	})
	return page(endpoints, limit, offset), len(endpoints), nil
}

func (m *MemBackend) SetEndpointAddress(_ context.Context, endpointID uuid.UUID, address, tailnetAddress string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ep, ok := m.endpoints[endpointID]; ok {
		ep.Address = address
		ep.TailnetAddress = tailnetAddress
		m.endpoints[endpointID] = ep
	}
	return nil
}

func (m *MemBackend) DeleteEndpoint(_ context.Context, endpointID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ep, ok := m.endpoints[endpointID]; ok {
		if ep.Role == RoleReadWrite {
			delete(m.writers, branchKey{clusterID: ep.ClusterID, branchID: ep.BranchID})
		}
		delete(m.endpoints, endpointID)
	}
	return nil
}

// --- task.StateStore: the id-keyed, idempotent operations the write-ahead task
// step catalog drives (mirroring *PgBackend's provision.go). The caller supplies
// the ids (assigned write-ahead at plan time), so a replayed step converges: an
// existing id is a no-op, a name collision with a *different* id is ErrConflict.
// These let the API tests drive a real worker against this in-memory backend. ---

// EnsureCluster idempotently creates a cluster and its default branch with the
// given ids. A repeated id is a no-op; a cluster-name collision within the pool
// (a different id) is ErrConflict; a missing pool is ErrNotFound.
func (m *MemBackend) EnsureCluster(_ context.Context, poolID, clusterID, branchID uuid.UUID, name string, pgVersion int, engine string, extensions []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pools[poolID]; !ok {
		return ErrNotFound
	}
	if existing, ok := m.clusters[clusterID]; ok {
		// Same id => idempotent no-op (mirrors ON CONFLICT (id) DO NOTHING).
		_ = existing
		return nil
	}
	nk := clusterNameKey{poolID: poolID, name: name}
	if _, taken := m.clusterNames[nk]; taken {
		return ErrConflict
	}
	now := m.now()
	// Seed the create-time externalized-extension set on the default branch row.
	// It is per-branch state: a fork inherits it, and every endpoint resolves
	// against its own branch's set.
	def := Branch{
		ClusterID:  clusterID,
		BranchID:   branchID,
		Name:       DefaultBranchName,
		PgVersion:  pgVersion,
		IsDefault:  true,
		CreatedAt:  now,
		Extensions: dedupSorted(extensions),
	}
	p := Cluster{ID: clusterID, StoragePoolID: poolID, Name: name, CreatedAt: now, Engine: normalizeEngine(engine), DefaultBranch: def}
	m.clusters[clusterID] = p
	m.branches[branchID] = def
	m.clusterNames[nk] = struct{}{}
	m.branchNames[branchNameKey{clusterID: clusterID, name: DefaultBranchName}] = struct{}{}
	return nil
}

// EnsureBranch idempotently creates a non-default branch with the given id,
// forked from parentBranchID at forkLSN. A repeated id is a no-op; a name
// collision within the cluster (a different id) is ErrConflict; a missing parent
// is ErrNotFound.
func (m *MemBackend) EnsureBranch(_ context.Context, parentBranchID, clusterID, branchID uuid.UUID, name, forkLSN string, pgVersion int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.branches[branchID]; ok {
		return nil // same id => idempotent no-op
	}
	parent, ok := m.branches[parentBranchID]
	if !ok || parent.ClusterID != clusterID {
		return ErrNotFound
	}
	nk := branchNameKey{clusterID: clusterID, name: name}
	if _, taken := m.branchNames[nk]; taken {
		return ErrConflict
	}
	pid := parentBranchID
	// The fork inherits the parent branch's extension set (copied onto its own
	// row), so an endpoint on it mounts the same set the parent's endpoints do.
	// See compute/architecture/extensions.md.
	m.branches[branchID] = Branch{
		ClusterID:      clusterID,
		BranchID:       branchID,
		Name:           name,
		PgVersion:      pgVersion,
		IsDefault:      false,
		ParentBranchID: &pid,
		ForkLSN:        forkLSN,
		CreatedAt:      m.now(),
		Extensions:     dedupSorted(parent.Extensions),
	}
	m.branchNames[nk] = struct{}{}
	return nil
}

// BranchPgSettings mirrors the Pg arm: the branch's baseline settings, nil for
// none or an absent branch.
func (m *MemBackend) BranchPgSettings(_ context.Context, clusterID, branchID uuid.UUID) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.branches[branchID]
	if !ok || b.ClusterID != clusterID || len(b.PgSettings) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(b.PgSettings))
	for k, v := range b.PgSettings {
		out[k] = v
	}
	return out, nil
}

// BranchExtensions returns the branch's own opted-in extension set (nil when
// empty or absent). Mirrors PgBackend, including the vestigial cluster id:
// branch_id is a primary key, so the branch is fully addressed by it and both
// backends ignore the first argument.
func (m *MemBackend) BranchExtensions(_ context.Context, _, branchID uuid.UUID) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.branches[branchID]
	if !ok || len(b.Extensions) == 0 {
		return nil, nil
	}
	return append([]string(nil), b.Extensions...), nil
}

// AddBranchExtensions adds names to the branch's durable extension set (absolute
// union, sorted+deduped). Idempotent; an absent branch or empty names is a no-op.
// Mirrors PgBackend, cluster id included: it is ignored here too.
func (m *MemBackend) AddBranchExtensions(_ context.Context, _, branchID uuid.UUID, names []string) error {
	if len(names) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.branches[branchID]
	if !ok {
		return nil
	}
	b.Extensions = dedupSorted(append(append([]string(nil), b.Extensions...), names...))
	m.branches[branchID] = b
	return nil
}

// endpointDeploy renders one endpoint's desired state. All four projections below
// go through it, so MemBackend cannot disagree with itself the way four hand-built
// literals could — the property PgBackend gets from sharing endpointDeploySelect.
//
// It projects no ingress slot, and cannot: allocations live in Postgres and the
// allocator is built on a pool, so a MemBackend deployment has no allocator at
// all, Deps.Ingress is nil, and the ingress_* modes are refused before an
// endpoint can hold a slot. PgBackend LEFT JOINs the live allocation for the
// same reason this does not need to. If MemBackend ever grows an allocator, this
// has to grow the five fields with it — the deployer refuses a zero slot on an
// ingress endpoint, so the failure would be loud rather than a ":0" address.
func (m *MemBackend) endpointDeploy(id uuid.UUID, e Endpoint, pgVersion int) EndpointDeploy {
	return EndpointDeploy{
		ID:           id,
		BranchID:     e.BranchID,
		ClusterID:    m.branches[e.BranchID].ClusterID,
		PgVersion:    pgVersion,
		CPU:          e.CPU,
		Memory:       e.Memory,
		Version:      e.Version,
		Role:         e.Role,
		Connectivity: e.Connectivity,
		AllowedCIDRs: e.AllowedCIDRs,
		Extensions:   m.endpointExts[id],
	}
}

// BranchEndpointDeploys returns the renderable desired state of every endpoint on
// the branch (pg_version from the branch, the rest from the endpoint). An absent
// branch yields an empty slice. Mirrors PgBackend, cluster id included: it is
// ignored here too.
func (m *MemBackend) BranchEndpointDeploys(_ context.Context, _, branchID uuid.UUID) ([]EndpointDeploy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []EndpointDeploy
	for id, e := range m.endpoints {
		if e.BranchID != branchID {
			continue
		}
		out = append(out, m.endpointDeploy(id, e, m.branches[e.BranchID].PgVersion))
	}
	return out, nil
}

// AllEndpointDeploys returns the renderable desired state of every live endpoint
// in the deployment (the fleet roll's enumeration), mirroring PgBackend. An
// endpoint whose branch is gone is not in the fleet — MemBackend deletes branch
// rows outright rather than tombstoning them, so a missing branch covers
// PgBackend's dropped_at case.
//
// Ordered by id, where PgBackend orders by (created_at, id): the map iteration
// has no order of its own, and a caller that reports one line per endpoint needs
// a stable one.
func (m *MemBackend) AllEndpointDeploys(_ context.Context) ([]EndpointDeploy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []EndpointDeploy
	for id, e := range m.endpoints {
		b, ok := m.branches[e.BranchID]
		if !ok {
			continue
		}
		// No address => never successfully deployed (a failed-create orphan, or a
		// local compute the controller does not place). PgBackend applies
		// `e.address IS NOT NULL`; the reason is there.
		if e.Address == "" {
			continue
		}
		out = append(out, m.endpointDeploy(id, e, b.PgVersion))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
}

// EndpointDeploysByIDs mirrors PgBackend's: the named endpoints, address-less
// ones included, with absent ids simply missing from the result.
func (m *MemBackend) EndpointDeploysByIDs(_ context.Context, ids []uuid.UUID) ([]EndpointDeploy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []EndpointDeploy
	for _, id := range ids {
		e, ok := m.endpoints[id]
		if !ok {
			continue
		}
		b, ok := m.branches[e.BranchID]
		if !ok {
			continue
		}
		out = append(out, m.endpointDeploy(id, e, b.PgVersion))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
}

// EndpointDeploy returns one endpoint's renderable desired state by id, mirroring
// PgBackend: pg_version comes from the endpoint's branch. ErrNotFound when the
// endpoint or its branch is gone — MemBackend deletes branch rows outright rather
// than tombstoning them, so a missing branch covers PgBackend's dropped_at case.
func (m *MemBackend) EndpointDeploy(_ context.Context, endpointID uuid.UUID) (EndpointDeploy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.endpoints[endpointID]
	if !ok {
		return EndpointDeploy{}, ErrNotFound
	}
	b, ok := m.branches[e.BranchID]
	if !ok {
		return EndpointDeploy{}, ErrNotFound
	}
	return m.endpointDeploy(endpointID, e, b.PgVersion), nil
}

// UpdateEndpointExtensions records what an endpoint was last deployed mounting.
//
// It used to be a no-op, on the grounds that the resolved set is recomputed at
// deploy so nothing read it back. That stopped being true when add_branch_extensions
// began skipping endpoints already mounting the branch's set: a backend that drops
// this write reports an empty set for every endpoint, so the skip can never fire and
// any test on it silently exercises the old unconditional redeploy. Sorted and
// deduped to match marshalExtensions, so the two backends agree on what they stored.
func (m *MemBackend) UpdateEndpointExtensions(_ context.Context, endpointID uuid.UUID, names []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.endpointExts[endpointID] = dedupSorted(names)
	return nil
}

// dedupSorted returns names as a sorted, de-duplicated slice (nil for empty).
func dedupSorted(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// EnsureEndpointRecord idempotently records an endpoint with the given id on the
// (clusterID, branchID) branch. A repeated id is a no-op; a *second* read_write
// endpoint (a different id on a branch that already has one) is ErrConflict, as
// is a name already taken on the branch; a missing branch is ErrNotFound.
func (m *MemBackend) EnsureEndpointRecord(_ context.Context, endpointID, clusterID, branchID uuid.UUID, role, name string,
	config, pgSettings, pageserverSettings, safekeeperSettings []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.endpoints[endpointID]; ok {
		return nil // same id => idempotent no-op
	}
	b, ok := m.branches[branchID]
	if !ok || b.ClusterID != clusterID {
		return ErrNotFound
	}
	// Mirror endpoints_name_per_branch. Unnamed endpoints never collide (NULLs
	// are distinct in the real unique index).
	if name != "" {
		for _, ep := range m.endpoints {
			if ep.BranchID == branchID && ep.Name == name {
				return ErrConflict
			}
		}
	}
	if role == RoleReadWrite {
		k := branchKey{clusterID: clusterID, branchID: branchID}
		if _, taken := m.writers[k]; taken {
			return ErrConflict
		}
		m.writers[k] = struct{}{}
	}
	// Mirror PgBackend, which reads these back out of the config jsonb.
	var cfg struct {
		CPU          string   `json:"cpu"`
		Memory       string   `json:"memory"`
		Connectivity string   `json:"connectivity"`
		AllowedCIDRs []string `json:"allowed_cidrs"`
	}
	if len(config) > 0 {
		_ = json.Unmarshal(config, &cfg)
	}
	var settings map[string]string
	if len(pgSettings) > 0 {
		_ = json.Unmarshal(pgSettings, &settings)
	}
	if len(settings) == 0 {
		settings = nil
	}
	m.endpoints[endpointID] = Endpoint{
		ID:                 endpointID,
		ClusterID:          clusterID,
		BranchID:           branchID,
		Role:               role,
		Name:               name,
		PgSettings:         settings,
		PageserverSettings: decodeAnyMap(pageserverSettings),
		SafekeeperSettings: decodeAnyMap(safekeeperSettings),
		CPU:                cfg.CPU,
		Memory:             cfg.Memory,
		Connectivity:       cfg.Connectivity,
		AllowedCIDRs:       cfg.AllowedCIDRs,
		CreatedAt:          m.now(),
	}
	return nil
}

// decodeAnyMap mirrors PgBackend's read-back of a daemon-settings column: absent,
// empty and unparseable all become nil.
func decodeAnyMap(doc []byte) map[string]any {
	if len(doc) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(doc, &out); err != nil || len(out) == 0 {
		return nil
	}
	return out
}

// UpdateEndpointResources merges the new cpu/memory into the stored endpoint,
// mirroring PgBackend's jsonb merge. Absent endpoint is a no-op.
func (m *MemBackend) UpdateEndpointResources(_ context.Context, endpointID uuid.UUID, cpu, memory string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ep, ok := m.endpoints[endpointID]; ok {
		ep.CPU = cpu
		ep.Memory = memory
		m.endpoints[endpointID] = ep
	}
	return nil
}

// SetEndpointYCPGeneration mirrors PgBackend's monotonic write: GREATEST, so a
// task carrying an older generation that completes late cannot drag the value
// backwards. Absent endpoint is a no-op.
func (m *MemBackend) SetEndpointYCPGeneration(_ context.Context, endpointID uuid.UUID, gen int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ep, ok := m.endpoints[endpointID]; ok {
		ep.YCPGeneration = maxYCPGeneration(ep.YCPGeneration, gen)
		m.endpoints[endpointID] = ep
	}
	return nil
}

// SetBranchYCPGeneration is the branch twin. Absent branch is a no-op.
func (m *MemBackend) SetBranchYCPGeneration(_ context.Context, branchID uuid.UUID, gen int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if br, ok := m.branches[branchID]; ok {
		br.YCPGeneration = maxYCPGeneration(br.YCPGeneration, gen)
		m.branches[branchID] = br
	}
	return nil
}

// maxYCPGeneration is the Go half of `GREATEST(COALESCE(col, 0), gen)`: a nil
// current value counts as zero, and the result is always a fresh pointer so two
// rows can never share one.
func maxYCPGeneration(current *int64, gen int64) *int64 {
	if current != nil && *current > gen {
		gen = *current
	}
	return &gen
}

// SetEndpointVersion records the endpoint's version, mirroring PgBackend's
// absolute UPDATE of the row's own column. Absent endpoint is a no-op.
func (m *MemBackend) SetEndpointVersion(_ context.Context, endpointID uuid.UUID, version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ep, ok := m.endpoints[endpointID]; ok {
		ep.Version = version
		m.endpoints[endpointID] = ep
	}
	return nil
}

// SetEndpointTargetVersion records the version the endpoint should run, mirroring
// PgBackend's.
func (m *MemBackend) SetEndpointTargetVersion(_ context.Context, endpointID uuid.UUID, version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ep, ok := m.endpoints[endpointID]; ok {
		ep.TargetVersion = version
		m.endpoints[endpointID] = ep
	}
	return nil
}

// UpdateBranchSettings stores the branch-wide settings the caller named,
// mirroring PgBackend: nil leaves a surface alone, an empty map clears it.
// Absent branch is a no-op.
func (m *MemBackend) UpdateBranchSettings(_ context.Context, branchID uuid.UUID, pg *map[string]string, writeToken *map[string]any, ycpGeneration *int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	br, ok := m.branches[branchID]
	if !ok {
		return nil
	}
	if pg != nil {
		br.PgSettings = orNil(*pg)
	}
	if writeToken != nil {
		br.WriteTokenSettings = orNil(*writeToken)
	}
	if ycpGeneration != nil {
		br.YCPGeneration = maxYCPGeneration(br.YCPGeneration, *ycpGeneration)
	}
	m.branches[branchID] = br
	return nil
}

// UpdateEndpointSettings does the same for one endpoint's three surfaces.
func (m *MemBackend) UpdateEndpointSettings(_ context.Context, endpointID uuid.UUID, pg *map[string]string, pageserver, safekeeper *map[string]any, ycpGeneration *int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ep, ok := m.endpoints[endpointID]
	if !ok {
		return nil
	}
	if pg != nil {
		ep.PgSettings = orNil(*pg)
	}
	if pageserver != nil {
		ep.PageserverSettings = orNil(*pageserver)
	}
	if safekeeper != nil {
		ep.SafekeeperSettings = orNil(*safekeeper)
	}
	if ycpGeneration != nil {
		ep.YCPGeneration = maxYCPGeneration(ep.YCPGeneration, *ycpGeneration)
	}
	m.endpoints[endpointID] = ep
	return nil
}

// orNil normalises a cleared surface to nil, which is how "none set" is spelled
// everywhere else in these structs — so a cleared surface and a never-set one
// read the same, as they do out of the database.
func orNil[T any](m map[string]T) map[string]T {
	if len(m) == 0 {
		return nil
	}
	return m
}

// EndpointIDsForBranch returns the ids of every endpoint on the branch (the
// delete_branch step's Execute-time enumeration). An absent branch yields an
// empty slice.
func (m *MemBackend) EndpointIDsForBranch(_ context.Context, clusterID, branchID uuid.UUID) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []uuid.UUID
	for id, e := range m.endpoints {
		if e.ClusterID == clusterID && e.BranchID == branchID {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// EndpointIDsForCluster returns the ids of every endpoint in the cluster (the
// delete_cluster step's Execute-time enumeration). An absent cluster yields an
// empty slice.
func (m *MemBackend) EndpointIDsForCluster(_ context.Context, clusterID uuid.UUID) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []uuid.UUID
	for id, e := range m.endpoints {
		if e.ClusterID == clusterID {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// ChildBranchIDs returns the ids of every branch in clusterID forked directly
// from branchID (the delete_branch step's pre-cascade check). An absent branch
// yields an empty slice.
func (m *MemBackend) ChildBranchIDs(_ context.Context, clusterID, branchID uuid.UUID) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []uuid.UUID
	for id, b := range m.branches {
		if b.ClusterID == clusterID && b.ParentBranchID != nil && *b.ParentBranchID == branchID {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// DeleteBranch removes the (clusterID, branchID) branch (idempotent; absent row
// is a no-op), keeping m.branchNames and the single-writer m.writers slot
// consistent. A still-referencing child fork makes the delete an ErrConflict
// rather than a silent drop, matching PgBackend's explicit no-live-children
// guard.
//
// PgBackend tombstones (`dropped_at`) where this drops the entry outright. That
// is deliberate: through the Backend interface the two are indistinguishable —
// every read filters tombstones, and the name a drop frees is freed here too
// (branches_name_per_cluster is partial for exactly that reason) — so modeling the
// column would add state without adding coverage. The one behaviour it does not
// reproduce is re-Ensuring a dropped branch under its ORIGINAL id, which the real
// backend converges to a still-dropped row and this would resurrect. Nothing does
// that: the cascade never re-creates what it just retired, and ids are never
// reused.
func (m *MemBackend) DeleteBranch(_ context.Context, clusterID, branchID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.branches[branchID]
	if !ok || b.ClusterID != clusterID {
		return nil // tolerate absent
	}
	for _, child := range m.branches {
		if child.ClusterID == clusterID && child.ParentBranchID != nil && *child.ParentBranchID == branchID {
			return ErrConflict // a child fork still references this branch
		}
	}
	// A still-present endpoint blocks the drop too, matching PgBackend's explicit
	// guard: the caller must retire endpoints first, and an endpoint added since
	// its enumeration has to fail the step so the replay retires it.
	for _, e := range m.endpoints {
		if e.ClusterID == clusterID && e.BranchID == branchID {
			return ErrConflict
		}
	}
	delete(m.branches, branchID)
	delete(m.branchNames, branchNameKey{clusterID: clusterID, name: b.Name})
	delete(m.writers, branchKey{clusterID: clusterID, branchID: branchID})
	return nil
}

// DeleteCluster removes the cluster and all its branches/endpoints (idempotent;
// absent rows are a no-op), mirroring *PgBackend.DeleteCluster: it clears
// m.endpoints/m.branches/m.clusters for the cluster plus the m.writers /
// m.branchNames / m.clusterNames bookkeeping consistently.
func (m *MemBackend) DeleteCluster(_ context.Context, clusterID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.clusters[clusterID]
	if !ok {
		return nil // tolerate absent
	}
	// Mirror PgBackend: a cluster with any live endpoint refuses the cascade, so
	// an endpoint added since the step's enumeration replays instead of leaking.
	for _, e := range m.endpoints {
		if e.ClusterID == clusterID {
			return ErrConflict
		}
	}
	for id, b := range m.branches {
		if b.ClusterID == clusterID {
			delete(m.branches, id)
			delete(m.branchNames, branchNameKey{clusterID: clusterID, name: b.Name})
		}
	}
	delete(m.clusters, clusterID)
	delete(m.clusterNames, clusterNameKey{poolID: p.StoragePoolID, name: p.Name})
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

// --- registered extensions (extensions.Store) --------------------------------
//
// The in-memory twin of amp_registry.extensions. Same contract as PgBackend:
// PutExtension fully replaces, a Get miss is (_, false, nil), and ExtensionUsers
// reports the live branches that opted in — so a test exercises the same refusal
// paths the database backend does.

// ListExtensions returns every registered extension, name-ordered.
func (m *MemBackend) ListExtensions(_ context.Context) ([]extensions.Extension, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.extensions))
	for n := range m.extensions {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]extensions.Extension, 0, len(names))
	for _, n := range names {
		out = append(out, m.extensions[n])
	}
	return out, nil
}

// GetExtension reads one registered extension.
func (m *MemBackend) GetExtension(_ context.Context, name string) (extensions.Extension, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.extensions[name]
	return e, ok, nil
}

// PutExtension creates or fully replaces a registration.
func (m *MemBackend) PutExtension(_ context.Context, e extensions.Extension) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.extensions == nil {
		m.extensions = make(map[string]extensions.Extension)
	}
	m.extensions[e.Name] = e
	return nil
}

// DeleteExtension removes a registration.
func (m *MemBackend) DeleteExtension(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.extensions, name)
	return nil
}

// ExtensionUsers returns the ids of live branches that opted into name.
func (m *MemBackend) ExtensionUsers(_ context.Context, name string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	// A dropped branch is removed from the map here (PgBackend tombstones with
	// dropped_at instead), so everything present is live.
	for id, b := range m.branches {
		for _, n := range b.Extensions {
			if n == name {
				out = append(out, id.String())
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
