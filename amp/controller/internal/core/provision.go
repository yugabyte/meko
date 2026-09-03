package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/amp/controller/internal/db"
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
// Cluster / DeleteBranch / DeleteCluster) are the task-only additions. The
// cascade primitives are id-keyed and idempotent: an absent parent enumerates to
// an empty slice and an absent row deletes to a no-op, so a delete task re-run
// after a crash converges.

// EnsureCluster idempotently creates a cluster and its default branch with the
// given ids (the create_cluster step). Re-running with the same ids is a no-op.
// A cluster-name collision within the pool surfaces as ErrConflict (the
// clusters_name_per_pool unique index, which is not the ON CONFLICT target); a
// missing pool surfaces as ErrNotFound (the FK on cluster.storage_pool_id).
func (b *PgBackend) EnsureCluster(ctx context.Context, poolID, clusterID, branchID uuid.UUID, name string, pgVersion int, engine string, extensions []string) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`INSERT INTO amp_registry.clusters (id, storage_pool_id, name, engine)
		 VALUES ($1::uuid, $2::uuid, $3, $4)
		 ON CONFLICT (id) DO NOTHING`,
		clusterID.String(), poolID.String(), name, normalizeEngine(engine))
	if err != nil {
		return mapClusterErr("insert cluster", err)
	}
	// Interlock against a concurrent pool delete, the pool-level twin of the
	// branch interlock in EnsureEndpointRecord. DeleteStoragePool guards on a READ
	// (no live cluster) and writes only the pool row; this writes only cluster and
	// branch rows, and the FK is satisfied by a tombstoned pool. Nothing either
	// side writes is read by the other, so both commit and leave a live cluster
	// under a tombstoned pool — invisible to every `dropped_at IS NULL` reader, so
	// nothing ever enumerates it. Writing the pool row here makes the two conflict.
	//
	// Only when the INSERT actually inserted: a replay of the same cluster id must
	// stay an idempotent no-op, and a row already there is proof the pool was live
	// when it landed, since the delete refuses while a live cluster remains.
	if tag.RowsAffected() > 0 {
		live, err := tx.Exec(ctx,
			`UPDATE amp_registry.storage_pools SET dropped_at = dropped_at
			  WHERE id = $1::uuid AND dropped_at IS NULL`,
			poolID.String())
		if err != nil {
			return mapPoolDropRaceErr("validate pool liveness", err)
		}
		if live.RowsAffected() == 0 {
			return ErrNotFound
		}
	}
	// Seed the create-time externalized-extension set on the default branch row.
	// It is per-branch state: a fork inherits it, and AddBranchExtensions unions
	// later opt-ins into a single branch (compute/architecture/extensions.md).
	// Empty when the create request opted into none. marshalExtensions sorts and
	// de-dupes the names Go-side (the jsonb_agg(DISTINCT ... ORDER BY) union is
	// AddBranchExtensions' job).
	exts, err := marshalExtensions(extensions)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO amp_registry.branches (cluster_id, branch_id, name, pg_version, is_default, extensions)
		 VALUES ($1::uuid, $2::uuid, $3, $4, true, $5::jsonb)
		 ON CONFLICT (branch_id) DO NOTHING`,
		clusterID.String(), branchID.String(), DefaultBranchName, pgVersion, exts); err != nil {
		return mapBranchErr("insert default branch", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return mapPoolDropRaceErr("commit ensure cluster+default branch", err)
	}
	return nil
}

// mapPoolDropRaceErr is mapBranchDropRaceErr one level up: it names the
// serialization abort the pool interlock provokes by design, and stays under no
// sentinel for the same reason — the request was never adjudicated, so the task
// worker re-runs the step and resolves it to success or a clean ErrNotFound.
func mapPoolDropRaceErr(what string, err error) error {
	if db.IsAbortedTransaction(err) {
		return fmt.Errorf("%s conflicted with a concurrent delete of the storage pool: %w", what, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// marshalExtensions renders a name set as a sorted, de-duplicated JSON array for
// the branches.extensions column (NOT NULL, defaults to '[]'). An empty/nil set
// yields "[]".
func marshalExtensions(names []string) (string, error) {
	seen := make(map[string]struct{}, len(names))
	dedup := make([]string, 0, len(names))
	for _, n := range names {
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		dedup = append(dedup, n)
	}
	sort.Strings(dedup)
	if len(dedup) == 0 {
		return "[]", nil
	}
	blob, err := json.Marshal(dedup)
	if err != nil {
		return "", fmt.Errorf("encode extensions: %w", err)
	}
	return string(blob), nil
}

// EnsureBranch idempotently creates a branch (a non-default branch) with the
// given id, forked from parentBranchID at forkLSN, under clusterID (the
// create_branch step). Re-running with the same id is a no-op. A name collision
// within the cluster surfaces as ErrConflict; a missing parent (the in-cluster
// fork FK) as ErrNotFound. forkLSN must be a well-formed "X/X" LSN (the caller
// validates before it reaches the pg_lsn cast). No pool id is needed: the branch
// is scoped to its cluster, and the in-cluster parent FK enforces lineage.
func (b *PgBackend) EnsureBranch(ctx context.Context, parentBranchID, clusterID, branchID uuid.UUID, name, forkLSN string, pgVersion int) error {
	// is_default = false; record the fork ancestry. The explicit pg_version is the
	// parent's, resolved write-ahead at plan time (a branch inherits its
	// ancestor's version), so it never drifts from the parent. Idempotent:
	// ON CONFLICT DO NOTHING. The fork inherits the parent's externalized-extension
	// set (p.extensions is copied onto the new row), so an endpoint on the fork
	// mounts the same set the parent's endpoints do (compute/architecture/extensions.md).
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
		`INSERT INTO amp_registry.branches (cluster_id, branch_id, name, pg_version, is_default, parent_branch_id, fork_lsn, extensions)
		 SELECT $1::uuid, $2::uuid, $3, $4, false, p.branch_id, $6::pg_lsn, p.extensions
		   FROM amp_registry.branches p
		  WHERE p.branch_id = $5::uuid AND p.cluster_id = $1::uuid AND p.dropped_at IS NULL
		 ON CONFLICT (branch_id) DO NOTHING`,
		clusterID.String(), branchID.String(), name, pgVersion, parentBranchID.String(), forkLSN)
	if err != nil {
		return mapBranchErr("insert branch", err)
	}
	// Zero rows is either "the parent is gone/dropped" or "this branch already
	// exists" (the idempotent replay). Only the former is an error, so probe.
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := b.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM amp_registry.branches
			                 WHERE cluster_id = $1::uuid AND branch_id = $2::uuid)`,
			clusterID.String(), branchID.String()).Scan(&exists); err != nil {
			return fmt.Errorf("classify ensure branch no-op: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}

// BranchPgSettings returns the branch's baseline PostgreSQL settings, or nil if
// it has none (or does not exist — the caller's own lookup reports that). Like
// the extension set it lives on the branch record, so a fork and every endpoint
// on the branch see it.
func (b *PgBackend) BranchPgSettings(ctx context.Context, _, branchID uuid.UUID) (map[string]string, error) {
	var raw []byte
	err := b.pool.QueryRow(ctx,
		`SELECT pg_settings FROM amp_registry.branches WHERE branch_id = $1::uuid`,
		branchID.String()).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read branch pg_settings: %w", err)
	}
	return decodePgSettings(raw, "branch")
}

// BranchExtensions returns the branch's opted-in extension names (the set every
// endpoint on it unions with the catalog defaults at create time). The set lives
// on the branch's own row — seeded on the default branch by EnsureCluster,
// inherited by a fork, extended per branch by AddBranchExtensions. An
// absent branch yields an empty slice, not an error, so the endpoint step
// degrades to the defaults rather than failing. See compute/architecture/extensions.md.
func (b *PgBackend) BranchExtensions(ctx context.Context, _, branchID uuid.UUID) ([]string, error) {
	var raw []byte
	err := b.pool.QueryRow(ctx,
		`SELECT extensions FROM amp_registry.branches
		  WHERE branch_id = $1::uuid AND dropped_at IS NULL`,
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

// AddBranchExtensions adds names to the (clusterID, branchID) branch's durable
// extension set — the add_branch_extensions opt-in. Adding is per branch. It is
// an *absolute* union: the row's extensions is recomputed as the sorted distinct
// set of its existing entries ∪ names, so replaying the step can't double-apply
// (satisfying the YB idempotency contract; no SET col = col + …). An absent
// branch matches zero rows (a no-op).
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
		  WHERE branch_id = $1::uuid AND dropped_at IS NULL`,
		branchID.String(), string(payload)); err != nil {
		return fmt.Errorf("add branch extensions: %w", err)
	}
	return nil
}

// BranchEndpointDeploys returns the redeploy record for every live endpoint on
// the (clusterID, branchID) branch: its id, the branch it serves, and the pinned
// pg version + compute size (pg_version from the branch, cpu/memory from the
// endpoint config). The add_branch_extensions step enumerates these at Execute
// time to re-render each endpoint with the new extension mounts. An absent branch
// yields an empty slice.
func (b *PgBackend) BranchEndpointDeploys(ctx context.Context, _, branchID uuid.UUID) ([]EndpointDeploy, error) {
	rows, err := b.pool.Query(ctx,
		`SELECT e.id::text, e.branch_id::text, eb.pg_version,
		        COALESCE(e.config->>'cpu', ''), COALESCE(e.config->>'memory', '')
		   FROM amp_registry.endpoints e
		   JOIN amp_registry.branches eb ON eb.branch_id = e.branch_id
		  WHERE eb.branch_id = $1::uuid AND eb.dropped_at IS NULL`,
		branchID.String())
	if err != nil {
		return nil, fmt.Errorf("list endpoint deploys for branch: %w", err)
	}
	defer rows.Close()
	var out []EndpointDeploy
	for rows.Next() {
		var (
			id, branchID string
			d            EndpointDeploy
		)
		if err := rows.Scan(&id, &branchID, &d.PgVersion, &d.CPU, &d.Memory); err != nil {
			return nil, fmt.Errorf("scan endpoint deploy: %w", err)
		}
		if d.ID, err = uuid.Parse(id); err != nil {
			return nil, fmt.Errorf("parse endpoint id: %w", err)
		}
		if d.BranchID, err = uuid.Parse(branchID); err != nil {
			return nil, fmt.Errorf("parse branch id: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list endpoint deploys for branch: %w", err)
	}
	return out, nil
}

// UpdateEndpointExtensions merges the resolved extension names into the
// endpoint's config jsonb (the (re)create/audit spec), leaving the other config
// fields intact — the DB-side record of what the endpoint last mounted, mirroring
// UpdateEndpointResources. Idempotent; an absent row is a no-op.
func (b *PgBackend) UpdateEndpointExtensions(ctx context.Context, endpointID uuid.UUID, names []string) error {
	payload, err := marshalExtensions(names)
	if err != nil {
		return err
	}
	if _, err := b.pool.Exec(ctx,
		`UPDATE amp_registry.endpoints
		    SET config = COALESCE(config, '{}'::jsonb) || jsonb_build_object('extensions', $2::jsonb)
		  WHERE id = $1::uuid`,
		endpointID.String(), payload); err != nil {
		return fmt.Errorf("update endpoint extensions: %w", err)
	}
	return nil
}

// orEmptyJSON substitutes `{}` for an absent document, since the settings columns
// are NOT NULL.
func orEmptyJSON(doc []byte) []byte {
	if len(doc) == 0 {
		return []byte("{}")
	}
	return doc
}

// EnsureEndpointRecord idempotently records an endpoint with the given id on
// branchID (the create_endpoint step's first effect). Re-running with the same id
// is a no-op (ON CONFLICT (id) DO NOTHING), so the single-writer partial unique
// index is not violated on replay. A *second* read_write endpoint (a different
// id) surfaces as ErrConflict; a missing or dropped branch as ErrNotFound.
//
// An empty name is stored as NULL, not "": NULLs are distinct in a unique index,
// so any number of unnamed endpoints coexist while two named the same on one
// branch collide.
//
// clusterID is accepted but not stored: an endpoint reaches its cluster through
// its branch, so there is no column for it to disagree with. It stays in the
// signature (and in the step's persisted args) because the caller plans the whole
// ladder write-ahead and the branch FK is what validates the pairing.
//
// The branch must also be LIVE, and that check is a conditional WRITE of the
// branch row rather than a read of it, so it serializes against a concurrent drop
// instead of write-skewing past it (see the interlock comment in the body).
func (b *PgBackend) EnsureEndpointRecord(ctx context.Context, endpointID, clusterID, branchID uuid.UUID, role, name string,
	config, pgSettings, pageserverSettings, safekeeperSettings []byte) error {
	_ = clusterID // implied by branchID; see the doc comment
	var cfg any
	if len(config) > 0 {
		cfg = config
	}
	var nameArg any
	if name != "" {
		nameArg = name
	}
	settings := orEmptyJSON(pgSettings)
	psSettings := orEmptyJSON(pageserverSettings)
	skSettings := orEmptyJSON(safekeeperSettings)

	// One transaction, so the record and the interlock below commit or roll back
	// together: a refused endpoint leaves no row behind.
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`INSERT INTO amp_registry.endpoints
		   (id, branch_id, role, name, config, pg_settings, pageserver_settings, safekeeper_settings)
		 VALUES ($1::uuid, $2::uuid, $3::amp_registry.endpoint_role, $4, $5::jsonb, $6::jsonb, $7::jsonb, $8::jsonb)
		 ON CONFLICT (id) DO NOTHING`,
		endpointID.String(), branchID.String(), role, nameArg, cfg,
		string(settings), string(psSettings), string(skSettings))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			// unique_violation: the branch already has a writer
			// (endpoints_one_writer_per_branch) or an endpoint by that name
			// (endpoints_name_per_branch).
			case "23505":
				return ErrConflict
			case "23503": // foreign_key_violation: the branch does not exist
				return ErrNotFound
			}
		}
		return fmt.Errorf("insert endpoint: %w", err)
	}

	// The branch must still be live, and the FK cannot say so: tombstoning leaves
	// the branch's row in place, so an endpoint on a dropped branch satisfies it.
	//
	// A conditional self-assignment UPDATE is what says it, and a read is not a
	// substitute — the guard the drop already carries is exactly a read.
	// DeleteBranch refuses to tombstone a branch that still has endpoints with a
	// `NOT EXISTS (endpoints)` subquery, while creating an endpoint writes a row in
	// a different table. Neither side writes what the other reads, so under
	// snapshot isolation the two miss each other and both commit (time increases
	// downwards):
	//
	//   create an endpoint on B            drop B
	//   ───────────────────────            ──────
	//   read B: live (the handler's
	//   pre-check, and an FK a
	//   tombstone still satisfies) ✓
	//                                      read B's endpoints: none ✓
	//   insert the endpoint row
	//                                      UPDATE B SET dropped_at = now()
	//   a live endpoint on a tombstoned branch ✗
	//
	// That state is not merely untidy. B is invisible to every
	// `dropped_at IS NULL` reader — so no cascade enumerates the endpoint to retire
	// it — while its compute keeps serving, and branch-drop GC then reclaims B's
	// pages and WAL out from under a running compute. Writing B's row here makes
	// the two statements conflict, so one of them loses.
	//
	// DeleteCluster tombstones every branch row in the cluster, B's included, so it
	// conflicts here the same way.
	//
	// The interlock runs only when the INSERT actually inserted: a replay of the
	// same endpoint id has to stay an idempotent no-op, and a row already there is
	// proof the branch was live when it landed, since the drop refuses while an
	// endpoint remains.
	if tag.RowsAffected() > 0 {
		live, err := tx.Exec(ctx,
			`UPDATE amp_registry.branches SET dropped_at = dropped_at
			  WHERE branch_id = $1::uuid AND dropped_at IS NULL`,
			branchID.String())
		if err != nil {
			return mapBranchDropRaceErr("validate branch liveness", err)
		}
		if live.RowsAffected() == 0 {
			// Rolled back with the transaction: the endpoint row never lands.
			return ErrNotFound
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return mapBranchDropRaceErr("commit ensure endpoint record", err)
	}
	return nil
}

// mapBranchDropRaceErr makes the serialization abort the branch interlock provokes
// by design legible, rather than an opaque SQLSTATE 40001 in the task log.
//
// Deliberately under no sentinel: the abort is not a verdict on the request — the
// request was never adjudicated, the transaction just lost the row — so it is
// neither ErrConflict nor ErrNotFound. The task worker re-runs the step on it
// (db.IsAbortedTransaction), which resolves to success or to a clean ErrNotFound
// once the winning drop is visible; the %w chain below is what it classifies on.
func mapBranchDropRaceErr(what string, err error) error {
	if db.IsAbortedTransaction(err) {
		return fmt.Errorf("%s conflicted with a concurrent drop of, or fork off, the branch: %w",
			what, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// EndpointIDsForBranch returns the ids of every endpoint on the
// (clusterID, branchID) branch (the delete_branch step's Execute-time
// enumeration). An absent branch yields an empty slice — no ErrNotFound — so a
// cascade re-run after a crash converges.
func (b *PgBackend) EndpointIDsForBranch(ctx context.Context, clusterID, branchID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := b.pool.Query(ctx,
		`SELECT e.id::text FROM amp_registry.endpoints e
		  JOIN amp_registry.branches b ON b.branch_id = e.branch_id
		 WHERE b.cluster_id = $1::uuid AND e.branch_id = $2::uuid`,
		clusterID.String(), branchID.String())
	if err != nil {
		return nil, fmt.Errorf("list endpoint ids for branch: %w", err)
	}
	defer rows.Close()
	return scanUUIDs(rows)
}

// EndpointIDsForCluster returns the ids of every endpoint in the cluster (the
// delete_cluster step's Execute-time enumeration). An absent cluster yields an
// empty slice — no ErrNotFound.
func (b *PgBackend) EndpointIDsForCluster(ctx context.Context, clusterID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := b.pool.Query(ctx,
		`SELECT e.id::text FROM amp_registry.endpoints e
		  JOIN amp_registry.branches b ON b.branch_id = e.branch_id
		 WHERE b.cluster_id = $1::uuid`,
		clusterID.String())
	if err != nil {
		return nil, fmt.Errorf("list endpoint ids for cluster: %w", err)
	}
	defer rows.Close()
	return scanUUIDs(rows)
}

// ChildBranchIDs returns the ids of every LIVE branch in clusterID forked
// directly from branchID (the delete_branch step's pre-cascade check). An absent
// branch yields an empty slice — no ErrNotFound. Tombstoned children are skipped:
// they keep their parent pointer forever (the row stays), so counting them would
// make a parent permanently undeletable.
func (b *PgBackend) ChildBranchIDs(ctx context.Context, clusterID, branchID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := b.pool.Query(ctx,
		`SELECT branch_id::text FROM amp_registry.branches
		  WHERE cluster_id = $1::uuid AND parent_branch_id = $2::uuid AND dropped_at IS NULL`,
		clusterID.String(), branchID.String())
	if err != nil {
		return nil, fmt.Errorf("list child branch ids: %w", err)
	}
	defer rows.Close()
	return scanUUIDs(rows)
}

// DeleteBranch tombstones the (clusterID, branchID) branch: it stamps
// `dropped_at` rather than deleting the row, because a branch id is never reused
// and the branch's page/WAL data in amp_storage outlives the drop. Every
// reader filters `dropped_at IS NULL`, so the branch stops existing as far as
// both the product API and the storage-layer control surface are concerned.
//
// `dropped_at` is ALL this writes: the branch-drop reaper owns `reap_after` and
// stamps it from this timestamp on its next tick (branchgc.stampDeadlines).
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
// Both guards are READS, and a read does not conflict with the writer it is
// guarding against — an endpoint creation that has not committed yet is invisible
// here. What closes that window is on the other side: EnsureEndpointRecord writes
// this same branch row, so the two serialize. See the interlock comment there.
//
// It also keeps *MemBackend.DeleteBranch (which reports ErrConflict) honest.
func (b *PgBackend) DeleteBranch(ctx context.Context, clusterID, branchID uuid.UUID) error {
	tag, err := b.pool.Exec(ctx,
		`UPDATE amp_registry.branches SET dropped_at = now()
		  WHERE cluster_id = $1::uuid AND branch_id = $2::uuid AND dropped_at IS NULL
		    AND NOT EXISTS (
		        SELECT 1 FROM amp_registry.branches c
		         WHERE c.cluster_id = $1::uuid AND c.parent_branch_id = $2::uuid
		           AND c.dropped_at IS NULL)
		    AND NOT EXISTS (
		        SELECT 1 FROM amp_registry.endpoints e WHERE e.branch_id = $2::uuid)`,
		clusterID.String(), branchID.String())
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
	//
	// Liveness is the WHOLE classification: the UPDATE moved nothing, so a row that
	// is still live had `dropped_at IS NULL`, which leaves one of the two NOT EXISTS
	// guards as the only thing that can have failed — it WAS blocked.
	//
	// Re-checking the guards here would be a bug rather than extra rigour. This runs
	// in a transaction of its own, so it would answer "is a blocker visible now?"
	// where the question is "did the tombstone happen?" — and a blocker whose whole
	// job is to make the caller retry can vanish in the gap: the racing fork's child
	// is dropped by its own task, the endpoint created after the step's enumeration
	// is deleted. "Retry me" would become "done", leaving a live branch whose
	// endpoints the step already destroyed and which GC will never reclaim for want
	// of a tombstone.
	var live bool
	if err := b.pool.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM amp_registry.branches
		     WHERE cluster_id = $1::uuid AND branch_id = $2::uuid AND dropped_at IS NULL)`,
		clusterID.String(), branchID.String()).Scan(&live); err != nil {
		return fmt.Errorf("classify branch tombstone no-op: %w", err)
	}
	if live {
		return ErrConflict
	}
	return nil
}

// DeleteCluster tombstones the cluster and every branch under it, and revokes
// its grants (idempotent; absent rows are a no-op). The caller retires the
// cluster's endpoints first. Each branch it tombstones is left for the reaper
// exactly as DeleteBranch leaves one, so a cluster's branches are reclaimed
// without a cluster-shaped code path of its own — the cluster row itself is not
// reclaimed by anything today.
//
// The cluster row has to be tombstoned rather than deleted *because* its branches
// are: the branch -> cluster FK is RESTRICT, so a cluster whose branches survive
// as tombstones can never be deleted. Tombstoning both keeps the whole lineage
// legible after the fact, and it retires the pre-tombstone dance this used to
// need — nulling every in-cluster parent pointer to sidestep the self-FK ordering
// before a bulk delete — because an UPDATE never trips a foreign key.
//
// Grants are still hard-deleted: access is not history worth keeping, and a
// revoked grant should not linger on a dropped cluster.
//
// This transaction deliberately does NOT touch endpoints, but it does refuse to
// run while any of the cluster's branches still has one. That check has to be
// explicit: tombstoning is an UPDATE that never writes `branch_id`, so unlike the
// old DELETE it leaves the endpoints FK unexamined and would otherwise succeed
// with live endpoints still attached. Failing here is what makes an endpoint
// added between the step's enumeration and this transaction replay the task and
// retire that endpoint's instance before dropping its record — preserving the
// invariant that an endpoint record is never dropped without retiring its
// instance. Like the one in DeleteBranch that check is a read, and what makes it
// hold against an endpoint creation still in flight is the branch-row interlock
// on the other side (EnsureEndpointRecord): the tombstone below writes every
// branch row in the cluster, so the two serialize.
func (b *PgBackend) DeleteCluster(ctx context.Context, clusterID uuid.UUID) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	id := clusterID.String()
	var hasEndpoints bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM amp_registry.endpoints e
		      JOIN amp_registry.branches b ON b.branch_id = e.branch_id
		     WHERE b.cluster_id = $1::uuid)`,
		id).Scan(&hasEndpoints); err != nil {
		return fmt.Errorf("check cluster endpoints: %w", err)
	}
	if hasEndpoints {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx,
		`UPDATE amp_registry.branches SET dropped_at = now() WHERE cluster_id = $1::uuid AND dropped_at IS NULL`,
		id); err != nil {
		return fmt.Errorf("tombstone cluster branches: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM amp_registry.user_cluster_grants WHERE cluster_id = $1::uuid`, id); err != nil {
		return fmt.Errorf("delete cluster grants: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE amp_registry.clusters SET dropped_at = now() WHERE id = $1::uuid AND dropped_at IS NULL`,
		id); err != nil {
		return fmt.Errorf("tombstone cluster: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit delete cluster: %w", err)
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
