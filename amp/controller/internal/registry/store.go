package registry

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/amp/controller/internal/db"
)

// Sentinel errors the handlers map onto HTTP status codes.
var (
	// ErrNotFound is returned when an addressed row does not exist.
	ErrNotFound = errors.New("registry: not found")
	// ErrConflict is returned when a request collides with existing state.
	ErrConflict = errors.New("registry: conflict")
	// ErrInvalid is returned when a request is semantically invalid
	// against current state. Retrying the same request cannot succeed.
	ErrInvalid = errors.New("registry: invalid request")
)

// Store is the storage layer's view of the control plane's resource model: the
// SAME `branches` and `endpoints` tables the product API serves, addressed the
// way the storage layer addresses them.
//
// There is no second copy of a branch. Both surfaces key on `branch_id` — it is
// the storage layer's BranchId and globally unique, so it is the whole primary
// key — and differ only in rendering: uuid on /api/v1, flat 32-lowercase-hex
// here. This store is that translation, not a separate registry, which is what
// makes the two views incapable of drifting: a branch dropped through either one
// is the same row with the same `dropped_at` stamp.
//
// A caller still has to say which project it means. Not to identify the branch —
// the id alone does that — but to scope the operation: `project_id` is an
// ordinary column on `branches`, and every read and write here is filtered by it
// so one project's launcher cannot reach another's rows. An endpoint carries no
// project at all and is scoped through its branch.
//
// Statements over the resource model are unqualified and resolve via the pool's
// search_path (--db-schema); the data plane is qualified `amp_storage`.
type Store struct {
	pool *db.Pool
}

// NewStore returns a Store backed by pool.
func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool}
}

// Ready reports whether the backing database is reachable.
func (s *Store) Ready(ctx context.Context) error {
	return s.pool.Healthy(ctx)
}

// --- branch id rendering ----------------------------------------------------

// branchIDHex is the SQL projection of a branch's uuid as the storage layer's
// flat BranchId: 32 lowercase hex digits, no dashes. The inverse is ParseBranchID.
const branchIDHex = `replace(branch_id::text, '-', '')`

// ParseBranchID parses a storage-layer BranchId into the uuid the resource model
// keys on. It accepts the flat 32-hex form the storage layer uses (and, because
// uuid.Parse does, the dashed form too).
//
// The transform is lossless and total in both directions — a BranchId is 16
// bytes either way — so there is no mapping to keep and no id that exists in one
// rendering but not the other. Note the ids minted by yb_amp_local and the
// integration harness are 16 random bytes rather than UUIDv4, so nothing may
// depend on the version/variant nibbles.
func ParseBranchID(branchID string) (uuid.UUID, error) {
	id, err := uuid.Parse(branchID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: malformed branch id %q: %w", ErrInvalid, branchID, err)
	}
	return id, nil
}

// FormatBranchID renders a branch uuid as the storage layer's 32-hex BranchId.
func FormatBranchID(id uuid.UUID) string {
	return hex.EncodeToString(id[:])
}

// --- error classification ---------------------------------------------------

// isSerializationAbort reports SQLSTATE 40001 (serialization_failure).
func isSerializationAbort(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40001"
}

// mapWriteErr translates the constraint violations a resource-model write can
// raise into this package's sentinels: a name already taken among live rows, or a
// second writer on a branch (branches_name_per_project / endpoints_name /
// endpoints_one_writer_per_branch) -> ErrConflict; a missing project or branch
// (foreign key) -> ErrNotFound.
func mapWriteErr(what string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%w: %s", ErrConflict, what)
		case "23503": // foreign_key_violation
			return fmt.Errorf("%w: %s", ErrNotFound, what)
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

// RegistryCounts returns the live inventory size the metrics collector exports
// as a gauge: the number of live branches, across every project. Queried on each
// scrape, so it must stay cheap.
//
// Tombstones are excluded — they are dropped branches, and counting them would
// make the gauge a monotonically rising total rather than an inventory.
func (s *Store) RegistryCounts(ctx context.Context) (branches int64, err error) {
	if err = s.pool.QueryRow(ctx,
		`SELECT count(*) FROM amp_registry.branches WHERE dropped_at IS NULL`).Scan(&branches); err != nil {
		return 0, fmt.Errorf("count branches: %w", err)
	}
	return branches, nil
}

// --- PostgreSQL version -----------------------------------------------------

// pgVersionNumPG18 is `PG_VERSION_NUM` of the PostgreSQL 18 build amp ships
// (18.2 → 180002; see pg_install/v18 pg_config.h). It is the seed value the
// controller records for a new branch's `pg_version`.
const pgVersionNumPG18 = 180002

// FullPgVersionID maps a PG major version to the full PgVersionId to record for
// a branch.
//
// PG 18 is the only major amp ships, and this is the sole writer of
// `amp_storage.branch_state.pg_version`, so anything else is a caller bug rather
// than a version to guess at. Synthesising `major*10000` would write a version
// no build in the tree can serve and defer the failure to the safekeeper's
// greeting check on a branch that is already provisioned; refuse it here, where
// the branch has not been created yet.
func FullPgVersionID(major int) (int, error) {
	if major != 18 {
		return 0, fmt.Errorf("unsupported PG major version %d: amp ships PG 18 only", major)
	}
	return pgVersionNumPG18, nil
}

// --- branch name registry ---------------------------------------------------

// --- branch creation -------------------------------------------------------

// RecordBranch declares a branch and seeds the storage-layer state it needs to
// be materialized. Idempotent on (projectID, branchID).
//
// RecordBranch is reached by every branch-creation path — yb_amp_local's `branch
// create` and the controller's record_branch_recipe step — and it provisions
// both of the branch's rows in one transaction, before any pageserver,
// safekeeper, or endpoint supervisor for the branch is started:
//
//   - branch — the branch's identity and creation record (its name, PG version,
//     and how to create it: a NULL parent means bootstrap, a set one means fork
//     that ancestor at fork_lsn). On the product path the create_branch step has
//     already written this row, so the insert converges to a no-op; on the
//     launcher path this is where the row is born. One row either way.
//   - amp_storage.branch_state — the per-branch state row the pageserver and
//     safekeeper share. Both daemons only ever validate that it exists and
//     UPDATE their own column group; neither creates it, so it has to be here
//     before either starts.
//
// A fork (AncestorBranchID set) is additionally validated against the ancestor's
// GC cutoff — ErrInvalid if the fork point's history is already reclaimed — via
// a write on the ancestor's row that serializes with the ancestor's GC cutoff
// advance (see the interlock comment in the body).
func (s *Store) RecordBranch(ctx context.Context, projectID uuid.UUID, branchID string, req BranchCreateRecord) error {
	err := s.recordBranch(ctx, projectID, branchID, req)
	// The fork interlock write-conflicts with the ancestor's GC cutoff
	// advance by design; surface the losing side legibly (409, retryable)
	// instead of a raw serialization abort at HTTP 500.
	if isSerializationAbort(err) {
		return fmt.Errorf(
			"%w: branch registration conflicted with a concurrent GC cutoff advance on the ancestor — retry: %w",
			ErrConflict, err)
	}
	return err
}

func (s *Store) recordBranch(ctx context.Context, projectID uuid.UUID, branchID string, req BranchCreateRecord) error {
	id, err := ParseBranchID(branchID)
	if err != nil {
		return err
	}
	// The ancestor is addressed in the flat rendering on the wire and in
	// amp_storage; the resource model's parent pointer is a uuid.
	var ancestorUUID *string
	if req.AncestorBranchID != nil {
		anc, err := ParseBranchID(*req.AncestorBranchID)
		if err != nil {
			return err
		}
		s := anc.String()
		ancestorUUID = &s
	}
	// pg_version is the branch's major; refuse an unmappable one here, before the
	// branch exists, rather than seeding a version no build can serve (see
	// FullPgVersionID).
	pgVersion, err := FullPgVersionID(req.PgVersion)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin record branch: %w", err)
	}
	defer tx.Rollback(ctx)

	// `extensions` is inherited from the ancestor at fork time: the child's row is
	// seeded with a snapshot of the ancestor's opted-in extension set (empty for a
	// bootstrap branch, whose ancestor is NULL). It is resolved in a *separate*
	// SELECT and passed as a plain parameter rather than a subquery in the INSERT
	// VALUES: YB does not support a table-referencing subquery inside an
	// `INSERT ... ON CONFLICT`. The scalar COALESCE always returns one row
	// (NULL -> '[]'), so there is no ErrNoRows to handle. The ancestor is addressed
	// by uuid here — one branch table, one key type — where the flat registry keyed
	// on the 32-hex rendering. See compute/architecture/extensions.md#branch-fork.
	inheritedExts := []byte("[]")
	if ancestorUUID != nil {
		var raw []byte
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE((SELECT extensions FROM amp_registry.branches WHERE branch_id = $1::uuid), '[]'::jsonb)`,
			*ancestorUUID).Scan(&raw); err != nil {
			return fmt.Errorf("read ancestor extensions: %w", err)
		}
		inheritedExts = raw
	}

	// is_default is false: a project's default branch is created with the project
	// itself, and every branch declared here hangs off it (or stands alone as
	// another root). ON CONFLICT DO NOTHING makes a re-record converge, and leaves
	// an existing creation record untouched rather than rewriting it — which is
	// also what keeps a re-record from clobbering an `extensions` set that the
	// branch's own create_endpoint opt-in (AddBranchExtensions) unioned in after
	// this row was first written.
	//
	// This INSERT is the branch's *only* creator on the fork path: the branch task
	// is one step so that the row, the branch_state seed, and the interlock below
	// all commit or roll back together. A fork therefore selects its parent rather
	// than naming it, which is what enforces that the parent is LIVE — the FK alone
	// cannot, since a tombstoned parent's row is still there.
	var tag pgconn.CommandTag
	if ancestorUUID != nil {
		tag, err = tx.Exec(ctx,
			`INSERT INTO amp_registry.branches
			   (project_id, branch_id, name, pg_version, is_default, parent_branch_id, fork_lsn, extensions)
			 SELECT $1::uuid, $2::uuid, $3, $4, false, p.branch_id, $6::pg_lsn, $7::jsonb
			   FROM amp_registry.branches p
			  WHERE p.branch_id = $5::uuid AND p.project_id = $1::uuid AND p.dropped_at IS NULL
			 ON CONFLICT (branch_id) DO NOTHING`,
			projectID.String(), id.String(), req.BranchName, req.PgVersion,
			ancestorUUID, req.StartLSN, string(inheritedExts))
	} else {
		tag, err = tx.Exec(ctx,
			`INSERT INTO amp_registry.branches
			   (project_id, branch_id, name, pg_version, is_default, parent_branch_id, fork_lsn, extensions)
			 VALUES ($1::uuid, $2::uuid, $3, $4, false, NULL, $5::pg_lsn, $6::jsonb)
			 ON CONFLICT (branch_id) DO NOTHING`,
			projectID.String(), id.String(), req.BranchName, req.PgVersion,
			req.StartLSN, string(inheritedExts))
	}
	if err != nil {
		return mapWriteErr(fmt.Sprintf("declare branch %s (%q)", branchID, req.BranchName), err)
	}
	// Zero rows is either an idempotent re-record (the branch already exists) or a
	// parent that is gone or tombstoned. Only the latter is an error.
	if tag.RowsAffected() == 0 && ancestorUUID != nil {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM amp_registry.branches
			                 WHERE project_id = $1::uuid AND branch_id = $2::uuid)`,
			projectID.String(), id.String()).Scan(&exists); err != nil {
			return fmt.Errorf("classify branch declare no-op: %w", err)
		}
		if !exists {
			return fmt.Errorf("%w: fork parent %s is not a live branch in project %s",
				ErrNotFound, *req.AncestorBranchID, projectID)
		}
	}

	// The seed the pageserver reads at attach to decide how to create the
	// timeline: a NULL ancestor means bootstrap (initdb + import at pg_version),
	// a set one means branch off that ancestor at ancestor_lsn. This is why the
	// row has to exist before any pageserver starts.
	//
	// `pg_version` here holds a full PgVersionId, not the major the branch row
	// carries. This is the column's only writer — both daemons just read it.
	//
	// Keyed by the flat branch id: amp_storage is the flat namespace, and it is
	// deliberately NOT project-scoped (the storage layer has no projects).
	var ancestorLSN *string
	if req.AncestorBranchID != nil {
		ancestorLSN = &req.StartLSN
	}
	stateRow, err := tx.Exec(ctx,
		`INSERT INTO amp_storage.branch_state
		   (branch_id, pg_version, ancestor_branch_id, ancestor_lsn)
		 VALUES ($1, $2, $3, ($4::pg_lsn - '0/0'::pg_lsn)::bigint)
		 ON CONFLICT (branch_id) DO NOTHING`,
		FormatBranchID(id), pgVersion, req.AncestorBranchID, ancestorLSN)
	if err != nil {
		return fmt.Errorf("record branch state: %w", err)
	}

	// There is a race between forking a child off the ancestor and the
	// ancestor's GC advancing its cutoff (time increases downwards):
	//
	//   initial state: cutoff = 100
	//
	//   fork child @110                    GC on the ancestor
	//   ───────────────                    ──────────────────
	//   validate: cutoff = 100 ✓
	//                                      compute cutoff = 120;
	//                                      sees no child row
	//                                      (not committed yet)
	//   commit child row
	//                                      publish 120, trim ≤ 120
	//   child's fork point is gone ✗
	//
	// It is handled by making the fork perform a dummy UPDATE on the
	// ancestor's branch_state row, in the same transaction as the child
	// row INSERT: the fork and the cutoff advance both write the same row
	// and hence are serialized. The WHERE is the validation, evaluated
	// inside that serialized statement. Gated on the child row being
	// NEWLY inserted: a re-record must stay a no-op even after the cutoff
	// advances. Mirrored by `fork_txn` in
	// compute/pageserver/src/page_store.rs (tests) — keep in lockstep.
	if req.AncestorBranchID != nil && stateRow.RowsAffected() == 1 {
		touched, err := tx.Exec(ctx,
			`UPDATE amp_storage.branch_state
			    SET applied_gc_cutoff_lsn = applied_gc_cutoff_lsn
			  WHERE branch_id = $1
			    AND COALESCE(applied_gc_cutoff_lsn, 0) <= ($2::pg_lsn - '0/0'::pg_lsn)::bigint`,
			*req.AncestorBranchID, req.StartLSN)
		if err != nil {
			return fmt.Errorf("validate fork point: %w", err)
		}
		if touched.RowsAffected() == 0 {
			// Rolled back with the txn: the child row above never lands.
			var exists bool
			err := tx.QueryRow(ctx,
				`SELECT true FROM amp_storage.branch_state WHERE branch_id = $1`,
				*req.AncestorBranchID).Scan(&exists)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: fork ancestor %s has no branch_state row",
					ErrNotFound, *req.AncestorBranchID)
			}
			if err != nil {
				return fmt.Errorf("probe fork ancestor %s: %w", *req.AncestorBranchID, err)
			}
			return fmt.Errorf("%w: fork point %s is below ancestor %s's GC cutoff (history already reclaimed)",
				ErrInvalid, req.StartLSN, *req.AncestorBranchID)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit record branch: %w", err)
	}
	return nil
}

// --- endpoint registry ----------------------------------------------------

// endpointSpecRole is the role a launcher-created endpoint takes. yb_amp_local
// only ever creates Primary endpoints (it rejects --lsn), so they are writers —
// which means endpoints_one_writer_per_branch now enforces the launcher's own
// single-endpoint-per-branch check at the schema level.
const endpointSpecRole = "read_write"

// EndpointCreate persists (or replaces) a launcher compute-endpoint record on
// the project's branch, keyed by its caller-chosen handle.
//
// The handle lands in `endpoint.name` and the launcher's opaque config in
// `endpoint.spec`; the row is otherwise an ordinary endpoint record, so it shows
// up on the product API alongside deployed ones. The id is minted here — the
// resource model keys endpoints by uuid — and is not the handle.
func (s *Store) EndpointCreate(ctx context.Context, projectID uuid.UUID, ep ComputeEndpoint) error {
	branchUUID, err := ParseBranchID(ep.BranchID)
	if err != nil {
		return err
	}
	if ep.EndpointID == "" {
		return fmt.Errorf("%w: endpoint_id is required", ErrInvalid)
	}
	// Two independent scopings, both required:
	//
	//   - the SELECT scopes the INSERT candidate — a branch outside the project (or
	//     absent, or dropped) produces no row to insert;
	//   - the DO UPDATE's own WHERE scopes the *conflict* arm. `endpoints_name` is
	//     a global unique index (an endpoint carries no project_id to scope by), so
	//     without it a name already taken in ANOTHER project would resolve the
	//     conflict against that project's row and silently reparent it here,
	//     overwriting its spec and reporting success.
	//
	// Zero rows therefore means one of: no such live branch in this project, or the
	// handle belongs to another project. Both are told apart below.
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO amp_registry.endpoints (id, branch_id, role, name, spec)
		 SELECT $1::uuid, b.branch_id, $3::amp_registry.endpoint_role, $4, $5::jsonb
		   FROM amp_registry.branches b
		  WHERE b.branch_id = $2::uuid AND b.project_id = $6::uuid AND b.dropped_at IS NULL
		 ON CONFLICT (name) DO UPDATE
		    SET branch_id = EXCLUDED.branch_id, spec = EXCLUDED.spec
		  WHERE EXISTS (
		      SELECT 1 FROM amp_registry.branches ob
		       WHERE ob.branch_id = amp_registry.endpoints.branch_id
		         AND ob.project_id = $6::uuid)`,
		uuid.New().String(), branchUUID.String(), endpointSpecRole,
		ep.EndpointID, []byte(ep.Spec), projectID.String())
	if err != nil {
		return mapWriteErr(fmt.Sprintf("create endpoint %s on branch %s", ep.EndpointID, ep.BranchID), err)
	}
	if tag.RowsAffected() == 0 {
		var takenElsewhere bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (
			    SELECT 1 FROM amp_registry.endpoints e
			     WHERE e.name = $1
			       AND NOT EXISTS (
			           SELECT 1 FROM amp_registry.branches ob
			            WHERE ob.branch_id = e.branch_id AND ob.project_id = $2::uuid))`,
			ep.EndpointID, projectID.String()).Scan(&takenElsewhere); err != nil {
			return fmt.Errorf("classify endpoint create no-op: %w", err)
		}
		if takenElsewhere {
			return fmt.Errorf("%w: endpoint handle %q already belongs to another project",
				ErrConflict, ep.EndpointID)
		}
		return fmt.Errorf("%w: no live branch %s in project %s", ErrNotFound, ep.BranchID, projectID)
	}
	return nil
}

// EndpointList returns the project's launcher endpoint records.
//
// Scoped to rows that carry a `spec`: those are the ones this surface owns and
// the only ones the launcher can deserialize. Endpoints the product API deployed
// have no spec, and handing them back would fail the launcher's unmarshal.
func (s *Store) EndpointList(ctx context.Context, projectID uuid.UUID) ([]ComputeEndpoint, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT e.name, `+"replace(e.branch_id::text, '-', '')"+`, e.spec
		   FROM amp_registry.endpoints e
		   JOIN amp_registry.branches b ON b.branch_id = e.branch_id
		  WHERE b.project_id = $1::uuid AND e.spec IS NOT NULL AND e.name IS NOT NULL
		  ORDER BY e.name`,
		projectID.String())
	if err != nil {
		return nil, fmt.Errorf("list endpoints: %w", err)
	}
	defer rows.Close()

	out := []ComputeEndpoint{}
	for rows.Next() {
		var ep ComputeEndpoint
		var spec []byte
		if err := rows.Scan(&ep.EndpointID, &ep.BranchID, &spec); err != nil {
			return nil, fmt.Errorf("scan endpoint: %w", err)
		}
		ep.Spec = json.RawMessage(spec)
		out = append(out, ep)
	}
	return out, rows.Err()
}

// EndpointDelete removes a launcher compute-endpoint record by its handle.
// Idempotent. Scoped to spec-carrying rows so this surface cannot delete an
// endpoint the product API deployed.
func (s *Store) EndpointDelete(ctx context.Context, projectID uuid.UUID, endpointID string) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM amp_registry.endpoints e
		  USING amp_registry.branches b
		  WHERE b.branch_id = e.branch_id AND b.project_id = $1::uuid
		    AND e.name = $2 AND e.spec IS NOT NULL`,
		projectID.String(), endpointID); err != nil {
		return fmt.Errorf("delete endpoint %s: %w", endpointID, err)
	}
	return nil
}
