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
