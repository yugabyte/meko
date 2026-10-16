package service

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/models"
)

func TestRefuseByocOnlyFields(t *testing.T) {
	for name, tc := range map[string]struct {
		kind, spec string
		refused    bool
	}{
		"tolerations":           {"endpoint", `{"tolerations":[{"operator":"Exists"}]}`, true},
		"spread":                {"endpoint", `{"topology_spread_constraints":[{"maxSkew":1}]}`, true},
		"empty list":            {"endpoint", `{"tolerations":[]}`, true},
		"null reads undeclared": {"endpoint", `{"tolerations":null}`, false},
		"neither":               {"endpoint", `{"role":"read_write"}`, false},
		"no spec":               {"endpoint", ``, false},
		"cluster writer":        {"cluster", `{"with_writer":true,"tolerations":[{"operator":"Exists"}]}`, true},
		"cluster neither":       {"cluster", `{"with_writer":true}`, false},
		"other kind":            {"branch", `{"tolerations":[]}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			err := refuseByocOnlyFields(tc.kind, json.RawMessage(tc.spec))
			if got := errors.Is(err, ErrValidation); got != tc.refused {
				t.Errorf("err = %v, want refused=%v", err, tc.refused)
			}
		})
	}
}

// SetDesired refuses the fields on a MANAGED environment before writing anything.
func TestSetDesired_RefusesByocOnlyFieldsOnManaged(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	managed := sqlmock.NewRows(envRowCols).AddRow(
		envUUID, testEnvID, acctA, "cloud", "us-east-1", "READY", "", 0, "",
		0, 400, "", nil, "", "", "[]", false, keyA, time.Now(), KindAmp, "[]", nil, TypeManaged, false,
	)
	expectScopedGet(mock, acctA, managed)

	_, err := svc.SetDesired(testEnvID, acctA, []models.DesiredResource{{
		Kind: "endpoint", Name: "pool/c/main/rw",
		Spec: json.RawMessage(`{"role":"read_write","tolerations":[{"operator":"Exists"}]}`),
	}})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a refused write still reached the database: %v", err)
	}
}

// A MANAGED registration is refused before the insert, since Register swallows a
// failed desired-state write.
func TestRegister_RefusesByocOnlyFieldsOnManaged(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	res := []models.DesiredResource{{
		Kind: "cluster", Name: "pool/c",
		Spec: json.RawMessage(`{"with_writer":true,"topology_spread_constraints":[{"maxSkew":1}]}`),
	}}
	_, err := svc.Register(RegisterEnvironmentRequest{
		Name: "acme-prod", Kind: KindAmp, Type: TypeManaged, APIKeyID: keyA, Desired: &res,
	}, "")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a refused registration reached the database: %v", err)
	}
}
