// Package core is the controller's domain layer: the resource operations the
// REST controllers (internal/api) call into, behind a Backend interface so the
// HTTP layer can be exercised against an in-memory fake (MemBackend) without a
// database. The real implementation (PgBackend) persists to YBDB, the
// controller's only state store (amp-controller.md §10).
//
// The user-facing resource ladder is storage pool -> cluster -> branch ->
// endpoint. The storage layer is flat: a branch's BranchID IS the storage-layer
// BranchId (there is no enclosing tenant), kept verbatim. A cluster is a
// control-plane grouping ("a database") that owns a set of branches and maps to
// no storage object.
//
// Scope: users (no auth), storage pools, cluster create (which provisions the
// cluster and its default branch, #92) and branch (which forks an existing
// branch, #130), and endpoints on a branch (#108). The six mutations
// (create/delete of cluster, branch, endpoint) now run as write-ahead tasks
// (#113): the handlers return 202 + a Task and the deploy/cascade work happens
// in the worker's steps. The synchronous Backend methods here serve the read
// paths, the create-row records, and the handlers' cheap 404 pre-checks.
// Cascade delete of branches/clusters drops rows via the task StateStore
// primitives (DeleteBranch/DeleteCluster), not core.Backend, which has no
// delete-branch/delete-cluster method.
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

// StoragePool is a grouping of clusters, owned by the user that created it
// (amp-controller.md §4.1). Sharing (pool grants) is out of scope for #92.
type StoragePool struct {
	ID          uuid.UUID
	Name        string
	OwnerUserID uuid.UUID
	CreatedAt   time.Time
}

// Cluster is a "database" under a storage pool: a control-plane grouping that
// owns a set of branches (ac-highlvl.md §6). ID is a control-plane id only — no
// storage object maps to a cluster. Creating a cluster creates its default branch
// at the same time, returned in DefaultBranch (always populated on reads). A
// cluster has exactly one default branch and zero or more branches off it.
type Cluster struct {
	ID            uuid.UUID // control-plane id (maps to no storage object)
	StoragePoolID uuid.UUID
	Name          string
	CreatedAt     time.Time
	// Engine is the named extension bundle the cluster was provisioned as
	// ("postgres", "aidb", "oracle"). Provenance, not a constraint: it seeds the
	// default branch's extensions at create and is never re-derived. Empty only
	// on a row written before the engine column existed.
	Engine string
	// DefaultBranch is the cluster's root branch (IsDefault). It is created with
	// the cluster and always exists; reads populate it.
	DefaultBranch Branch
}

// DefaultEngine is the engine a cluster gets when the create request names none.
// It mirrors extensions.EnginePostgres — core deliberately does not import the
// catalog (the `extensions []string` params would shadow it), so
// TestDefaultEngineMatchesCatalog keeps the two in step.
const DefaultEngine = "postgres"

// normalizeEngine defaults an unset engine. The column is NOT NULL, and an
// explicit "" would bypass its DEFAULT.
func normalizeEngine(engine string) string {
	if engine == "" {
		return DefaultEngine
	}
	return engine
}

// Branch is one logical Postgres database's data — what Postgres itself calls a
// cluster. BranchID is the storage-layer BranchId verbatim (the
// flat storage identity). The product identity is the (ClusterID, BranchID)
// pair, which scopes lineage to a cluster; the storage layer keys on BranchID
// alone. The cluster's first branch is its default (IsDefault); CreateBranch
// forks an existing branch into a non-default one.
type Branch struct {
	ClusterID uuid.UUID // control-plane grouping (maps to no storage object)
	BranchID  uuid.UUID // the storage-layer BranchId
	Name      string
	PgVersion int
	IsDefault bool
	// ParentBranchID is the in-cluster branch this one was forked from, or nil
	// for the cluster's default branch.
	ParentBranchID *uuid.UUID
	// ForkLSN is the ancestor LSN the branch was taken at (the storage layer's
	// "X/X" pg_lsn form), or "" for the default branch.
	ForkLSN   string
	CreatedAt time.Time
	// Extensions is the branch's opted-in externalized extension set (beyond the
	// catalog defaults). A fork inherits its parent's set. See
	// compute/architecture/extensions.md.
	Extensions []string
	// PgSettings are extra PostgreSQL settings applied to every compute on this
	// branch. A fork inherits its parent's, and an endpoint's own settings merge
	// over these (see Endpoint.PgSettings). Nil when none are set.
	PgSettings map[string]string
	// WriteTokenSettings is how the supervisor holds this branch's write lease
	// (`ttl_ms`, `renew_seconds`). On the branch because the lease is one row per
	// branch and every endpoint on it contends for that row. Nil when unset.
	WriteTokenSettings map[string]any
	// YCPGeneration is the highest YCP desired-state generation fully applied to
	// this branch, echoed back so the reconcile agent can tell an applied update
	// from one whose async task never finished (meko#985). Written by the worker
	// once every step of the task carrying it succeeded, never from the 202. Nil
	// when no YCP update has been applied — a resource YCP does not manage, or one
	// it has only ever created.
	YCPGeneration *int64
}

// EffectivePgSettings is the settings an endpoint's compute actually runs with:
// its branch's baseline with the endpoint's own merged over it. A key the
// endpoint defines wins, which is what makes branch-wide policy overridable per
// endpoint (one read replica on a different work_mem than its writer).
//
// Returns nil when neither side has any, so a caller can treat "no settings" as
// one case rather than an empty map.
func EffectivePgSettings(branch, endpoint map[string]string) map[string]string {
	if len(branch) == 0 && len(endpoint) == 0 {
		return nil
	}
	out := make(map[string]string, len(branch)+len(endpoint))
	for k, v := range branch {
		out[k] = v
	}
	for k, v := range endpoint {
		out[k] = v
	}
	return out
}

// Endpoint is the durable record of a compute endpoint on a branch. Address
// is the connection target returned to users; it is empty until the endpoint is
// deployed (set via SetEndpointAddress once Helm/Docker reports the endpoint
// ready). Role is the endpoint_role (read_write | read_only) with the
// single-writer rule enforced in the schema.
type Endpoint struct {
	ID        uuid.UUID
	ClusterID uuid.UUID
	BranchID  uuid.UUID
	Role      string
	// Name is the caller's handle for this endpoint, unique among its branch's
	// endpoints. Empty when the caller did not choose one — an endpoint is always
	// addressable by ID.
	Name string
	// PgSettings are this endpoint's own extra PostgreSQL settings. The
	// EFFECTIVE set for its compute is the branch's merged with these, with these
	// winning on a key both define — see EffectivePgSettings. Nil when none are
	// set.
	PgSettings map[string]string
	// PageserverSettings and SafekeeperSettings are this endpoint's overrides for
	// the two storage daemons in its pod, merged into their toml at pod start.
	// Unlike PgSettings there is no branch-level equivalent to merge with, so what
	// is here is the whole story. Nil when none are set.
	PageserverSettings map[string]any
	SafekeeperSettings map[string]any
	// Extended is the caller's opaque extended_config, stored and returned
	// verbatim. The control plane never looks inside it; it exists so whatever
	// deploys the compute can carry what the API does not model (the ports a
	// locally-deployed compute binds). Nil when the endpoint has none.
	Extended []byte
	Address  string
	// TailnetAddress is the endpoint's Tailscale (tailnet) connection target,
	// set alongside Address when the endpoint is exposed on the tailnet; empty
	// otherwise.
	TailnetAddress string
	// CPU and Memory are the endpoint's current compute reservation (Kubernetes
	// quantities), read from the stored config. Empty when the config predates
	// resource tracking. Surfaced on the API so callers see the current size and
	// can resize relative to it.
	CPU       string
	Memory    string
	CreatedAt time.Time
	// YCPGeneration is the highest YCP desired-state generation fully applied to
	// this endpoint, echoed back so the reconcile agent can tell an applied update
	// from one whose async task never finished (meko#985). Written by the worker
	// once every step of the task carrying it succeeded, never from the 202. Nil
	// when no YCP update has been applied — a resource YCP does not manage, or one
	// it has only ever created.
	YCPGeneration *int64
}

// EndpointDeploy is the minimal record the add_branch_extensions step needs to
// re-render an endpoint's deploy spec so it remounts a changed extension set: the
// endpoint id, the branch it serves, and its pinned pg version + compute size
// (read back from the stored config). It is a core type so *PgBackend can return
// it while task.StateStore stays free of the domain layer.
type EndpointDeploy struct {
	ID        uuid.UUID
	BranchID  uuid.UUID
	PgVersion int
	CPU       string
	Memory    string
}

// DefaultBranchName is the name given to a cluster's default (root) branch. The
// cluster carries its own user-chosen name; its default branch is always "main"
// (the trunk line of history), independent of the cluster name, matching the
// convention users expect from git/Neon. Forks off it choose their own names.
//
// The default branch's row is also where a cluster's externalized-extension set
// lives (Branch.Extensions on the is_default row): the set is cluster-wide, and
// every endpoint on any branch resolves against it. See
// compute/architecture/extensions.md.
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
	// and live (idempotent). Returns ErrNotFound if owner does not exist, or
	// ErrConflict if the name is taken by a different live pool -- or if the id
	// belongs to a deleted one, since a tombstone keeps the primary key and only
	// names are reusable.
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
	// RenameStoragePool renames the pool and returns it. Returns ErrNotFound if
	// the pool does not exist, or ErrConflict if another live pool holds the name.
	RenameStoragePool(ctx context.Context, poolID uuid.UUID, name string) (StoragePool, error)
	// DeleteStoragePool tombstones an empty pool, freeing its name. Returns
	// ErrNotFound if the pool does not exist, or ErrConflict if it still has a
	// live cluster. Tombstoned clusters do not block the delete: they are
	// unrecoverable history that keeps a FK to the pool, so the pool row outlives
	// them rather than being removed.
	DeleteStoragePool(ctx context.Context, poolID uuid.UUID) error

	// CreateCluster creates a cluster (a "database") named name in the pool,
	// together with its default branch at Postgres major version pgVersion, and
	// returns the cluster with DefaultBranch populated. Returns ErrNotFound if
	// the pool does not exist, or ErrConflict if a cluster with that name already
	// exists in the pool.
	CreateCluster(ctx context.Context, poolID uuid.UUID, name string, pgVersion int) (Cluster, error)
	// ListClusters returns a page of the clusters in the pool ordered by
	// (created_at, id), each with DefaultBranch populated, and the total number
	// of clusters in the pool (ignoring limit/offset). Returns ErrNotFound if
	// the pool does not exist. When includeFailed is false, clusters whose
	// create_cluster task failed (orphans left mid-provisioning) are excluded
	// from both the page and the total; when true they are included.
	ListClusters(ctx context.Context, poolID uuid.UUID, limit, offset int, includeFailed bool) ([]Cluster, int, error)
	// GetCluster returns the cluster addressed by (poolID, clusterID) with
	// DefaultBranch populated, validating that the cluster belongs to the pool.
	// Returns ErrNotFound if any link is missing.
	GetCluster(ctx context.Context, poolID, clusterID uuid.UUID) (Cluster, error)

	// CreateBranch forks the branch addressed by (poolID, clusterID,
	// parentBranchID) into a new branch in the same cluster, named name and
	// recording its fork ancestry (parent_branch_id, fork_lsn). The Postgres
	// version is inherited from the parent. forkLSN is the ancestor LSN to
	// branch at (the storage layer's "X/X" pg_lsn form). Returns ErrNotFound if
	// the pool, cluster, or parent branch does not exist, or ErrConflict if a
	// branch named name already exists in the cluster.
	CreateBranch(ctx context.Context, poolID, clusterID, parentBranchID uuid.UUID, name, forkLSN string) (Branch, error)
	// ListBranches returns a page of the branches in the cluster ordered by
	// (created_at, branch_id), and the total number of branches in the cluster
	// (ignoring limit/offset). Returns ErrNotFound if the pool or cluster does
	// not exist.
	ListBranches(ctx context.Context, poolID, clusterID uuid.UUID, limit, offset int) ([]Branch, int, error)
	// GetBranch returns the branch addressed by (poolID, clusterID, branchID),
	// validating the pool->cluster->branch chain by ids. Returns ErrNotFound if
	// any link is missing.
	GetBranch(ctx context.Context, poolID, clusterID, branchID uuid.UUID) (Branch, error)

	// CreateEndpoint records an endpoint on the (clusterID, branchID) branch.
	// config is the deploy spec stored for (re)creation. The address is set
	// later (SetEndpointAddress) once the endpoint is deployed. Returns
	// ErrNotFound if the branch does not exist, or ErrConflict if a read_write
	// endpoint already exists for the branch (the single-writer rule).
	CreateEndpoint(ctx context.Context, clusterID, branchID uuid.UUID, role string, config []byte) (Endpoint, error)

	// GetEndpoint returns the endpoint addressed by (poolID, clusterID,
	// branchID, endpointID), validating the pool->cluster->branch->endpoint
	// chain by ids. Returns ErrNotFound if any link is missing.
	GetEndpoint(ctx context.Context, poolID, clusterID, branchID, endpointID uuid.UUID) (Endpoint, error)

	// ListEndpoints returns a page of the endpoints bound to the branch
	// (addressed by (poolID, clusterID, branchID)) ordered by (created_at, id),
	// and the total number of endpoints for the branch (ignoring limit/offset).
	// Returns ErrNotFound if the pool, cluster, or branch does not exist.
	ListEndpoints(ctx context.Context, poolID, clusterID, branchID uuid.UUID, limit, offset int) ([]Endpoint, int, error)

	// SetEndpointAddress records an endpoint's client address and its tailnet
	// address (tailnetAddress is "" when not tailnet-exposed). Idempotent
	// (absolute UPDATE keyed by id); a missing endpoint is not an error.
	SetEndpointAddress(ctx context.Context, endpointID uuid.UUID, address, tailnetAddress string) error

	// DeleteEndpoint removes an endpoint record. Idempotent: deleting an absent
	// row is a no-op.
	DeleteEndpoint(ctx context.Context, endpointID uuid.UUID) error

	// UpdateBranchSettings replaces the branch-wide settings that are non-nil
	// and leaves the rest alone, so a caller that knows about one surface cannot
	// blank another it has never heard of. An empty map clears a surface.
	UpdateBranchSettings(ctx context.Context, branchID uuid.UUID, pg *map[string]string, writeToken *map[string]any) error
	// UpdateEndpointSettings does the same for one endpoint's three surfaces.
	UpdateEndpointSettings(ctx context.Context, endpointID uuid.UUID, pg *map[string]string, pageserver, safekeeper *map[string]any) error
}
