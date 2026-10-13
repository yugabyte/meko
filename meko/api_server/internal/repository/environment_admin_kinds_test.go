package repository

import (
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/models"
)

func TestAdminOnlyKindsAndDeclarations(t *testing.T) {
	for _, tc := range []struct {
		kind, name        string
		kindAdminOnly, is bool
	}{
		{KindController, ControllerName, true, true},
		{KindFleetVersion, FleetName, true, true},
		{KindController, "pool-a/ctl", true, false},
		{KindFleetVersion, "controller", true, false},
		{KindController, FleetName, true, false},
		{"storage-pool", "controller", false, false},
		{"extension", "fleet", false, false},
		{"Controller", "controller", false, false},
		{"", "", false, false},
	} {
		if got := IsAdminOnlyKind(tc.kind); got != tc.kindAdminOnly {
			t.Errorf("IsAdminOnlyKind(%q) = %v, want %v", tc.kind, got, tc.kindAdminOnly)
		}
		if got := IsAdminDeclaration(tc.kind, tc.name); got != tc.is {
			t.Errorf("IsAdminDeclaration(%q, %q) = %v, want %v", tc.kind, tc.name, got, tc.is)
		}
	}
}

// A public full replace omits the admin-only rows because a public read never shows
// them, so a prune that removed what was omitted would delete the controller on an
// ordinary read-modify-write. A row of those kinds under another name is not a
// declaration the agent honors, so it is pruned like any other. Only the DELETEs
// registered here may run; the prune walks a map, so their order is not fixed.
func TestReplaceDesired_NeverPrunesTheAdminDeclarations(t *testing.T) {
	existing := func() *sqlmock.Rows {
		return sqlmock.NewRows([]string{"kind", "name"}).
			AddRow(KindController, ControllerName).
			AddRow(KindFleetVersion, FleetName).
			AddRow(KindController, "p/ctl").
			AddRow("storage-pool", "p")
	}
	prune := `DELETE FROM meko_system\.environment_desired_resources\s+WHERE env_uuid=\$1 AND kind=\$2 AND name=\$3`
	t.Run("a set that omits them", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock: %v", err)
		}
		defer db.Close()
		mock.MatchExpectationsInOrder(false)
		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT kind, name FROM meko_system\.environment_desired_resources WHERE env_uuid = \$1`).
			WithArgs(envUUID).WillReturnRows(existing())
		mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
			WithArgs(envUUID, "storage-pool", "p", sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(prune).WithArgs(envUUID, KindController, "p/ctl").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		if err := NewEnvironmentRepo(db).ReplaceDesired(envUUID,
			[]models.DesiredResource{{Kind: "storage-pool", Name: "p"}}); err != nil {
			t.Fatalf("ReplaceDesired: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
	// {"resources": [], "allow_empty": true}: everything else goes, they stay.
	t.Run("an empty set", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock: %v", err)
		}
		defer db.Close()
		mock.MatchExpectationsInOrder(false)
		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT kind, name FROM meko_system\.environment_desired_resources WHERE env_uuid = \$1`).
			WithArgs(envUUID).WillReturnRows(existing())
		mock.ExpectExec(prune).WithArgs(envUUID, "storage-pool", "p").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(prune).WithArgs(envUUID, KindController, "p/ctl").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		if err := NewEnvironmentRepo(db).ReplaceDesired(envUUID, []models.DesiredResource{}); err != nil {
			t.Fatalf("ReplaceDesired: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
}

// Every write path but UpsertAdminDesired refuses the admin-only kinds before any
// statement runs: no expectation is registered, so one that ran would fail here.
func TestDesiredWrites_RefuseAdminOnlyKinds(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	for _, kind := range adminOnlyKinds {
		d := models.DesiredResource{Kind: kind, Name: "x", Spec: []byte(`{"version":"1"}`)}
		if err := repo.ReplaceDesired(envUUID, []models.DesiredResource{
			{Kind: "storage-pool", Name: "p"}, d,
		}); !errors.Is(err, ErrAdminOnlyKind) {
			t.Errorf("ReplaceDesired(%s) err = %v, want ErrAdminOnlyKind", kind, err)
		}
		if _, err := repo.UpsertDesired(envUUID, d); !errors.Is(err, ErrAdminOnlyKind) {
			t.Errorf("UpsertDesired(%s) err = %v, want ErrAdminOnlyKind", kind, err)
		}
		if err := repo.DeleteDesired(envUUID, kind, "x"); !errors.Is(err, ErrAdminOnlyKind) {
			t.Errorf("DeleteDesired(%s) err = %v, want ErrAdminOnlyKind", kind, err)
		}
	}
	for _, kind := range []string{"storage-pool", "cluster", "extension"} {
		_, err := repo.UpsertAdminDesired(envUUID, models.DesiredResource{Kind: kind, Name: "x"})
		if !errors.Is(err, ErrAdminOnlyKind) {
			t.Errorf("UpsertAdminDesired(%s) err = %v, want ErrAdminOnlyKind", kind, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// The admin writer shares UpsertDesired's statement, so the generation rule — kept on
// an identical spec, bumped on a change — is the same one.
func TestUpsertAdminDesired_WritesUnderTheGenerationRule(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(`INSERT INTO meko_system\.environment_desired_resources .* `+
		`WHEN meko_system\.environment_desired_resources\.spec IS DISTINCT FROM EXCLUDED\.spec .* RETURNING generation`).
		WithArgs(envUUID, KindController, ControllerName, []byte(`{"version":"2.1.0"}`)).
		WillReturnRows(sqlmock.NewRows([]string{"generation"}).AddRow(int64(4)))

	d, err := NewEnvironmentRepo(db).UpsertAdminDesired(envUUID, models.DesiredResource{
		Kind: KindController, Name: ControllerName, Spec: []byte(`{"version":"2.1.0"}`),
	})
	if err != nil {
		t.Fatalf("UpsertAdminDesired: %v", err)
	}
	if d.Generation != 4 || string(d.Spec) != `{"version":"2.1.0"}` {
		t.Errorf("stored row = %+v, want the spec at generation 4", d)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A patch naming an admin-only kind fails the batch before any statement, whatever
// its intent — including a patch earlier in the batch that would have been fine.
func TestApplyDesiredPatchesTx_RefusesAdminOnlyKinds(t *testing.T) {
	for name, p := range map[string]models.ResourcePatch{
		"flagless": {Kind: KindController, Name: "pool-a/x", Spec: []byte(`{"version":"1"}`)},
		"create":   {Kind: KindFleetVersion, Name: "pool-a", Spec: []byte(`{"version":"1"}`), Create: true},
		"update":   {Kind: KindController, Name: "pool-a/x", Spec: []byte(`{"version":"1"}`), Update: true},
		"delete":   {Kind: KindFleetVersion, Name: "pool-a", Delete: true},
	} {
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			mock.ExpectBegin()
			mock.ExpectRollback()

			tx, err := db.Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			err = applyDesiredPatchesTx(tx, envUUID, "pool-a", []models.ResourcePatch{
				{Kind: "branch", Name: "pool-a/db/feature", Delete: true}, p,
			}, nil)
			if !errors.Is(err, ErrAdminOnlyKind) {
				t.Fatalf("err = %v, want ErrAdminOnlyKind", err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatalf("rollback: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("a refused batch ran a statement: %v", err)
			}
		})
	}
}

// The cascade matches by name across kinds, so it names the two declarations it must
// spare; a row of their kinds under the pool is not one, and goes with the pool.
func TestApplyDesiredPatchesTx_CascadeSparesTheAdminDeclarations(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM meko_system\.environment_desired_resources\s+`+
		`WHERE env_uuid=\$1 AND \(name=\$2 OR starts_with\(name, \$2 \|\| '/'\)\) `+
		`AND NOT \(\(kind='controller' AND name='controller'\) OR \(kind='fleet-version' AND name='fleet'\)\)$`).
		WithArgs(envUUID, "pool-a").
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := applyDesiredPatchesTx(tx, envUUID, "pool-a", []models.ResourcePatch{
		{Kind: "storage-pool", Name: "pool-a", Delete: true},
	}, nil); err != nil {
		t.Fatalf("applyDesiredPatchesTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestGetDesired(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)
	const query = `SELECT COALESCE\(spec::text, '\{\}'\), generation FROM meko_system\.environment_desired_resources\s+` +
		`WHERE env_uuid=\$1 AND kind=\$2 AND name=\$3`

	mock.ExpectQuery(query).WithArgs(envUUID, KindController, ControllerName).
		WillReturnRows(sqlmock.NewRows([]string{"spec", "generation"}).AddRow(`{"version":"2.1.0"}`, int64(2)))
	d, err := repo.GetDesired(envUUID, KindController, ControllerName)
	if err != nil || d == nil {
		t.Fatalf("GetDesired = (%v, %v), want the row", d, err)
	}
	if d.Kind != KindController || d.Name != ControllerName || d.Generation != 2 ||
		string(d.Spec) != `{"version":"2.1.0"}` {
		t.Errorf("row = %+v", d)
	}

	mock.ExpectQuery(query).WithArgs(envUUID, KindFleetVersion, FleetName).
		WillReturnRows(sqlmock.NewRows([]string{"spec", "generation"}))
	d, err = repo.GetDesired(envUUID, KindFleetVersion, FleetName)
	if err != nil || d != nil {
		t.Fatalf("GetDesired(absent) = (%v, %v), want (nil, nil)", d, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
