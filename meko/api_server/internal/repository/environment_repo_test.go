package repository

import (
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"meko/api_server/internal/models"
)

func TestEnvironmentRepo_GetByAPIKeyID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	// found: an exact hit on the unique api_key_id index.
	mock.ExpectQuery(`FROM meko_system\.environment_api_keys WHERE api_key_id = \$1`).
		WithArgs(keyA).
		WillReturnRows(listRow("acme-prod"))
	e, err := repo.GetByAPIKeyID(keyA)
	if err != nil || e == nil {
		t.Fatalf("GetByAPIKeyID = (%v, %v), want the bound environment", e, err)
	}
	if e.APIKeyID != keyA {
		t.Errorf("api key id = %q, want %q", e.APIKeyID, keyA)
	}

	// unbound key → (nil, nil), which is also how a revoked key surfaces.
	mock.ExpectQuery(`FROM meko_system\.environment_api_keys WHERE api_key_id = \$1`).
		WithArgs(keyB).
		WillReturnError(sql.ErrNoRows)
	e, err = repo.GetByAPIKeyID(keyB)
	if err != nil || e != nil {
		t.Fatalf("GetByAPIKeyID(miss) = (%v, %v), want (nil, nil)", e, err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestEnvironmentRepo_Get_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments e WHERE e\.uuid = \$1`).
		WithArgs("99999999-9999-9999-9999-999999999999").
		WillReturnError(sql.ErrNoRows)
	e, err := repo.GetByUUID("99999999-9999-9999-9999-999999999999")
	if err != nil {
		t.Fatalf("Get err = %v, want nil", err)
	}
	if e != nil {
		t.Fatalf("Get = %+v, want nil (not found)", e)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ListDesired feeds two callers with the same rows: the agent, which reconciles
// them, and the console, which shows an operator what an environment was told to
// run. The spec has to come back as the JSON it went in as — a spec that decodes to
// nothing is an agent that deploys a defaulted workload instead of the declared one.
func TestEnvironmentRepo_ListDesired(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	spec := `{"image":"registry/meko-mcp-server:2.1.0","image_pull_secret":"meko-ecr"}`
	mock.ExpectQuery(`SELECT kind, name, COALESCE\(spec::text, '\{\}'\), generation FROM meko_system\.environment_desired_resources`).
		WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "name", "spec", "generation"}).
			AddRow("database", "meko", "{}", int64(0)).
			AddRow("mcp-server", "mcp", spec, int64(3)))

	got, err := repo.ListDesired(envUUID)
	if err != nil {
		t.Fatalf("ListDesired err = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListDesired returned %d resources, want 2", len(got))
	}
	if got[0].Kind != "database" || got[0].Name != "meko" {
		t.Errorf("first resource = %s/%s, want database/meko", got[0].Kind, got[0].Name)
	}
	if string(got[1].Spec) != spec {
		t.Errorf("spec round-trip = %s, want %s", got[1].Spec, spec)
	}
	// Generation rides along so the agent can diff a mutated spec from an
	// unchanged one.
	if got[1].Generation != 3 {
		t.Errorf("generation round-trip = %d, want 3", got[1].Generation)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestEnvironmentRepo_ReplaceDesired(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	// Upsert-and-prune: read the existing keys, upsert each desired resource
	// (generation bumped in SQL only when the spec changed), then delete the ones
	// no longer desired. Here "stale" is withdrawn and "alpha" is kept.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT kind, name FROM meko_system\.environment_desired_resources WHERE env_uuid = \$1`).
		WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "name"}).
			AddRow("datapack", "alpha").
			AddRow("datapack", "stale"))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "datapack", "alpha", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM meko_system\.environment_desired_resources\s+`+
		`WHERE env_uuid=\$1 AND kind=\$2 AND name=\$3`).
		WithArgs(envUUID, "datapack", "stale").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = repo.ReplaceDesired(envUUID,
		[]models.DesiredResource{{Kind: "datapack", Name: "alpha"}})
	if err != nil {
		t.Fatalf("ReplaceDesired err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// An environment whose desired state was never written has no counter row; it must
// read as 0, a token like any other, not as an error that fails the agent's read.
func TestEnvironmentRepo_DesiredRev(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)
	const env = "11111111-1111-1111-1111-111111111111"

	mock.ExpectQuery(`SELECT rev FROM meko_system\.environment_desired_revs WHERE env_uuid = \$1`).
		WithArgs(env).WillReturnRows(sqlmock.NewRows([]string{"rev"}))
	if rev, err := repo.DesiredRev(env); err != nil || rev != 0 {
		t.Errorf("no row: DesiredRev = %d, %v; want 0, nil", rev, err)
	}
	mock.ExpectQuery(`SELECT rev FROM meko_system\.environment_desired_revs`).
		WithArgs(env).WillReturnRows(sqlmock.NewRows([]string{"rev"}).AddRow(int64(12)))
	if rev, err := repo.DesiredRev(env); err != nil || rev != 12 {
		t.Errorf("DesiredRev = %d, %v; want 12, nil", rev, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// The long-poll's watcher reads every held environment in one query; one with no
// counter row is left out of the map, which the caller reads as 0.
func TestEnvironmentRepo_DesiredRevs(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)
	const a, b = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"

	mock.ExpectQuery(`SELECT env_uuid::text, rev FROM meko_system\.environment_desired_revs ` +
		`WHERE env_uuid = ANY\(\$1::uuid\[\]\)`).
		WithArgs(pq.Array([]string{a, b})).
		WillReturnRows(sqlmock.NewRows([]string{"env_uuid", "rev"}).AddRow(a, int64(4)))
	revs, err := repo.DesiredRevs([]string{a, b})
	if err != nil {
		t.Fatalf("DesiredRevs: %v", err)
	}
	if len(revs) != 1 || revs[a] != 4 {
		t.Errorf("DesiredRevs = %v, want only %s at 4", revs, a)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Rows deregistered before the deregister action cleared them still hold the agent's
// last inventory and live=true (CLOUDGA-37659). They must read as neither.
func TestEnvironmentRepo_DeregisteredReadsNotLiveWithNoInventory(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	for state, wantLive := range map[string]bool{"DEREGISTERED": false, "READY": true} {
		inventory := `[{"kind":"cluster","id":"cluster/pool-a/c1","state":"ready"}]`
		mock.ExpectQuery(`FROM meko_system\.environments e WHERE e\.uuid = \$1`).
			WillReturnRows(sqlmock.NewRows(listRowCols).AddRow(
				envUUID, "acme-prod", listAccount, "cloud", "us-east-1", state, "", 0, "",
				0, 400, "", nil, "", "", "[]", true, keyA, time.Now(), "amp", inventory, nil, "BYOC", false,
			))
		e, err := repo.GetByUUID(envUUID)
		if err != nil || e == nil {
			t.Fatalf("%s: GetByUUID = (%v, %v)", state, e, err)
		}
		if e.Live != wantLive {
			t.Errorf("%s: live = %v, want %v", state, e.Live, wantLive)
		}
		wantRes := inventory
		if !wantLive {
			wantRes = "[]"
		}
		if string(e.Resources) != wantRes {
			t.Errorf("%s: resources = %s, want %s", state, e.Resources, wantRes)
		}
	}
}
