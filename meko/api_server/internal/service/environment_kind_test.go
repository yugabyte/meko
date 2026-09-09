package service

import (
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/repository"
)

// envRowCols is the column set scanEnvironment reads, in order. A row built with
// it stands in for a stored environment in the enroll/status tests.
var envRowCols = []string{
	"uuid", "id", "account_id", "placement", "region", "state", "k8s_version", "node_count", "provider",
	"datapacks", "max_datapacks", "agent_version", "last_heartbeat_at", "mcp_base_url",
	"cert_fingerprint", "components", "live", "api_key_id", "created_at", "kind", "resources",
	"agent_base_url",
}

// envRow builds a stored-environment row of the given id/kind with harmless
// defaults for everything the test does not care about.
func envRow(id, kind string) *sqlmock.Rows {
	return sqlmock.NewRows(envRowCols).AddRow(
		envUUID, id, acctA, "cloud", "us-east-1", "PENDING", "", 0, "",
		0, 400, "", nil, "", "", "[]", false, keyA, time.Now(), kind, "[]", nil,
	)
}

// The kind is baked into the id so an amp and a meko environment of the same name
// can coexist, and an amp environment seeds no meko-stack — it is a running
// controller reporting what it owns, not a data plane to bring up.
func TestRegister_AmpKindQualifiesIDAndSeedsNothing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	// Registration seeds nothing for either kind, so the assertion below is that no
	// desired-state write happens at all — not that a kind-specific default was
	// suppressed. There is no default left to suppress.
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), repository.NewEnvironmentTaskRepo(db))

	// Only the environment insert — no desired-state write for amp.
	expectRegisterInsert(mock, envUUID)

	e, err := svc.Register(RegisterEnvironmentRequest{Name: "acme-prod", Kind: "amp", APIKeyID: keyA}, "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if e.ID != "amp-acme-prod" {
		t.Errorf("amp environment id = %q, want %q", e.ID, "amp-acme-prod")
	}
	if e.Kind != KindAmp {
		t.Errorf("kind = %q, want %q", e.Kind, KindAmp)
	}
	if e.APIKeyID != keyA {
		t.Errorf("api key id = %q, want the registered binding %q", e.APIKeyID, keyA)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations (an amp registration must not write desired state): %v", err)
	}
}

// An omitted kind is meko, so the existing register flow is unchanged — including
// its id, which stays a bare slug (no prefix) for backward compatibility.
func TestRegister_DefaultsToMekoKind(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), repository.NewEnvironmentTaskRepo(db)) // registration writes no desired state

	expectRegisterInsert(mock, envUUID)

	e, err := svc.Register(RegisterEnvironmentRequest{Name: "acme-prod", APIKeyID: keyA}, "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if e.ID != "acme-prod" || e.Kind != KindMeko {
		t.Errorf("register = (id %q, kind %q), want (acme-prod, meko)", e.ID, e.Kind)
	}
}

// An unknown kind is a 400, not a silent default: registering "amp" as "app" must
// fail rather than quietly create a meko environment.
func TestRegister_RejectsUnknownKind(t *testing.T) {
	svc := NewEnvironmentService(nil, nil) // validation precedes any repo call
	_, err := svc.Register(RegisterEnvironmentRequest{Name: "acme-prod", Kind: "app", APIKeyID: keyA}, "")
	if !errors.Is(err, ErrValidation) {
		t.Errorf("Register(kind=app) err = %v, want ErrValidation", err)
	}
}

// An agent must not attach to an environment of a different kind: a meko-registered
// environment rejects an amp enrollment (and vice versa) so it cannot start
// reporting the wrong thing.
func TestEnrollByKey_RejectsKindMismatch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), repository.NewEnvironmentTaskRepo(db))

	mock.ExpectQuery(`FROM meko_system\.environment_api_keys WHERE api_key_id = \$1`).
		WithArgs(keyA).
		WillReturnRows(envRow("acme-prod", KindMeko))

	e, err := svc.EnrollByKey(keyA, "cloud", KindAmp, ClusterFacts{})
	if e != nil {
		t.Fatal("an amp agent must not enroll into a meko environment")
	}
	if !errors.Is(err, ErrKindMismatch) {
		t.Errorf("err = %v, want ErrKindMismatch", err)
	}
}

// argContains matches a bind arg (string or []byte) that contains substr. Used to
// assert the reported inventory reaches the resources column.
type argContains string

func (a argContains) Match(v driver.Value) bool {
	switch b := v.(type) {
	case []byte:
		return strings.Contains(string(b), string(a))
	case string:
		return strings.Contains(b, string(a))
	default:
		return false
	}
}

// A status report's resources are stored on the environment: the amp controller's
// inventory becomes the resources column ($17) the console reads.
func TestApplyStatus_StoresReportedResources(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), repository.NewEnvironmentTaskRepo(db))

	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments e WHERE e\.uuid = \$1`).
		WithArgs(envUUID).
		WillReturnRows(envRow("amp-acme-prod", KindAmp))

	// $1..$16 are anything; $17 (resources) must carry the reported pool.
	anyArgs := make([]driver.Value, 16)
	for i := range anyArgs {
		anyArgs[i] = sqlmock.AnyArg()
	}
	mock.ExpectExec(`UPDATE meko_system\.environments SET`).
		WithArgs(append(anyArgs, argContains("storage_pool"))...).
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := svc.ApplyStatus(envUUID, StatusReport{
		EnvironmentState: "READY",
		Resources: []ResourceStatus{
			{Kind: "storage_pool", ID: "pool-1", Name: "default"},
		},
	})
	if err != nil {
		t.Fatalf("ApplyStatus: %v", err)
	}
	if !res.Found {
		t.Fatal("ApplyStatus returned Found=false for a known environment")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("reported inventory did not reach the resources column: %v", err)
	}
}
