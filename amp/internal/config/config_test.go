package config

import (
	"testing"
	"time"
)

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv("AMP_DATABASE_URL", "")
	if _, err := Load(nil); err == nil {
		t.Fatal("expected error when database URL is missing")
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load([]string{"--database-url", "postgres://localhost/db"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", c.ListenAddr)
	}
	if c.DB.Schema != "amp" {
		t.Errorf("DB.Schema = %q, want amp", c.DB.Schema)
	}
	if c.Log.Format != "json" {
		t.Errorf("Log.Format = %q, want json", c.Log.Format)
	}
	if c.Task.PollInterval != 2*time.Second {
		t.Errorf("Task.PollInterval = %v, want 2s", c.Task.PollInterval)
	}
}

func TestLoadFlagOverridesEnv(t *testing.T) {
	t.Setenv("AMP_DATABASE_URL", "postgres://env/db")
	t.Setenv("AMP_LISTEN", ":9000")
	c, err := Load([]string{"--listen", ":7000"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DB.URL != "postgres://env/db" {
		t.Errorf("DB.URL = %q, want env value", c.DB.URL)
	}
	if c.ListenAddr != ":7000" {
		t.Errorf("ListenAddr = %q, want flag to win (:7000)", c.ListenAddr)
	}
}

func TestLoadRejectsBadSchema(t *testing.T) {
	for _, bad := range []string{"a-b", "amp;drop", "1abc", "has space", ""} {
		if _, err := Load([]string{"--database-url", "postgres://localhost/db", "--db-schema", bad}); err == nil {
			t.Errorf("schema %q: expected error, got nil", bad)
		}
	}
}

func TestStorageControllerDisabledByDefault(t *testing.T) {
	c, err := Load([]string{"--database-url", "postgres://localhost/db"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.StorageController.Enabled {
		t.Error("storage controller should be disabled by default")
	}
}

func TestStorageControllerDefaultsDatabaseToUniverse(t *testing.T) {
	c, err := Load([]string{
		"--database-url", "postgres://localhost/db",
		"--storage-controller",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sc := c.StorageController
	if !sc.Enabled {
		t.Fatal("storage controller should be enabled")
	}
	if sc.DatabaseURL != "postgres://localhost/db" {
		t.Errorf("DatabaseURL = %q, want it to default to the universe URL", sc.DatabaseURL)
	}
	if sc.BinPath != "storage_controller" {
		t.Errorf("BinPath = %q, want default storage_controller", sc.BinPath)
	}
	if sc.ListenAddr != "127.0.0.1:50051" {
		t.Errorf("ListenAddr = %q, want default 127.0.0.1:50051", sc.ListenAddr)
	}
}

func TestStorageControllerKeepsExplicitDatabaseURL(t *testing.T) {
	c, err := Load([]string{
		"--database-url", "postgres://localhost/db",
		"--storage-controller",
		"--storage-controller-database-url", "postgres://localhost/storctl",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.StorageController.DatabaseURL != "postgres://localhost/storctl" {
		t.Errorf("DatabaseURL = %q, want the explicit value", c.StorageController.DatabaseURL)
	}
}

func TestStorageControllerRejectsBadListen(t *testing.T) {
	_, err := Load([]string{
		"--database-url", "postgres://localhost/db",
		"--storage-controller",
		"--storage-controller-listen", "not-a-host-port",
	})
	if err == nil {
		t.Fatal("expected error for invalid listen address")
	}
}

func TestStorageControllerDisabledSkipsValidation(t *testing.T) {
	// A bad listen address is ignored while the service is disabled.
	if _, err := Load([]string{
		"--database-url", "postgres://localhost/db",
		"--storage-controller-listen", "not-a-host-port",
	}); err != nil {
		t.Fatalf("Load should not validate a disabled storage controller: %v", err)
	}
}
