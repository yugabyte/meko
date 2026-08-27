package service_test

import (
	"errors"
	"testing"

	"meko/api_server/internal/service"
)

func TestRegisterEnvironment_RequiresValidName(t *testing.T) {
	svc := service.NewEnvironmentService(nil, service.BootstrapDefaults{}) // validation happens before any repo call
	for _, name := range []string{"", "ab"} {
		_, err := svc.Register(service.RegisterEnvironmentRequest{
			Name: name, APIKeyID: "b0000000-0000-0000-0000-00000000000a",
		}, "")
		if !errors.Is(err, service.ErrValidation) {
			t.Errorf("Register(name=%q) err = %v, want ErrValidation", name, err)
		}
	}
}
