package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// errTest stands in for any database failure the desired-state write can hit.
var errTest = errors.New("database unavailable")

// testRelease is a complete release with recognisable refs, so a test can tell which
// component a seeded image belongs to without matching real tags.
var testRelease = DataPlaneRelease{
	Database:         "registry/yugabyte-db/ybdb-meko-release:t",
	APIServer:        "registry/meko/meko-api-server:t",
	MCPServer:        "registry/meko/meko-mcp-server:t",
	RAGWorker:        "registry/meko/rag-agent:t",
	InferenceGateway: "registry/meko/inference-gateway:t",
	UI:               "registry/meko/meko-ui:t",
}

// Registering an environment is a statement of intent: nobody registers a data plane
// in order to run nothing in it. So registration seeds desired state, and an agent
// bootstraps Meko as soon as it enrolls instead of idling until somebody remembers to
// send it a separate command.
func TestRegister_SeedsBootstrapDesiredState(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), BootstrapDefaults{
		Enabled:         true,
		Release:         testRelease,
		ImagePullSecret: "meko-ecr",
		SecretRef:       "meko-data-plane",
	})

	mock.ExpectExec(`INSERT INTO meko_system\.environments`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM meko_system\.environment_desired_resources`).
		WithArgs("acme-prod").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs("acme-prod", "meko-stack", "meko", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	e, token, err := svc.Register(RegisterEnvironmentRequest{Name: "acme-prod", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if e == nil || token == "" {
		t.Fatal("Register returned no environment or no token")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Registering takes a name and nothing else, and what it seeds is the whole stack: the
// caller is not expected to know which six image tags go together, which is the reason
// the release is pinned on the control plane at all.
func TestRegister_SeedsTheWholeStackInOneResource(t *testing.T) {
	svc := &EnvironmentService{bootstrap: BootstrapDefaults{
		Enabled: true, Release: testRelease,
		ImagePullSecret: "meko-ecr", SecretRef: "meko-data-plane",
	}}
	res := svc.initialDesired(RegisterEnvironmentRequest{Name: "acme"}, KindMeko)
	if len(res) != 1 || res[0].Kind != "meko-stack" {
		t.Fatalf("seeded %+v, want a single meko-stack resource", res)
	}

	// The spec has to decode: the agent unmarshals it, and a malformed one would make
	// it report a bad spec for a resource the server invented.
	var spec mekoStackSpec
	if err := json.Unmarshal(res[0].Spec, &spec); err != nil {
		t.Fatalf("seeded spec is not valid JSON (%s): %v", res[0].Spec, err)
	}
	if spec.Images != testRelease {
		t.Errorf("seeded images = %+v, want the configured release %+v", spec.Images, testRelease)
	}
	if spec.ImagePullSecret != "meko-ecr" || spec.SecretRef != "meko-data-plane" {
		t.Errorf("Secret names not carried into the seed: %+v", spec)
	}

	// Secret NAMES only. If a value ever reaches desired state it is persisted in the
	// fleet registry and handed to every agent that asks, which is the one thing this
	// protocol promises not to do.
	if s := string(res[0].Spec); strings.Contains(s, "password") || strings.Contains(s, "sk-") {
		t.Errorf("seeded spec looks like it carries secret material: %s", s)
	}
}

// A datapack is a customer's own artifact, created through the api-server this stack
// runs. Seeding a placeholder would provision one nobody asked for.
func TestRegister_SeedsNoDatapack(t *testing.T) {
	svc := &EnvironmentService{bootstrap: BootstrapDefaults{Enabled: true, Release: testRelease}}
	for _, r := range svc.initialDesired(RegisterEnvironmentRequest{Name: "acme"}, KindMeko) {
		if r.Kind == "datapack" {
			t.Errorf("registration seeded a datapack (%s), which nobody asked for", r.Name)
		}
	}
}

// The caller can override the seed, including with an explicit empty set — which is
// how you register an environment that must run nothing until commanded.
func TestRegister_ExplicitDesiredWins(t *testing.T) {
	svc := &EnvironmentService{bootstrap: BootstrapDefaults{Enabled: true, Release: testRelease}}

	empty := []models.DesiredResource{}
	got := svc.initialDesired(RegisterEnvironmentRequest{Name: "acme", Desired: &empty}, KindMeko)
	if len(got) != 0 {
		t.Errorf("explicit empty desired state was overridden with %+v", got)
	}

	custom := []models.DesiredResource{
		{Kind: "database", Name: "byo", Spec: json.RawMessage(`{"mode":"external","host":"db"}`)},
	}
	got = svc.initialDesired(RegisterEnvironmentRequest{Name: "acme", Desired: &custom}, KindMeko)
	if len(got) != 1 || got[0].Name != "byo" {
		t.Errorf("explicit desired state not used: %+v", got)
	}
}

// --no-bootstrap-defaults turns the whole thing off, for a control plane whose
// operators want to declare every environment by hand.
func TestRegister_BootstrapDisabled(t *testing.T) {
	svc := &EnvironmentService{bootstrap: BootstrapDefaults{Enabled: false, Release: testRelease}}
	if got := svc.initialDesired(RegisterEnvironmentRequest{Name: "acme"}, KindMeko); got != nil {
		t.Errorf("seeded %+v with bootstrap defaults disabled", got)
	}
}

// A failed desired-state write must not fail the registration. The enrollment token
// is minted and returned exactly once, so discarding it here would leave an
// environment no agent can ever attach to — while the desired-state write can simply
// be retried, or replaced by the operator.
func TestRegister_SurvivesDesiredStateWriteFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), BootstrapDefaults{Enabled: true})

	mock.ExpectExec(`INSERT INTO meko_system\.environments`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectBegin().WillReturnError(errTest)

	e, token, err := svc.Register(RegisterEnvironmentRequest{Name: "acme-prod", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("Register err = %v, want nil: a desired-state failure must not lose the token", err)
	}
	if e == nil || token == "" {
		t.Fatal("Register returned no environment or no token")
	}
}
