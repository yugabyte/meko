package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/repository"
)

// Test accounts and environment key. Named because account ids are uuids: inline
// literals push these expectations past the 120-column limit.
const (
	acctA   = "a0000000-0000-0000-0000-000000000001"
	acctB   = "a0000000-0000-0000-0000-000000000002"
	envUUID = "11111111-1111-1111-1111-111111111111"
	// keyA/keyB are api key ids: keyA is what envRow stores, keyB a replacement.
	keyA = "b0000000-0000-0000-0000-00000000000a"
	keyB = "b0000000-0000-0000-0000-00000000000b"
)

// insertReturns is what the register upsert hands back: the row's key plus the
// api_key_id the row held before the write ("" on a fresh insert).
func insertReturns(uuid, oldKey string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"uuid", "old_api_key_id"}).AddRow(uuid, oldKey)
}

// newEnvSvc builds a service over a mocked DB. envRow (environment_kind_test.go)
// stores its environments under the account acctA, which is the owner every test
// here scopes to.
func newEnvSvc(t *testing.T) (*EnvironmentService, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewEnvironmentService(repository.NewEnvironmentRepo(db), BootstrapDefaults{}), mock, func() { db.Close() }
}

// testEnvID is the environment every test here reads; envRow stores it under acctA.
const testEnvID = "acme-prod"

// expectScopedGet mirrors the scoped lookup: an exact hit on (account_id, id).
// rows is what the database has for that pair -- empty when the account does not
// own it, which is how "not yours" reaches the caller.
func expectScopedGet(mock sqlmock.Sqlmock, accountID string, rows *sqlmock.Rows) {
	mock.ExpectQuery(`FROM meko_system\.environments WHERE account_id = \$1 AND id = \$2`).
		WithArgs(accountID, testEnvID).
		WillReturnRows(rows)
}

func envRows() *sqlmock.Rows { return envRow(testEnvID, KindMeko) }

// Scoping happens in SQL, so another account's environment is not read and then
// rejected -- the query simply matches nothing, and the caller cannot tell that
// from an environment which was never registered.
func TestGet_HidesOtherAccountsEnvironment(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	expectScopedGet(mock, acctA, envRows())
	e, err := svc.Get(testEnvID, acctA)
	if err != nil || e == nil {
		t.Fatalf("Get as the owner = (%v, %v), want the environment", e, err)
	}

	expectScopedGet(mock, acctB, sqlmock.NewRows(envRowCols))
	e, err = svc.Get(testEnvID, acctB)
	if err != nil || e != nil {
		t.Fatalf("Get as another account = (%v, %v), want (nil, nil)", e, err)
	}

	// Unscoped takes the id alone, and stays unambiguous while one account holds it.
	mock.ExpectQuery(`FROM meko_system\.environments WHERE id = \$1 LIMIT 2`).
		WithArgs(testEnvID).
		WillReturnRows(envRows())
	e, err = svc.Get(testEnvID, "")
	if err != nil || e == nil {
		t.Fatalf("unscoped Get = (%v, %v), want the environment", e, err)
	}
}

// Denying the action is not enough -- it must also not write. The update is the step
// that would suspend or deregister another account's environment, so the test
// asserts no exec was issued.
func TestAction_DeniesOtherAccountWithoutWriting(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	expectScopedGet(mock, acctB, sqlmock.NewRows(envRowCols))

	e, err := svc.Action(testEnvID, acctB, "deregister")
	if err != nil {
		t.Fatalf("Action: %v", err)
	}
	if e != nil {
		t.Error("another account's environment must not be actionable")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Action wrote to another account's environment: %v", err)
	}
}

func TestSetDesired_DeniesOtherAccountWithoutWriting(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	expectScopedGet(mock, acctB, sqlmock.NewRows(envRowCols))

	found, err := svc.SetDesired(testEnvID, acctB, nil)
	if err != nil {
		t.Fatalf("SetDesired: %v", err)
	}
	if found {
		t.Error("another account's desired state must not be replaceable")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("SetDesired wrote to another account's environment: %v", err)
	}
}

// The scope comes from the account YBM authenticated; the body is whatever the
// caller typed. When they disagree the scope wins, or an account could register
// environments into another's fleet just by naming it in the payload.
func TestRegister_ScopeOverridesBodyAccount(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`INSERT INTO meko_system\.environments`).
		WillReturnRows(insertReturns(envUUID, ""))
	mock.ExpectCommit()

	e, _, err := svc.Register(RegisterEnvironmentRequest{Name: "acme-prod", AccountID: acctB, APIKeyID: keyA}, acctA)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if e.AccountID != acctA {
		t.Errorf("account = %q, want the scope's acct-1 and not the body's", e.AccountID)
	}
}

// Without a scope the body still sets the account, which is how the fleet console
// has always registered.
func TestRegister_UnscopedKeepsBodyAccount(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`INSERT INTO meko_system\.environments`).
		WillReturnRows(insertReturns(envUUID, ""))
	mock.ExpectCommit()

	e, _, err := svc.Register(RegisterEnvironmentRequest{Name: "acme-prod", AccountID: acctA, APIKeyID: keyA}, "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if e.AccountID != acctA {
		t.Errorf("account = %q, want the body's acct-1", e.AccountID)
	}
}

// A name another account already holds is not a conflict: (account_id, id) is what
// is unique, so the second account registers its own environment and gets its own
// key. The guard this replaced could only refuse that caller.
func TestRegister_NameHeldByAnotherAccountIsItsOwnEnvironment(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`ON CONFLICT \(account_id, id\) DO UPDATE`).
		WillReturnRows(insertReturns("55555555-5555-5555-5555-555555555555", ""))
	mock.ExpectCommit()

	e, replaced, err := svc.Register(RegisterEnvironmentRequest{Name: "acme-prod", APIKeyID: keyA}, acctB)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if e.UUID != "55555555-5555-5555-5555-555555555555" {
		t.Errorf("uuid = %q, want the key the insert returned", e.UUID)
	}
	if e.ID != "acme-prod" || e.AccountID != acctB {
		t.Errorf("registered (%q, %q), want acct-2's own acme-prod", e.AccountID, e.ID)
	}
	if replaced != "" {
		t.Errorf("replaced key = %q, want none on a fresh registration", replaced)
	}
}

// Re-registering a name onto a different key reports the binding it replaced, so
// YBM can revoke the key nothing authenticates with any more. Re-registering onto
// the SAME key reports nothing -- there is nothing to revoke.
func TestRegister_ReportsReplacedAPIKey(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`ON CONFLICT \(account_id, id\) DO UPDATE`).
		WillReturnRows(insertReturns(envUUID, keyA))
	mock.ExpectCommit()
	_, replaced, err := svc.Register(RegisterEnvironmentRequest{Name: "acme-prod", APIKeyID: keyB}, acctA)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if replaced != keyA {
		t.Errorf("replaced key = %q, want the previous binding %q", replaced, keyA)
	}

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`ON CONFLICT \(account_id, id\) DO UPDATE`).
		WillReturnRows(insertReturns(envUUID, keyB))
	mock.ExpectCommit()
	_, replaced, err = svc.Register(RegisterEnvironmentRequest{Name: "acme-prod", APIKeyID: keyB}, acctA)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if replaced != "" {
		t.Errorf("replaced key = %q, want none when the binding is unchanged", replaced)
	}
}

// account_id is a UUID column, so a malformed account has to be refused before the
// insert rather than surfacing as a database error. Empty stays legal — that is an
// unaccounted environment, stored under the sentinel.
func TestRegister_RejectsNonUUIDAccount(t *testing.T) {
	svc, _, done := newEnvSvc(t)
	defer done()

	for _, bad := range []string{"acme-corp", "e2e", "local", "not-a-uuid"} {
		_, _, err := svc.Register(RegisterEnvironmentRequest{Name: "acme-prod", AccountID: bad, APIKeyID: keyA}, "")
		if !errors.Is(err, ErrValidation) {
			t.Errorf("Register(account_id=%q) err = %v, want ErrValidation", bad, err)
		}
	}
}

// The key is stored in its canonical form, not as sent. uuid.Parse accepts
// uppercase (and braces, and urn:uuid:), while the upsert compares the value
// against the stored api_key_id as canonical text — so without normalizing, a
// re-registration of the SAME key in another spelling reads as a rebind and YBM
// revokes the live key.
func TestRegister_NormalizesAPIKeyID(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`ON CONFLICT \(account_id, id\) DO UPDATE`).
		WillReturnRows(insertReturns(envUUID, keyA))
	mock.ExpectCommit()

	e, replaced, err := svc.Register(RegisterEnvironmentRequest{
		Name: "acme-prod", APIKeyID: strings.ToUpper(keyA),
	}, acctA)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if e.APIKeyID != keyA {
		t.Errorf("stored key = %q, want the canonical %q", e.APIKeyID, keyA)
	}
	if replaced != "" {
		t.Errorf("replaced key = %q, want none: the binding is unchanged", replaced)
	}
}

// api_key_id is the environment's only agent credential binding, so registering
// without one (or with a malformed one) is refused before any write.
func TestRegister_RequiresAPIKeyID(t *testing.T) {
	svc := NewEnvironmentService(nil, BootstrapDefaults{}) // validation precedes any repo call
	for _, bad := range []string{"", "not-a-uuid", "ycp_enr_deadbeef"} {
		_, _, err := svc.Register(RegisterEnvironmentRequest{Name: "acme-prod", APIKeyID: bad}, "")
		if !errors.Is(err, ErrValidation) {
			t.Errorf("Register(api_key_id=%q) err = %v, want ErrValidation", bad, err)
		}
	}
}
