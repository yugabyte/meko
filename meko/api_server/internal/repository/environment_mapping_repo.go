package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/lib/pq"

	"meko/api_server/internal/models"
)

// ErrNoEligibleEnvironment is returned when a region has no READY MANAGED amp
// environment for a storage pool to pin to. It is the region-addressed equivalent of a
// 404: the caller named a region nothing can serve.
var ErrNoEligibleEnvironment = errors.New("no eligible MANAGED environment for region")

// ErrConcurrentPoolUpdate is returned when a region PATCH lost a Postgres deadlock
// (SQLSTATE 40P01) to a concurrent write. The transaction rolled back whole, so the
// identical request is safe to retry -- hence a retryable status rather than a 500.
// The sorted lock acquisition in ApplyStoragePoolPatches makes an advisory-lock
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
// It runs in three phases so that two concurrent PATCHes cannot deadlock, regardless
// of the region order in their bodies:
//
//  1. Resolve every region to the MANAGED environment that serves the pool there,
//     first-touch-assigning one when unmapped. Done in region order so two callers
//     first-touching the same regions insert their mapping rows in the same order.
//  2. Take the per-(environment, storage-pool) advisory lock for every distinct
//     environment resolved, in one pass sorted by environment uuid. These locks are
//     what serialize this pool's desired-state writes against a concurrent PATCH;
//     acquiring them all up front in a single global order is what removes the
//     deadlock (see below). Transaction-scoped, so every one releases at commit or
//     rollback.
//  3. Apply each region's batch, auto-creating the storage-pool row on first touch.
//     Order no longer matters here -- every lock this call needs is already held.
//
// The deadlock the ordering avoids: locks were previously taken one region at a time
// in body order and held until commit, so PATCH A over [east, west] and PATCH B over
// [west, east] would each grab their first lock and then block on the other's --
// Postgres aborts one with 40P01, surfaced as a 500. Resolving first and then locking
// in a single sorted pass makes both callers request the same lock sequence, so one
// simply waits for the other.
//
// The lock is keyed on (environment uuid, storage pool) via the two-int4 form, so
// different pools on the same environment still run in parallel -- a PATCH is scoped
// to one pool and rows are namespaced by the pool prefix, so two pools never touch
// each other's rows.
//
// poolSpec is the synthesized storage-pool spec used only when the pool row is
// auto-created (the caller derived its owner from the pool name).
//
// A lost deadlock (40P01) is translated to ErrConcurrentPoolUpdate so the caller can
// map it to a retryable status; every other error is returned verbatim.
func (r *EnvironmentRepo) ApplyStoragePoolPatches(
	storagePool string, regions []models.RegionResources, poolSpec []byte,
) ([]StoragePoolAssignment, error) {
	assignments, err := r.applyStoragePoolPatches(storagePool, regions, poolSpec)
	if err != nil && isDeadlock40P01(err) {
		return nil, fmt.Errorf("%w: %v", ErrConcurrentPoolUpdate, err)
	}
	return assignments, err
}

func (r *EnvironmentRepo) applyStoragePoolPatches(
	storagePool string, regions []models.RegionResources, poolSpec []byte,
) ([]StoragePoolAssignment, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

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

	// Phase 2: lock every distinct environment, sorted by uuid, before any write.
	seen := make(map[string]bool, len(envByIdx))
	lockOrder := make([]string, 0, len(envByIdx))
	for _, envUUID := range envByIdx {
		if !seen[envUUID] {
			seen[envUUID] = true
			lockOrder = append(lockOrder, envUUID)
		}
	}
	sort.Strings(lockOrder)
	for _, envUUID := range lockOrder {
		if _, err := tx.Exec(
			`SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`, envUUID, storagePool,
		); err != nil {
			return nil, fmt.Errorf("lock environment %s pool %s: %w", envUUID, storagePool, err)
		}
	}

	// Phase 3: apply each region's batch. Body order, so the response mirrors the
	// request; the returned assignments feed the read-back.
	assignments := make([]StoragePoolAssignment, 0, len(regions))
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
		if err := applyDesiredPatchesTx(tx, envUUID, patches); err != nil {
			return nil, err
		}
		assignments = append(assignments, StoragePoolAssignment{EnvUUID: envUUID, Region: rr.Region})
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
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
//   - a pin to a DEREGISTERED environment fails over to a READY one and re-pins;
//   - no pin picks a READY MANAGED amp environment and records it.
//
// Assignments (new or re-pin) require a READY MANAGED amp environment (see
// pickManagedEnvTx); ErrNoEligibleEnvironment when the region offers none. The ON
// CONFLICT re-read handles two callers racing the first assignment: the deterministic
// pick makes them choose the same environment, and the unique (storage_pool_name,
// region) index makes exactly one row win.
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

// pickManagedEnvTx deterministically chooses one READY MANAGED amp environment in a
// region: the oldest, with uuid as a stable tiebreak so the choice never depends on
// scan order. ErrNoEligibleEnvironment when the region offers none.
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
		  WHERE type = 'MANAGED' AND COALESCE(kind, 'meko') = 'amp' AND state = 'READY' AND region = $1
		  ORDER BY created_at ASC, uuid ASC LIMIT 1`, region).Scan(&envUUID)
	if errors.Is(err, sql.ErrNoRows) {
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
