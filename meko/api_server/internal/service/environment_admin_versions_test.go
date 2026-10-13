package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// adminOnlySets are declarations a public path must refuse. The identical one is a
// byte-for-byte copy of what the admin route stores: a 400 that depended on whether
// the request changed anything would make the answer depend on what is stored.
var adminOnlySets = map[string][]models.DesiredResource{
	"controller": {{Kind: repository.KindController, Name: repository.ControllerName,
		Spec: json.RawMessage(`{"version":"2.1.0"}`)}},
	"fleet-version": {{Kind: repository.KindFleetVersion, Name: repository.FleetName,
		Spec: json.RawMessage(`{"version":"2.1.0"}`)}},
	"identical to the stored row": {{Kind: repository.KindController, Name: repository.ControllerName,
		Spec: json.RawMessage(`{"version":"2.1.0"}`), Generation: 3}},
	"beside a storage pool": {
		{Kind: "storage-pool", Name: "p"},
		{Kind: repository.KindFleetVersion, Name: repository.FleetName, Spec: json.RawMessage(`{"version":"2.1.0"}`)},
	},
}

func wantAdminOnlyRefusal(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation so the handler answers 400", err)
	}
	if !strings.Contains(err.Error(), "admin API") {
		t.Errorf("err = %v, want it to say the kind is set through the admin API", err)
	}
}

// Refused before the lookup, so the answer is the same 400 for an environment that
// does not exist, one another account owns, and one that would take the write. No
// expectation is registered: a lookup fails the test.
func TestSetDesired_RefusesAdminOnlyKindsBeforeAnyLookup(t *testing.T) {
	for name, res := range adminOnlySets {
		for _, scope := range []AccountScope{acctA, acctB, ""} {
			t.Run(name+"/"+string(scope), func(t *testing.T) {
				svc, mock, done := newEnvSvc(t)
				defer done()
				_, err := svc.SetDesired("no-such-env", scope, res)
				wantAdminOnlyRefusal(t, err)
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Error(err)
				}
			})
		}
	}
}

// Register swallows a failed desired-state write, so a refusal left to the repository
// would register the environment and silently drop its declaration. It comes first,
// before even the MANAGED 403.
func TestRegister_RefusesAdminOnlyKindsBeforeInsert(t *testing.T) {
	for name, res := range adminOnlySets {
		t.Run(name, func(t *testing.T) {
			svc, mock, done := newEnvSvc(t)
			defer done()
			_, err := svc.Register(RegisterEnvironmentRequest{
				Name: "acme-prod", Kind: KindAmp, Type: TypeManaged, APIKeyID: keyA, Desired: &res,
			}, acctA)
			wantAdminOnlyRefusal(t, err)
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("a refused registration reached the database: %v", err)
			}
		})
	}
}

// Every intent is refused, a delete included: no public path removes one either.
func TestSetResourcesByStoragePool_RefusesAdminOnlyKinds(t *testing.T) {
	for name, p := range map[string]models.ResourcePatch{
		"flagless": {Kind: repository.KindController, Name: poolUUID + "/x", Spec: []byte(`{"version":"1"}`)},
		"create":   {Kind: repository.KindFleetVersion, Name: poolUUID, Spec: []byte(`{"version":"1"}`), Create: true},
		"update":   {Kind: repository.KindController, Name: poolUUID, Spec: []byte(`{"version":"1"}`), Update: true},
		"delete":   {Kind: repository.KindFleetVersion, Name: poolUUID, Delete: true},
	} {
		t.Run(name, func(t *testing.T) {
			svc, mock, done := newEnvSvc(t)
			defer done()
			_, err := svc.SetResourcesByStoragePool(poolUUID, []models.RegionResources{{
				Region:    "us-east-1",
				Resources: []models.ResourcePatch{{Kind: "cluster", Name: poolUUID + "/c"}, p},
			}})
			wantAdminOnlyRefusal(t, err)
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("a refused patch reached the database: %v", err)
			}
		})
	}
}

// The admin-only kinds stay in resourceKinds for the admin routes, but a public caller
// told to use one would only be refused again.
func TestCheckKinds_DoesNotAdvertiseAdminOnlyKinds(t *testing.T) {
	err := checkKinds(KindAmp, []models.DesiredResource{{Kind: "clustr", Name: "p/c"}})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "cluster") {
		t.Errorf("err = %v, want it to list the kinds a public caller may use", err)
	}
	for _, kind := range []string{repository.KindController, repository.KindFleetVersion} {
		if strings.Contains(err.Error(), kind) {
			t.Errorf("err = %v, advertises admin-only kind %q", err, kind)
		}
	}
}

// What the stored spec must look like for a re-declaration to keep its generation:
// the same bytes for the same request, and for a bare version the {"version":"X"} the
// public POST stored before this route existed.
func TestAdminVersionSpecsEncodeDeterministically(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec any
		want string
	}{
		{"bare controller version", ControllerVersion{Version: "2.1.0"}, `{"version":"2.1.0"}`},
		{"empty values", ControllerVersion{Version: "2.1.0", Values: map[string]json.RawMessage{}}, `{"version":"2.1.0"}`},
		{"values in key order", ControllerVersion{
			Version: "2.1.0", ChartVersion: "2.1.0-b3",
			Values: map[string]json.RawMessage{"b.x": json.RawMessage(`1`), "a.y": json.RawMessage(`{"k":true}`)},
		}, `{"version":"2.1.0","chart_version":"2.1.0-b3","values":{"a.y":{"k":true},"b.x":1}}`},
		{"fleet version", FleetVersion{Version: "2.1.0"}, `{"version":"2.1.0"}`},
	} {
		raw, err := json.Marshal(tc.spec)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if string(raw) != tc.want {
			t.Errorf("%s encodes as %s, want %s", tc.name, raw, tc.want)
		}
	}
}

// Bad bodies are refused before the environment is looked up.
func TestSetControllerVersion_ValidatesBeforeAnyLookup(t *testing.T) {
	for name, v := range map[string]ControllerVersion{
		"no version":           {},
		"blank version":        {Version: "  "},
		"padded version":       {Version: " 2.1.0"},
		"padded chart_version": {Version: "2.1.0", ChartVersion: "2.1.0 "},
	} {
		t.Run(name, func(t *testing.T) {
			svc, mock, done := newEnvSvc(t)
			defer done()
			if _, _, err := svc.SetControllerVersion(testEnvID, "", "", v); !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Error(err)
			}
		})
	}
	svc, mock, done := newEnvSvc(t)
	defer done()
	_, _, err := svc.SetFleetVersion(testEnvID, "", "", FleetVersion{Version: "2.1.0\n"})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("fleet err = %v, want ErrValidation", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
