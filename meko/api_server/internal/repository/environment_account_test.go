package repository

import (
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/models"
)

// listRowCols is the column set scanEnvironment reads, in order.
var listRowCols = []string{
	"uuid", "id", "account_id", "placement", "region", "state", "k8s_version", "node_count", "provider",
	"datapacks", "max_datapacks", "agent_version", "last_heartbeat_at", "mcp_base_url",
	"cert_fingerprint", "components", "live", "enroll_token_hash", "enroll_token_expires_at",
	"status_token_hash", "created_at", "kind", "resources",
}

// listAccount is the account every row here belongs to; the tests that need a
// second account add its row inline.
const listAccount = "acct-1"

func listRow(id string) *sqlmock.Rows {
	return sqlmock.NewRows(listRowCols).AddRow(
		"11111111-1111-1111-1111-111111111111", id, listAccount, "cloud", "us-east-1", "READY", "", 0, "",
		0, 400, "", nil, "", "", "[]", true, "", nil, "", time.Now(), "meko", "[]",
	)
}

// A scoped list must filter in SQL rather than fetch the fleet and discard: the
// account is the only thing standing between two tenants' environments.
func TestEnvironmentRepo_ListFiltersByAccount(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectQuery(`FROM meko_system\.environments WHERE account_id = \$1 ORDER BY live DESC`).
		WithArgs("acct-1").
		WillReturnRows(listRow("acme-prod"))

	envs, err := repo.List("acct-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(envs) != 1 || envs[0].AccountID != "acct-1" {
		t.Fatalf("List(acct-1) = %+v, want the one acct-1 environment", envs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The unscoped list keeps its pre-account behavior — no WHERE and no bound
// argument — which is what the fleet console and the e2e call.
func TestEnvironmentRepo_ListUnscopedHasNoFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	// The regexp requires ORDER BY to follow the table directly, so it fails if a
	// WHERE clause is emitted.
	mock.ExpectQuery(`FROM meko_system\.environments ORDER BY live DESC`).
		WillReturnRows(listRow("acme-prod").AddRow(
			"22222222-2222-2222-2222-222222222222", "other-prod", "acct-2", "cloud", "us-east-1", "READY", "", 0, "",
			0, 400, "", nil, "", "", "[]", true, "", nil, "", time.Now(), "meko", "[]",
		))

	envs, err := repo.List("")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(envs) != 2 {
		t.Fatalf("List(unscoped) returned %d environments, want both", len(envs))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A scoped read is an exact hit on the (account_id, id) unique index -- not a fetch
// by id followed by an ownership comparison, which would read the other account's
// row before discarding it.
func TestEnvironmentRepo_GetScopedHitsTheUniqueIndex(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectQuery(`FROM meko_system\.environments WHERE account_id = \$1 AND id = \$2`).
		WithArgs("acct-1", "acme-prod").
		WillReturnRows(listRow("acme-prod"))

	e, err := repo.Get("acct-1", "acme-prod")
	if err != nil || e == nil {
		t.Fatalf("Get = (%v, %v), want the environment", e, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Once two accounts may each hold "prod", an unscoped caller naming it has no single
// answer. Picking one would hand whichever row sorted first to a caller with no right
// to it, so it is refused instead.
func TestEnvironmentRepo_GetUnscopedRefusesAmbiguousID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectQuery(`FROM meko_system\.environments WHERE id = \$1 LIMIT 2`).
		WithArgs("prod").
		WillReturnRows(listRow("prod").AddRow(
			"33333333-3333-3333-3333-333333333333", "prod", "acct-2", "cloud", "us-east-1", "READY", "", 0, "",
			0, 400, "", nil, "", "", "[]", true, "", nil, "", time.Now(), "meko", "[]",
		))

	if _, err := repo.Get("", "prod"); !errors.Is(err, ErrAmbiguousID) {
		t.Fatalf("unscoped Get of a shared id err = %v, want ErrAmbiguousID", err)
	}
}

// One account holding the id is still unambiguous unscoped, which is every row that
// exists before the per-account index -- so the console and the e2e are unaffected.
func TestEnvironmentRepo_GetUnscopedResolvesSingleMatch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectQuery(`FROM meko_system\.environments WHERE id = \$1 LIMIT 2`).
		WithArgs("acme-prod").
		WillReturnRows(listRow("acme-prod"))

	e, err := repo.Get("", "acme-prod")
	if err != nil || e == nil {
		t.Fatalf("Get = (%v, %v), want the environment", e, err)
	}
}

// Registration conflicts on (account_id, id), so a name another account holds is not
// a conflict at all. Insert also has to hand back the key it landed on, since the
// desired-state write is keyed on it.
func TestEnvironmentRepo_InsertConflictsOnAccountAndIDAndReturnsUUID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectQuery(`ON CONFLICT \(account_id, id\) DO UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"uuid"}).AddRow("44444444-4444-4444-4444-444444444444"))

	e := &models.Environment{ID: "acme-prod", AccountID: "acct-2", State: "PENDING"}
	if err := repo.Insert(e); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if e.UUID != "44444444-4444-4444-4444-444444444444" {
		t.Errorf("uuid = %q, want the key the insert returned", e.UUID)
	}
}

// Writing an environment back never touches its id or its account: neither is
// editable, and an UPDATE keyed on the uuid cannot move a row between accounts.
func TestEnvironmentRepo_UpdateKeysOnUUID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectExec(`UPDATE meko_system\.environments SET`).
		WithArgs(
			"44444444-4444-4444-4444-444444444444",
			sqlmock.AnyArg(), sqlmock.AnyArg(), "SUSPENDED", sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = repo.Update(&models.Environment{
		UUID: "44444444-4444-4444-4444-444444444444", ID: "acme-prod", AccountID: "acct-1", State: "SUSPENDED",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
