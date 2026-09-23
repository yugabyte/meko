package repository

import (
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"meko/api_server/internal/models"
)

const poolRegion = "us-east-1"

// First touch of a (storage_pool, region): no mapping exists, so the call picks a
// READY MANAGED environment, records the mapping, locks the environment, auto-creates
// the storage-pool row (absent), and applies the child patch — all in one commit.
func TestApplyStoragePoolPatches_FirstTouchAssignsAndSynthesizes(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectBegin()
	// No existing pin.
	mock.ExpectQuery(`SELECT m\.environment_id, e\.state`).
		WithArgs("acct-1", poolRegion).
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "state"}))
	// Pick a READY MANAGED environment in the region.
	mock.ExpectQuery(`SELECT uuid FROM meko_system\.environments\s+WHERE type = 'MANAGED' AND COALESCE\(kind, 'meko'\) = 'amp'`).
		WithArgs(poolRegion).
		WillReturnRows(sqlmock.NewRows([]string{"uuid"}).AddRow(envUUID))
	// Record the mapping, then re-read the winner.
	mock.ExpectExec(`INSERT INTO meko_system\.environment_aeon_account_mapping`).
		WithArgs(envUUID, poolRegion, "acct-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT environment_id FROM meko_system\.environment_aeon_account_mapping`).
		WithArgs("acct-1", poolRegion).
		WillReturnRows(sqlmock.NewRows([]string{"environment_id"}).AddRow(envUUID))
	// Serialize this environment's writes.
	mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(envUUID, "acct-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Pool row absent -> synthesize it.
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs(envUUID, "acct-1").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	// applyDesiredPatchesTx: storage-pool (synthesized) then the cluster.
	mock.ExpectQuery(`SELECT COALESCE\(spec::text`).
		WithArgs(envUUID, "storage-pool", "acct-1").
		WillReturnRows(sqlmock.NewRows([]string{"spec"}))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "storage-pool", "acct-1", []byte(`{"owner":"x"}`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT COALESCE\(spec::text`).
		WithArgs(envUUID, "cluster", "acct-1/main-db").
		WillReturnRows(sqlmock.NewRows([]string{"spec"}))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "cluster", "acct-1/main-db", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	assignments, err := repo.ApplyStoragePoolPatches("acct-1",
		[]models.RegionResources{{
			Region:    poolRegion,
			Resources: []models.ResourcePatch{{Kind: "cluster", Name: "acct-1/main-db", Spec: []byte(`{"pg_version":17}`)}},
		}},
		[]byte(`{"owner":"x"}`),
	)
	if err != nil {
		t.Fatalf("ApplyStoragePoolPatches: %v", err)
	}
	if len(assignments) != 1 || assignments[0].EnvUUID != envUUID || assignments[0].Region != poolRegion {
		t.Errorf("assignments = %+v, want one for %s/%s", assignments, envUUID, poolRegion)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// An existing pin to a non-DEREGISTERED environment is reused — no pick, no insert —
// and a pool that already exists is not re-synthesized.
func TestApplyStoragePoolPatches_ReusesPinNoSynthesis(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT m\.environment_id, e\.state`).
		WithArgs("acct-1", poolRegion).
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "state"}).AddRow(envUUID, "READY"))
	mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(envUUID, "acct-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Pool already exists -> no synthesis.
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs(envUUID, "acct-1").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`SELECT COALESCE\(spec::text`).
		WithArgs(envUUID, "cluster", "acct-1/main-db").
		WillReturnRows(sqlmock.NewRows([]string{"spec"}).AddRow(`{"pg_version":16}`))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "cluster", "acct-1/main-db", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if _, err := repo.ApplyStoragePoolPatches("acct-1",
		[]models.RegionResources{{
			Region:    poolRegion,
			Resources: []models.ResourcePatch{{Kind: "cluster", Name: "acct-1/main-db", Spec: []byte(`{"pg_version":17}`)}},
		}},
		[]byte(`{"owner":"x"}`),
	); err != nil {
		t.Fatalf("ApplyStoragePoolPatches: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A region with no READY MANAGED environment fails with ErrNoEligibleEnvironment and
// rolls the whole call back.
func TestApplyStoragePoolPatches_NoEligibleEnvIsError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT m\.environment_id, e\.state`).
		WithArgs("acct-1", poolRegion).
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "state"}))
	mock.ExpectQuery(`SELECT uuid FROM meko_system\.environments\s+WHERE type = 'MANAGED' AND COALESCE\(kind, 'meko'\) = 'amp'`).
		WithArgs(poolRegion).
		WillReturnRows(sqlmock.NewRows([]string{"uuid"})) // none
	mock.ExpectRollback()

	_, err = repo.ApplyStoragePoolPatches("acct-1",
		[]models.RegionResources{{
			Region:    poolRegion,
			Resources: []models.ResourcePatch{{Kind: "cluster", Name: "acct-1/main-db", Spec: []byte(`{}`)}},
		}},
		[]byte(`{"owner":"x"}`),
	)
	if err == nil {
		t.Fatal("err = nil, want ErrNoEligibleEnvironment")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A pin to a DEREGISTERED environment fails over: the call picks a new READY env and
// re-pins the mapping to it, then applies against the new env.
func TestApplyStoragePoolPatches_DeregisteredPinFailsOverAndRepins(t *testing.T) {
	const oldEnv = "99999999-9999-9999-9999-999999999999"
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectBegin()
	// Existing pin, but its environment is DEREGISTERED -> fail over.
	mock.ExpectQuery(`SELECT m\.environment_id, e\.state`).
		WithArgs("acct-1", poolRegion).
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "state"}).AddRow(oldEnv, "DEREGISTERED"))
	mock.ExpectQuery(`SELECT uuid FROM meko_system\.environments\s+WHERE type = 'MANAGED' AND COALESCE\(kind, 'meko'\) = 'amp'`).
		WithArgs(poolRegion).
		WillReturnRows(sqlmock.NewRows([]string{"uuid"}).AddRow(envUUID))
	// Re-pin the mapping to the new environment.
	mock.ExpectExec(`UPDATE meko_system\.environment_aeon_account_mapping\s+SET environment_id`).
		WithArgs(envUUID, "acct-1", poolRegion).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Everything after resolves against the NEW env.
	mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(envUUID, "acct-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs(envUUID, "acct-1").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`SELECT COALESCE\(spec::text`).
		WithArgs(envUUID, "cluster", "acct-1/main-db").
		WillReturnRows(sqlmock.NewRows([]string{"spec"}))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "cluster", "acct-1/main-db", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	assignments, err := repo.ApplyStoragePoolPatches("acct-1",
		[]models.RegionResources{{
			Region:    poolRegion,
			Resources: []models.ResourcePatch{{Kind: "cluster", Name: "acct-1/main-db", Spec: []byte(`{"pg_version":17}`)}},
		}},
		[]byte(`{"owner":"x"}`),
	)
	if err != nil {
		t.Fatalf("ApplyStoragePoolPatches: %v", err)
	}
	if len(assignments) != 1 || assignments[0].EnvUUID != envUUID {
		t.Errorf("assignments = %+v, want the re-pinned env %s", assignments, envUUID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Two regions apply inside ONE transaction: a single Begin/Commit wraps both regions'
// resolve + lock + apply, and both assignments come back.
func TestApplyStoragePoolPatches_MultiRegionAtomic(t *testing.T) {
	const envWest = "88888888-8888-8888-8888-888888888888"
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectBegin()
	// Phase 1 resolves every region up front, in region order (east < west).
	mock.ExpectQuery(`SELECT m\.environment_id, e\.state`).
		WithArgs("acct-1", "us-east-1").
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "state"}).AddRow(envUUID, "READY"))
	mock.ExpectQuery(`SELECT m\.environment_id, e\.state`).
		WithArgs("acct-1", "us-west-2").
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "state"}).AddRow(envWest, "READY"))
	// Phase 2 locks every distinct env in one pass, sorted by uuid (envUUID < envWest).
	mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(envUUID, "acct-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(envWest, "acct-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Phase 3 applies each region's batch in body order.
	mock.ExpectQuery(`SELECT EXISTS`).WithArgs(envUUID, "acct-1").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`SELECT COALESCE\(spec::text`).
		WithArgs(envUUID, "cluster", "acct-1/east-db").
		WillReturnRows(sqlmock.NewRows([]string{"spec"}))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "cluster", "acct-1/east-db", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT EXISTS`).WithArgs(envWest, "acct-1").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`SELECT COALESCE\(spec::text`).
		WithArgs(envWest, "cluster", "acct-1/west-db").
		WillReturnRows(sqlmock.NewRows([]string{"spec"}))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envWest, "cluster", "acct-1/west-db", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	assignments, err := repo.ApplyStoragePoolPatches("acct-1",
		[]models.RegionResources{
			{Region: "us-east-1", Resources: []models.ResourcePatch{{Kind: "cluster", Name: "acct-1/east-db", Spec: []byte(`{}`)}}},
			{Region: "us-west-2", Resources: []models.ResourcePatch{{Kind: "cluster", Name: "acct-1/west-db", Spec: []byte(`{}`)}}},
		},
		[]byte(`{"owner":"x"}`),
	)
	if err != nil {
		t.Fatalf("ApplyStoragePoolPatches: %v", err)
	}
	if len(assignments) != 2 || assignments[0].EnvUUID != envUUID || assignments[1].EnvUUID != envWest {
		t.Errorf("assignments = %+v, want one per region", assignments)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A failure in the second region rolls the whole call back: region one's writes do not
// survive, because the two regions share one transaction.
func TestApplyStoragePoolPatches_RollsBackWhenSecondRegionFails(t *testing.T) {
	const envWest = "88888888-8888-8888-8888-888888888888"
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectBegin()
	// Phase 1 resolves both regions, phase 2 locks both (sorted by uuid).
	mock.ExpectQuery(`SELECT m\.environment_id, e\.state`).
		WithArgs("acct-1", "us-east-1").
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "state"}).AddRow(envUUID, "READY"))
	mock.ExpectQuery(`SELECT m\.environment_id, e\.state`).
		WithArgs("acct-1", "us-west-2").
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "state"}).AddRow(envWest, "READY"))
	mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(envUUID, "acct-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(envWest, "acct-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Phase 3: region 1 applies fully; region 2's apply fails -> the whole tx rolls back.
	mock.ExpectQuery(`SELECT EXISTS`).WithArgs(envUUID, "acct-1").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`SELECT COALESCE\(spec::text`).
		WithArgs(envUUID, "cluster", "acct-1/east-db").
		WillReturnRows(sqlmock.NewRows([]string{"spec"}))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "cluster", "acct-1/east-db", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT EXISTS`).WithArgs(envWest, "acct-1").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`SELECT COALESCE\(spec::text`).
		WithArgs(envWest, "cluster", "acct-1/west-db").
		WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	_, err = repo.ApplyStoragePoolPatches("acct-1",
		[]models.RegionResources{
			{Region: "us-east-1", Resources: []models.ResourcePatch{{Kind: "cluster", Name: "acct-1/east-db", Spec: []byte(`{}`)}}},
			{Region: "us-west-2", Resources: []models.ResourcePatch{{Kind: "cluster", Name: "acct-1/west-db", Spec: []byte(`{}`)}}},
		},
		[]byte(`{"owner":"x"}`),
	)
	if err == nil {
		t.Fatal("err = nil, want the second region to fail the whole call")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a failed region did not roll the call back: %v", err)
	}
}

// When the caller names the storage-pool row itself, synthesis is skipped entirely —
// no EXISTS probe — and the pool is applied as the caller sent it.
func TestApplyStoragePoolPatches_ExplicitPoolSkipsSynthesis(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT m\.environment_id, e\.state`).
		WithArgs("acct-1", poolRegion).
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "state"}).AddRow(envUUID, "READY"))
	mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(envUUID, "acct-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// No SELECT EXISTS: the caller owns the pool row. Apply pool then cluster as given.
	mock.ExpectQuery(`SELECT COALESCE\(spec::text`).
		WithArgs(envUUID, "storage-pool", "acct-1").
		WillReturnRows(sqlmock.NewRows([]string{"spec"}))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "storage-pool", "acct-1", []byte(`{"owner_user_id":"caller"}`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT COALESCE\(spec::text`).
		WithArgs(envUUID, "cluster", "acct-1/main-db").
		WillReturnRows(sqlmock.NewRows([]string{"spec"}))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "cluster", "acct-1/main-db", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if _, err := repo.ApplyStoragePoolPatches("acct-1",
		[]models.RegionResources{{
			Region: poolRegion,
			Resources: []models.ResourcePatch{
				{Kind: "storage-pool", Name: "acct-1", Spec: []byte(`{"owner_user_id":"caller"}`)},
				{Kind: "cluster", Name: "acct-1/main-db", Spec: []byte(`{}`)},
			},
		}},
		[]byte(`{"owner":"synth"}`),
	); err != nil {
		t.Fatalf("ApplyStoragePoolPatches: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A delete-only batch parents nothing, so synthesis is skipped (no EXISTS probe) and
// only the subtree DELETE runs.
func TestApplyStoragePoolPatches_DeleteOnlyBatchNoSynthesis(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT m\.environment_id, e\.state`).
		WithArgs("acct-1", poolRegion).
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "state"}).AddRow(envUUID, "READY"))
	mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(envUUID, "acct-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// No SELECT EXISTS, no INSERT: just the subtree delete.
	mock.ExpectExec(`DELETE FROM meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "acct-1/main-db").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if _, err := repo.ApplyStoragePoolPatches("acct-1",
		[]models.RegionResources{{
			Region:    poolRegion,
			Resources: []models.ResourcePatch{{Kind: "cluster", Name: "acct-1/main-db", Delete: true}},
		}},
		[]byte(`{"owner":"x"}`),
	); err != nil {
		t.Fatalf("ApplyStoragePoolPatches: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A Postgres deadlock (40P01) taking the advisory lock is translated to the retryable
// ErrConcurrentPoolUpdate sentinel, and the transaction rolls back.
func TestApplyStoragePoolPatches_DeadlockTranslated(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT m\.environment_id, e\.state`).
		WithArgs("acct-1", poolRegion).
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "state"}).AddRow(envUUID, "READY"))
	mock.ExpectExec(`pg_advisory_xact_lock`).WithArgs(envUUID, "acct-1").
		WillReturnError(&pq.Error{Code: "40P01", Message: "deadlock detected"})
	mock.ExpectRollback()

	_, err = repo.ApplyStoragePoolPatches("acct-1",
		[]models.RegionResources{{
			Region:    poolRegion,
			Resources: []models.ResourcePatch{{Kind: "cluster", Name: "acct-1/main-db", Spec: []byte(`{}`)}},
		}},
		[]byte(`{"owner":"x"}`),
	)
	if !errors.Is(err, ErrConcurrentPoolUpdate) {
		t.Fatalf("err = %v, want ErrConcurrentPoolUpdate", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ListStoragePoolEnvs is a pure read that joins the mapping to environments and skips
// DEREGISTERED ones (the SQL does; here we assert the rows it returns are surfaced).
func TestListStoragePoolEnvs(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectQuery(`SELECT m\.environment_id, m\.region\s+FROM meko_system\.environment_aeon_account_mapping[\s\S]*COALESCE\(e\.kind, 'meko'\) = 'amp'`).
		WithArgs("acct-1").
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "region"}).
			AddRow(envUUID, "us-east-1").
			AddRow(envUUID, "us-west-2"))

	got, err := repo.ListStoragePoolEnvs("acct-1")
	if err != nil {
		t.Fatalf("ListStoragePoolEnvs: %v", err)
	}
	if len(got) != 2 || got[0].Region != "us-east-1" || got[1].Region != "us-west-2" {
		t.Errorf("got %+v, want two assignments", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
