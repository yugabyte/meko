package mekostack

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	releaseTagPrefix = "meko-"
	chartInRepo      = "meko/deploy/helm/meko"
	// meko-build tags release images <version>-<first 9 chars of the sha>.
	imageShaLen = 9
	// ReleaseVersion of a meko-build custom image tag, custom-<sha>.
	customBuild = "custom"
)

// GitRunner runs git with args in dir and returns its stdout.
type GitRunner func(ctx context.Context, dir string, args ...string) (string, error)

func osGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Releases reads meko release versions off a git checkout's origin.
type Releases struct {
	RepoDir string
	Git     GitRunner
}

func NewReleases(repoDir string) *Releases { return &Releases{RepoDir: repoDir, Git: osGit} }

// ReleaseVersion is the version part of a release image tag, 0.261002.0 of
// 0.261002.0-b75b2a658.
func ReleaseVersion(imageTag string) string {
	v, _, _ := strings.Cut(imageTag, "-")
	return v
}

// Previous returns the newest release on origin older than version. A custom
// build (custom-<sha>) is built off a branch, so every release counts as older.
func (r *Releases) Previous(ctx context.Context, version string) (string, error) {
	out, err := r.Git(ctx, r.RepoDir, "ls-remote", "--tags", "origin", "refs/tags/"+releaseTagPrefix+"*")
	if err != nil {
		return "", err
	}
	want, ok := parseVersion(version)
	custom := version == customBuild
	if !ok && !custom {
		return "", fmt.Errorf("%q is not a release version", version)
	}
	var older []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || strings.HasSuffix(f[1], "^{}") {
			continue
		}
		v := strings.TrimPrefix(f[1], "refs/tags/"+releaseTagPrefix)
		if pv, ok := parseVersion(v); ok && (custom || lessVersion(pv, want)) {
			older = append(older, v)
		}
	}
	if len(older) == 0 {
		return "", fmt.Errorf("no release before %s on origin", version)
	}
	sort.Slice(older, func(i, j int) bool {
		a, _ := parseVersion(older[i])
		b, _ := parseVersion(older[j])
		return lessVersion(a, b)
	})
	return older[len(older)-1], nil
}

// Chart extracts the chart release version shipped into dir and returns its
// path and the image tag meko-build published for that release.
func (r *Releases) Chart(ctx context.Context, version, dir string) (chartPath, imageTag string, err error) {
	tag := releaseTagPrefix + version
	ref := "refs/tags/" + tag
	if _, err := r.Git(ctx, r.RepoDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		if _, err := r.Git(ctx, r.RepoDir, "fetch", "--no-tags", "--depth", "1", "origin", ref+":"+ref); err != nil {
			return "", "", fmt.Errorf("fetch release tag %s: %w", tag, err)
		}
	}
	sha, err := r.Git(ctx, r.RepoDir, "rev-parse", ref+"^{commit}")
	if err != nil {
		return "", "", err
	}
	sha = strings.TrimSpace(sha)
	if len(sha) < imageShaLen {
		return "", "", fmt.Errorf("release tag %s resolves to %q", tag, sha)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	archive := filepath.Join(dir, tag+".tar")
	if _, err := r.Git(ctx, r.RepoDir, "archive", "--output", archive, ref, chartInRepo); err != nil {
		return "", "", fmt.Errorf("extract the chart of %s: %w", tag, err)
	}
	if out, err := exec.CommandContext(ctx, "tar", "-xf", archive, "-C", dir).CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("untar %s: %w: %s", archive, err, out)
	}
	return filepath.Join(dir, chartInRepo), version + "-" + sha[:imageShaLen], nil
}

// RepoRoot is the top of the git checkout path sits in.
func RepoRoot(ctx context.Context, path string) (string, error) {
	out, err := osGit(ctx, path, "rev-parse", "--show-toplevel")
	return strings.TrimSpace(out), err
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func lessVersion(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
