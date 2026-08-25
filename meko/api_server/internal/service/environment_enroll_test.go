package service

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/repository"
)

// Enrollment is the key-authenticated first check-in: it resolves the environment
// by the unique api_key_id, moves it to ATTESTING, and marks it live. Idempotent
// on purpose — the agent repeats it at every startup, so nothing is consumed and
// nothing is minted.
func TestEnrollByKey_ChecksIn(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), BootstrapDefaults{})

	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments WHERE api_key_id = \$1`).
		WithArgs(keyA).
		WillReturnRows(envRow("acme-prod", KindMeko))

	mock.ExpectExec(`UPDATE meko_system\.environments SET`).
		WithArgs(
			envUUID,                            // $1  uuid — the key the update is applied by
			sqlmock.AnyArg(), sqlmock.AnyArg(), // $2 placement, $3 region
			"ATTESTING", // $4  state
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(),
			true,                               // $15 live
			sqlmock.AnyArg(), sqlmock.AnyArg(), // $16 kind, $17 resources
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	e, err := svc.EnrollByKey(keyA, "cloud", KindMeko, ClusterFacts{Provider: "aws"})
	if err != nil {
		t.Fatalf("EnrollByKey: %v", err)
	}
	if e == nil {
		t.Fatal("enroll should have resolved the bound environment")
	}
	if e.MCPBaseURL == "" {
		t.Error("a meko enrollment must synthesize the MCP base URL")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("enroll did not write back the environment as expected: %v", err)
	}
}

// A key bound to no environment resolves to nil with no error — the handler's
// 401. A revoked key and a never-registered one are deliberately the same answer.
func TestEnrollByKey_UnknownKeyIsNil(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), BootstrapDefaults{})

	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments WHERE api_key_id = \$1`).
		WithArgs(keyB).
		WillReturnRows(sqlmock.NewRows(envRowCols))

	e, err := svc.EnrollByKey(keyB, "cloud", KindMeko, ClusterFacts{})
	if err != nil {
		t.Fatalf("EnrollByKey: %v", err)
	}
	if e != nil {
		t.Fatal("an unbound key must not enroll")
	}
}

// ResolveAgent is every non-enroll agent call's lookup: the key id alone names
// the environment.
func TestResolveAgent_ByKeyID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), BootstrapDefaults{})

	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments WHERE api_key_id = \$1`).
		WithArgs(keyA).
		WillReturnRows(envRow("acme-prod", KindMeko))
	e, err := svc.ResolveAgent(keyA)
	if err != nil || e == nil {
		t.Fatalf("ResolveAgent = (%v, %v), want the bound environment", e, err)
	}
	if e.APIKeyID != keyA {
		t.Errorf("resolved binding = %q, want %q", e.APIKeyID, keyA)
	}

	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments WHERE api_key_id = \$1`).
		WithArgs(keyB).
		WillReturnRows(sqlmock.NewRows(envRowCols))
	e, err = svc.ResolveAgent(keyB)
	if err != nil || e != nil {
		t.Fatalf("ResolveAgent(unbound) = (%v, %v), want (nil, nil)", e, err)
	}
}
