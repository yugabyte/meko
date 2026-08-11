package config

import (
	"strings"
	"testing"
	"time"
)

// A valid --ycp config resolves, trims a trailing slash off the URL, and defaults
// the placement and report interval.
func TestLoad_YCP_Valid(t *testing.T) {
	c, err := Load([]string{
		"--database-url", "postgres://localhost/db",
		"--ycp",
		"--ycp-url", "https://devcloud.yugabyte.com/ycp/",
		"--ycp-env-name", "acme",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.YCP.Enabled {
		t.Fatal("ycp not enabled")
	}
	if c.YCP.URL != "https://devcloud.yugabyte.com/ycp" {
		t.Errorf("ycp url = %q, want the trailing slash trimmed", c.YCP.URL)
	}
	if c.YCP.EnvName != "acme" {
		t.Errorf("ycp env name = %q", c.YCP.EnvName)
	}
	if c.YCP.Placement != "cloud" {
		t.Errorf("placement default = %q, want cloud", c.YCP.Placement)
	}
	if c.YCP.ReportInterval != 30*time.Second {
		t.Errorf("report interval default = %s, want 30s", c.YCP.ReportInterval)
	}
}

// --ycp with no URL or no env name is a configuration error, not a silent no-op.
func TestLoad_YCP_RequiresURLAndEnvName(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no url", []string{"--database-url", "postgres://x/db", "--ycp", "--ycp-env-name", "acme"}, "url"},
		{"no env name", []string{"--database-url", "postgres://x/db", "--ycp", "--ycp-url", "https://x"}, "env name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

// Disabled (the default), none of the ycp fields are required.
func TestLoad_YCP_DisabledNeedsNothing(t *testing.T) {
	c, err := Load([]string{"--database-url", "postgres://localhost/db"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.YCP.Enabled {
		t.Fatal("ycp should be off by default")
	}
}
