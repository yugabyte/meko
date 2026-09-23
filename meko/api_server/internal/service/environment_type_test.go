package service

import (
	"errors"
	"testing"
)

// Register defaults type to BYOC, accepts MANAGED (case-insensitively), and rejects
// anything else with ErrValidation before touching the database — the same shape as
// the kind gate.
func TestRegister_TypeDefaultsAcceptsAndRejects(t *testing.T) {
	t.Run("default is BYOC", func(t *testing.T) {
		svc, mock, done := newEnvSvc(t)
		defer done()
		expectRegisterInsert(mock, envUUID)

		e, err := svc.Register(RegisterEnvironmentRequest{Name: "acme-prod", APIKeyID: keyA}, "")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		if e.Type != TypeBYOC {
			t.Errorf("type = %q, want %q", e.Type, TypeBYOC)
		}
	})

	t.Run("MANAGED accepted case-insensitively", func(t *testing.T) {
		svc, mock, done := newEnvSvc(t)
		defer done()
		expectRegisterInsert(mock, envUUID)

		e, err := svc.Register(RegisterEnvironmentRequest{Name: "acme-prod", APIKeyID: keyA, Type: "managed"}, "")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		if e.Type != TypeManaged {
			t.Errorf("type = %q, want %q", e.Type, TypeManaged)
		}
	})

	t.Run("unknown rejected before any write", func(t *testing.T) {
		svc, mock, done := newEnvSvc(t)
		defer done()
		// No expectations: the value is refused before the insert transaction opens.

		_, err := svc.Register(RegisterEnvironmentRequest{Name: "acme-prod", APIKeyID: keyA, Type: "hybrid"}, "")
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("Register err = %v, want ErrValidation", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("an unknown type still reached the database: %v", err)
		}
	})
}
