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
	"id", "account_id", "placement", "region", "state", "k8s_version", "node_count", "provider",
	"datapacks", "max_datapacks", "agent_version", "last_heartbeat_at", "mcp_base_url",
	"cert_fingerprint", "components", "live", "enroll_token_hash", "enroll_token_expires_at",
	"status_token_hash", "created_at", "kind", "resources",
}

// envRow builds a stored-environment row of the given id/kind with harmless
// defaults for everything the test does not care about.
func envRow(id, kind string) *sqlmock.Rows {
	return sqlmock.NewRows(envRowCols).AddRow(
		id, "acme", "cloud", "us-east-1", "PENDING", "", 0, "",
		0, 400, "", nil, "", "", "[]", false, "", nil, "", time.Now(), kind, "[]",
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
	// Bootstrap defaults ON: an amp registration must still seed nothing, proving
	// the kind gate — not a disabled default — is what suppresses the meko-stack.
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), BootstrapDefaults{Enabled: true, Release: testRelease})

	// Only the environment upsert — no desired-state write for amp.
	mock.ExpectExec(`INSERT INTO meko_system\.environments`).WillReturnResult(sqlmock.NewResult(0, 1))

	e, token, err := svc.Register(RegisterEnvironmentRequest{Name: "acme-prod", Kind: "amp"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if e.ID != "amp-acme-prod" {
		t.Errorf("amp environment id = %q, want %q", e.ID, "amp-acme-prod")
	}
	if e.Kind != KindAmp {
		t.Errorf("kind = %q, want %q", e.Kind, KindAmp)
	}
	if token == "" || !strings.HasPrefix(token, "ycp_enr_") {
		t.Errorf("token = %q, want a ycp_enr_ enrollment token", token)
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
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), BootstrapDefaults{}) // disabled: no desired write

	mock.ExpectExec(`INSERT INTO meko_system\.environments`).WillReturnResult(sqlmock.NewResult(0, 1))

	e, _, err := svc.Register(RegisterEnvironmentRequest{Name: "acme-prod"})
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
	svc := NewEnvironmentService(nil, BootstrapDefaults{}) // validation precedes any repo call
	_, _, err := svc.Register(RegisterEnvironmentRequest{Name: "acme-prod", Kind: "app"})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("Register(kind=app) err = %v, want ErrValidation", err)
	}
}

// An agent must not attach to an environment of a different kind: a meko-registered
// environment rejects an amp enrollment (and vice versa) so it cannot start
// reporting the wrong thing.
func TestEnrollWithToken_RejectsKindMismatch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), BootstrapDefaults{})

	const plain = "ycp_enr_deadbeef"
	mock.ExpectQuery(`SELECT id FROM meko_system\.environments WHERE enroll_token_hash = \$1`).
		WithArgs(hashToken(plain)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("acme-prod"))
	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments WHERE id = \$1`).
		WithArgs("acme-prod").
		WillReturnRows(envRow("acme-prod", KindMeko))

	_, _, ok, err := svc.EnrollWithToken(plain, "cloud", KindAmp, ClusterFacts{})
	if ok {
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
// inventory becomes the resources column ($22) the console reads.
func TestApplyStatus_StoresReportedResources(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), BootstrapDefaults{})

	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments WHERE id = \$1`).
		WithArgs("amp-acme-prod").
		WillReturnRows(envRow("amp-acme-prod", KindAmp))

	// $1..$21 are anything; $22 (resources) must carry the reported pool.
	anyArgs := make([]driver.Value, 21)
	for i := range anyArgs {
		anyArgs[i] = sqlmock.AnyArg()
	}
	mock.ExpectExec(`INSERT INTO meko_system\.environments`).
		WithArgs(append(anyArgs, argContains("storage_pool"))...).
		WillReturnResult(sqlmock.NewResult(0, 1))

	ok, err := svc.ApplyStatus("amp-acme-prod", StatusReport{
		EnvironmentState: "READY",
		Resources: []ResourceStatus{
			{Kind: "storage_pool", ID: "pool-1", Name: "default"},
		},
	})
	if err != nil {
		t.Fatalf("ApplyStatus: %v", err)
	}
	if !ok {
		t.Fatal("ApplyStatus returned ok=false for a known environment")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("reported inventory did not reach the resources column: %v", err)
	}
}
