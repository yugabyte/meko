package core

import (
	"context"
	"encoding/json"
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
	clusters     map[uuid.UUID]Cluster
	branches     map[uuid.UUID]Branch
	clusterNames map[clusterNameKey]struct{} // cluster names are unique within their pool
	branchNames  map[branchNameKey]struct{}  // branch names are unique within their cluster
	endpoints    map[uuid.UUID]Endpoint
	writers      map[branchKey]struct{} // at most one read_write endpoint per branch
	now          func() time.Time
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
		names:        make(map[string]struct{}),
		clusters:     make(map[uuid.UUID]Cluster),
		branches:     make(map[uuid.UUID]Branch),
		clusterNames: make(map[clusterNameKey]struct{}),
		branchNames:  make(map[branchNameKey]struct{}),
		endpoints:    make(map[uuid.UUID]Endpoint),
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
func (m *MemBackend) EnsureCluster(_ context.Context, poolID, clusterID, branchID uuid.UUID, name string, pgVersion int) error {
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
	def := Branch{
		ClusterID: clusterID,
		BranchID:  branchID,
		Name:      DefaultBranchName,
		PgVersion: pgVersion,
		IsDefault: true,
		CreatedAt: now,
	}
	p := Cluster{ID: clusterID, StoragePoolID: poolID, Name: name, CreatedAt: now, DefaultBranch: def}
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
	// A fork inherits a snapshot of the parent's extension set (copied, so a
	// later change to either side doesn't alias the other).
	var inherited []string
	if len(parent.Extensions) > 0 {
		inherited = append([]string(nil), parent.Extensions...)
	}
	m.branches[branchID] = Branch{
		ClusterID:      clusterID,
		BranchID:       branchID,
		Name:           name,
		PgVersion:      pgVersion,
		IsDefault:      false,
		ParentBranchID: &pid,
		ForkLSN:        forkLSN,
		CreatedAt:      m.now(),
		Extensions:     inherited,
	}
	m.branchNames[nk] = struct{}{}
	return nil
}

// BranchExtensions returns the branch's opted-in extension names. An absent
// branch yields an empty slice, not an error (mirrors PgBackend).
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

func (m *MemBackend) BranchExtensions(_ context.Context, clusterID, branchID uuid.UUID) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.branches[branchID]
	if !ok || b.ClusterID != clusterID {
		return nil, nil
	}
	if len(b.Extensions) == 0 {
		return nil, nil
	}
	return append([]string(nil), b.Extensions...), nil
}

// AddBranchExtensions adds names to the branch's durable extension set (absolute
// union, existing-first order preserved). Idempotent; an absent branch or empty
// names is a no-op. Mirrors PgBackend.
func (m *MemBackend) AddBranchExtensions(_ context.Context, clusterID, branchID uuid.UUID, names []string) error {
	if len(names) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.branches[branchID]
	if !ok || b.ClusterID != clusterID {
		return nil
	}
	seen := make(map[string]struct{}, len(b.Extensions)+len(names))
	union := make([]string, 0, len(b.Extensions)+len(names))
	for _, n := range append(append([]string(nil), b.Extensions...), names...) {
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		union = append(union, n)
	}
	b.Extensions = union
	m.branches[branchID] = b
	return nil
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
	// Mirror PgBackend, which reads cpu/memory back out of the config jsonb.
	var cfg struct {
		CPU    string `json:"cpu"`
		Memory string `json:"memory"`
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
