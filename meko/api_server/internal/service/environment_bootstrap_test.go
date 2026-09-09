package service

import (
	"encoding/json"
	"errors"
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

// Registration declares nothing of its own. This is the behaviour change the
// reconcile model rests on: a data plane's workloads come from its chart, and the
// agent converges only what lives inside them, so a seeded meko-stack had no
// consumer once cmd/agent retired. It is asserted directly rather than inferred
// from a loop over the result, which passes for free once the result is empty.
func TestRegister_SeedsNothing(t *testing.T) {
	svc := &EnvironmentService{bootstrap: BootstrapDefaults{Enabled: true, Release: testRelease}}
	for _, name := range []string{"acme-meko", "acme-amp"} {
		if got := svc.initialDesired(RegisterEnvironmentRequest{Name: name}); got != nil {
			t.Errorf("%s: registration seeded %+v, want nothing", name, got)
		}
	}
}

// What the caller declares is what the environment starts with — now the only way
// it starts with anything. An explicit empty set stays non-nil, because it is a
// declaration ("run nothing until commanded") rather than an absence, and Register
// only stores a non-nil result.
func TestRegister_ExplicitDesiredWins(t *testing.T) {
	svc := &EnvironmentService{bootstrap: BootstrapDefaults{Enabled: true, Release: testRelease}}

	empty := []models.DesiredResource{}
	got := svc.initialDesired(RegisterEnvironmentRequest{Name: acctA, Desired: &empty})
	if got == nil || len(got) != 0 {
		t.Errorf("explicit empty desired state = %+v, want an empty non-nil set", got)
	}

	custom := []models.DesiredResource{
		{Kind: "agent", Name: "ycp"},
		{Kind: "datapack", Name: "ycp/smoke", Spec: json.RawMessage(`{"description":"d"}`)},
	}
	got = svc.initialDesired(RegisterEnvironmentRequest{Name: acctA, Desired: &custom})
	if len(got) != 2 || got[0].Kind != "agent" || got[1].Name != "ycp/smoke" {
		t.Errorf("explicit desired state not used: %+v", got)
	}
}

// A failed desired-state write must not fail the registration: the environment and
// its key binding are already stored, and the desired-state write can simply be
// retried, or replaced by the operator.
func TestRegister_SurvivesDesiredStateWriteFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), repository.NewEnvironmentTaskRepo(db), BootstrapDefaults{Enabled: true})

	expectRegisterInsert(mock, envUUID)
	mock.ExpectBegin().WillReturnError(errTest)

	e, err := svc.Register(RegisterEnvironmentRequest{Name: "acme-prod", Region: "us-east-1", APIKeyID: keyA}, "")
	if err != nil {
		t.Fatalf("Register err = %v, want nil: a desired-state failure must not fail the registration", err)
	}
	if e == nil {
		t.Fatal("Register returned no environment")
	}
}
