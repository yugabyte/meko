package repository

import (
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"meko/api_server/internal/models"
)

// Test accounts and environment key. Named because account ids are uuids: inline
// literals push these expectations past the 120-column limit.
const (
	acctA   = "a0000000-0000-0000-0000-000000000001"
	acctB   = "a0000000-0000-0000-0000-000000000002"
	envUUID = "11111111-1111-1111-1111-111111111111"
	// keyA/keyB are api key ids: keyA is what listRow stores, keyB a replacement.
	keyA = "b0000000-0000-0000-0000-00000000000a"
	keyB = "b0000000-0000-0000-0000-00000000000b"
)

// listRowCols is the column set scanEnvironment reads, in order.
var listRowCols = []string{
	"uuid", "id", "account_id", "placement", "region", "state", "k8s_version", "node_count", "provider",
	"datapacks", "max_datapacks", "agent_version", "last_heartbeat_at", "mcp_base_url",
	"cert_fingerprint", "components", "live", "api_key_id", "created_at", "kind", "resources",
}

// listAccount is the account every row here belongs to; the tests that need a
// second account add its row inline.
const listAccount = acctA

func listRow(id string) *sqlmock.Rows {
	return sqlmock.NewRows(listRowCols).AddRow(
		envUUID, id, listAccount, "cloud", "us-east-1", "READY", "", 0, "",
		0, 400, "", nil, "", "", "[]", true, keyA, time.Now(), "meko", "[]",
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
		WithArgs(acctA).
		WillReturnRows(listRow("acme-prod"))

	envs, err := repo.List(acctA)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(envs) != 1 || envs[0].AccountID != acctA {
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
			"22222222-2222-2222-2222-222222222222", "other-prod", acctB, "cloud", "us-east-1", "READY", "", 0, "",
			0, 400, "", nil, "", "", "[]", true, keyB, time.Now(), "meko", "[]",
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
		WithArgs(acctA, "acme-prod").
		WillReturnRows(listRow("acme-prod"))

	e, err := repo.Get(acctA, "acme-prod")
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
			"33333333-3333-3333-3333-333333333333", "prod", acctB, "cloud", "us-east-1", "READY", "", 0, "",
			0, 400, "", nil, "", "", "[]", true, keyB, time.Now(), "meko", "[]",
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
// desired-state write is keyed on it — and the api_key_id the row held before, so a
// rebind surfaces the key YBM must revoke.
func TestEnvironmentRepo_InsertConflictsOnAccountAndIDAndReturnsUUID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`ON CONFLICT \(account_id, id\) DO UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"uuid", "old_api_key_id"}).
			AddRow("44444444-4444-4444-4444-444444444444", ""))
	mock.ExpectCommit()

	e := &models.Environment{ID: "acme-prod", AccountID: acctB, State: "PENDING", APIKeyID: keyA}
	replaced, err := repo.Insert(e)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if e.UUID != "44444444-4444-4444-4444-444444444444" {
		t.Errorf("uuid = %q, want the key the insert returned", e.UUID)
	}
	if replaced != "" {
		t.Errorf("replaced key = %q, want none on a fresh insert", replaced)
	}
}

// A re-register that rebinds the row to a new key reports the old binding; the
// same key back is not a replacement.
func TestEnvironmentRepo_InsertReportsReplacedKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`ON CONFLICT \(account_id, id\) DO UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"uuid", "old_api_key_id"}).AddRow(envUUID, keyA))
	mock.ExpectCommit()
	replaced, err := repo.Insert(&models.Environment{ID: "acme-prod", AccountID: acctA, State: "PENDING", APIKeyID: keyB})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if replaced != keyA {
		t.Errorf("replaced key = %q, want the previous binding %q", replaced, keyA)
	}

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`ON CONFLICT \(account_id, id\) DO UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"uuid", "old_api_key_id"}).AddRow(envUUID, keyB))
	mock.ExpectCommit()
	replaced, err = repo.Insert(&models.Environment{ID: "acme-prod", AccountID: acctA, State: "PENDING", APIKeyID: keyB})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if replaced != "" {
		t.Errorf("replaced key = %q, want none when the binding is unchanged", replaced)
	}
}

// One key drives one environment: environments_api_key_id_uniq forbids binding a
// key another environment holds, and that has to surface as ErrAPIKeyBound (409)
// rather than a raw driver error behind a 500.
func TestEnvironmentRepo_InsertRejectsKeyBoundElsewhere(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`ON CONFLICT \(account_id, id\) DO UPDATE`).
		WillReturnError(&pq.Error{
			Code:       "23505",
			Constraint: "environments_api_key_id_uniq",
			Message:    `duplicate key value violates unique constraint "environments_api_key_id_uniq"`,
		})

	_, err = repo.Insert(&models.Environment{ID: "acme-prod", AccountID: acctA, State: "PENDING", APIKeyID: keyA})
	if !errors.Is(err, ErrAPIKeyBound) {
		t.Fatalf("Insert err = %v, want ErrAPIKeyBound", err)
	}
}

// A unique violation on any OTHER index is not a key-binding conflict, so it must
// keep surfacing as itself rather than being reported as one.
func TestEnvironmentRepo_InsertOtherUniqueViolationIsNotKeyBound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`ON CONFLICT \(account_id, id\) DO UPDATE`).
		WillReturnError(&pq.Error{Code: "23505", Constraint: "environments_pkey", Message: "duplicate key"})

	_, err = repo.Insert(&models.Environment{ID: "acme-prod", AccountID: acctA, State: "PENDING", APIKeyID: keyA})
	if err == nil || errors.Is(err, ErrAPIKeyBound) {
		t.Fatalf("Insert err = %v, want a plain error rather than ErrAPIKeyBound", err)
	}
}

// Writing an environment back never touches its id, its account, or its key
// binding: the first two are not editable, and the binding changes only through
// registration — 17 bind parameters, none of them api_key_id.
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
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = repo.Update(&models.Environment{
		UUID: "44444444-4444-4444-4444-444444444444", ID: "acme-prod", AccountID: acctA, State: "SUSPENDED",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// An unaccounted environment is stored under the sentinel, not NULL: NULLs compare
// as distinct inside environments_account_id_uniq, so a nullable column would let
// unlimited unaccounted rows share a name. The sentinel is confined to storage —
// callers above the repository see "".
func TestEnvironmentRepo_UnaccountedUsesTheSentinel(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`INSERT INTO meko_system\.environments`).
		WithArgs("acme-prod", "00000000-0000-0000-0000-000000000000", sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"uuid", "old_api_key_id"}).
			AddRow("44444444-4444-4444-4444-444444444444", ""))
	mock.ExpectCommit()

	e := &models.Environment{ID: "acme-prod", AccountID: "", State: "PENDING", APIKeyID: keyA}
	if _, err := repo.Insert(e); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unaccounted insert did not use the sentinel: %v", err)
	}
}
