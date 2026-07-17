package yba

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
)

const aarch64URL = "https://software.yugabyte.com/releases/2025.2.1.0/yugabyte-2025.2.1.0-b141-el8-aarch64.tar.gz"

// onExtractMetadata scripts the two-step extract flow (POST → resourceUUID, GET
// → terminal ResponseExtractMetadata) that both the create and update branches
// share, using the REAL shapes verified against YB source.
func onExtractMetadata(f *fakeYBA) {
	f.on("POST /api/v1/customers/cust1/ybdb_release/extract_metadata", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"resourceUUID": "res-1", "success": true})
	})
	f.on("GET /api/v1/customers/cust1/ybdb_release/extract_metadata/res-1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"metadata_uuid": "res-1", "status": "success", "version": "2025.2.1.0-b141",
			"architecture": "aarch64", "platform": "LINUX", "sha256": "deadbeef",
			"yb_type": "YBDB", "release_type": "STS", "release_date_msecs": 1700000000000,
		})
	})
}

// TestEnsureReleaseCreatesWhenVersionAbsent: the version is not in the catalog at
// all → extract + POST /ybdb_release create; no PUT.
func TestEnsureReleaseCreatesWhenVersionAbsent(t *testing.T) {
	f := newFake()
	defer f.close()
	var created atomic.Bool
	f.on("GET /api/v1/customers/cust1/ybdb_release", func(w http.ResponseWriter, _ *http.Request) {
		if !created.Load() {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		writeJSON(w, http.StatusOK, []any{map[string]any{
			"release_uuid": "rel-1", "version": "2025.2.1.0-b141",
			"artifacts": []any{map[string]any{"platform": "LINUX", "architecture": "aarch64"}},
		}})
	})
	onExtractMetadata(f)
	var createBody map[string]any
	f.on("POST /api/v1/customers/cust1/ybdb_release", func(w http.ResponseWriter, r *http.Request) {
		createBody = captureBody(t, r)
		created.Store(true)
		writeJSON(w, http.StatusOK, map[string]any{"resourceUUID": "rel-1"})
	})

	if err := f.client().EnsureRelease(context.Background(), "2025.2.1.0-b141", "arm", aarch64URL); err != nil {
		t.Fatalf("ensure release: %v", err)
	}
	if createBody == nil {
		t.Fatalf("create not called")
	}
	if createBody["release_date_msecs"] != float64(1700000000000) {
		t.Errorf("release_date_msecs = %v", createBody["release_date_msecs"])
	}
	arts, _ := createBody["artifacts"].([]any)
	if len(arts) != 1 {
		t.Fatalf("artifacts = %v, want 1 (aarch64)", arts)
	}
	a0, _ := arts[0].(map[string]any)
	if a0["architecture"] != "aarch64" || a0["package_url"] != aarch64URL || a0["sha256"] != "deadbeef" {
		t.Errorf("artifact = %v", a0)
	}
	for _, req := range f.requests {
		if req == "PUT /api/v1/customers/cust1/ybdb_release/rel-1" {
			t.Errorf("PUT issued though version was absent")
		}
	}
}

// TestEnsureReleaseAddsArtifactWhenVersionExists: the version exists carrying
// only x86_64 → extract + PUT that PRESERVES x86_64 and appends aarch64; no POST
// create (which would 400 "version already exists").
func TestEnsureReleaseAddsArtifactWhenVersionExists(t *testing.T) {
	f := newFake()
	defer f.close()
	var updated atomic.Bool
	f.on("GET /api/v1/customers/cust1/ybdb_release", func(w http.ResponseWriter, _ *http.Request) {
		arts := []any{map[string]any{"platform": "LINUX", "architecture": "x86_64", "package_file_id": "file-x86"}}
		if updated.Load() {
			arts = append(arts, map[string]any{"platform": "LINUX", "architecture": "aarch64", "package_url": aarch64URL})
		}
		writeJSON(w, http.StatusOK, []any{map[string]any{
			"release_uuid": "rel-1", "version": "2025.2.1.0-b141", "artifacts": arts,
		}})
	})
	onExtractMetadata(f)
	var putBody map[string]any
	f.on("PUT /api/v1/customers/cust1/ybdb_release/rel-1", func(w http.ResponseWriter, r *http.Request) {
		putBody = captureBody(t, r)
		updated.Store(true)
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
	})

	if err := f.client().EnsureRelease(context.Background(), "2025.2.1.0-b141", "arm", aarch64URL); err != nil {
		t.Fatalf("ensure release: %v", err)
	}
	if putBody == nil {
		t.Fatalf("PUT (add-artifact) not called")
	}
	arts, _ := putBody["artifacts"].([]any)
	if len(arts) != 2 {
		t.Fatalf("PUT artifacts = %v, want 2 (x86_64 preserved + aarch64)", arts)
	}
	var haveX86, haveArm bool
	for _, a := range arts {
		m, _ := a.(map[string]any)
		switch m["architecture"] {
		case "x86_64":
			haveX86 = m["package_file_id"] == "file-x86"
		case "aarch64":
			haveArm = m["package_url"] == aarch64URL
		}
	}
	if !haveX86 {
		t.Errorf("existing x86_64 artifact not preserved in PUT: %v", arts)
	}
	if !haveArm {
		t.Errorf("aarch64 artifact not appended in PUT: %v", arts)
	}
	for _, req := range f.requests {
		if req == "POST /api/v1/customers/cust1/ybdb_release" {
			t.Errorf("POST create issued though version already exists")
		}
	}
}

// TestEnsureReleaseSkipsWhenPresent: the version already carries aarch64, so no
// extract/create/update is issued (idempotent).
func TestEnsureReleaseSkipsWhenPresent(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/ybdb_release", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []any{map[string]any{
			"release_uuid": "rel-1", "version": "2025.2.1.0-b141",
			"artifacts": []any{map[string]any{"platform": "LINUX", "architecture": "aarch64"}},
		}})
	})
	if err := f.client().EnsureRelease(context.Background(), "2025.2.1.0-b141", "aarch64", aarch64URL); err != nil {
		t.Fatalf("ensure release: %v", err)
	}
	for _, req := range f.requests {
		if req == "POST /api/v1/customers/cust1/ybdb_release/extract_metadata" {
			t.Errorf("extract_metadata called though artifact already present")
		}
	}
}

func TestReleaseTarballURL(t *testing.T) {
	// The single build-suffixed version drives both the release DIRECTORY (bare
	// 2025.2.1.0) and the tarball FILENAME (full 2025.2.1.0-b141), byte-identical
	// to the live-green path.
	got := ReleaseTarballURL("2025.2.1.0-b141", "arm")
	want := "https://software.yugabyte.com/releases/2025.2.1.0/yugabyte-2025.2.1.0-b141-el8-aarch64.tar.gz"
	if got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
	if x := ReleaseTarballURL("2025.2.1.0-b141", "x86"); x != "https://software.yugabyte.com/releases/2025.2.1.0/yugabyte-2025.2.1.0-b141-el8-x86_64.tar.gz" {
		t.Errorf("x86 url = %q", x)
	}
}

func TestReleaseURLFromConfig(t *testing.T) {
	derived := releaseURLFromConfig(fakeConfig{m: map[string]any{
		"versions.yugabytedb": "2025.2.1.0-b141", "apps.yba.aws.arch": "arm",
	}})
	if derived != "https://software.yugabyte.com/releases/2025.2.1.0/yugabyte-2025.2.1.0-b141-el8-aarch64.tar.gz" {
		t.Errorf("derived = %q", derived)
	}
	override := releaseURLFromConfig(fakeConfig{m: map[string]any{"apps.yba.release_url": "s3://custom/x.tar.gz"}})
	if override != "s3://custom/x.tar.gz" {
		t.Errorf("override = %q", override)
	}
}
