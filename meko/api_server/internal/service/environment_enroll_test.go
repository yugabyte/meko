package service

import (
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/repository"
)

// A successful enroll must leave the enrollment token intact.
//
// Burning it looks right — it is a bootstrap credential and the exchange it exists
// for has happened — but EnrollLookup requires a non-empty hash, so clearing it
// makes the agent's re-enroll-on-401 path permanently dead instead of TTL-bounded.
// An agent whose status token is rotated, or which failed to persist its state, is
// then locked out with no recovery short of re-registering the environment (which
// resets its state and datapack counts). The TTL is the intended bound.
//
// This test exists to stop the burn being reintroduced without a re-issue path.
func TestEnrollWithToken_PreservesTheEnrollmentToken(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), BootstrapDefaults{})

	const plain = "ycp_enr_deadbeef"
	tokenHash := hashToken(plain)
	expires := time.Now().Add(20 * time.Minute).UTC()

	mock.ExpectQuery(`SELECT id FROM meko_system\.environments WHERE enroll_token_hash = \$1`).
		WithArgs(tokenHash).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("acme-prod"))

	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments WHERE id = \$1`).
		WithArgs("acme-prod").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "account_id", "placement", "region", "state", "k8s_version", "node_count", "provider",
			"datapacks", "max_datapacks", "agent_version", "last_heartbeat_at", "mcp_base_url",
			"cert_fingerprint", "components", "live", "enroll_token_hash", "enroll_token_expires_at",
			"status_token_hash", "created_at", "kind", "resources",
		}).AddRow(
			"acme-prod", "acme", "cloud", "us-east-2", "PENDING", "", 0, "",
			// created_at is a non-pointer time.Time, so it cannot be NULL here.
			0, 400, "", nil, "", "", "[]", false, tokenHash, expires, "", time.Now(), "meko", "[]",
		))

	// $17 is enroll_token_hash and $18 its expiry. The regex below pins that mapping
	// rather than assuming it: it matches the ON CONFLICT assignments, so reordering
	// envColumnsInsert breaks this expectation instead of silently shifting which
	// column the positional assertions below are checking.
	mock.ExpectExec(`enroll_token_hash=\$17, enroll_token_expires_at=\$18`).
		WithArgs(
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "ATTESTING",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), true,
			tokenHash,                          // $17 — still there
			expires,                            // $18 — still there
			sqlmock.AnyArg(), sqlmock.AnyArg(), // $19 status_token_hash, $20 created_at
			sqlmock.AnyArg(), sqlmock.AnyArg(), // $21 kind, $22 resources
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	_, statusToken, ok, err := svc.EnrollWithToken(plain, "cloud", KindMeko, ClusterFacts{Provider: "aws"})
	if err != nil {
		t.Fatalf("EnrollWithToken: %v", err)
	}
	if !ok {
		t.Fatal("enroll should have succeeded")
	}
	if statusToken == "" {
		t.Fatal("no status token returned — the agent has nothing to persist")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("enroll did not write back the environment as expected: %v", err)
	}
}
