// Package fetch implements the artifact source fetchers: http, github_release,
// s3, local. Each lands one file on local disk (the artifact cache); none of
// them touch a remote target — that's artifact.Deployer's job. Deliberately
// decoupled from the artifact package's Spec type (plain arguments only) so
// there is no import cycle between the two.
package fetch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Local is a fetched (or already-cached) file on disk.
type Local struct {
	Path   string
	Size   int64
	Cached bool // true when an existing file of the same size was reused
}

// statSize reports the size of an existing plain file at path, or (0, false).
func statSize(path string) (int64, bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return 0, false
	}
	return fi.Size(), true
}

// writeFile streams r into destPath via a temp file + rename, so a failed or
// cancelled download never leaves a partial file at the final path.
func writeFile(destPath string, r io.Reader) (Local, error) {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return Local{}, fmt.Errorf("mkdir %s: %w", filepath.Dir(destPath), err)
	}
	tmp := destPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return Local{}, fmt.Errorf("create %s: %w", tmp, err)
	}
	n, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return Local{}, fmt.Errorf("write %s: %w", tmp, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return Local{}, fmt.Errorf("close %s: %w", tmp, closeErr)
	}
	if n == 0 {
		_ = os.Remove(tmp)
		return Local{}, fmt.Errorf("fetched empty file for %s", destPath)
	}
	if err := os.Rename(tmp, destPath); err != nil {
		_ = os.Remove(tmp)
		return Local{}, fmt.Errorf("rename %s: %w", tmp, err)
	}
	return Local{Path: destPath, Size: n}, nil
}

// HTTP downloads url to destPath via a plain GET. If destPath already exists
// and the server's Content-Length matches its size, the download is skipped.
func HTTP(ctx context.Context, client *http.Client, url, destPath string) (Local, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Local{}, fmt.Errorf("http fetch %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Local{}, fmt.Errorf("http fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Local{}, fmt.Errorf("http fetch %s: status %d", url, resp.StatusCode)
	}
	if existing, ok := statSize(destPath); ok && resp.ContentLength > 0 && existing == resp.ContentLength {
		return Local{Path: destPath, Size: existing, Cached: true}, nil
	}
	return writeFile(destPath, resp.Body)
}

// ghAsset is the subset of the GitHub "get a release" API response used here.
// URL is the asset API endpoint (…/releases/assets/<id>); BrowserDownloadURL is
// the web download link. Asset BYTES are fetched from URL with Accept:
// application/octet-stream — the browser link 404s for a PRIVATE repo even with
// a token, so it is only a fallback when the API URL is absent.
type ghAsset struct {
	Name               string `json:"name"`
	URL                string `json:"url"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type ghRelease struct {
	Assets []ghAsset `json:"assets"`
}

// GithubRelease resolves releaseURL — the GitHub API "get a release by tag"
// endpoint, e.g. https://api.github.com/repos/<org>/<repo>/releases/tags/<tag>,
// already {version}-substituted by the caller — picks the asset whose name
// matches globPattern (path.Match semantics), and downloads it to destPath.
// token, when non-empty, is sent as "Authorization: Bearer <token>" on both
// requests.
func GithubRelease(ctx context.Context, client *http.Client, releaseURL, globPattern, token, destPath string) (Local, error) {
	if client == nil {
		client = http.DefaultClient
	}
	rel, err := fetchRelease(ctx, client, releaseURL, token)
	if err != nil {
		return Local{}, err
	}
	asset, err := matchAsset(rel, globPattern)
	if err != nil {
		return Local{}, fmt.Errorf("github release %s: %w", releaseURL, err)
	}
	return downloadAsset(ctx, client, asset, token, destPath)
}

func fetchRelease(ctx context.Context, client *http.Client, releaseURL, token string) (ghRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseURL, nil)
	if err != nil {
		return ghRelease{}, fmt.Errorf("github release %s: %w", releaseURL, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return ghRelease{}, fmt.Errorf("github release %s: %w", releaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ghRelease{}, fmt.Errorf("github release %s: status %d", releaseURL, resp.StatusCode)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return ghRelease{}, fmt.Errorf("github release %s: decode: %w", releaseURL, err)
	}
	return rel, nil
}

func matchAsset(rel ghRelease, globPattern string) (ghAsset, error) {
	for _, a := range rel.Assets {
		if ok, _ := filepath.Match(globPattern, a.Name); ok {
			return a, nil
		}
	}
	return ghAsset{}, fmt.Errorf("no asset matches %q", globPattern)
}

func downloadAsset(ctx context.Context, client *http.Client, asset ghAsset, token, destPath string) (Local, error) {
	// Fetch the bytes from the asset API URL with Accept: application/octet-stream
	// — GitHub redirects it to the signed blob for both public and private repos.
	// The browser_download_url 404s for a private repo even with a bearer token,
	// so it is only a fallback for the (unusual) response lacking an API url.
	url := asset.URL
	if url == "" {
		url = asset.BrowserDownloadURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Local{}, fmt.Errorf("github asset %s: %w", url, err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Local{}, fmt.Errorf("github asset %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Local{}, fmt.Errorf("github asset %s: status %d", url, resp.StatusCode)
	}
	if existing, ok := statSize(destPath); ok && resp.ContentLength > 0 && existing == resp.ContentLength {
		return Local{Path: destPath, Size: existing, Cached: true}, nil
	}
	return writeFile(destPath, resp.Body)
}

// AWSRunner runs the aws CLI and returns its combined output. Real use shells
// to the ambient-authed `aws` binary (architecture §4.8); tests inject a stub.
type AWSRunner func(ctx context.Context, args ...string) ([]byte, error)

// DefaultAWSRunner shells out to the local `aws` CLI.
func DefaultAWSRunner(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "aws", args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("aws %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return out, nil
}

// S3 fetches an s3:// URL to destPath via `aws s3 cp`, run locally — the
// operator's aws CLI is ambient-authed (architecture §4.8), so no credentials
// are threaded through here. An SDK-based fetcher can replace this later
// without changing the artifact contract.
func S3(ctx context.Context, run AWSRunner, s3URL, destPath string) (Local, error) {
	if run == nil {
		run = DefaultAWSRunner
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return Local{}, fmt.Errorf("mkdir %s: %w", filepath.Dir(destPath), err)
	}
	if _, err := run(ctx, "s3", "cp", s3URL, destPath); err != nil {
		return Local{}, err
	}
	fi, err := os.Stat(destPath)
	if err != nil {
		return Local{}, fmt.Errorf("s3 cp %s: no file at %s after copy: %w", s3URL, destPath, err)
	}
	if fi.Size() == 0 {
		return Local{}, fmt.Errorf("s3 cp %s: downloaded empty file", s3URL)
	}
	return Local{Path: destPath, Size: fi.Size()}, nil
}

// FromLocalPath treats srcPath as an already-local file and copies it into
// destPath (the cache), skipping the copy when destPath already matches its
// size.
func FromLocalPath(srcPath, destPath string) (Local, error) {
	fi, err := os.Stat(srcPath)
	if err != nil {
		return Local{}, fmt.Errorf("local artifact %s: %w", srcPath, err)
	}
	if fi.Size() == 0 {
		return Local{}, fmt.Errorf("local artifact %s: empty file", srcPath)
	}
	if existing, ok := statSize(destPath); ok && existing == fi.Size() {
		return Local{Path: destPath, Size: existing, Cached: true}, nil
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return Local{}, fmt.Errorf("open %s: %w", srcPath, err)
	}
	defer f.Close()
	return writeFile(destPath, f)
}
