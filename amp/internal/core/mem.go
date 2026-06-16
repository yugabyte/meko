package core

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemBackend is an in-memory Backend that mirrors PgBackend's semantics (owner
// existence checks, globally-unique pool names, owner-scoped listing). It lets
// the API layer be tested end-to-end — through the generated client and the
// real controllers — without a database. It is safe for concurrent use.
type MemBackend struct {
	mu    sync.Mutex
	users map[uuid.UUID]User
	pools map[uuid.UUID]StoragePool
	names map[string]struct{} // pool names are unique across the placement group
	now   func() time.Time
}

// NewMemBackend returns an empty in-memory backend.
func NewMemBackend() *MemBackend {
	return &MemBackend{
		users: make(map[uuid.UUID]User),
		pools: make(map[uuid.UUID]StoragePool),
		names: make(map[string]struct{}),
		now:   time.Now,
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

func (m *MemBackend) ListStoragePools(_ context.Context, owner uuid.UUID) ([]StoragePool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[owner]; !ok {
		return nil, ErrNotFound
	}
	var pools []StoragePool
	for _, p := range m.pools {
		if p.OwnerUserID == owner {
			pools = append(pools, p)
		}
	}
	sortPoolsByCreatedAt(pools)
	return pools, nil
}

func (m *MemBackend) GetStoragePool(_ context.Context, owner, poolID uuid.UUID) (StoragePool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pools[poolID]
	if !ok || p.OwnerUserID != owner {
		return StoragePool{}, ErrNotFound
	}
	return p, nil
}

func sortPoolsByCreatedAt(pools []StoragePool) {
	// Insertion sort keeps a stable, dependency-free ordering matching the
	// pg backend's ORDER BY created_at, id (lists are tiny in tests).
	for i := 1; i < len(pools); i++ {
		for j := i; j > 0 && less(pools[j], pools[j-1]); j-- {
			pools[j], pools[j-1] = pools[j-1], pools[j]
		}
	}
}

func less(a, b StoragePool) bool {
	if a.CreatedAt.Equal(b.CreatedAt) {
		return a.ID.String() < b.ID.String()
	}
	return a.CreatedAt.Before(b.CreatedAt)
}
