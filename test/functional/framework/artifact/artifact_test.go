package artifact_test

import (
	"testing"

	"github.com/yugabyte/yb-amp/test/functional/framework/artifact"
	"github.com/yugabyte/yb-amp/test/functional/framework/core"
)

// TestResolveDerivesBasePlaceholder locks the single-version → {name} (full) +
// {name_base} (build-stripped) split used by the framework-owned yugabytedb
// artifact, and asserts the embedded default resolves BYTE-IDENTICAL to the
// live-green release paths.
func TestResolveDerivesBasePlaceholder(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	specs, err := artifact.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	yb, ok := specs["yugabytedb"]
	if !ok {
		t.Fatalf("yugabytedb not resolved: %v", specs)
	}
	if yb.GlobPath != "yugabyte-2025.2.1.0-b141-el8-aarch64.tar.gz" {
		t.Errorf("glob_path = %q, want the b141 filename", yb.GlobPath)
	}
	if yb.URL != "https://software.yugabyte.com/releases/2025.2.1.0/yugabyte-2025.2.1.0-b141-el8-aarch64.tar.gz" {
		t.Errorf("url = %q, want release DIRECTORY 2025.2.1.0 + b141 filename", yb.URL)
	}
	if yb.Version != "yugabyte-2025.2.1.0" {
		t.Errorf("version = %q, want the bare extracted-directory name", yb.Version)
	}
}

func TestResolveSubstitutesAndValidates(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, []string{
		"versions.benchbase=4.0.1",
		"artifacts.benchbase.glob_path=benchbase-{benchbase}.tar.gz",
		"artifacts.benchbase.url=https://example.test/benchbase-{benchbase}.tar.gz",
		"artifacts.benchbase.type=http",
		"artifacts.benchbase.remote_unzip=bool:true",
		"artifacts.benchbase.target=client",
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	specs, err := artifact.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	spec, ok := specs["benchbase"]
	if !ok {
		t.Fatalf("benchbase not resolved: %v", specs)
	}
	if spec.GlobPath != "benchbase-4.0.1.tar.gz" {
		t.Errorf("glob_path = %q, want substituted version", spec.GlobPath)
	}
	if spec.URL != "https://example.test/benchbase-4.0.1.tar.gz" {
		t.Errorf("url = %q, want substituted version", spec.URL)
	}
	if spec.Type != artifact.TypeHTTP {
		t.Errorf("type = %q, want http", spec.Type)
	}
	if !spec.RemoteUnzip {
		t.Errorf("remote_unzip should be true")
	}
	if spec.Target != "client" {
		t.Errorf("target = %q, want client", spec.Target)
	}
	// Version defaults to the resolved glob_path when not explicitly declared.
	if spec.Version != spec.GlobPath {
		t.Errorf("version = %q, want default to glob_path %q", spec.Version, spec.GlobPath)
	}
}

func TestResolveExplicitVersionField(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, []string{
		"versions.yb_version=2025.1.0.0",
		"artifacts.tool.glob_path=tool.jar",
		"artifacts.tool.url=https://example.test/tool.jar",
		"artifacts.tool.type=http",
		"artifacts.tool.version={yb_version}",
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	specs, err := artifact.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := specs["tool"].Version; got != "2025.1.0.0" {
		t.Errorf("version = %q, want 2025.1.0.0", got)
	}
}

// TestResolveNoArtifactsSectionIsEmptyNotError uses LoadConfigNoDefaults: the
// embedded configs/default.yaml now declares one real entry (artifacts.
// yugabytedb, for the foundation suite's bare-AMI cluster deploy), so an
// absent-section config is the no-embedded-layer case, not the normal load.
func TestResolveNoArtifactsSectionIsEmptyNotError(t *testing.T) {
	cfg, err := core.LoadConfigNoDefaults(nil, nil, nil)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	specs, err := artifact.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(specs) != 0 {
		t.Errorf("specs = %v, want empty", specs)
	}
}

func TestResolveUnresolvedPlaceholderErrors(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, []string{
		"artifacts.tool.glob_path=tool-{missing}.jar",
		"artifacts.tool.url=https://example.test/tool.jar",
		"artifacts.tool.type=http",
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if _, err := artifact.Resolve(cfg); err == nil {
		t.Fatal("expected an unresolved-placeholder error")
	}
}

func TestResolveUnknownTypeErrors(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, []string{
		"artifacts.tool.glob_path=tool.jar",
		"artifacts.tool.url=https://example.test/tool.jar",
		"artifacts.tool.type=ftp",
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if _, err := artifact.Resolve(cfg); err == nil {
		t.Fatal("expected an unknown-type error")
	}
}

func TestResolveMissingGlobPathErrors(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, []string{
		"artifacts.tool.url=https://example.test/tool.jar",
		"artifacts.tool.type=http",
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if _, err := artifact.Resolve(cfg); err == nil {
		t.Fatal("expected a missing-glob_path error")
	}
}

func TestResolveMissingURLErrors(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, []string{
		"artifacts.tool.glob_path=tool.jar",
		"artifacts.tool.type=http",
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if _, err := artifact.Resolve(cfg); err == nil {
		t.Fatal("expected a missing-url error")
	}
}
