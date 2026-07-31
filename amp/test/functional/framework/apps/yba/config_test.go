package yba

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fakeConfig is a minimal ConfigGetter backed by a dotted-key map (values are
// string / int / []map[string]any). A missing key returns the caller's default.
type fakeConfig struct{ m map[string]any }

func (c fakeConfig) GetString(key, def string) string {
	if v, ok := c.m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

func (c fakeConfig) GetInt(key string, def int) int {
	if v, ok := c.m[key]; ok {
		if n, ok := v.(int); ok {
			return n
		}
	}
	return def
}

func (c fakeConfig) GetBool(key string, def bool) bool {
	if v, ok := c.m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

func (c fakeConfig) GetMap(key string, def map[string]any) map[string]any {
	if v, ok := c.m[key]; ok {
		if m, ok := v.(map[string]any); ok {
			return m
		}
	}
	return def
}

func (c fakeConfig) GetMapSlice(key string, def []map[string]any) []map[string]any {
	if v, ok := c.m[key]; ok {
		if s, ok := v.([]map[string]any); ok {
			return s
		}
	}
	return def
}

// envResolver mimics core.Secrets.Value over env-var secrets: an unset var is an
// error (as a genuinely-needed secret would be at point of use).
func envResolver(name string) (string, error) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("secret %q: env var unset", name)
	}
	return v, nil
}

// TestBuildAWSProviderSpecResolvesCredsAndKeypair: creds resolve by NAME from
// env-var secrets, the keypair private-key CONTENTS are read off disk, and the
// regions/zones come from config.
func TestBuildAWSProviderSpecResolvesCredsAndKeypair(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "yb-dev-aws-2.pem")
	if err := os.WriteFile(keyPath, []byte("PRIVATE-KEY-CONTENT"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	t.Setenv("AWS_ACCESS_KEY", "AKIATEST")
	t.Setenv("AWS_SECRET_KEY", "secretvalue")

	cfg := fakeConfig{m: map[string]any{
		"apps.yba.aws.access_key_secret": "aws_access_key",
		"apps.yba.aws.secret_key_secret": "aws_secret_key",
		"apps.yba.aws.keypair_name":      "yb-dev-aws-2",
		"apps.yba.aws.keypair_file":      keyPath,
		"apps.yba.aws.regions": []map[string]any{{
			"code":              "us-west-2",
			"security_group_id": "sg-1",
			"vpc_id":            "vpc-1",
			"zones":             []any{map[string]any{"name": "us-west-2a", "subnet": "subnet-1"}},
		}},
	}}
	// The resolver maps the secret NAMES declared in config to env-var names.
	resolve := func(name string) (string, error) {
		switch name {
		case "aws_access_key":
			return envResolver("AWS_ACCESS_KEY")
		case "aws_secret_key":
			return envResolver("AWS_SECRET_KEY")
		}
		return "", fmt.Errorf("unknown secret %q", name)
	}

	spec, err := BuildAWSProviderSpec(cfg, resolve)
	if err != nil {
		t.Fatalf("build spec: %v", err)
	}
	if spec.AccessKey != "AKIATEST" || spec.SecretKey != "secretvalue" {
		t.Errorf("creds = %q/%q", spec.AccessKey, spec.SecretKey)
	}
	if spec.KeyPairName != "yb-dev-aws-2" || spec.KeyPairContent != "PRIVATE-KEY-CONTENT" {
		t.Errorf("keypair = %q / %q", spec.KeyPairName, spec.KeyPairContent)
	}
	if len(spec.Regions) != 1 || spec.Regions[0].Code != "us-west-2" {
		t.Fatalf("regions = %+v", spec.Regions)
	}
	z := spec.Regions[0].Zones
	if len(z) != 1 || z[0].Subnet != "subnet-1" || z[0].Name != "us-west-2a" {
		t.Errorf("zones = %+v", z)
	}
}

// TestBuildAWSProviderSpecMissingSecretErrors: a needed cred secret that fails
// to resolve is a hard error (YBA stores creds — no ambient fallback).
func TestBuildAWSProviderSpecMissingSecretErrors(t *testing.T) {
	cfg := fakeConfig{m: map[string]any{}}
	resolve := func(name string) (string, error) { return envResolver(name) } // all unset
	if _, err := BuildAWSProviderSpec(cfg, resolve); err == nil {
		t.Fatalf("expected error when access-key secret unresolved")
	}
}

// TestBuildUniverseSpecDefaults: an empty config yields the ampqa defaults, and
// the version falls back to versions.yugabytedb.
func TestBuildUniverseSpecDefaults(t *testing.T) {
	cfg := fakeConfig{m: map[string]any{"versions.yugabytedb": "2025.2.1.0"}}
	s := BuildUniverseSpec(cfg)
	if s.NamePrefix != DefaultUniverseNamePrefix {
		t.Errorf("NamePrefix = %q", s.NamePrefix)
	}
	if s.NumNodes != DefaultNumNodes || s.ReplicationFactor != DefaultReplicationFactor {
		t.Errorf("nodes/RF = %d/%d", s.NumNodes, s.ReplicationFactor)
	}
	if s.InstanceType != DefaultInstanceType {
		t.Errorf("InstanceType = %q, want %q", s.InstanceType, DefaultInstanceType)
	}
	if s.Region != DefaultUniverseRegion {
		t.Errorf("Region = %q", s.Region)
	}
	if s.VolumeSizeGB != DefaultVolumeSizeGB || s.VolumeCount != DefaultVolumeCount {
		t.Errorf("volume = %d/%d", s.VolumeSizeGB, s.VolumeCount)
	}
	if s.YBSoftwareVersion != "2025.2.1.0" {
		t.Errorf("version = %q, want fallback to versions.yugabytedb", s.YBSoftwareVersion)
	}
}

// TestBuildUniverseSpecExplicitVersionWins: apps.yba.universe.yb_version beats
// the versions.yugabytedb fallback.
func TestBuildUniverseSpecExplicitVersionWins(t *testing.T) {
	cfg := fakeConfig{m: map[string]any{
		"apps.yba.universe.yb_version": "2.25.1.0-b9",
		"versions.yugabytedb":          "2025.2.1.0",
	}}
	if got := BuildUniverseSpec(cfg).YBSoftwareVersion; got != "2.25.1.0-b9" {
		t.Errorf("version = %q, want explicit yb_version", got)
	}
}
