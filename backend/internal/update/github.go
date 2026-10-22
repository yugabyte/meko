// Package update finds, installs and rolls back Meko's releases, and finds extensions' releases.
//
// A release install lives in an app folder (~/.local/share/meko/app, or $XDG_DATA_HOME/meko/app),
// outside the data folder: one folder per
// version, app/current pointing at the one in use, and ~/.local/bin/meko pointing at
// app/current/bin/meko. Updating stages the new version beside the others, checks that its binary
// runs here, records update-pending.json and repoints app/current; the service manager then starts
// the new version. A version that fails to start three times is rolled back (pending.go).
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultRepo is where Meko's and AgentK's releases are published.
const DefaultRepo = "yugabyte/agent-k-local"

// Release is one GitHub release.
type Release struct {
	Tag        string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	URL        string  `json:"html_url"`
	Assets     []Asset `json:"assets"`
}

// Asset is a file attached to a release.
type Asset struct {
	Name       string `json:"name"`
	URL        string `json:"url"`                  // the API's URL: needs a token on an internal repo
	BrowserURL string `json:"browser_download_url"` // works without one on a public repo
}

// Asset returns the asset by name.
func (r Release) Asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// Client reads a repo's releases from GitHub's API.
type Client struct {
	Repo  string
	Token string // GitHub token; empty works only for a public repo
	API   string // https://api.github.com; tests point it elsewhere
	HTTP  *http.Client
}

// NewClient is a client for repo, signed in with GITHUB_TOKEN or else gh's token, if either is
// there (the repo is internal for now).
func NewClient(repo string) *Client {
	if repo == "" {
		repo = DefaultRepo
	}
	return &Client{Repo: repo, Token: githubToken(), API: "https://api.github.com",
		HTTP: &http.Client{Timeout: 10 * time.Minute}}
}

// githubToken is GITHUB_TOKEN, or the token gh is signed in with. A service starts with a short
// PATH, so gh is also looked for where Homebrew and gh's own installs put it.
func githubToken() string {
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t
	}
	gh, err := exec.LookPath("gh")
	if err != nil {
		home, _ := os.UserHomeDir()
		for _, p := range []string{"/opt/homebrew/bin/gh", "/usr/local/bin/gh", filepath.Join(home, ".local", "bin", "gh")} {
			if _, serr := os.Stat(p); serr == nil {
				gh = p
				break
			}
		}
	}
	if gh == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, gh, "auth", "token").Output() //nolint:gosec // G204: gh, found above
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (c *Client) get(ctx context.Context, url, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "meko-updater")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req) //nolint:gosec // G704: GitHub's API and its asset URLs
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		hint := ""
		if c.Token == "" && (resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404) {
			hint = ". The repo is internal: sign in with gh auth login, or set GITHUB_TOKEN"
		}
		return nil, fmt.Errorf("GitHub returned %s for %s%s", resp.Status, url, hint)
	}
	return resp, nil
}

// Releases lists the repo's newest 100 releases.
func (c *Client) Releases(ctx context.Context) ([]Release, error) {
	resp, err := c.get(ctx, fmt.Sprintf("%s/repos/%s/releases?per_page=100", c.API, c.Repo), "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []Release
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("unexpected reply listing %s's releases: %w", c.Repo, err)
	}
	return out, nil
}

// maxDownload bounds an asset's size.
const maxDownload = 1 << 30

// Download saves an asset to dest.
func (c *Client) Download(ctx context.Context, a Asset, dest string) error {
	url, accept := a.BrowserURL, "application/octet-stream"
	if c.Token != "" && a.URL != "" {
		url = a.URL // GitHub redirects to the file; the token isn't sent on to another host
	}
	resp, err := c.get(ctx, url, accept)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // G304: a path we chose
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxDownload+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxDownload {
		err = errors.New("the download is larger than 1 GiB")
	}
	if err != nil {
		return fmt.Errorf("downloading %s: %w", a.Name, err)
	}
	return nil
}
