package service_test

import (
	"errors"
	"testing"

	"meko/api_server/internal/service"
)

func TestRegisterEnvironment_RequiresValidName(t *testing.T) {
	svc := service.NewEnvironmentService(nil, service.BootstrapDefaults{}) // validation happens before any repo call
	for _, name := range []string{"", "ab"} {
		_, _, err := svc.Register(service.RegisterEnvironmentRequest{Name: name})
		if !errors.Is(err, service.ErrValidation) {
			t.Errorf("Register(name=%q) err = %v, want ErrValidation", name, err)
		}
	}
}

func TestEnrollWithToken_RequiresToken(t *testing.T) {
	svc := service.NewEnvironmentService(nil, service.BootstrapDefaults{})
	_, _, ok, err := svc.EnrollWithToken("", "cloud", service.ClusterFacts{})
	if ok {
		t.Fatal("empty token should not enroll")
	}
	if !errors.Is(err, service.ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}
