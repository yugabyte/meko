package repository

import (
	"database/sql/driver"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/models"
)

// envRowColsK is the column set Get/List scan, in order, including the kind and
// resources columns added for amp environments.
var envRowColsK = []string{
	"id", "account_id", "placement", "region", "state", "k8s_version", "node_count", "provider",
	"datapacks", "max_datapacks", "agent_version", "last_heartbeat_at", "mcp_base_url",
	"cert_fingerprint", "components", "live", "enroll_token_hash", "enroll_token_expires_at",
	"status_token_hash", "created_at", "kind", "resources",
}

// Get reads the kind and the reported inventory back off the row.
func TestEnvironmentRepo_Get_ScansKindAndResources(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	inventory := `[{"kind":"storage_pool","id":"pool-1","name":"default"}]`
	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments WHERE id = \$1`).
		WithArgs("amp-acme").
		WillReturnRows(sqlmock.NewRows(envRowColsK).AddRow(
			"amp-acme", "acme", "cloud", "us-east-1", "READY", "", 0, "",
			0, 400, "", nil, "", "", "[]", true, "", nil, "", time.Now(), "amp", inventory,
		))

	e, err := repo.Get("amp-acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if e.Kind != "amp" {
		t.Errorf("kind = %q, want amp", e.Kind)
	}
	if !strings.Contains(string(e.Resources), "storage_pool") {
		t.Errorf("resources = %s, want the reported inventory", e.Resources)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

type argContainsK string

func (a argContainsK) Match(v driver.Value) bool {
	switch b := v.(type) {
	case []byte:
		return strings.Contains(string(b), string(a))
	case string:
		return strings.Contains(b, string(a))
	default:
		return false
	}
}

// Upsert writes the kind ($21) and the resources inventory ($22).
func TestEnvironmentRepo_Upsert_PersistsKindAndResources(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	anyArgs := make([]driver.Value, 20) // $1..$20
	for i := range anyArgs {
		anyArgs[i] = sqlmock.AnyArg()
	}
	args := append(anyArgs, "amp", argContainsK("storage_pool")) // $21 kind, $22 resources
	mock.ExpectExec(`INSERT INTO meko_system\.environments`).
		WithArgs(args...).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = repo.Upsert(&models.Environment{
		ID: "amp-acme", Kind: "amp", Placement: "cloud", State: "READY",
		Resources: json.RawMessage(`[{"kind":"storage_pool","id":"pool-1"}]`),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("kind/resources not persisted as expected: %v", err)
	}
}
