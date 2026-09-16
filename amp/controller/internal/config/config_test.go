package config

import (
	"path/filepath"
	"strings"
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
	// The controller's own schema (the task log + schema_migrations). The resource
	// model is not relocatable and always lives in `amp_registry`
	// (db.ResourceSchema), so this knob names only the former.
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

// The local driver launches nothing, so it must load with none of what a launch
// needs configured: no backing YugabyteDB, no endpoint image, no timeout. That
// is exactly how yb_amp_local starts its controller.
func TestDeployLocalDriverNeedsNothingElse(t *testing.T) {
	c, err := Load([]string{
		"--database-url", "postgres://localhost/db",
		"--deploy",
		"--deploy-driver", "local",
	})
	if err != nil {
		t.Fatalf("--deploy-driver=local should load with no launch config: %v", err)
	}
	if !c.Deploy.Enabled || c.Deploy.Driver != DriverLocal {
		t.Fatalf("deploy = (%v, %q), want (true, %q)", c.Deploy.Enabled, c.Deploy.Driver, DriverLocal)
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

// A typo in --deploy-cloud is silent otherwise: the endpoint chart renders no
// annotations for an unknown cloud, so an endpoint asked for nlb_internal would
// come up internet-facing.
func TestDeployCloud(t *testing.T) {
	chart := t.TempDir()
	args := func(cloud string) []string {
		return []string{
			"--database-url", "postgres://localhost/db",
			"--deploy",
			"--deploy-driver", "kubernetes",
			"--deploy-namespace", "endpoints",
			"--deploy-chart", chart,
			"--endpoint-image-repo", "registry.example.com/endpoint",
			"--endpoint-image-tag", "latest",
			"--deploy-yb-host-ports", "yb-tserver:5433",
			"--deploy-cloud", cloud,
		}
	}
	for _, cloud := range []string{"aws", "gcp", "azure"} {
		c, err := Load(args(cloud))
		if err != nil {
			t.Errorf("cloud %q: unexpected error: %v", cloud, err)
			continue
		}
		if c.Deploy.Cloud != cloud {
			t.Errorf("cloud %q: Cloud = %q", cloud, c.Deploy.Cloud)
		}
	}
	// Empty is the normal case: detect from the cluster's nodes.
	if c, err := Load(args("")); err != nil || c.Deploy.Cloud != "" {
		t.Errorf("empty cloud: Cloud=%v err=%v, want empty/nil", c, err)
	}
	if _, err := Load(args("gke")); err == nil {
		t.Error("expected error for an invalid --deploy-cloud")
	}
}

// TestDeployYBTLS asserts the TLS mode is validated (a libpq sslmode), that TLS
// demands an issuer and a CA bundle, and that the cert lifetimes parse.
func TestDeployYBTLS(t *testing.T) {
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
	c, err := Load(base())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Deploy.BackingYBTLS() {
		t.Errorf("TLS on by default: mode %q", c.Deploy.BackingYBTLSMode)
	}

	c, err = Load(append(base(), "--deploy-yb-tls-mode", "verify-full"))
	if err != nil {
		t.Fatalf("Load(tls): %v", err)
	}
	if !c.Deploy.BackingYBTLS() || c.Deploy.BackingYBTLSIssuerName != "amp-ca" || c.Deploy.BackingYBTLSCABundleConfigMap != "amp-ca-bundle" {
		t.Errorf("TLS defaults not applied: %+v", c.Deploy)
	}
	if _, err := Load(append(base(), "--deploy-yb-tls-mode", "sometimes")); err == nil {
		t.Error("expected error for an unknown TLS mode")
	}
	if _, err := Load(append(base(), "--deploy-yb-tls-mode", "verify-full", "--deploy-yb-tls-issuer-name", "")); err == nil {
		t.Error("expected error when TLS is on without an issuer name")
	}
	if _, err := Load(append(base(), "--deploy-yb-tls-mode", "verify-full", "--deploy-yb-tls-cert-duration", "30d")); err == nil {
		t.Error("expected error for a cert duration Go cannot parse")
	}
	// disable is TLS off, so the issuer is not required.
	if _, err := Load(append(base(), "--deploy-yb-tls-mode", "disable", "--deploy-yb-tls-issuer-name", "")); err != nil {
		t.Errorf("disable with no issuer: %v", err)
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

// #602: the flat per-endpoint size override. Off by default (prod keeps the
// per-vCPU policy); a bad quantity fails startup rather than at endpoint-create.
func TestDeployDiskSizeOverride(t *testing.T) {
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

	c, err := Load(base())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Deploy.DiskSizeOverride != "" {
		t.Errorf("DiskSizeOverride = %q, want empty by default", c.Deploy.DiskSizeOverride)
	}

	for _, ok := range []string{"10Gi", "512Mi", "2G", "1073741824"} {
		c, err := Load(append(base(), "--deploy-disk-size-override", ok))
		if err != nil {
			t.Errorf("Load(--deploy-disk-size-override %s): %v", ok, err)
			continue
		}
		if c.Deploy.DiskSizeOverride != ok {
			t.Errorf("DiskSizeOverride = %q, want %q", c.Deploy.DiskSizeOverride, ok)
		}
	}

	for _, bad := range []string{"10 Gi", "ten", "10GB", "-5Gi"} {
		if _, err := Load(append(base(), "--deploy-disk-size-override", bad)); err == nil {
			t.Errorf("--deploy-disk-size-override %q: want error, got nil", bad)
		}
	}

	// Env-provided, since that is how the chart delivers it.
	t.Setenv("AMP_DEPLOY_DISK_SIZE_OVERRIDE", "10Gi")
	c, err = Load(base())
	if err != nil {
		t.Fatalf("Load(env): %v", err)
	}
	if c.Deploy.DiskSizeOverride != "10Gi" {
		t.Errorf("DiskSizeOverride from env = %q, want 10Gi", c.Deploy.DiskSizeOverride)
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

func TestExtensionRegistryDefaultFlagEnv(t *testing.T) {
	// Default: quay.io/yugabyte (resolved regardless of whether deploy is on).
	c, err := Load([]string{"--database-url", "postgres://localhost/db"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Deploy.ExtensionRegistry != "quay.io/yugabyte" {
		t.Errorf("ExtensionRegistry = %q, want default quay.io/yugabyte", c.Deploy.ExtensionRegistry)
	}

	// Flag override.
	c, err = Load([]string{"--database-url", "postgres://localhost/db", "--extension-registry", "reg.example.com/amp"})
	if err != nil {
		t.Fatalf("Load(flag): %v", err)
	}
	if c.Deploy.ExtensionRegistry != "reg.example.com/amp" {
		t.Errorf("ExtensionRegistry = %q, want the flag value", c.Deploy.ExtensionRegistry)
	}

	// Env, with the flag winning over it.
	t.Setenv("AMP_DATABASE_URL", "postgres://localhost/db")
	t.Setenv("AMP_EXTENSION_REGISTRY", "env.example.com/amp")
	if c, err = Load(nil); err != nil || c.Deploy.ExtensionRegistry != "env.example.com/amp" {
		t.Fatalf("env ExtensionRegistry = %q err=%v, want env.example.com/amp", c.Deploy.ExtensionRegistry, err)
	}
	if c, err = Load([]string{"--extension-registry", "flag.example.com/amp"}); err != nil || c.Deploy.ExtensionRegistry != "flag.example.com/amp" {
		t.Fatalf("flag-over-env ExtensionRegistry = %q err=%v, want flag.example.com/amp", c.Deploy.ExtensionRegistry, err)
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

// --- Voyager migration feature ---------------------------------------------

// TestVoyagerDisabledByDefault: an unset --voyager-database-url leaves the
// feature off and Load succeeds exactly as before (no new required config).
func TestVoyagerDisabledByDefault(t *testing.T) {
	c, err := Load([]string{"--database-url", "postgres://localhost/db"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Voyager.DatabaseURL != "" {
		t.Errorf("Voyager.DatabaseURL = %q, want empty (disabled)", c.Voyager.DatabaseURL)
	}
}

// TestVoyagerEnabledDockerDefaults: the minimum enabling config on the docker
// driver — a database URL and an image — is accepted, and the API-level
// parallel-jobs default is applied.
func TestVoyagerEnabledDockerDefaults(t *testing.T) {
	c, err := Load([]string{
		"--database-url", "postgres://localhost/db",
		"--voyager-database-url", "postgres://voyager/db",
		"--voyager-image", "yb/voyager:amp",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Voyager.PVCSize != "20Gi" {
		t.Errorf("PVCSize = %q, want 20Gi", c.Voyager.PVCSize)
	}
}

// TestVoyagerEnabledRequiresImage: enabling the feature without an image is a
// Load error.
func TestVoyagerEnabledRequiresImage(t *testing.T) {
	_, err := Load([]string{
		"--database-url", "postgres://localhost/db",
		"--voyager-database-url", "postgres://voyager/db",
	})
	if err == nil {
		t.Fatal("expected an error when the Voyager feature is enabled without an image")
	}
}

// TestVoyagerKubernetesInheritsNamespace: on the kubernetes driver the Voyager
// namespace defaults to the deploy namespace, and an explicit override wins.
func TestVoyagerKubernetesInheritsNamespace(t *testing.T) {
	c, err := Load([]string{
		"--database-url", "postgres://localhost/db",
		"--deploy-driver", "kubernetes",
		"--deploy-namespace", "amp",
		"--voyager-database-url", "postgres://voyager/db",
		"--voyager-image", "yb/voyager:amp",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Voyager.Namespace != "amp" {
		t.Errorf("Voyager.Namespace = %q, want it to inherit the deploy namespace (amp)", c.Voyager.Namespace)
	}

	override, err := Load([]string{
		"--database-url", "postgres://localhost/db",
		"--deploy-driver", "kubernetes",
		"--deploy-namespace", "amp",
		"--voyager-database-url", "postgres://voyager/db",
		"--voyager-image", "yb/voyager:amp",
		"--voyager-namespace", "amp-voyager",
	})
	if err != nil {
		t.Fatalf("Load(override): %v", err)
	}
	if override.Voyager.Namespace != "amp-voyager" {
		t.Errorf("Voyager.Namespace = %q, want the explicit override (amp-voyager)", override.Voyager.Namespace)
	}
}

// TestVoyagerKubernetesRequiresNamespace: on the kubernetes driver with no
// deploy namespace to inherit, enabling Voyager is rejected.
func TestVoyagerKubernetesRequiresNamespace(t *testing.T) {
	_, err := Load([]string{
		"--database-url", "postgres://localhost/db",
		"--deploy-driver", "kubernetes",
		"--voyager-database-url", "postgres://voyager/db",
		"--voyager-image", "yb/voyager:amp",
	})
	if err == nil {
		t.Fatal("expected an error when the kubernetes Voyager namespace is empty")
	}
}

// TestVoyagerRejectsLocalDriver: Voyager jobs are launched through a deploy
// driver, so the records-only local driver cannot run them.
func TestVoyagerRejectsLocalDriver(t *testing.T) {
	_, err := Load([]string{
		"--database-url", "postgres://localhost/db",
		"--deploy-driver", "local",
		"--voyager-database-url", "postgres://voyager/db",
		"--voyager-image", "yb/voyager:amp",
	})
	if err == nil {
		t.Fatal("expected an error enabling Voyager on the local deploy driver")
	}
}

// The ingress knobs all carry defaults, so an invalid one is inert until the day
// somebody enables the feature — which is why Validate checks them whether or
// not it is on, and why these run against a deployment that never turned it on.
func TestDeployIngressDefaults(t *testing.T) {
	c, err := Load(kubernetesDeployArgs(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := c.Deploy
	if d.IngressEnabled {
		t.Error("IngressEnabled = true by default; the feature must be opt-in, or a deployment " +
			"that never asked for it starts refusing nothing and placing endpoints on shards")
	}
	if d.IngressZoneInternal != "" || d.IngressZoneExternal != "" {
		t.Errorf("zones default to %q/%q, want empty", d.IngressZoneInternal, d.IngressZoneExternal)
	}
	if d.IngressPlacement != "spread" {
		t.Errorf("IngressPlacement = %q, want spread", d.IngressPlacement)
	}
	// The parsed range is a side effect of validation; nothing else fills it.
	if d.IngressPortLo != 20000 || d.IngressPortHi != 20049 {
		t.Errorf("parsed port range = %d-%d, want 20000-20049", d.IngressPortLo, d.IngressPortHi)
	}
	if d.IngressQuarantine != 10*time.Minute {
		t.Errorf("IngressQuarantine = %v, want 10m", d.IngressQuarantine)
	}
	if d.IngressHeadroom != 10 {
		t.Errorf("IngressHeadroom = %d, want 10", d.IngressHeadroom)
	}
	if d.IngressMaxShards != 24 {
		t.Errorf("IngressMaxShards = %d, want 24", d.IngressMaxShards)
	}
}

// Each of these is a misconfiguration that would otherwise surface as a
// half-built shard or an endpoint on an address nothing answers, hours later.
func TestDeployIngressRejectsBadConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string // substring the message must carry, so the operator can act on it
	}{
		{"range without a dash", []string{"--deploy-ingress-port-range", "20000"}, "lo-hi"},
		{"non-numeric low", []string{"--deploy-ingress-port-range", "x-20049"}, "low port"},
		{"non-numeric high", []string{"--deploy-ingress-port-range", "20000-y"}, "high port"},
		{"inverted range", []string{"--deploy-ingress-port-range", "20050-20000"}, "below low port"},
		{"reserved low port", []string{"--deploy-ingress-port-range", "80-1000"}, "reserved"},
		// The one that bites: a listener in the kernel's outbound source-port
		// range fails to bind only sometimes, under load, on one node.
		{"ephemeral high port", []string{"--deploy-ingress-port-range", "32000-40000"}, "ephemeral"},
		{"port above 65535", []string{"--deploy-ingress-port-range", "20000-70000"}, "1-65535"},
		{"unknown placement", []string{"--deploy-ingress-placement", "balanced"}, "spread"},
		// Only a watermark the ceiling itself cannot hold is unreachable: 10 ports
		// a shard, 24 shards, so 240 is the most that can ever be free.
		{"headroom above the ceiling's capacity", []string{
			"--deploy-ingress-port-range", "20000-20009",
			"--deploy-ingress-headroom", "240",
		}, "unreachable"},
		{"negative headroom", []string{"--deploy-ingress-headroom", "-1"}, "negative"},
		{"zero max shards", []string{"--deploy-ingress-max-shards", "0"}, "at least 1"},
		{"negative quarantine", []string{"--deploy-ingress-quarantine", "-5m"}, "negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(append(kubernetesDeployArgs(t), tc.args...))
			if err == nil {
				t.Fatalf("Load accepted %v, want an error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q, so it does not tell the operator what to fix",
					err, tc.want)
			}
		})
	}
}

// Headroom is measured across a family's ready shards, not against one shard, so
// a watermark wider than a single shard is legitimate — it just settles once the
// second shard is ready. Pinned because the obvious-looking check (headroom <
// one shard's width) refuses a valid fleet-wide watermark, and 239 of a 240-port
// ceiling is where an off-by-one in the real bound would hide.
func TestDeployIngressAcceptsHeadroomWiderThanOneShard(t *testing.T) {
	c, err := Load(append(kubernetesDeployArgs(t),
		"--deploy-ingress-enabled",
		"--deploy-ingress-port-range", "20000-20009",
		"--deploy-ingress-headroom", "239",
		"--deploy-ingress-placement", "pack",
		"--deploy-ingress-zone-internal", "internal.pg.example",
		"--deploy-ingress-zone-external", "external.pg.example",
	))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := c.Deploy
	if !d.IngressEnabled {
		t.Error("IngressEnabled = false after --deploy-ingress-enabled")
	}
	if d.IngressPlacement != "pack" {
		t.Errorf("IngressPlacement = %q, want pack", d.IngressPlacement)
	}
	if d.IngressHeadroom != 239 {
		t.Errorf("IngressHeadroom = %d, want 239", d.IngressHeadroom)
	}
	if d.IngressZoneInternal != "internal.pg.example" || d.IngressZoneExternal != "external.pg.example" {
		t.Errorf("zones = %q/%q, want the two passed in", d.IngressZoneInternal, d.IngressZoneExternal)
	}
}

// Enabling ingress with no zone at all is valid: the names are still allocated
// and still unique, they are simply not published, and an endpoint's address is
// its shard's own host:port. That is what a kind or CI cluster with no
// external-dns gets, and it routes identically — which is the whole point of
// keeping the switch and the zones independent.
func TestDeployIngressEnabledWithoutZone(t *testing.T) {
	c, err := Load(append(kubernetesDeployArgs(t), "--deploy-ingress-enabled"))
	if err != nil {
		t.Fatalf("Load rejected ingress enabled with no zone: %v", err)
	}
	if !c.Deploy.IngressEnabled {
		t.Error("IngressEnabled = false")
	}
	if c.Deploy.IngressZoneInternal != "" || c.Deploy.IngressZoneExternal != "" {
		t.Errorf("zones = %q/%q, want both empty", c.Deploy.IngressZoneInternal, c.Deploy.IngressZoneExternal)
	}
	// One zone configured and the other not is equally valid: that family
	// publishes, the other is addressed by its shard.
	if _, err := Load(append(kubernetesDeployArgs(t),
		"--deploy-ingress-enabled",
		"--deploy-ingress-zone-internal", "internal.pg.example",
	)); err != nil {
		t.Errorf("Load rejected ingress enabled with only the internal zone: %v", err)
	}
}

// Both families in one zone publishes internal names publicly, and the allocator
// stores each name it mints — so this is the one ingress typo that cannot be
// undone by fixing the config later. It has to fail at startup.
func TestDeployIngressRejectsSharedZone(t *testing.T) {
	_, err := Load(append(kubernetesDeployArgs(t),
		"--deploy-ingress-enabled",
		"--deploy-ingress-zone-internal", "pg.example",
		"--deploy-ingress-zone-external", "pg.example",
	))
	if err == nil {
		t.Fatal("Load accepted both families in one zone; internal endpoint names would be " +
			"published in the public zone and could not be corrected without reissuing addresses")
	}
	if !strings.Contains(err.Error(), "pg.example") {
		t.Errorf("error %q does not name the offending zone", err)
	}
	// Distinct zones stay legal, and so does leaving one family unconfigured.
	if _, err := Load(append(kubernetesDeployArgs(t),
		"--deploy-ingress-enabled",
		"--deploy-ingress-zone-internal", "internal.pg.example",
		"--deploy-ingress-zone-external", "external.pg.example",
	)); err != nil {
		t.Errorf("Load rejected two distinct zones: %v", err)
	}
}

// kubernetesDeployArgs is the minimum that gets Load as far as the ingress
// checks, which only run for an enabled kubernetes deployment.
func kubernetesDeployArgs(t *testing.T) []string {
	t.Helper()
	return []string{
		"--database-url", "postgres://localhost/db",
		"--deploy",
		"--deploy-driver", "kubernetes",
		"--deploy-namespace", "endpoints",
		"--deploy-chart", t.TempDir(),
		"--endpoint-image-repo", "registry.example.com/endpoint",
		"--deploy-yb-host-ports", "yb-tserver:5433",
	}
}
