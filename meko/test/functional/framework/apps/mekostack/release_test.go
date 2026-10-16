package mekostack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const lsRemote = `690c3ae39992b0f896ff608993d33591a4039312	refs/tags/meko-0.261002.0
b75b2a65864345089961d69a84851e4cbdffc0ac	refs/tags/meko-0.261002.0^{}
1111111111111111111111111111111111111111	refs/tags/meko-0.260930.0
2222222222222222222222222222222222222222	refs/tags/meko-0.260929.0
3333333333333333333333333333333333333333	refs/tags/meko-0.260930.1
4444444444444444444444444444444444444444	refs/tags/meko-docs-x
`

func TestPreviousRelease(t *testing.T) {
	r := &Releases{RepoDir: "/repo", Git: func(_ context.Context, _ string, args ...string) (string, error) {
		return lsRemote, nil
	}}
	got, err := r.Previous(context.Background(), ReleaseVersion("0.261002.0-b75b2a658"))
	if err != nil || got != "0.260930.1" {
		t.Fatalf("Previous = %q, %v, want 0.260930.1 (patch releases count, non-version tags do not)", got, err)
	}
	if _, err := r.Previous(context.Background(), "0.260929.0"); err == nil {
		t.Errorf("no older release should be an error")
	}
	got, err = r.Previous(context.Background(), ReleaseVersion("custom-2884d06e1"))
	if err != nil || got != "0.261002.0" {
		t.Fatalf("Previous of a custom build = %q, %v, want the newest release 0.261002.0", got, err)
	}
}

// The chart comes out of the release tag itself, not the working tree, and the
// image tag is <version>-<first 9 chars of the tagged sha>.
func TestReleaseChartFromTag(t *testing.T) {
	repo := t.TempDir()
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		out, err := osGit(ctx, repo, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}
	run("init", "-q")
	run("config", "user.email", "qa@example.com")
	run("config", "user.name", "qa")
	chart := filepath.Join(repo, chartInRepo)
	if err := os.MkdirAll(chart, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chart, "Chart.yaml"), []byte("version: old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "old")
	run("tag", "meko-0.260930.0")
	sha := run("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(chart, "Chart.yaml"), []byte("version: new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("commit", "-qam", "new")

	path, tag, err := NewReleases(repo).Chart(ctx, "0.260930.0", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if tag != "0.260930.0-"+sha[:9] {
		t.Errorf("image tag = %q, want 0.260930.0-%s", tag, sha[:9])
	}
	b, err := os.ReadFile(filepath.Join(path, "Chart.yaml"))
	if err != nil || string(b) != "version: old\n" {
		t.Errorf("chart at %s = %q, %v, want the tagged version", path, b, err)
	}
}

func TestPresetsInKeepsOnlyWhatTheChartShips(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"values-gcp.yaml", "values-byoc.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := PresetsIn(dir, []string{"values-gcp.yaml", "values-byoc.yaml", "values-byoc-bundled-db.yaml"})
	if strings.Join(got, ",") != "values-gcp.yaml,values-byoc.yaml" {
		t.Errorf("PresetsIn = %v", got)
	}
}

func TestDatabaseImageInTakesTheLastFileNamingOne(t *testing.T) {
	dir := t.TempDir()
	for f, body := range map[string]string{
		"values.yaml":                 "yugabyte:\n  Image:\n    repository: quay.io/yugabyte/meko-yugabytedb\n    tag: old\n",
		"values-gcp.yaml":             "mekoUi: {}\n",
		"values-byoc-bundled-db.yaml": "yugabyte:\n  Image:\n    repository: quay.io/yugabyte/yugabyte-itest\n    tag: new\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for files, want := range map[string]string{
		"values-gcp.yaml": "quay.io/yugabyte/meko-yugabytedb:old",
		"values-gcp.yaml,values-byoc-bundled-db.yaml": "quay.io/yugabyte/yugabyte-itest:new",
	} {
		got, err := DatabaseImageIn(dir, strings.Split(files, ","))
		if err != nil || got != want {
			t.Errorf("DatabaseImageIn(%s) = %q, %v, want %q", files, got, err, want)
		}
	}
}
