package core

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemBackend is an in-memory Backend that mirrors PgBackend's semantics (owner
// existence checks, globally-unique pool names, top-level pools, paginated
// lists). It lets the API layer be tested end-to-end — through the generated
// client and the real controllers — without a database. It is safe for
// concurrent use.
type MemBackend struct {
	mu           sync.Mutex
	users        map[uuid.UUID]User
	pools        map[uuid.UUID]StoragePool
	names        map[string]struct{} // pool names are unique across the placement group
	timelines    map[uuid.UUID]Timeline
	timelinePool map[uuid.UUID]uuid.UUID    // timeline id -> the pool it lives in (pg joins tenant.storage_pool_id)
	tenantNames  map[tenantNameKey]struct{} // tenant names are unique within their pool
	endpoints    map[uuid.UUID]Endpoint
	writers      map[timelineKey]struct{} // at most one read_write endpoint per timeline
	now          func() time.Time
}

// tenantNameKey makes a tenant name unique within its pool (the schema's
// UNIQUE (storage_pool_id, name)).
type tenantNameKey struct {
	poolID uuid.UUID
	name   string
}

// timelineKey identifies a timeline (the schema's (tenant_id, timeline_id) PK).
type timelineKey struct {
	tenantID   uuid.UUID
	timelineID uuid.UUID
}

// NewMemBackend returns an empty in-memory backend.
func NewMemBackend() *MemBackend {
	return &MemBackend{
		users:        make(map[uuid.UUID]User),
		pools:        make(map[uuid.UUID]StoragePool),
		names:        make(map[string]struct{}),
		timelines:    make(map[uuid.UUID]Timeline),
		timelinePool: make(map[uuid.UUID]uuid.UUID),
		tenantNames:  make(map[tenantNameKey]struct{}),
		endpoints:    make(map[uuid.UUID]Endpoint),
		writers:      make(map[timelineKey]struct{}),
		now:          time.Now,
	}
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

func (m *MemBackend) CreateTimeline(_ context.Context, poolID uuid.UUID, name string, pgVersion int) (Timeline, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pools[poolID]; !ok {
		return Timeline{}, ErrNotFound
	}
	key := tenantNameKey{poolID: poolID, name: name}
	if _, taken := m.tenantNames[key]; taken {
		return Timeline{}, ErrConflict
	}
	t := Timeline{
		TenantID:   uuid.New(),
		TimelineID: uuid.New(),
		Name:       name,
		PgVersion:  pgVersion,
		IsMain:     true,
		CreatedAt:  m.now(),
	}
	m.timelines[t.TimelineID] = t
	m.timelinePool[t.TimelineID] = poolID
	m.tenantNames[key] = struct{}{}
	return t, nil
}

func (m *MemBackend) ListTimelines(_ context.Context, poolID uuid.UUID, limit, offset int) ([]Timeline, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pools[poolID]; !ok {
		return nil, 0, ErrNotFound
	}
	var timelines []Timeline
	for id, tl := range m.timelines {
		if m.timelinePool[id] == poolID {
			timelines = append(timelines, tl)
		}
	}
	sort.SliceStable(timelines, func(i, j int) bool {
		return createdBefore(timelines[i].CreatedAt, timelines[j].CreatedAt, timelines[i].TimelineID, timelines[j].TimelineID)
	})
	return page(timelines, limit, offset), len(timelines), nil
}

func (m *MemBackend) CreateEndpoint(_ context.Context, tenantID, timelineID uuid.UUID, role, address string) (Endpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tl, ok := m.timelines[timelineID]
	if !ok || tl.TenantID != tenantID {
		return Endpoint{}, ErrNotFound
	}
	if role == "read_write" {
		k := timelineKey{tenantID: tenantID, timelineID: timelineID}
		if _, taken := m.writers[k]; taken {
			return Endpoint{}, ErrConflict
		}
		m.writers[k] = struct{}{}
	}
	e := Endpoint{
		ID:         uuid.New(),
		TenantID:   tenantID,
		TimelineID: timelineID,
		Role:       role,
		Address:    address,
		CreatedAt:  m.now(),
	}
	m.endpoints[e.ID] = e
	return e, nil
}

func (m *MemBackend) ListEndpoints(_ context.Context, poolID, timelineID uuid.UUID, limit, offset int) ([]Endpoint, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pools[poolID]; !ok {
		return nil, 0, ErrNotFound
	}
	// The timeline must live in the addressed pool, mirroring the pg backend's
	// join through tenant.storage_pool_id.
	if m.timelinePool[timelineID] != poolID {
		return nil, 0, ErrNotFound
	}
	var endpoints []Endpoint
	for _, e := range m.endpoints {
		if e.TimelineID == timelineID {
			endpoints = append(endpoints, e)
		}
	}
	sort.SliceStable(endpoints, func(i, j int) bool {
		return createdBefore(endpoints[i].CreatedAt, endpoints[j].CreatedAt, endpoints[i].ID, endpoints[j].ID)
	})
	return page(endpoints, limit, offset), len(endpoints), nil
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
