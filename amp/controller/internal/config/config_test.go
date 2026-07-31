package config

import (
	"path/filepath"
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
	if c.DB.Schema != "controller" {
		t.Errorf("DB.Schema = %q, want controller", c.DB.Schema)
	}
	if c.Log.Format != "json" {
		t.Errorf("Log.Format = %q, want json", c.Log.Format)
	}
	if c.Task.PollInterval != 2*time.Second {
		t.Errorf("Task.PollInterval = %v, want 2s", c.Task.PollInterval)
	}
	if c.Task.MaxConcurrent != 50 {
		t.Errorf("Task.MaxConcurrent = %d, want 50", c.Task.MaxConcurrent)
	}
	if c.Task.ShutdownGrace != 10*time.Second {
		t.Errorf("Task.ShutdownGrace = %v, want 10s", c.Task.ShutdownGrace)
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
	for _, bad := range []string{"a-b", "controller;drop", "1abc", "has space", ""} {
		if _, err := Load([]string{"--database-url", "postgres://localhost/db", "--db-schema", bad}); err == nil {
			t.Errorf("schema %q: expected error, got nil", bad)
		}
	}
}

func TestDeployDisabledByDefault(t *testing.T) {
	c, err := Load([]string{"--database-url", "postgres://localhost/db"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := c.Deploy
	if d.Enabled {
		t.Error("deploy provisioner should be disabled by default")
	}
	// Defaults are resolved regardless of Enabled; validation is the no-op.
	if d.HelmBin != "helm" {
		t.Errorf("HelmBin = %q, want default helm", d.HelmBin)
	}
	if d.EndpointImageTag != "latest" {
		t.Errorf("EndpointImageTag = %q, want default latest", d.EndpointImageTag)
	}
	if d.BackingYBUser != "yugabyte" {
		t.Errorf("BackingYBUser = %q, want default yugabyte", d.BackingYBUser)
	}
	if d.BackingYBDBName != "yb_storage" {
		t.Errorf("BackingYBDBName = %q, want default yb_storage", d.BackingYBDBName)
	}
	if d.ComputePort != 55433 {
		t.Errorf("ComputePort = %d, want default 55433", d.ComputePort)
	}
	if d.Timeout != 10*time.Minute {
		t.Errorf("Timeout = %v, want default 10m", d.Timeout)
	}
}

func TestDeployDisabledSkipsValidation(t *testing.T) {
	// Missing required deploy fields are ignored while the provisioner is off.
	if _, err := Load([]string{"--database-url", "postgres://localhost/db"}); err != nil {
		t.Fatalf("Load should not validate a disabled deploy provisioner: %v", err)
	}
}

func TestDeployEnabledWithAllRequired(t *testing.T) {
	chart := t.TempDir()
	c, err := Load([]string{
		"--database-url", "postgres://localhost/db",
		"--deploy",
		"--deploy-driver", "kubernetes",
		"--deploy-namespace", "endpoints",
		"--deploy-chart", chart,
		"--endpoint-image-repo", "registry.example.com/endpoint",
		"--deploy-yb-host-ports", "yb-tserver:5433",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := c.Deploy
	if !d.Enabled {
		t.Fatal("deploy should be enabled")
	}
	if d.Namespace != "endpoints" {
		t.Errorf("Namespace = %q, want endpoints", d.Namespace)
	}
	if d.EndpointImageRepo != "registry.example.com/endpoint" {
		t.Errorf("EndpointImageRepo = %q", d.EndpointImageRepo)
	}
	if d.BackingYBHostPorts != "yb-tserver:5433" {
		t.Errorf("BackingYBHostPorts = %q", d.BackingYBHostPorts)
	}
	// ChartPath must be resolved to an absolute path.
	if !filepath.IsAbs(d.ChartPath) {
		t.Errorf("ChartPath = %q, want absolute", d.ChartPath)
	}
	wantAbs, err := filepath.Abs(chart)
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}
	if d.ChartPath != wantAbs {
		t.Errorf("ChartPath = %q, want %q", d.ChartPath, wantAbs)
	}
}

func TestDeployDockerDriver(t *testing.T) {
	c, err := Load([]string{
		"--database-url", "postgres://localhost/db",
		"--deploy",
		"--deploy-driver", "docker",
		"--deploy-endpoint-image", "yb-amp/neon-endpoint:dev",
		"--deploy-network", "yb-amp",
		"--deploy-yb-host-ports", "host.docker.internal:5433",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := c.Deploy
	if d.Driver != DriverDocker {
		t.Errorf("Driver = %q, want docker", d.Driver)
	}
	if d.EndpointImage != "yb-amp/neon-endpoint:dev" {
		t.Errorf("EndpointImage = %q", d.EndpointImage)
	}
	if d.Network != "yb-amp" {
		t.Errorf("Network = %q", d.Network)
	}
	// Kubernetes-only fields (a chart dir, image repo) are not required for docker.
}

func TestDeployDefaultsToDocker(t *testing.T) {
	// With --deploy but no --deploy-driver, the default driver is docker, so the
	// Kubernetes-only fields (chart, image repo) are not required.
	c, err := Load([]string{
		"--database-url", "postgres://localhost/db",
		"--deploy",
		"--deploy-yb-host-ports", "host.docker.internal:5433",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Deploy.Driver != DriverDocker {
		t.Errorf("Driver = %q, want default docker", c.Deploy.Driver)
	}
}

func TestDeployDockerRequiresImage(t *testing.T) {
	_, err := Load([]string{
		"--database-url", "postgres://localhost/db",
		"--deploy",
		"--deploy-driver", "docker",
		"--deploy-endpoint-image", "", // override the default to empty
		"--deploy-yb-host-ports", "host.docker.internal:5433",
	})
	if err == nil {
		t.Fatal("expected error when --deploy-driver=docker and the endpoint image is empty")
	}
}

func TestDeployRejectsUnknownDriver(t *testing.T) {
	_, err := Load([]string{
		"--database-url", "postgres://localhost/db",
		"--deploy",
		"--deploy-driver", "podman",
		"--deploy-yb-host-ports", "yb:5433",
	})
	if err == nil {
		t.Fatal("expected error for an unknown --deploy-driver")
	}
}

func TestDeployServiceType(t *testing.T) {
	chart := t.TempDir()
	args := func(svcType string) []string {
		return []string{
			"--database-url", "postgres://localhost/db",
			"--deploy",
			"--deploy-driver", "kubernetes",
			"--deploy-namespace", "endpoints",
			"--deploy-chart", chart,
			"--endpoint-image-repo", "registry.example.com/endpoint",
			"--endpoint-image-tag", "latest",
			"--deploy-yb-host-ports", "yb-tserver:5433",
			"--deploy-service-type", svcType,
		}
	}
	for _, st := range []string{"ClusterIP", "NodePort", "LoadBalancer"} {
		c, err := Load(args(st))
		if err != nil {
			t.Errorf("service type %q: unexpected error: %v", st, err)
			continue
		}
		if c.Deploy.ServiceType != st {
			t.Errorf("service type %q: ServiceType = %q", st, c.Deploy.ServiceType)
		}
	}
	// Empty string defaults to ClusterIP.
	if c, err := Load(args("")); err != nil || c.Deploy.ServiceType != "ClusterIP" {
		t.Errorf("empty service type: ServiceType=%v err=%v, want ClusterIP/nil", c, err)
	}
	// An unknown type is rejected up front.
	if _, err := Load(args("NodePortt")); err == nil {
		t.Error("expected error for an invalid --deploy-service-type")
	}
}

func TestDeployStorageClass(t *testing.T) {
	chart := t.TempDir()
	base := func() []string {
		return []string{
			"--database-url", "postgres://localhost/db",
			"--deploy",
			"--deploy-driver", "kubernetes",
			"--deploy-namespace", "endpoints",
			"--deploy-chart", chart,
			"--endpoint-image-repo", "registry.example.com/endpoint",
			"--deploy-yb-host-ports", "yb-tserver:5433",
		}
	}

	// No storage class: empty (emptyDir fallback), but a disk size default is set.
	c, err := Load(base())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Deploy.StorageClass != "" {
		t.Errorf("StorageClass = %q, want empty by default", c.Deploy.StorageClass)
	}
	if c.Deploy.DiskSize != "10Gi" {
		t.Errorf("DiskSize = %q, want default 10Gi", c.Deploy.DiskSize)
	}

	// Storage class set: carried through, with the size default.
	c, err = Load(append(base(), "--deploy-storage-class", "yb-standard"))
	if err != nil {
		t.Fatalf("Load(storage class): %v", err)
	}
	if c.Deploy.StorageClass != "yb-standard" {
		t.Errorf("StorageClass = %q, want yb-standard", c.Deploy.StorageClass)
	}

	// Storage class set with an empty disk size is rejected.
	if _, err := Load(append(base(), "--deploy-storage-class", "yb-standard", "--deploy-disk-size", "")); err == nil {
		t.Error("expected error when --deploy-storage-class is set and --deploy-disk-size is empty")
	}
}

func TestDeployEnabledMissingRequired(t *testing.T) {
	chart := t.TempDir()
	// base: a fully-valid set of args; each subtest removes/breaks one field.
	base := func() map[string]string {
		return map[string]string{
			"--database-url":         "postgres://localhost/db",
			"--deploy-driver":        "kubernetes",
			"--deploy-namespace":     "endpoints",
			"--deploy-chart":         chart,
			"--endpoint-image-repo":  "registry.example.com/endpoint",
			"--endpoint-image-tag":   "latest",
			"--deploy-yb-host-ports": "yb-tserver:5433",
			"--deploy-compute-port":  "55433",
			"--deploy-timeout":       "10m",
		}
	}

	tests := []struct {
		name   string
		mutate func(m map[string]string)
	}{
		{"no namespace", func(m map[string]string) { delete(m, "--deploy-namespace") }},
		{"nonexistent chart path", func(m map[string]string) { m["--deploy-chart"] = filepath.Join(chart, "does-not-exist") }},
		{"no image repo", func(m map[string]string) { delete(m, "--endpoint-image-repo") }},
		{"no image tag", func(m map[string]string) { m["--endpoint-image-tag"] = "" }},
		{"no backing yb host ports", func(m map[string]string) { delete(m, "--deploy-yb-host-ports") }},
		{"bad compute port", func(m map[string]string) { m["--deploy-compute-port"] = "70000" }},
		{"non-positive timeout", func(m map[string]string) { m["--deploy-timeout"] = "0s" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := base()
			tt.mutate(m)
			args := []string{"--deploy"}
			for k, v := range m {
				args = append(args, k, v)
			}
			if _, err := Load(args); err == nil {
				t.Errorf("expected error for %q, got nil", tt.name)
			}
		})
	}
}

func TestDeployFromEnv(t *testing.T) {
	chart := t.TempDir()
	t.Setenv("AMP_DATABASE_URL", "postgres://localhost/db")
	t.Setenv("AMP_DEPLOY", "true")
	t.Setenv("AMP_DEPLOY_DRIVER", "kubernetes")
	t.Setenv("AMP_DEPLOY_NAMESPACE", "env-endpoints")
	t.Setenv("AMP_DEPLOY_CHART", chart)
	t.Setenv("AMP_HELM_BIN", "/usr/local/bin/helm")
	t.Setenv("AMP_ENDPOINT_IMAGE_REPO", "registry.example.com/env-endpoint")
	t.Setenv("AMP_ENDPOINT_IMAGE_TAG", "v9")
	t.Setenv("AMP_DEPLOY_YB_HOST_PORTS", "env-tserver:5433")
	t.Setenv("AMP_DEPLOY_YB_USER", "env_user")
	t.Setenv("AMP_DEPLOY_YB_DBNAME", "env_db")
	t.Setenv("AMP_DEPLOY_COMPUTE_PORT", "6000")
	t.Setenv("AMP_DEPLOY_TIMEOUT", "5m")

	c, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := c.Deploy
	if !d.Enabled {
		t.Fatal("AMP_DEPLOY=true should enable the provisioner")
	}
	if d.Namespace != "env-endpoints" {
		t.Errorf("Namespace = %q, want env-endpoints", d.Namespace)
	}
	if d.HelmBin != "/usr/local/bin/helm" {
		t.Errorf("HelmBin = %q, want env value", d.HelmBin)
	}
	if d.EndpointImageRepo != "registry.example.com/env-endpoint" {
		t.Errorf("EndpointImageRepo = %q, want env value", d.EndpointImageRepo)
	}
	if d.EndpointImageTag != "v9" {
		t.Errorf("EndpointImageTag = %q, want v9", d.EndpointImageTag)
	}
	if d.BackingYBHostPorts != "env-tserver:5433" {
		t.Errorf("BackingYBHostPorts = %q, want env value", d.BackingYBHostPorts)
	}
	if d.BackingYBUser != "env_user" {
		t.Errorf("BackingYBUser = %q, want env_user", d.BackingYBUser)
	}
	if d.BackingYBDBName != "env_db" {
		t.Errorf("BackingYBDBName = %q, want env_db", d.BackingYBDBName)
	}
	if d.ComputePort != 6000 {
		t.Errorf("ComputePort = %d, want 6000", d.ComputePort)
	}
	if d.Timeout != 5*time.Minute {
		t.Errorf("Timeout = %v, want 5m", d.Timeout)
	}
}

func TestDeployFlagOverridesEnv(t *testing.T) {
	chart := t.TempDir()
	t.Setenv("AMP_DATABASE_URL", "postgres://localhost/db")
	t.Setenv("AMP_DEPLOY", "true")
	t.Setenv("AMP_DEPLOY_DRIVER", "kubernetes")
	t.Setenv("AMP_DEPLOY_NAMESPACE", "env-endpoints")
	t.Setenv("AMP_DEPLOY_CHART", chart)
	t.Setenv("AMP_ENDPOINT_IMAGE_REPO", "registry.example.com/env-endpoint")
	t.Setenv("AMP_DEPLOY_YB_HOST_PORTS", "env-tserver:5433")

	c, err := Load([]string{"--deploy-namespace", "flag-endpoints"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Deploy.Namespace != "flag-endpoints" {
		t.Errorf("Namespace = %q, want flag to win (flag-endpoints)", c.Deploy.Namespace)
	}
}
