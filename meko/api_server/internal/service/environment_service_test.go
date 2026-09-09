package service_test

import (
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

func TestRegisterEnvironment_RequiresValidName(t *testing.T) {
	svc := service.NewEnvironmentService(nil, nil) // validation happens before any repo call
	for _, name := range []string{"", "ab"} {
		_, err := svc.Register(service.RegisterEnvironmentRequest{
			Name: name, APIKeyID: "b0000000-0000-0000-0000-00000000000a",
		}, "")
		if !errors.Is(err, service.ErrValidation) {
			t.Errorf("Register(name=%q) err = %v, want ErrValidation", name, err)
		}
	}
}

// The registration's agent base URL is stored with the environment, so the install
// hint a later read renders points the agent at the same place the register
// response did.
func TestRegisterEnvironment_KeepsTheAgentBaseURL(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := service.NewEnvironmentService(repository.NewEnvironmentRepo(db), repository.NewEnvironmentTaskRepo(db))

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO meko_system\.environments`).
		WillReturnRows(sqlmock.NewRows([]string{"uuid"}).AddRow("11111111-1111-1111-1111-111111111111"))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_api_keys`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	e, err := svc.Register(service.RegisterEnvironmentRequest{
		Name: "acme-prod", APIKeyID: "b0000000-0000-0000-0000-00000000000a",
		AgentBaseURL: "  https://cloud.yugabyte.com/api/public/v1  ",
	}, "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if e.AgentBaseURL != "https://cloud.yugabyte.com/api/public/v1" {
		t.Errorf("agent base url = %q, want the registered one, trimmed", e.AgentBaseURL)
	}
}
