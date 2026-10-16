package service

import (
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/models"
)

// poolUUID is a valid storage-pool name (an aeon account id) for the cases that must
// pass the PATCH UUID gate and exercise a later validation branch.
const poolUUID = "aaaaaaaa-1111-2222-3333-444444444444"

func TestStoragePoolFromName(t *testing.T) {
	cases := map[string]string{
		"acct-1":                   "acct-1",
		"acct-1/main-db":           "acct-1",
		"acct-1/main-db/feature-x": "acct-1",
		"  acct-1/main-db  ":       "acct-1",
	}
	for in, want := range cases {
		if got := storagePoolFromName(in); got != want {
			t.Errorf("storagePoolFromName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnderStoragePool(t *testing.T) {
	if !underStoragePool("acct-1", "acct-1") {
		t.Error("the pool itself must be under the pool")
	}
	if !underStoragePool("acct-1", "acct-1/main-db") {
		t.Error("a child must be under the pool")
	}
	if underStoragePool("acct-1", "acct-2/main-db") {
		t.Error("another pool's resource must not be under this pool")
	}
	// A shared prefix that is not a path boundary must not match.
	if underStoragePool("acct-1", "acct-12/main-db") {
		t.Error("a prefix that is not a path segment must not match")
	}
}

// deriveOwner is deterministic: a pool name parses to itself when it is a UUID, and to
// a stable UUIDv5 otherwise.
func TestDeriveOwner(t *testing.T) {
	const asUUID = "22222222-2222-2222-2222-222222222222"
	if got := deriveOwner(asUUID).String(); got != asUUID {
		t.Errorf("deriveOwner(uuid) = %q, want the uuid itself", got)
	}
	a := deriveOwner("acct-1")
	b := deriveOwner("acct-1")
	c := deriveOwner("acct-2")
	if a != b {
		t.Error("deriveOwner must be deterministic for the same name")
	}
	if a == c {
		t.Error("deriveOwner must differ for different names")
	}
}

// createOnlyDefault marks a flagless non-delete patch as a create, and leaves a delete,
// an explicit create, or an explicit update untouched — without mutating the caller's
// slices.
func TestCreateOnlyDefault(t *testing.T) {
	in := []models.RegionResources{{
		Region: "us-east-1",
		Resources: []models.ResourcePatch{
			{Kind: "cluster", Name: "p/flagless"},
			{Kind: "cluster", Name: "p/del", Delete: true},
			{Kind: "cluster", Name: "p/upd", Update: true},
			{Kind: "cluster", Name: "p/cre", Create: true},
		},
	}}
	out := createOnlyDefault(in)

	got := out[0].Resources
	if !got[0].Create {
		t.Error("a flagless write must default to create")
	}
	if got[1].Create {
		t.Error("a delete must not be turned into a create")
	}
	if got[2].Create || !got[2].Update {
		t.Error("an explicit update must be left as update")
	}
	if !got[3].Create {
		t.Error("an explicit create must stay a create")
	}
	// The caller's backing array is untouched.
	if in[0].Resources[0].Create {
		t.Error("createOnlyDefault mutated the caller's slice")
	}
}

// The whole call is validated before any query: an empty or non-UUID storage pool, a
// name outside the pool subtree, and a kind a MANAGED amp env does not serve are all
// rejected with ErrValidation and never reach the database. The non-pool cases use a
// UUID pool so they exercise their own branch rather than short-circuiting on the UUID
// check.
func TestSetResourcesByStoragePool_ValidationBeforeAnyWrite(t *testing.T) {
	cases := []struct {
		name    string
		pool    string
		regions []models.RegionResources
	}{
		{
			name:    "empty storage pool",
			pool:    "",
			regions: []models.RegionResources{{Region: "us-east-1", Resources: []models.ResourcePatch{{Kind: "cluster", Name: poolUUID + "/c"}}}},
		},
		{
			name:    "non-uuid storage pool",
			pool:    "acct-1",
			regions: []models.RegionResources{{Region: "us-east-1", Resources: []models.ResourcePatch{{Kind: "cluster", Name: "acct-1/c", Spec: []byte(`{}`)}}}},
		},
		{
			name:    "name outside the pool subtree",
			pool:    poolUUID,
			regions: []models.RegionResources{{Region: "us-east-1", Resources: []models.ResourcePatch{{Kind: "cluster", Name: "other-pool/c", Spec: []byte(`{}`)}}}},
		},
		{
			name:    "kind an amp env does not serve",
			pool:    poolUUID,
			regions: []models.RegionResources{{Region: "us-east-1", Resources: []models.ResourcePatch{{Kind: "datapack", Name: poolUUID + "/d", Spec: []byte(`{}`)}}}},
		},
		{
			name: "endpoint tolerations on MANAGED",
			pool: poolUUID,
			regions: []models.RegionResources{{Region: "us-east-1", Resources: []models.ResourcePatch{{
				Kind: "endpoint", Name: poolUUID + "/c/main/rw",
				Spec: []byte(`{"role":"read_write","tolerations":[{"operator":"Exists"}]}`),
			}}}},
		},
		{
			name: "endpoint spread constraints on MANAGED, even empty",
			pool: poolUUID,
			regions: []models.RegionResources{{Region: "us-east-1", Resources: []models.ResourcePatch{{
				Kind: "endpoint", Name: poolUUID + "/c/main/rw",
				Spec: []byte(`{"role":"read_write","topology_spread_constraints":[]}`), Update: true,
			}}}},
		},
		{
			name: "cluster writer tolerations on MANAGED",
			pool: poolUUID,
			regions: []models.RegionResources{{Region: "us-east-1", Resources: []models.ResourcePatch{{
				Kind: "cluster", Name: poolUUID + "/c",
				Spec: []byte(`{"with_writer":true,"tolerations":[{"operator":"Exists"}]}`),
			}}}},
		},
		{
			name:    "non-object spec",
			pool:    poolUUID,
			regions: []models.RegionResources{{Region: "us-east-1", Resources: []models.ResourcePatch{{Kind: "cluster", Name: poolUUID + "/c", Spec: []byte(`[1,2]`)}}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, mock, done := newEnvSvc(t)
			defer done()

			_, err := svc.SetResourcesByStoragePool(tc.pool, tc.regions)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("a rejected call still reached the database: %v", err)
			}
		})
	}
}

// ListResourcesByName derives the storage pool from the name's first segment and, when
// nothing is mapped to it, returns an empty slice without error.
func TestListResourcesByName_UnmappedPoolIsEmpty(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	mock.ExpectQuery(`FROM meko_system\.environment_aeon_account_mapping`).
		WithArgs("acct-1").
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "region"}))

	got, err := svc.ListResourcesByName("acct-1/main-db", "", false)
	if err != nil {
		t.Fatalf("ListResourcesByName: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want empty", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// An environment that was mapped but deleted between the mapping read and the read-back
// is skipped rather than failing the whole response — GetByUUID returns no row.
func TestListResourcesByName_SkipsRacedDelete(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	mock.ExpectQuery(`FROM meko_system\.environment_aeon_account_mapping`).
		WithArgs("acct-1").
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "region"}).
			AddRow("11111111-1111-1111-1111-111111111111", "us-east-1"))
	// The env is gone: GetByUUID returns no row, so resourcesForAssignments skips it.
	mock.ExpectQuery(`WHERE e\.uuid = \$1`).
		WithArgs("11111111-1111-1111-1111-111111111111").
		WillReturnRows(sqlmock.NewRows([]string{"uuid"}))

	got, err := svc.ListResourcesByName("acct-1/main-db", "", false)
	if err != nil {
		t.Fatalf("ListResourcesByName: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want the raced-delete env skipped", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
