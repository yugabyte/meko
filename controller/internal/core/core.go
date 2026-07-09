// Package core is the controller's domain layer: the resource operations the
// REST controllers (internal/api) call into, behind a Backend interface so the
// HTTP layer can be exercised against an in-memory fake (MemBackend) without a
// database. The real implementation (PgBackend) persists to YBDB, the
// controller's only state store (amp-controller.md §10).
//
// The user-facing resource ladder is storage pool -> project -> branch ->
// endpoint. The storage layer is flat: a branch's BranchID IS the storage-layer
// BranchId (there is no enclosing tenant), kept verbatim. A project is a
// control-plane grouping ("a database") that owns a set of branches and maps to
// no storage object.
//
// Scope: users (no auth), storage pools, project create (which provisions the
// project and its default branch, #92) and branch (which forks an existing
// branch, #130), and endpoints on a branch (#108). The six mutations
// (create/delete of project, branch, endpoint) now run as write-ahead tasks
// (#113): the handlers return 202 + a Task and the deploy/cascade work happens
// in the worker's steps. The synchronous Backend methods here serve the read
// paths, the create-row records, and the handlers' cheap 404 pre-checks.
// Cascade delete of branches/projects drops rows via the task StateStore
// primitives (DeleteBranch/DeleteProject), not core.Backend, which has no
// delete-branch/delete-project method.
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

// StoragePool is a grouping of projects, owned by the user that created it
// (amp-controller.md §4.1). Sharing (pool grants) is out of scope for #92.
type StoragePool struct {
	ID          uuid.UUID
	Name        string
	OwnerUserID uuid.UUID
	CreatedAt   time.Time
}

// Project is a "database" under a storage pool: a control-plane grouping that
// owns a set of branches (ac-highlvl.md §6). ID is a control-plane id only — no
// storage object maps to a project. Creating a project creates its default branch
// at the same time, returned in DefaultBranch (always populated on reads). A
// project has exactly one default branch and zero or more branches off it.
type Project struct {
	ID            uuid.UUID // control-plane id (maps to no storage object)
	StoragePoolID uuid.UUID
	Name          string
	CreatedAt     time.Time
	// DefaultBranch is the project's root branch (IsDefault). It is created with
	// the project and always exists; reads populate it.
	DefaultBranch Branch
}

// Branch is a PG cluster. BranchID is the storage-layer BranchId verbatim (the
// flat storage identity). The product identity is the (ProjectID, BranchID)
// pair, which scopes lineage to a project; the storage layer keys on BranchID
// alone. The project's first branch is its default (IsDefault); CreateBranch
// forks an existing branch into a non-default one.
type Branch struct {
	ProjectID uuid.UUID // control-plane grouping (maps to no storage object)
	BranchID  uuid.UUID // the storage-layer BranchId
	Name      string
	PgVersion int
	IsDefault bool
	// ParentBranchID is the in-project branch this one was forked from, or nil
	// for the project's default branch.
	ParentBranchID *uuid.UUID
	// ForkLSN is the ancestor LSN the branch was taken at (the storage layer's
	// "X/X" pg_lsn form), or "" for the default branch.
	ForkLSN   string
	CreatedAt time.Time
}

// Endpoint is the durable record of a compute endpoint on a branch. Address
// is the connection target returned to users; it is empty until the endpoint is
// deployed (set via SetEndpointAddress once Helm/Docker reports the endpoint
// ready). Role is the endpoint_role (read_write | read_only) with the
// single-writer rule enforced in the schema.
type Endpoint struct {
	ID        uuid.UUID
	ProjectID uuid.UUID
	BranchID  uuid.UUID
	Role      string
	Address   string
	CreatedAt time.Time
}

// DefaultBranchName is the name given to a project's default (root) branch. The
// project carries its own user-chosen name; its default branch is always "main"
// (the trunk line of history), independent of the project name, matching the
// convention users expect from git/Neon. Forks off it choose their own names.
const DefaultBranchName = "main"

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

	// CreateUser creates a user (auto-generated id) and returns it.
	CreateUser(ctx context.Context, isAdmin bool) (User, error)
	// EnsureUser creates a user with an explicit id (e.g. an external identity
	// subject) or, if one with that id already exists, returns it unchanged.
	// Idempotent — the integration path calls it on every request.
	EnsureUser(ctx context.Context, id uuid.UUID, isAdmin bool) (User, error)
	// GetUser returns a user by id, or ErrNotFound.
	GetUser(ctx context.Context, id uuid.UUID) (User, error)
	// ListUsers returns a page of users ordered by (created_at, id), and the
	// total number of users (ignoring limit/offset). limit must be >= 1 and
	// offset >= 0 (the API layer clamps them).
	ListUsers(ctx context.Context, limit, offset int) ([]User, int, error)

	// CreateStoragePool creates a pool (auto-generated id) owned by owner.
	// Returns ErrNotFound if owner does not exist, or ErrConflict if the name is
	// already taken.
	CreateStoragePool(ctx context.Context, owner uuid.UUID, name string) (StoragePool, error)
	// CreateStoragePoolWithID creates a pool with an explicit id (e.g. a Meko
	// datapack UUID), or returns the existing pool if that id is already present
	// (idempotent). Returns ErrNotFound if owner does not exist, or ErrConflict
	// if the name is taken by a different pool.
	CreateStoragePoolWithID(ctx context.Context, id, owner uuid.UUID, name string) (StoragePool, error)
	// ListStoragePools returns a page of every pool in the placement group
	// ordered by (created_at, id), and the total number of pools (ignoring
	// limit/offset). Owner filtering is out of scope for now (pool sharing is
	// deferred).
	ListStoragePools(ctx context.Context, limit, offset int) ([]StoragePool, int, error)
	// GetStoragePool returns a pool by id, or ErrNotFound. Pools are top-level,
	// so this is not owner-scoped: a pool will eventually be acted on by more
	// than the one user who created it.
	GetStoragePool(ctx context.Context, poolID uuid.UUID) (StoragePool, error)

	// CreateProject creates a project (a "database") named name in the pool,
	// together with its default branch at Postgres major version pgVersion, and
	// returns the project with DefaultBranch populated. Returns ErrNotFound if
	// the pool does not exist, or ErrConflict if a project with that name already
	// exists in the pool.
	CreateProject(ctx context.Context, poolID uuid.UUID, name string, pgVersion int) (Project, error)
	// ListProjects returns a page of the projects in the pool ordered by
	// (created_at, id), each with DefaultBranch populated, and the total number
	// of projects in the pool (ignoring limit/offset). Returns ErrNotFound if
	// the pool does not exist.
	ListProjects(ctx context.Context, poolID uuid.UUID, limit, offset int) ([]Project, int, error)
	// GetProject returns the project addressed by (poolID, projectID) with
	// DefaultBranch populated, validating that the project belongs to the pool.
	// Returns ErrNotFound if any link is missing.
	GetProject(ctx context.Context, poolID, projectID uuid.UUID) (Project, error)

	// CreateBranch forks the branch addressed by (poolID, projectID,
	// parentBranchID) into a new branch in the same project, named name and
	// recording its fork ancestry (parent_branch_id, fork_lsn). The Postgres
	// version is inherited from the parent. forkLSN is the ancestor LSN to
	// branch at (the storage layer's "X/X" pg_lsn form). Returns ErrNotFound if
	// the pool, project, or parent branch does not exist, or ErrConflict if a
	// branch named name already exists in the project.
	CreateBranch(ctx context.Context, poolID, projectID, parentBranchID uuid.UUID, name, forkLSN string) (Branch, error)
	// ListBranches returns a page of the branches in the project ordered by
	// (created_at, branch_id), and the total number of branches in the project
	// (ignoring limit/offset). Returns ErrNotFound if the pool or project does
	// not exist.
	ListBranches(ctx context.Context, poolID, projectID uuid.UUID, limit, offset int) ([]Branch, int, error)
	// GetBranch returns the branch addressed by (poolID, projectID, branchID),
	// validating the pool->project->branch chain by ids. Returns ErrNotFound if
	// any link is missing.
	GetBranch(ctx context.Context, poolID, projectID, branchID uuid.UUID) (Branch, error)

	// CreateEndpoint records an endpoint on the (projectID, branchID) branch.
	// config is the deploy spec stored for (re)creation. The address is set
	// later (SetEndpointAddress) once the endpoint is deployed. Returns
	// ErrNotFound if the branch does not exist, or ErrConflict if a read_write
	// endpoint already exists for the branch (the single-writer rule).
	CreateEndpoint(ctx context.Context, projectID, branchID uuid.UUID, role string, config []byte) (Endpoint, error)

	// GetEndpoint returns the endpoint addressed by (poolID, projectID,
	// branchID, endpointID), validating the pool->project->branch->endpoint
	// chain by ids. Returns ErrNotFound if any link is missing.
	GetEndpoint(ctx context.Context, poolID, projectID, branchID, endpointID uuid.UUID) (Endpoint, error)

	// ListEndpoints returns a page of the endpoints bound to the branch
	// (addressed by (poolID, projectID, branchID)) ordered by (created_at, id),
	// and the total number of endpoints for the branch (ignoring limit/offset).
	// Returns ErrNotFound if the pool, project, or branch does not exist.
	ListEndpoints(ctx context.Context, poolID, projectID, branchID uuid.UUID, limit, offset int) ([]Endpoint, int, error)

	// SetEndpointAddress records an endpoint's client address. Idempotent
	// (absolute UPDATE keyed by id); a missing endpoint is not an error.
	SetEndpointAddress(ctx context.Context, endpointID uuid.UUID, address string) error

	// DeleteEndpoint removes an endpoint record. Idempotent: deleting an absent
	// row is a no-op.
	DeleteEndpoint(ctx context.Context, endpointID uuid.UUID) error
}
