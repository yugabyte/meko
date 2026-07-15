package cloud_test

import (
	"strings"
	"testing"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/cloud"
	awscloud "github.com/yugabyte/yb-amp/test/system/framework/apps/cloud/aws"
	gcpcloud "github.com/yugabyte/yb-amp/test/system/framework/apps/cloud/gcp"
	"github.com/yugabyte/yb-amp/test/system/framework/core"
)

func loadConfig(t *testing.T, overrides ...string) *core.Config {
	t.Helper()
	cfg, err := core.LoadConfig(nil, nil, overrides)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// TestNewAWSResolvesDriver: cloud.New("aws", ...) returns a usable VM whose Meta
// reflects the apps.cloud.aws.* defaults (no explicit creds → ambient chain).
func TestNewAWSResolvesDriver(t *testing.T) {
	cfg := loadConfig(t)
	vm, err := cloud.New("aws", cfg, nil)
	if err != nil {
		t.Fatalf("New(aws): %v", err)
	}
	if vm == nil {
		t.Fatal("New(aws) returned nil VM")
	}
	meta := vm.Meta()
	if meta.Region != awscloud.DefaultRegion {
		t.Errorf("Meta().Region = %q, want %q", meta.Region, awscloud.DefaultRegion)
	}
	if meta.SSHUser != awscloud.DefaultSSHUser {
		t.Errorf("Meta().SSHUser = %q, want %q", meta.SSHUser, awscloud.DefaultSSHUser)
	}
	if meta.KeyFile != awscloud.DefaultKeyFile {
		t.Errorf("Meta().KeyFile = %q, want %q", meta.KeyFile, awscloud.DefaultKeyFile)
	}
}

// TestNewEmptyDefaultsToAWS: the empty cloud name resolves to the AWS driver.
func TestNewEmptyDefaultsToAWS(t *testing.T) {
	if _, err := cloud.New("", loadConfig(t), nil); err != nil {
		t.Fatalf("New(\"\"): %v", err)
	}
}

// TestNewAzureNotImplemented: the not-yet-landed azure driver returns a clear
// not-implemented error, not a nil VM.
func TestNewAzureNotImplemented(t *testing.T) {
	cfg := loadConfig(t)
	vm, err := cloud.New("azure", cfg, nil)
	if err == nil {
		t.Errorf("New(azure) = nil error, want not-implemented")
	}
	if vm != nil {
		t.Errorf("New(azure) returned non-nil VM, want nil")
	}
	if err != nil && !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("New(azure) error = %q, want a not-implemented message", err)
	}
}

// TestNewGCPResolvesDriver: cloud.New("gcp", ...) returns a usable VM whose
// Meta reflects the apps.cloud.gcp.* defaults (no explicit service account →
// ambient gcloud auth chain).
func TestNewGCPResolvesDriver(t *testing.T) {
	cfg := loadConfig(t)
	vm, err := cloud.New("gcp", cfg, nil)
	if err != nil {
		t.Fatalf("New(gcp): %v", err)
	}
	if vm == nil {
		t.Fatal("New(gcp) returned nil VM")
	}
	meta := vm.Meta()
	if meta.Region != gcpcloud.DefaultZone {
		t.Errorf("Meta().Region = %q, want the configured zone %q", meta.Region, gcpcloud.DefaultZone)
	}
	if meta.SSHUser != gcpcloud.DefaultSSHUser {
		t.Errorf("Meta().SSHUser = %q, want %q", meta.SSHUser, gcpcloud.DefaultSSHUser)
	}
	if meta.KeyFile != gcpcloud.DefaultKeyFile {
		t.Errorf("Meta().KeyFile = %q, want %q", meta.KeyFile, gcpcloud.DefaultKeyFile)
	}
}

// TestNewGCPServiceAccountMissingResolverError: cloud.New surfaces the gcp
// driver's error when a service_account_secret is configured but no resolver
// is available — the factory must not silently fall back to ambient auth.
func TestNewGCPServiceAccountMissingResolverError(t *testing.T) {
	cfg := loadConfig(t, "apps.cloud.gcp.service_account_secret=gcp_key")
	if _, err := cloud.New("gcp", cfg, nil); err == nil {
		t.Fatal("New(gcp) with service_account_secret set and nil resolver = nil error, want an error")
	}
}

// TestNewUnknownCloud: an unrecognized cloud name is a clear error.
func TestNewUnknownCloud(t *testing.T) {
	if _, err := cloud.New("digitalocean", loadConfig(t), nil); err == nil {
		t.Fatal("New(digitalocean) = nil error, want unknown-cloud error")
	}
}

// TestNewAWSStaticCredsHalfConfiguredError: cloud.New surfaces the aws driver's
// both-or-neither credential rule (one secret set without the other is an
// error), proving explicit creds are resolved through the factory.
func TestNewAWSStaticCredsHalfConfiguredError(t *testing.T) {
	cfg := loadConfig(t, "apps.cloud.aws.access_key_secret=aws_access")
	if _, err := cloud.New("aws", cfg, func(string) (string, error) { return "x", nil }); err == nil {
		t.Fatal("New(aws) with only access_key_secret set = nil error, want both-or-neither error")
	}
}
