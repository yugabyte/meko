package artifact_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yugabyte/yb-amp/test/functional/framework/artifact"
	"github.com/yugabyte/yb-amp/test/functional/framework/core"
)

// An artifact declares its credential BY NAME (secret: <name>); the caller
// resolves that name through the secrets resolver. This checks the two halves
// line up end-to-end over the real YAML load path.
func TestArtifactSecretResolvesByName(t *testing.T) {
	t.Setenv("GH_PACKAGE_TOKEN", "ghp-secret-value")

	dir := t.TempDir()
	yaml := `
secrets:
  gh_package: {env: GH_PACKAGE_TOKEN}
versions:
  tool: "1.2.3"
artifacts:
  mytool:
    glob_path: "mytool-{tool}.tar.gz"
    url: "https://api.github.com/repos/org/repo/releases/tags/v{tool}"
    type: github_release
    secret: gh_package
    target: client
`
	path := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := core.LoadConfigNoDefaults([]string{path}, nil, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	specs, err := artifact.Resolve(cfg)
	if err != nil {
		t.Fatalf("resolve artifacts: %v", err)
	}
	spec, ok := specs["mytool"]
	if !ok {
		t.Fatalf("mytool not resolved: %v", specs)
	}
	if spec.Secret != "gh_package" {
		t.Fatalf("spec.Secret = %q, want gh_package", spec.Secret)
	}

	secrets, err := core.NewSecrets(cfg)
	if err != nil {
		t.Fatalf("NewSecrets: %v", err)
	}
	val, err := secrets.Value(spec.Secret)
	if err != nil {
		t.Fatalf("resolve secret %q: %v", spec.Secret, err)
	}
	if val != "ghp-secret-value" {
		t.Errorf("resolved artifact secret = %q, want the env var's value", val)
	}

	// And the resolved config snapshot must NOT carry the section values.
	if got := cfg.GetString("secrets.gh_package.env", ""); got != "GH_PACKAGE_TOKEN" {
		t.Errorf("precondition: secrets section should hold the env NAME, got %q", got)
	}
}
