package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/lib/pq"

	"meko/api_server/internal/models"
)

// ErrNoEligibleEnvironment is returned when a region has no MANAGED amp environment
// at all for a storage pool to pin to. It is the region-addressed equivalent of a 404:
// the caller named a region nothing serves.
var ErrNoEligibleEnvironment = errors.New("no eligible MANAGED environment for region")

// ErrEnvironmentUnavailable is returned when a region has MANAGED amp environments but
// none can take a new storage pool right now (still enrolling, suspended, a stale
// heartbeat, or DEGRADED by more than one tenant's resources). Unlike
// ErrNoEligibleEnvironment the same call can succeed later, so it is a retryable
// status, not a 404.
var ErrEnvironmentUnavailable = errors.New("MANAGED environment temporarily unavailable for region")

// ErrConcurrentPoolUpdate is returned when a region PATCH lost a Postgres deadlock
// (SQLSTATE 40P01) to a concurrent write. The transaction rolled back whole, so the
// identical request is safe to retry -- hence a retryable status rather than a 500.
// The single pool-scoped advisory lock in ApplyStoragePoolPatches makes an advisory-lock
// deadlock unreachable, so this is a backstop for any other cycle (e.g. row-level).
var ErrConcurrentPoolUpdate = errors.New("storage pool update lost a concurrent-write race; retry")

// isDeadlock40P01 reports whether err is a Postgres deadlock. The control plane runs on
// plain PostgreSQL, which raises 40P01 when it breaks a lock cycle.
func isDeadlock40P01(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "40P01"
}

// StoragePoolAssignment is one (environment, region) a region-addressed call resolved
// to, returned so the service can read each environment's resources back for the
// response.
type StoragePoolAssignment struct {
	EnvUUID string
	Region  string
}

// ApplyStoragePoolPatches applies a storage pool's region-addressed patches across
// every named region in ONE transaction, all-or-nothing. Any error rolls the whole
// call back, so a bad item in one region leaves every region untouched.
//
// It takes a single pool-scoped advisory lock up front, then:
//
//  1. Resolve every region to the MANAGED environment that serves the pool there,
//     first-touch-assigning one when unmapped, in region order.
//  2. Apply each region's batch, auto-creating the storage-pool row on first touch.
//
// The pool-scoped lock serializes every PATCH for one storage pool — a PATCH is scoped
// to exactly one pool, so it takes exactly one advisory lock and no ordering cycle is
// possible. Serializing same-pool PATCHes (including ones to different regions) is what
// makes the per-pool cluster-name uniqueness check race-free. Different pools use
// different keys and still run in parallel — even two pools pinned to the same shared
// environment, since their rows are namespaced by the pool prefix.
//
// poolSpec is the synthesized storage-pool spec used only when the pool row is
// auto-created (the caller derived its owner from the pool name).
// guard, when non-nil, vets every update onto an existing resource (UpdateGuard).
//
// A lost deadlock (40P01) is translated to ErrConcurrentPoolUpdate so the caller can
// map it to a retryable status; every other error is returned verbatim.
func (r *EnvironmentRepo) ApplyStoragePoolPatches(
	storagePool string, regions []models.RegionResources, poolSpec []byte, guard UpdateGuard,
) ([]StoragePoolAssignment, error) {
	assignments, err := r.applyStoragePoolPatches(storagePool, regions, poolSpec, guard)
	if err != nil && isDeadlock40P01(err) {
		return nil, fmt.Errorf("%w: %v", ErrConcurrentPoolUpdate, err)
	}
	return assignments, err
}

func (r *EnvironmentRepo) applyStoragePoolPatches(
	storagePool string, regions []models.RegionResources, poolSpec []byte, guard UpdateGuard,
) ([]StoragePoolAssignment, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Lock the whole storage pool before touching anything. One key per call (a PATCH
	// is scoped to one pool), so no lock-ordering cycle exists and no sorted pass is
	// needed. This serializes every PATCH for the pool — including ones to different
	// regions — which is what makes the cross-environment cluster-name check race-free.
	// Different pools use different keys and stay concurrent, even when pinned to the
	// same shared environment (their rows are namespaced by the pool prefix).
	if _, err := tx.Exec(
		`SELECT pg_advisory_xact_lock(hashtext('storage-pool:' || $1))`, storagePool,
	); err != nil {
		return nil, fmt.Errorf("lock storage pool %s: %w", storagePool, err)
	}

	// Phase 1: resolve every region to its environment, in region order.
	envByIdx := make([]string, len(regions))
	order := make([]int, len(regions))
	for i := range regions {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return regions[order[a]].Region < regions[order[b]].Region })
	for _, i := range order {
		envUUID, err := assignStoragePoolEnvTx(tx, storagePool, regions[i].Region)
		if err != nil {
			return nil, err
		}
		envByIdx[i] = envUUID
	}

	// Phase 2: apply each region's batch. Body order, so the response mirrors the
	// request; the returned assignments feed the read-back.
	assignments := make([]StoragePoolAssignment, 0, len(regions))
	var changedEnvs []string
	for i, rr := range regions {
		envUUID := envByIdx[i]
		patches := rr.Resources
		synth, err := needsPoolSynthesis(tx, envUUID, storagePool, patches)
		if err != nil {
			return nil, err
		}
		if synth {
			// Prepend so the pool exists before any child patch in the same batch.
			patches = append([]models.ResourcePatch{{
				Kind: "storage-pool", Name: storagePool, Spec: poolSpec,
			}}, patches...)
		}
		changed, err := applyDesiredPatchesTx(tx, envUUID, storagePool, patches, guard)
		if err != nil {
			return nil, err
		}
		if changed {
			changedEnvs = append(changedEnvs, envUUID)
		}
		assignments = append(assignments, StoragePoolAssignment{EnvUUID: envUUID, Region: rr.Region})
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	r.bumpDesiredRev(changedEnvs...)
	return assignments, nil
}

// ListStoragePoolEnvs returns every (environment, region) a storage pool is mapped to,
// skipping DEREGISTERED environments (a withdrawn environment reads as if unmapped)
// and any non-amp environment (belt-and-suspenders: assignment is amp-only, so a
// non-amp pin should not exist, but hiding one keeps the LIST consistent with what a
// PATCH would resolve). A pure read for the by-name LIST: it never assigns, so an
// unmapped pool returns an empty slice.
func (r *EnvironmentRepo) ListStoragePoolEnvs(storagePool string) ([]StoragePoolAssignment, error) {
	rows, err := r.db.Query(
		`SELECT m.environment_id, m.region
		   FROM meko_system.environment_aeon_account_mapping m
		   JOIN meko_system.environments e ON e.uuid = m.environment_id
		  WHERE m.storage_pool_name = $1 AND e.type = 'MANAGED'
		    AND COALESCE(e.kind, 'meko') = 'amp' AND e.state <> 'DEREGISTERED'
		  ORDER BY m.region ASC`, storagePool)
	if err != nil {
		return nil, fmt.Errorf("list storage pool environments: %w", err)
	}
	defer rows.Close()
	out := []StoragePoolAssignment{}
	for rows.Next() {
		var a StoragePoolAssignment
		if err := rows.Scan(&a.EnvUUID, &a.Region); err != nil {
			return nil, fmt.Errorf("scan storage pool environment: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// assignStoragePoolEnvTx resolves a (storage_pool, region) to its environment,
// assigning one on first touch. It is the sticky mapping's read-or-write:
//
//   - an existing pin whose environment is not DEREGISTERED is reused (kept alive
//     through SUSPENDED/DRAINING/etc — only a withdrawn environment fails over);
//   - a pin to a DEREGISTERED environment fails over to an eligible one and re-pins;
//   - no pin picks an eligible MANAGED amp environment and records it.
//
// Assignments (new or re-pin) require an eligible MANAGED amp environment (see
// pickManagedEnvTx); ErrNoEligibleEnvironment or ErrEnvironmentUnavailable when the
// region offers none. The ON CONFLICT re-read handles two callers racing the first
// assignment: the deterministic pick makes them choose the same environment, and the
// unique (storage_pool_name, region) index makes exactly one row win.
func assignStoragePoolEnvTx(tx *sql.Tx, storagePool, region string) (string, error) {
	var envUUID, state string
	err := tx.QueryRow(
		`SELECT m.environment_id, e.state
		   FROM meko_system.environment_aeon_account_mapping m
		   JOIN meko_system.environments e ON e.uuid = m.environment_id
		  WHERE m.storage_pool_name = $1 AND m.region = $2`, storagePool, region).Scan(&envUUID, &state)
	switch {
	case err == nil && state != "DEREGISTERED":
		return envUUID, nil
	case err == nil:
		next, rerr := pickManagedEnvTx(tx, region)
		if rerr != nil {
			return "", rerr
		}
		if _, uerr := tx.Exec(
			`UPDATE meko_system.environment_aeon_account_mapping
			    SET environment_id = $1, updated_on = NOW()
			  WHERE storage_pool_name = $2 AND region = $3`, next, storagePool, region); uerr != nil {
			return "", fmt.Errorf("re-pin storage pool %s/%s: %w", storagePool, region, uerr)
		}
		return next, nil
	case errors.Is(err, sql.ErrNoRows):
		next, rerr := pickManagedEnvTx(tx, region)
		if rerr != nil {
			return "", rerr
		}
		if _, ierr := tx.Exec(
			`INSERT INTO meko_system.environment_aeon_account_mapping
			     (environment_id, region, storage_pool_name)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (storage_pool_name, region) DO NOTHING`, next, region, storagePool); ierr != nil {
			return "", fmt.Errorf("assign storage pool %s/%s: %w", storagePool, region, ierr)
		}
		if rerr := tx.QueryRow(
			`SELECT environment_id FROM meko_system.environment_aeon_account_mapping
			  WHERE storage_pool_name = $1 AND region = $2`, storagePool, region).Scan(&next); rerr != nil {
			return "", fmt.Errorf("read storage pool assignment %s/%s: %w", storagePool, region, rerr)
		}
		return next, nil
	default:
		return "", fmt.Errorf("resolve storage pool %s/%s: %w", storagePool, region, err)
	}
}

// PickManagedEnv returns the environment a first PATCH for a new storage pool would
// pin in region, without pinning it: the same rule and errors as pickManagedEnvTx,
// in a transaction that writes nothing. A pool already mapped there may sit elsewhere.
func (r *EnvironmentRepo) PickManagedEnv(region string) (string, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return "", fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // reads only, so there is nothing to keep
	return pickManagedEnvTx(tx, region)
}

// pickManagedEnvTx deterministically chooses one eligible MANAGED amp environment in a
// region: READY before DEGRADED, then the oldest, with uuid as a stable tiebreak so the
// choice never depends on scan order.
//
// Either state needs a heartbeat under 90s old, the console's stale threshold: the row
// keeps its last reported state when the agent stops reporting, so READY alone says
// nothing about now. DEGRADED is eligible only when the agent also reported
// accepts_new_pools (CLOUDGA-37501): the agent degrades the whole environment over any
// one resource, and one tenant's stuck resize must not turn every new tenant away, but
// a degradation that is the environment's own — its controller unreachable, telemetry
// failing — must. The agent decides which, since only it can tell them apart.
//
// No row: ErrEnvironmentUnavailable when the region has an environment that could
// become eligible, ErrNoEligibleEnvironment when it has none. DRAINING counts as none,
// since it is being withdrawn and a retry would not land on it.
//
// kind = 'amp' as well as type = 'MANAGED': the two columns are independent, and these
// resources are amp kinds (storage-pool/cluster/branch/endpoint). Pinning them to a
// MANAGED meko environment would pass the KindAmp validation but never converge —
// permanent drift — so a meko environment is not eligible here. COALESCE mirrors
// envColumns: a legacy null-kind row reads as meko and is correctly excluded.
func pickManagedEnvTx(tx *sql.Tx, region string) (string, error) {
	var envUUID string
	err := tx.QueryRow(
		`SELECT uuid FROM meko_system.environments
		  WHERE type = 'MANAGED' AND COALESCE(kind, 'meko') = 'amp' AND region = $1
		    AND last_heartbeat_at > NOW() - interval '90 seconds'
		    AND (state = 'READY' OR (state = 'DEGRADED' AND accepts_new_pools))
		  ORDER BY (state = 'READY') DESC, created_at ASC, uuid ASC LIMIT 1`, region).Scan(&envUUID)
	if errors.Is(err, sql.ErrNoRows) {
		var served bool
		if qerr := tx.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM meko_system.environments
			  WHERE type = 'MANAGED' AND COALESCE(kind, 'meko') = 'amp' AND region = $1
			    AND state NOT IN ('DEREGISTERED', 'DRAINING'))`, region).Scan(&served); qerr != nil {
			return "", fmt.Errorf("check managed environments for %s: %w", region, qerr)
		}
		if served {
			return "", fmt.Errorf("%w: %s", ErrEnvironmentUnavailable, region)
		}
		return "", fmt.Errorf("%w: %s", ErrNoEligibleEnvironment, region)
	}
	if err != nil {
		return "", fmt.Errorf("pick managed environment for %s: %w", region, err)
	}
	return envUUID, nil
}

// needsPoolSynthesis reports whether the storage-pool row must be auto-created before
// applying a region's batch: true only when the caller did not name the pool itself,
// the batch has at least one non-delete patch (a delete-only batch parents nothing),
// and the pool row does not already exist.
func needsPoolSynthesis(tx *sql.Tx, envUUID, pool string, patches []models.ResourcePatch) (bool, error) {
	hasNonDelete := false
	for _, p := range patches {
		if p.Kind == "storage-pool" && p.Name == pool {
			return false, nil // caller owns the pool row explicitly
		}
		if !p.Delete {
			hasNonDelete = true
		}
	}
	if !hasNonDelete {
		return false, nil
	}
	exists, err := poolRowExistsTx(tx, envUUID, pool)
	if err != nil {
		return false, err
	}
	return !exists, nil
}

// poolRowExistsTx reports whether a storage-pool desired row already exists for an
// environment, so a first touch can auto-create it and a later one skip that.
func poolRowExistsTx(tx *sql.Tx, envUUID, pool string) (bool, error) {
	var exists bool
	err := tx.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM meko_system.environment_desired_resources
		   WHERE env_uuid = $1 AND kind = 'storage-pool' AND name = $2)`, envUUID, pool).Scan(&exists)
	return exists, err
}
