package api

import (
	"database/sql"
	"testing"

	_ "github.com/lib/pq"
)

const registryTestConnStr = "postgresql://localhost:5433/yugabyte?user=yugabyte&password=yugabyte&sslmode=disable"

func openRegistryTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", registryTestConnStr)
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("failed to ping test database: %v", err)
	}
	return db
}

func TestBuildServiceRegistry_AllFieldsPopulated(t *testing.T) {
	db := openRegistryTestDB(t)
	defer db.Close()

	cfg := ServerConfig{DB: db}
	reg := buildServiceRegistry(cfg)

	if reg.accountRepo == nil {
		t.Error("accountRepo is nil")
	}
	if reg.ybaRepo == nil {
		t.Error("ybaRepo is nil")
	}
	if reg.ybaSvc == nil {
		t.Error("ybaSvc is nil")
	}
	if reg.dpRepo == nil {
		t.Error("dpRepo is nil")
	}
	if reg.dpSvc == nil {
		t.Error("dpSvc is nil")
	}
	if reg.langfuseSvc == nil {
		t.Error("langfuseSvc is nil")
	}
	if reg.tierRepo == nil {
		t.Error("tierRepo is nil")
	}
	if reg.agentSvc == nil {
		t.Error("agentSvc is nil")
	}
	if reg.kbSvc == nil {
		t.Error("kbSvc is nil")
	}
}

func TestBuildServiceRegistry_LangfuseDisabledWithoutConfig(t *testing.T) {
	db := openRegistryTestDB(t)
	defer db.Close()

	cfg := ServerConfig{DB: db}
	reg := buildServiceRegistry(cfg)

	if reg.langfuseSvc == nil {
		t.Fatal("langfuseSvc is nil")
	}
	if reg.langfuseSvc.Enabled() {
		t.Error("expected langfuse to be disabled without host/admin key")
	}
}
