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
	"uuid", "id", "account_id", "placement", "region", "state", "k8s_version", "node_count", "provider",
	"datapacks", "max_datapacks", "agent_version", "last_heartbeat_at", "mcp_base_url",
	"cert_fingerprint", "components", "live", "api_key_id", "created_at", "kind", "resources",
	"agent_base_url", "type", "accepts_new_pools",
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
	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments e WHERE e\.uuid = \$1`).
		WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows(envRowColsK).AddRow(
			envUUID, "amp-acme", acctA, "cloud", "us-east-1", "READY", "", 0, "",
			0, 400, "", nil, "", "", "[]", true, keyA, time.Now(), "amp", inventory, nil, "BYOC", false,
		))

	e, err := repo.GetByUUID(envUUID)
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

// Insert writes the kind ($18), the resources inventory ($19), the agent base
// URL ($20) — so a later GET renders the hint the registration returned — and the
// type ($21).
func TestEnvironmentRepo_Insert_PersistsKindAndResources(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewEnvironmentRepo(db)

	anyArgs := make([]driver.Value, 17) // $1..$17
	for i := range anyArgs {
		anyArgs[i] = sqlmock.AnyArg()
	}
	// $18 kind, $19 resources, $20 agent_base_url, $21 type
	args := append(anyArgs, "amp", argContainsK("storage_pool"), "https://cloud.yugabyte.com/api/public/v1", "BYOC")
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO meko_system\.environments`).
		WithArgs(args...).
		WillReturnRows(sqlmock.NewRows([]string{"uuid"}).AddRow(envUUID))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_api_keys`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = repo.Insert(&models.Environment{
		ID: "amp-acme", Kind: "amp", Placement: "cloud", State: "READY", APIKeyID: keyA,
		Resources:    json.RawMessage(`[{"kind":"storage_pool","id":"pool-1"}]`),
		AgentBaseURL: "https://cloud.yugabyte.com/api/public/v1",
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("kind/resources/agent_base_url not persisted as expected: %v", err)
	}
}
