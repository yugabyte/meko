// Package core is the controller's domain layer: the resource operations the
// REST controllers (internal/api) call into, behind a Backend interface so the
// HTTP layer can be exercised against an in-memory fake (MemBackend) without a
// database. The real implementation (PgBackend) persists to YBDB, the
// controller's only state store (yblight-controller.md §10).
//
// Scope is issue #92's initial surface: users (no auth) and storage pools.
// Timelines are logged-only in the API for now (no task framework yet), so they
// have no Backend method.
package core

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors the controllers map onto HTTP status codes. Backends return
// these; the API layer translates ErrNotFound -> 404 and ErrConflict -> 409.
var (
	// ErrNotFound is returned when an addressed resource (or a required parent,
	// such as the owning user) does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict is returned when a request collides with existing state, such
	// as a storage-pool name that is already taken.
	ErrConflict = errors.New("conflict")
)

// User is a control-plane user. There is no authentication yet (#92); a user is
// just an identity that owns storage pools.
type User struct {
	ID        uuid.UUID
	IsAdmin   bool
	CreatedAt time.Time
}

// StoragePool is a grouping of tenants, owned by the user that created it
// (yblight-controller.md §4.1). Sharing (pool grants) is out of scope for #92.
type StoragePool struct {
	ID          uuid.UUID
	Name        string
	OwnerUserID uuid.UUID
	CreatedAt   time.Time
}

// Backend is the controller's state store. Every method must be safe to call
// concurrently.
type Backend interface {
	// Healthy reports whether the state store is reachable (readiness probe).
	Healthy(ctx context.Context) error

	// CreateUser creates a user and returns it.
	CreateUser(ctx context.Context, isAdmin bool) (User, error)
	// GetUser returns a user by id, or ErrNotFound.
	GetUser(ctx context.Context, id uuid.UUID) (User, error)

	// CreateStoragePool creates a pool owned by owner. Returns ErrNotFound if
	// owner does not exist, or ErrConflict if the name is already taken.
	CreateStoragePool(ctx context.Context, owner uuid.UUID, name string) (StoragePool, error)
	// ListStoragePools returns the pools owned by owner (no sharing, #92).
	// Returns ErrNotFound if owner does not exist.
	ListStoragePools(ctx context.Context, owner uuid.UUID) ([]StoragePool, error)
	// GetStoragePool returns a pool owned by owner, or ErrNotFound if it does
	// not exist or is not owned by that user (which hides its existence).
	GetStoragePool(ctx context.Context, owner, poolID uuid.UUID) (StoragePool, error)
}
