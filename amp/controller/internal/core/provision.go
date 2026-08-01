package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// This file adds the *idempotent, id-keyed* provisioning operations the
// write-ahead task framework's step catalog drives (controller/internal/task).
// They differ from the Backend create methods above only in that the caller
// supplies the row ids (assigned write-ahead when the task is planned) instead
// of the backend generating them — which is what lets a step be re-run after a
// crash and converge (INSERT ... ON CONFLICT (pk) DO NOTHING) rather than create
// a duplicate. They are deliberately *not* on the Backend interface: the
// synchronous read / pre-check path uses the id-generating Get methods and never
// these; only the worker's steps drive these id-keyed operations. *PgBackend
// structurally satisfies task.StateStore.
//
// SetEndpointAddress and DeleteEndpoint (already on Backend) round out the
// task.StateStore create/set/delete-endpoint surface; the Ensure* methods plus
// the cascade-delete primitives below (EndpointIDsForBranch / EndpointIDsFor
// Project / DeleteBranch / DeleteProject) are the task-only additions. The
// cascade primitives are id-keyed and idempotent: an absent parent enumerates to
// an empty slice and an absent row deletes to a no-op, so a delete task re-run
// after a crash converges.

// EnsureProject idempotently creates a project and its default branch with the
// given ids (the create_project step). Re-running with the same ids is a no-op.
// A project-name collision within the pool surfaces as ErrConflict (the
// projects_name_per_pool unique index, which is not the ON CONFLICT target); a
// missing pool surfaces as ErrNotFound (the FK on project.storage_pool_id).
func (b *PgBackend) EnsureProject(ctx context.Context, poolID, projectID, branchID uuid.UUID, name string, pgVersion int) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`INSERT INTO amp_registry.projects (id, storage_pool_id, name) VALUES ($1::uuid, $2::uuid, $3)
		 ON CONFLICT (id) DO NOTHING`,
		projectID.String(), poolID.String(), name); err != nil {
		return mapProjectErr("insert project", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO amp_registry.branches (project_id, branch_id, name, pg_version, is_default)
		 VALUES ($1::uuid, $2::uuid, $3, $4, true)
		 ON CONFLICT (branch_id) DO NOTHING`,
		projectID.String(), branchID.String(), DefaultBranchName, pgVersion); err != nil {
		return mapBranchErr("insert default branch", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit ensure project+default branch: %w", err)
	}
	return nil
}

// EnsureBranch idempotently creates a branch (a non-default branch) with the
// given id, forked from parentBranchID at forkLSN, under projectID (the
// create_branch step). Re-running with the same id is a no-op. A name collision
// within the project surfaces as ErrConflict; a missing parent (the in-project
// fork FK) as ErrNotFound. forkLSN must be a well-formed "X/X" LSN (the caller
// validates before it reaches the pg_lsn cast). No pool id is needed: the branch
// is scoped to its project, and the in-project parent FK enforces lineage.
func (b *PgBackend) EnsureBranch(ctx context.Context, parentBranchID, projectID, branchID uuid.UUID, name, forkLSN string, pgVersion int) error {
	// is_default = false; record the fork ancestry. The explicit pg_version is the
	// parent's, resolved write-ahead at plan time (a branch inherits its
	// ancestor's version), so it never drifts from the parent. Idempotent:
	// ON CONFLICT DO NOTHING. The branch's extension set is NOT here — a fork
	// inherits it via RecordBranch (compute/architecture/extensions.md#branch-fork).
	//
	// The parent is selected rather than passed as a bare value, so that
	// `dropped_at IS NULL` is part of the write: the FK alone no longer rejects a
	// dropped parent, because tombstoning leaves its row in place. Without this a
	// fork planned while the parent was live, but executed by the worker after the
	// parent was dropped, would resurrect lineage under a tombstone — the plan/
	// execute gap is unbounded (worker backlog, restart, lease reclaim). A
	// tombstoned or absent parent selects nothing, so the step fails ErrNotFound.
	// Mirrors the same guard in the synchronous CreateBranch (pg.go).
	tag, err := b.pool.Exec(ctx,
		`INSERT INTO amp_registry.branches (project_id, branch_id, name, pg_version, is_default, parent_branch_id, fork_lsn)
		 SELECT $1::uuid, $2::uuid, $3, $4, false, p.branch_id, $6::pg_lsn
		   FROM amp_registry.branches p
		  WHERE p.branch_id = $5::uuid AND p.project_id = $1::uuid AND p.dropped_at IS NULL
		 ON CONFLICT (branch_id) DO NOTHING`,
		projectID.String(), branchID.String(), name, pgVersion, parentBranchID.String(), forkLSN)
	if err != nil {
		return mapBranchErr("insert branch", err)
	}
	// Zero rows is either "the parent is gone/dropped" or "this branch already
	// exists" (the idempotent replay). Only the former is an error, so probe.
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := b.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM amp_registry.branches
			                 WHERE project_id = $1::uuid AND branch_id = $2::uuid)`,
			projectID.String(), branchID.String()).Scan(&exists); err != nil {
			return fmt.Errorf("classify ensure branch no-op: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}

// BranchExtensions returns the branch's opted-in extension names (the durable
// set an endpoint unions with the catalog defaults at create time). The set lives
// on the branch record itself, so a fork and a read-replica endpoint both see it.
// An absent branch yields an empty slice, not an error, so the endpoint step
// degrades to the defaults rather than failing. See
// compute/architecture/extensions.md.
func (b *PgBackend) BranchExtensions(ctx context.Context, _, branchID uuid.UUID) ([]string, error) {
	var raw []byte
	err := b.pool.QueryRow(ctx,
		`SELECT extensions FROM amp_registry.branches WHERE branch_id = $1::uuid`,
		branchID.String()).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read branch extensions: %w", err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, fmt.Errorf("decode branch extensions: %w", err)
	}
	return names, nil
}

// AddBranchExtensions adds names to the branch's durable extension set — the
// createEndpoint opt-in the create_endpoint step persists before resolving
// mounts. It is an *absolute* union: extensions is recomputed as the sorted
// distinct set of the existing entries ∪ names, so replaying the step can't
// double-apply (satisfying the YB idempotency contract; no SET col = col + …).
// An absent branch matches zero rows (a no-op, mirroring BranchExtensions).
func (b *PgBackend) AddBranchExtensions(ctx context.Context, _, branchID uuid.UUID, names []string) error {
	if len(names) == 0 {
		return nil
	}
	payload, err := json.Marshal(names)
	if err != nil {
		return fmt.Errorf("encode branch extensions: %w", err)
	}
	if _, err := b.pool.Exec(ctx,
		`UPDATE amp_registry.branches
		    SET extensions = COALESCE(
		        (SELECT jsonb_agg(DISTINCT e ORDER BY e)
		           FROM jsonb_array_elements_text(extensions || $2::jsonb) AS e),
		        '[]'::jsonb)
		  WHERE branch_id = $1::uuid`,
		branchID.String(), string(payload)); err != nil {
		return fmt.Errorf("add branch extensions: %w", err)
	}
	return nil
}

// EnsureEndpointRecord idempotently records an endpoint with the given id on
// branchID (the create_endpoint step's first effect). Re-running with the same id
// is a no-op (ON CONFLICT (id) DO NOTHING), so the single-writer partial unique
// index is not violated on replay. A *second* read_write endpoint (a different
// id) surfaces as ErrConflict; a missing branch as ErrNotFound.
//
// projectID is accepted but not stored: an endpoint reaches its project through
// its branch, so there is no column for it to disagree with. It stays in the
// signature (and in the step's persisted args) because the caller plans the whole
// ladder write-ahead and the branch FK is what validates the pairing.
func (b *PgBackend) EnsureEndpointRecord(ctx context.Context, endpointID, projectID, branchID uuid.UUID, role string, config []byte) error {
	_ = projectID // implied by branchID; see the doc comment
	var cfg any
	if len(config) > 0 {
		cfg = config
	}
	_, err := b.pool.Exec(ctx,
		`INSERT INTO amp_registry.endpoints (id, branch_id, role, config)
		 VALUES ($1::uuid, $2::uuid, $3::amp_registry.endpoint_role, $4::jsonb)
		 ON CONFLICT (id) DO NOTHING`,
		endpointID.String(), branchID.String(), role, cfg)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23505": // unique_violation: a read_write endpoint already exists (endpoints_one_writer_per_branch)
				return ErrConflict
			case "23503": // foreign_key_violation: the branch does not exist
				return ErrNotFound
			}
		}
		return fmt.Errorf("insert endpoint: %w", err)
	}
	return nil
}

// EndpointIDsForBranch returns the ids of every endpoint on the
// (projectID, branchID) branch (the delete_branch step's Execute-time
// enumeration). An absent branch yields an empty slice — no ErrNotFound — so a
// cascade re-run after a crash converges.
func (b *PgBackend) EndpointIDsForBranch(ctx context.Context, projectID, branchID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := b.pool.Query(ctx,
		`SELECT e.id::text FROM amp_registry.endpoints e
		  JOIN amp_registry.branches b ON b.branch_id = e.branch_id
		 WHERE b.project_id = $1::uuid AND e.branch_id = $2::uuid`,
		projectID.String(), branchID.String())
	if err != nil {
		return nil, fmt.Errorf("list endpoint ids for branch: %w", err)
	}
	defer rows.Close()
	return scanUUIDs(rows)
}

// EndpointIDsForProject returns the ids of every endpoint in the project (the
// delete_project step's Execute-time enumeration). An absent project yields an
// empty slice — no ErrNotFound.
func (b *PgBackend) EndpointIDsForProject(ctx context.Context, projectID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := b.pool.Query(ctx,
		`SELECT e.id::text FROM amp_registry.endpoints e
		  JOIN amp_registry.branches b ON b.branch_id = e.branch_id
		 WHERE b.project_id = $1::uuid`,
		projectID.String())
	if err != nil {
		return nil, fmt.Errorf("list endpoint ids for project: %w", err)
	}
	defer rows.Close()
	return scanUUIDs(rows)
}

// ChildBranchIDs returns the ids of every LIVE branch in projectID forked
// directly from branchID (the delete_branch step's pre-cascade check). An absent
// branch yields an empty slice — no ErrNotFound. Tombstoned children are skipped:
// they keep their parent pointer forever (the row stays), so counting them would
// make a parent permanently undeletable.
func (b *PgBackend) ChildBranchIDs(ctx context.Context, projectID, branchID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := b.pool.Query(ctx,
		`SELECT branch_id::text FROM amp_registry.branches
		  WHERE project_id = $1::uuid AND parent_branch_id = $2::uuid AND dropped_at IS NULL`,
		projectID.String(), branchID.String())
	if err != nil {
		return nil, fmt.Errorf("list child branch ids: %w", err)
	}
	defer rows.Close()
	return scanUUIDs(rows)
}

// DeleteBranch tombstones the (projectID, branchID) branch: it stamps
// `dropped_at` rather than deleting the row, because a branch id is never reused
// and the branch's page/WAL data in amp_storage outlives the drop (#36). Every
// reader filters `dropped_at IS NULL`, so the branch stops existing as far as
// both the product API and the storage-layer control surface are concerned.
//
// Idempotent twice over: an absent row is a no-op, and the `dropped_at IS NULL`
// guard keeps a re-run from moving an existing tombstone's timestamp.
//
// The caller retires the branch's endpoints first.
//
// Tombstoning trips NO foreign key: an UPDATE that never writes `branch_id`
// leaves both RESTRICT references — a child fork's `parent_branch_id` and an
// endpoint's `branch_id` — unexamined, where the old DELETE would have been
// rejected by either. Both guarantees are therefore carried explicitly by the
// NOT EXISTS guards below, or this write would simply always succeed:
//
//   - a live child fork must still block the drop (no orphaned ancestor), and
//   - a still-present endpoint must still block it, so an endpoint created
//     between the step's enumeration and this transaction fails the step and the
//     replay retires its instance. Losing that is how a compute instance leaks:
//     the branch would vanish from every reader while its endpoint row, and the
//     process behind it, lived on with nothing left to enumerate them.
//
// It also keeps *MemBackend.DeleteBranch (which reports ErrConflict) honest.
func (b *PgBackend) DeleteBranch(ctx context.Context, projectID, branchID uuid.UUID) error {
	tag, err := b.pool.Exec(ctx,
		`UPDATE amp_registry.branches SET dropped_at = now()
		  WHERE project_id = $1::uuid AND branch_id = $2::uuid AND dropped_at IS NULL
		    AND NOT EXISTS (
		        SELECT 1 FROM amp_registry.branches c
		         WHERE c.project_id = $1::uuid AND c.parent_branch_id = $2::uuid
		           AND c.dropped_at IS NULL)
		    AND NOT EXISTS (
		        SELECT 1 FROM amp_registry.endpoints e WHERE e.branch_id = $2::uuid)`,
		projectID.String(), branchID.String())
	if err != nil {
		return fmt.Errorf("tombstone branch: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	// Nothing moved: either there was nothing to move (absent or already
	// tombstoned — both a converged no-op for a replayed cascade), or a live child
	// fork or a still-present endpoint blocked it. Only the latter is an error, so
	// classify before returning.
	var blocked bool
	if err := b.pool.QueryRow(ctx,
		`SELECT (EXISTS (
		    SELECT 1 FROM amp_registry.branches c
		     WHERE c.project_id = $1::uuid AND c.parent_branch_id = $2::uuid
		       AND c.dropped_at IS NULL)
		   OR EXISTS (
		    SELECT 1 FROM amp_registry.endpoints e WHERE e.branch_id = $2::uuid))
		  AND EXISTS (
		    SELECT 1 FROM amp_registry.branches
		     WHERE project_id = $1::uuid AND branch_id = $2::uuid AND dropped_at IS NULL)`,
		projectID.String(), branchID.String()).Scan(&blocked); err != nil {
		return fmt.Errorf("classify branch tombstone no-op: %w", err)
	}
	if blocked {
		return ErrConflict
	}
	return nil
}

// DeleteProject tombstones the project and every branch under it, and revokes
// its grants (idempotent; absent rows are a no-op). The caller retires the
// project's endpoints first.
//
// The project row has to be tombstoned rather than deleted *because* its branches
// are: the branch -> project FK is RESTRICT, so a project whose branches survive
// as tombstones can never be deleted. Tombstoning both keeps the whole lineage
// legible after the fact, and it retires the pre-tombstone dance this used to
// need — nulling every in-project parent pointer to sidestep the self-FK ordering
// before a bulk delete — because an UPDATE never trips a foreign key.
//
// Grants are still hard-deleted: access is not history worth keeping, and a
// revoked grant should not linger on a dropped project.
//
// This transaction deliberately does NOT touch endpoints, but it does refuse to
// run while any of the project's branches still has one. That check has to be
// explicit: tombstoning is an UPDATE that never writes `branch_id`, so unlike the
// old DELETE it leaves the endpoints FK unexamined and would otherwise succeed
// with live endpoints still attached. Failing here is what makes an endpoint
// added between the step's enumeration and this transaction replay the task and
// retire that endpoint's instance before dropping its record — preserving the
// invariant that an endpoint record is never dropped without retiring its
// instance.
func (b *PgBackend) DeleteProject(ctx context.Context, projectID uuid.UUID) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	id := projectID.String()
	var hasEndpoints bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM amp_registry.endpoints e
		      JOIN amp_registry.branches b ON b.branch_id = e.branch_id
		     WHERE b.project_id = $1::uuid)`,
		id).Scan(&hasEndpoints); err != nil {
		return fmt.Errorf("check project endpoints: %w", err)
	}
	if hasEndpoints {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx,
		`UPDATE amp_registry.branches SET dropped_at = now() WHERE project_id = $1::uuid AND dropped_at IS NULL`,
		id); err != nil {
		return fmt.Errorf("tombstone project branches: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM amp_registry.user_project_grants WHERE project_id = $1::uuid`, id); err != nil {
		return fmt.Errorf("delete project grants: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE amp_registry.projects SET dropped_at = now() WHERE id = $1::uuid AND dropped_at IS NULL`,
		id); err != nil {
		return fmt.Errorf("tombstone project: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit delete project: %w", err)
	}
	return nil
}

// scanUUIDs scans a single-column (id::text) result set into a slice of UUIDs.
func scanUUIDs(rows pgx.Rows) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scan id: %w", err)
		}
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("parse id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ids: %w", err)
	}
	return ids, nil
}
