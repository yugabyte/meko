// Package core is the controller's domain layer: the resource operations the
// REST controllers (internal/api) call into, behind a Backend interface so the
// HTTP layer can be exercised against an in-memory fake (MemBackend) without a
// database. The real implementation (PgBackend) persists to YBDB, the
// controller's only state store (yblight-controller.md §10).
//
// Scope: users (no auth), storage pools, timeline create (which provisions a
// tenant + its main timeline, #92), and endpoints on a timeline (#108: create
// records the row and the API synchronously deploys it; get/list/delete round
// out the lifecycle). Timeline delete and branch remain logged-only in the API
// for now (no task framework yet), so they have no Backend method.
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

// Timeline is a PG cluster: identity is the (TenantID, TimelineID) pair
// (yblc-highlvl.md §6). Creating a tenant (a "database") creates its main
// timeline at the same time (IsMain), which is what CreateTimeline returns.
type Timeline struct {
	TenantID   uuid.UUID
	TimelineID uuid.UUID
	Name       string
	PgVersion  int
	IsMain     bool
	CreatedAt  time.Time
}

// Endpoint is the durable record of a compute endpoint on a timeline. Address
// is the connection target returned to users; it is empty until the endpoint is
// deployed (set via SetEndpointAddress once Helm/Docker reports the endpoint
// ready). Role is the endpoint_role (read_write | read_only) with the
// single-writer rule enforced in the schema.
type Endpoint struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	TimelineID uuid.UUID
	Role       string
	Address    string
	CreatedAt  time.Time
}

// Endpoint roles (the endpoint_role enum).
const (
	RoleReadWrite = "read_write"
	RoleReadOnly  = "read_only"
)

// ValidRole reports whether role is a known endpoint role.
func ValidRole(role string) bool {
	return role == RoleReadWrite || role == RoleReadOnly
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
	// ListUsers returns a page of users ordered by (created_at, id), and the
	// total number of users (ignoring limit/offset). limit must be >= 1 and
	// offset >= 0 (the API layer clamps them).
	ListUsers(ctx context.Context, limit, offset int) ([]User, int, error)

	// CreateStoragePool creates a pool owned by owner. Returns ErrNotFound if
	// owner does not exist, or ErrConflict if the name is already taken.
	CreateStoragePool(ctx context.Context, owner uuid.UUID, name string) (StoragePool, error)
	// ListStoragePools returns a page of every pool in the placement group
	// ordered by (created_at, id), and the total number of pools (ignoring
	// limit/offset). Owner filtering is out of scope for now (pool sharing is
	// deferred).
	ListStoragePools(ctx context.Context, limit, offset int) ([]StoragePool, int, error)
	// GetStoragePool returns a pool by id, or ErrNotFound. Pools are top-level,
	// so this is not owner-scoped: a pool will eventually be acted on by more
	// than the one user who created it.
	GetStoragePool(ctx context.Context, poolID uuid.UUID) (StoragePool, error)

	// CreateTimeline creates a tenant (a "database") named name in the pool,
	// together with its main timeline at Postgres major version pgVersion, and
	// returns the timeline. Returns ErrNotFound if the pool does not exist, or
	// ErrConflict if a tenant with that name already exists in the pool.
	CreateTimeline(ctx context.Context, poolID uuid.UUID, name string, pgVersion int) (Timeline, error)
	// ListTimelines returns a page of the timelines across all tenants in the
	// pool ordered by (created_at, timeline_id), and the total number of
	// timelines in the pool (ignoring limit/offset). Returns ErrNotFound if the
	// pool does not exist.
	ListTimelines(ctx context.Context, poolID uuid.UUID, limit, offset int) ([]Timeline, int, error)

	// GetTimeline returns the timeline addressed by (poolID, timelineID),
	// validating that the timeline's tenant belongs to the pool. Pools are
	// top-level, so this is not owner-scoped. Returns ErrNotFound if any link is
	// missing.
	GetTimeline(ctx context.Context, poolID, timelineID uuid.UUID) (Timeline, error)

	// CreateEndpoint records an endpoint on the (tenantID, timelineID) timeline.
	// config is the deploy spec stored for (re)creation. The address is set
	// later (SetEndpointAddress) once the endpoint is deployed. Returns
	// ErrNotFound if the timeline does not exist, or ErrConflict if a read_write
	// endpoint already exists for the timeline (the single-writer rule).
	CreateEndpoint(ctx context.Context, tenantID, timelineID uuid.UUID, role string, config []byte) (Endpoint, error)

	// GetEndpoint returns the endpoint addressed by (poolID, timelineID,
	// endpointID), validating the pool->timeline->endpoint chain by ids. Pools
	// are top-level, so this is not owner-scoped. Returns ErrNotFound if any
	// link is missing.
	GetEndpoint(ctx context.Context, poolID, timelineID, endpointID uuid.UUID) (Endpoint, error)

	// ListEndpoints returns a page of the endpoints bound to the timeline
	// (addressed by its id within the pool) ordered by (created_at, id), and the
	// total number of endpoints for the timeline (ignoring limit/offset).
	// Returns ErrNotFound if the pool or the timeline does not exist.
	ListEndpoints(ctx context.Context, poolID, timelineID uuid.UUID, limit, offset int) ([]Endpoint, int, error)

	// SetEndpointAddress records an endpoint's client address. Idempotent
	// (absolute UPDATE keyed by id); a missing endpoint is not an error.
	SetEndpointAddress(ctx context.Context, endpointID uuid.UUID, address string) error

	// DeleteEndpoint removes an endpoint record. Idempotent: deleting an absent
	// row is a no-op.
	DeleteEndpoint(ctx context.Context, endpointID uuid.UUID) error
}
