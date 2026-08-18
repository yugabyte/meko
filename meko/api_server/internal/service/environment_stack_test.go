package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/repository"
)

func desiredRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"kind", "name", "spec", "generation"})
}

// The stack is handed to an agent when it enrolls, which is what makes bringing Meko up
// a single round trip: nothing has to be commanded, and no second call has to succeed.
func TestEnrolledStack_ReturnsTheStackTheEnvironmentWasRegisteredWith(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), BootstrapDefaults{})

	const spec = `{"images":{"database":"registry/yugabyte-db/ybdb-meko-release:t"},"secret_ref":"creds"}`
	mock.ExpectQuery(`SELECT kind, name, .* FROM meko_system\.environment_desired_resources`).
		WithArgs("acme-prod").
		WillReturnRows(desiredRows().AddRow("meko-stack", "meko", spec, int64(0)))

	got, err := svc.EnrolledStack("acme-prod")
	if err != nil {
		t.Fatalf("EnrolledStack: %v", err)
	}
	var decoded mekoStackSpec
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("stack is not valid JSON (%s): %v", got, err)
	}
	if !strings.Contains(decoded.Images.Database, "ybdb-meko-release") {
		t.Errorf("stack images = %+v, want the stored ones", decoded.Images)
	}
	if decoded.SecretRef != "creds" {
		t.Errorf("secret_ref = %q, want the stored one", decoded.SecretRef)
	}
}

// Read from the environment's stored resources, not recomposed from the bootstrap
// defaults: an operator who replaced the stack through POST /desired must get the
// replacement handed to the next agent that enrolls, not the pinned release again.
func TestEnrolledStack_ReflectsAReplacedStack(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), BootstrapDefaults{
		Enabled: true, Release: testRelease,
	})

	mock.ExpectQuery(`SELECT kind, name, .* FROM meko_system\.environment_desired_resources`).
		WithArgs("acme-prod").
		WillReturnRows(desiredRows().AddRow("meko-stack", "meko", `{"images":{"ui":"operators-own-ui:1"}}`, int64(0)))

	got, err := svc.EnrolledStack("acme-prod")
	if err != nil {
		t.Fatalf("EnrolledStack: %v", err)
	}
	if !strings.Contains(string(got), "operators-own-ui:1") {
		t.Errorf("stack = %s, want the operator's replacement", got)
	}
	if strings.Contains(string(got), testRelease.UI) {
		t.Errorf("stack = %s, want the stored spec rather than the pinned release", got)
	}
}

// An environment with no stack hands back nothing rather than an empty object. The
// agent reads a zero-length stack as "run whatever desired state declares", which is
// what --no-bootstrap-defaults means; an empty JSON object would instead be a stack
// with no images in it, and the agent would report six failed components.
func TestEnrolledStack_AbsentIsNil(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), BootstrapDefaults{})

	mock.ExpectQuery(`SELECT kind, name, .* FROM meko_system\.environment_desired_resources`).
		WithArgs("acme-prod").
		WillReturnRows(desiredRows().AddRow("database", "byo", `{"mode":"external","host":"db"}`, int64(0)))

	got, err := svc.EnrolledStack("acme-prod")
	if err != nil {
		t.Fatalf("EnrolledStack: %v", err)
	}
	if got != nil {
		t.Errorf("stack = %s, want nil for an environment with no meko-stack", got)
	}
}
