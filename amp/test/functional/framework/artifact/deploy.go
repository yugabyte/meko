package artifact

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/artifact/fetch"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// DefaultCacheDir is ~/.ampqa/artifacts, used when Deployer.CacheDir is unset.
func DefaultCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".ampqa", "artifacts"), nil
}

// cachePath is the local cache location for spec: <cacheDir>/<name>/<version>/<file>.
func cachePath(cacheDir string, spec Spec) string {
	return filepath.Join(cacheDir, spec.Name, spec.Version, spec.GlobPath)
}

// Deployer fetches artifacts into a local cache and delivers them to targets
// over RemoteExecutors. Its fields are dependency-injected so unit tests run
// with no real network, no real aws/curl, and no real SSH.
type Deployer struct {
	CacheDir   string          // default: DefaultCacheDir()
	HTTPClient *http.Client    // default: http.DefaultClient
	AWSRun     fetch.AWSRunner // default: fetch.DefaultAWSRunner
}

// DeployOptions parameterizes one Deploy call.
type DeployOptions struct {
	// RemoteDir is the directory on the target(s) the artifact lands in;
	// combined with spec.GlobPath for the full remote path.
	RemoteDir string
	// Label is matched against Spec.Target. An empty or "all" Spec.Target
	// always matches; an empty Label matches any Spec.Target too (the caller
	// didn't filter, so Deploy is permissive) — only a non-empty Label vs a
	// non-empty, different Spec.Target is a no-op.
	Label string
	// Secret is the resolved credential value for Spec.Secret — the caller
	// looks it up under secrets.<name> in config. artifact/ never reads
	// secrets itself, so it stays agnostic of the secrets config shape.
	Secret string
}

// fetchCapable is implemented by executors that can pull a file off
// themselves (exec/ssh.Client does, via Fetch). Deploy's InternalDownload
// relay uses it when available.
type fetchCapable interface {
	Fetch(ctx context.Context, remotePath, localPath string) error
}

// Deploy delivers spec to every executor in execs, per spec's delivery flags.
// A Spec.Target that doesn't match opts.Label is a no-op (not an error) —
// Deploy is commonly called once per declared artifact against a target
// group, and only some artifacts apply to a given group.
func (d *Deployer) Deploy(ctx context.Context, execs []exec.RemoteExecutor, spec Spec, opts DeployOptions) error {
	if !targetMatches(spec.Target, opts.Label) {
		return nil
	}
	if len(execs) == 0 {
		return fmt.Errorf("deploy %s: no targets", spec.Name)
	}
	remotePath := path.Join(opts.RemoteDir, spec.GlobPath)

	if spec.RemoteDownload {
		for i, ex := range execs {
			if err := d.deployRemoteDownload(ctx, ex, spec, remotePath, opts.Secret); err != nil {
				return fmt.Errorf("deploy %s to target %d: %w", spec.Name, i, err)
			}
		}
		return nil
	}

	local, err := d.ensureCached(ctx, spec, opts.Secret)
	if err != nil {
		return fmt.Errorf("deploy %s: %w", spec.Name, err)
	}

	if spec.InternalDownload {
		return d.deployInternalFanout(ctx, execs, spec, local.Path, remotePath)
	}
	for i, ex := range execs {
		if err := d.uploadAndUnzip(ctx, ex, spec, local.Path, remotePath); err != nil {
			return fmt.Errorf("deploy %s to target %d: %w", spec.Name, i, err)
		}
	}
	return nil
}

func targetMatches(specTarget, label string) bool {
	if specTarget == "" || specTarget == "all" || label == "" {
		return true
	}
	return specTarget == label
}

// ensureCached lands spec in the local cache (unless already there at the
// expected size) and returns its local path.
func (d *Deployer) ensureCached(ctx context.Context, spec Spec, secretValue string) (fetch.Local, error) {
	cacheDir := d.CacheDir
	if cacheDir == "" {
		dir, err := DefaultCacheDir()
		if err != nil {
			return fetch.Local{}, err
		}
		cacheDir = dir
	}
	dest := cachePath(cacheDir, spec)
	switch spec.Type {
	case TypeHTTP:
		return fetch.HTTP(ctx, d.HTTPClient, spec.URL, dest)
	case TypeGithubRelease:
		return fetch.GithubRelease(ctx, d.HTTPClient, spec.URL, spec.GlobPath, secretValue, dest)
	case TypeS3:
		return fetch.S3(ctx, d.AWSRun, spec.URL, dest)
	case TypeLocal:
		return fetch.FromLocalPath(spec.URL, dest)
	default:
		return fetch.Local{}, fmt.Errorf("unknown artifact type %q", spec.Type)
	}
}

// deployRemoteDownload runs the fetch command directly on ex, then unpacks if
// requested. Skipped entirely (idempotent) when remotePath already exists
// with a non-zero size.
func (d *Deployer) deployRemoteDownload(ctx context.Context, ex exec.RemoteExecutor, spec Spec, remotePath, secretValue string) error {
	if size, ok := remoteSize(ctx, ex, remotePath); ok && size > 0 {
		return nil
	}
	if _, err := ex.Run(ctx, fmt.Sprintf("mkdir -p %s", shellQuote(path.Dir(remotePath)))); err != nil {
		return fmt.Errorf("mkdir remote dir: %w", err)
	}
	cmd, err := remoteFetchCommand(spec, remotePath, secretValue)
	if err != nil {
		return err
	}
	res, err := ex.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("remote fetch: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("remote fetch exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return d.maybeUnzip(ctx, ex, spec, remotePath)
}

// remoteFetchCommand builds the command that lands spec at remotePath when run
// directly on the target (curl for http/github_release, aws-cli for s3, cp for
// local). Secrets are passed as a leading shell env assignment rather than a
// literal -H/--token argument — best-effort "env not argv": with the current
// single-string exec.RemoteExecutor.Run there is no side channel to carry a
// separate environment, so the value still travels inside the transported
// command string; true isolation needs a transport with its own env channel
// (e.g. an ssh.Session.Setenv), which is future work.
func remoteFetchCommand(spec Spec, remotePath, secretValue string) (string, error) {
	switch spec.Type {
	case TypeHTTP:
		return fmt.Sprintf("curl -fsSL -o %s %s", shellQuote(remotePath), shellQuote(spec.URL)), nil
	case TypeGithubRelease:
		// remote_download + github_release requires the declaration's url to
		// already be the exact asset URL — the target has no GH-API-by-tag
		// resolution logic of its own, unlike the local-cache path.
		if secretValue == "" {
			return fmt.Sprintf("curl -fsSL -o %s %s", shellQuote(remotePath), shellQuote(spec.URL)), nil
		}
		inner := fmt.Sprintf(`curl -fsSL -H "Authorization: Bearer $AMPQA_TOKEN" -o %s %s`, remotePath, spec.URL)
		return fmt.Sprintf("AMPQA_TOKEN=%s sh -c %s", shellQuote(secretValue), shellQuote(inner)), nil
	case TypeS3:
		return fmt.Sprintf("aws s3 cp %s %s", shellQuote(spec.URL), shellQuote(remotePath)), nil
	case TypeLocal:
		return fmt.Sprintf("cp %s %s", shellQuote(spec.URL), shellQuote(remotePath)), nil
	default:
		return "", fmt.Errorf("unknown artifact type %q", spec.Type)
	}
}

// uploadAndUnzip copies localPath to ex at remotePath (skipping if already
// present at the right size) and unpacks if requested.
func (d *Deployer) uploadAndUnzip(ctx context.Context, ex exec.RemoteExecutor, spec Spec, localPath, remotePath string) error {
	if size, ok := remoteSize(ctx, ex, remotePath); ok && size > 0 {
		return nil
	}
	if _, err := ex.Run(ctx, fmt.Sprintf("mkdir -p %s", shellQuote(path.Dir(remotePath)))); err != nil {
		return fmt.Errorf("mkdir remote dir: %w", err)
	}
	if err := ex.Copy(ctx, localPath, remotePath); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return d.maybeUnzip(ctx, ex, spec, remotePath)
}

// deployInternalFanout lands spec on execs[0] (fetch-once), then delivers to
// the rest.
//
// KISS simplification (documented per the implementation plan): true
// host-to-host transfer would need every target reachable directly from
// execs[0], which nothing here provides. Instead this relays through the
// coordinator's local disk — one Fetch off the seed target into a temp file,
// then one Copy per remaining target — so the far side only ever downloads
// from the seed once (one egress from the artifact source), not once per
// target. If the seed executor can't be fetched from (no Fetch method), it
// falls back to uploading from the local cache to every remaining target
// individually.
func (d *Deployer) deployInternalFanout(ctx context.Context, execs []exec.RemoteExecutor, spec Spec, localPath, remotePath string) error {
	if err := d.uploadAndUnzip(ctx, execs[0], spec, localPath, remotePath); err != nil {
		return fmt.Errorf("internal_download seed target: %w", err)
	}
	if len(execs) == 1 {
		return nil
	}

	fetcher, ok := execs[0].(fetchCapable)
	if !ok {
		for i := 1; i < len(execs); i++ {
			if err := d.uploadAndUnzip(ctx, execs[i], spec, localPath, remotePath); err != nil {
				return fmt.Errorf("internal_download fallback target %d: %w", i, err)
			}
		}
		return nil
	}

	// Unique per call via os.CreateTemp: a static temp path collides when parallel
	// deploys share a name + remote basename (the framework runs parallel by design).
	relayFile, err := os.CreateTemp("", fmt.Sprintf("ampqa-relay-%s-*-%s", spec.Name, path.Base(remotePath)))
	if err != nil {
		return fmt.Errorf("internal_download create relay temp: %w", err)
	}
	relay := relayFile.Name()
	_ = relayFile.Close()
	defer os.Remove(relay)
	if err := fetcher.Fetch(ctx, remotePath, relay); err != nil {
		return fmt.Errorf("internal_download relay fetch: %w", err)
	}
	for i := 1; i < len(execs); i++ {
		if err := d.uploadAndUnzip(ctx, execs[i], spec, relay, remotePath); err != nil {
			return fmt.Errorf("internal_download target %d: %w", i, err)
		}
	}
	return nil
}

func (d *Deployer) maybeUnzip(ctx context.Context, ex exec.RemoteExecutor, spec Spec, remotePath string) error {
	if !spec.RemoteUnzip {
		return nil
	}
	dir := path.Dir(remotePath)
	var cmd string
	switch {
	case strings.HasSuffix(remotePath, ".tar.gz"), strings.HasSuffix(remotePath, ".tgz"):
		cmd = fmt.Sprintf("tar -xzf %s -C %s", shellQuote(remotePath), shellQuote(dir))
	case strings.HasSuffix(remotePath, ".zip"):
		cmd = fmt.Sprintf("unzip -o %s -d %s", shellQuote(remotePath), shellQuote(dir))
	case strings.HasSuffix(remotePath, ".tar"):
		cmd = fmt.Sprintf("tar -xf %s -C %s", shellQuote(remotePath), shellQuote(dir))
	default:
		return fmt.Errorf("remote_unzip set but %s has no recognized archive extension", remotePath)
	}
	res, err := ex.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("unzip: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("unzip exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// remoteSize stats remotePath on ex, trying `stat -c %s` then falling back to
// `wc -c` (differing coreutils/busybox availability across target images).
func remoteSize(ctx context.Context, ex exec.RemoteExecutor, remotePath string) (int64, bool) {
	q := shellQuote(remotePath)
	res, err := ex.Run(ctx, fmt.Sprintf("stat -c %%s %s 2>/dev/null || wc -c < %s 2>/dev/null", q, q))
	if err != nil || res.ExitCode != 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// shellQuote wraps s in single quotes, escaping embedded single quotes, so it
// is safe as one argument in a remote /bin/sh command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
