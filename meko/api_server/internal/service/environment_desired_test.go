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

// Registration declares nothing of its own. This is the behaviour change the
// reconcile model rests on: a data plane's workloads come from its chart, and the
// agent converges only what lives inside them, so a seeded meko-stack had no
// consumer once cmd/agent retired. It is asserted directly rather than inferred
// from a loop over the result, which passes for free once the result is empty.
func TestRegister_SeedsNothing(t *testing.T) {
	svc := &EnvironmentService{}
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
	svc := &EnvironmentService{}

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
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), repository.NewEnvironmentTaskRepo(db))

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

// A kind the environment's agent cannot converge is refused where the operator is
// standing, instead of being stored, shipped, and reported back as permanent drift a
// tick later. The two kinds are checked against each other's environments, because
// the mistake worth catching is a valid kind on the wrong kind of environment as much
// as a typo.
func TestSetDesired_RefusesAKindTheAgentDoesNotServe(t *testing.T) {
	cases := []struct {
		envKind string
		res     []models.DesiredResource
		wantErr bool
	}{
		{KindMeko, []models.DesiredResource{{Kind: "agent", Name: "a"}}, false},
		{KindMeko, []models.DesiredResource{{Kind: "datapack", Name: "a/b"}}, false},
		{KindMeko, []models.DesiredResource{{Kind: "datapak", Name: "a/b"}}, true},
		{KindMeko, []models.DesiredResource{{Kind: "meko-stack", Name: "s"}}, true},
		{KindMeko, []models.DesiredResource{{Kind: "storage-pool", Name: "p"}}, true},
		{KindAmp, []models.DesiredResource{{Kind: "storage-pool", Name: "p"}}, false},
		{KindAmp, []models.DesiredResource{{Kind: "datapack", Name: "a/b"}}, true},
		{KindAmp, []models.DesiredResource{{Kind: "controller", Name: "controller"}}, false},
		{KindMeko, []models.DesiredResource{{Kind: "controller", Name: "controller"}}, true},
		{KindAmp, []models.DesiredResource{{Kind: "fleet-version", Name: "fleet"}}, false},
		{KindMeko, []models.DesiredResource{{Kind: "fleet-version", Name: "fleet"}}, true},
		{KindAmp, []models.DesiredResource{{Kind: "extension", Name: "postgis"}}, false},
		{KindMeko, []models.DesiredResource{{Kind: "extension", Name: "postgis"}}, true},
		// An empty set is a declaration ("run nothing"), never a bad one.
		{KindMeko, []models.DesiredResource{}, false},
	}
	for _, c := range cases {
		err := checkKinds(c.envKind, c.res)
		if (err != nil) != c.wantErr {
			t.Errorf("checkKinds(%s, %+v) err = %v, wantErr = %v", c.envKind, c.res, err, c.wantErr)
			continue
		}
		// A 400 is only useful if it says which entry and what was expected.
		if c.wantErr && !errors.Is(err, ErrValidation) {
			t.Errorf("checkKinds(%s, %+v) = %v, want it to wrap ErrValidation so the handler answers 400",
				c.envKind, c.res, err)
		}
	}
}

// An environment kind with no list yet must not be blocked from declaring anything:
// the backend is the authority, and this check is an early filter, not a gate.
func TestSetDesired_UnknownEnvironmentKindIsNotGated(t *testing.T) {
	if err := checkKinds("something-new", []models.DesiredResource{{Kind: "whatever"}}); err != nil {
		t.Errorf("checkKinds on an unlisted environment kind = %v, want nil", err)
	}
}
