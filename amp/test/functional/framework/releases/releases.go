// Package releases reads the amp report portal's list of REAL product releases.
//
// A test that upgrades through releases must not invent them. The images it
// deploys have to be the ones amp-build actually cut and pushed, in the order
// they were cut, or the run proves nothing about upgrades customers will take.
// The portal already knows: `GET /releases` returns the ordered, deployable
// releases (registry scrape + build provenance, one entry per version).
//
// A `custom-<sha>` or `br-<topic>-<sha>` tag is a branch build, never a
// release, and never appears here.
package releases

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Repo names under the registry namespace. Image refs are left bare
// (`amp-controller:<tag>`) so ampstack's registry knob resolves them, the same
// way its configured defaults are written.
const (
	ControllerRepo = "amp-controller"
	EndpointRepo   = "amp-endpoint"
)

// defaultTimeout bounds a portal read. The portal is reference data here, not
// the system under test: a slow one must not eat a test's budget.
const defaultTimeout = 30 * time.Second

// Release is one product release as the portal knows it.
type Release struct {
	Version string `json:"version"`
	Tag     string `json:"tag"`
	Sha     string `json:"sha"`
	Branch  string `json:"branch"`
	Date    string `json:"date"`
}

// ControllerImage is the controller image ref for this release.
func (r Release) ControllerImage() string { return ControllerRepo + ":" + r.Tag }

// EndpointImage is the endpoint image ref for this release.
func (r Release) EndpointImage() string { return EndpointRepo + ":" + r.Tag }

// Client reads releases from one portal.
type Client struct {
	base string
	hc   *http.Client
	logf func(string, ...any)
}

// New returns a client for the portal at baseURL (the same URL the report sink
// streams to).
func New(baseURL string) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), hc: &http.Client{Timeout: defaultTimeout}}
}

// WithLogf routes the client's own notes (which source answered) into the
// caller's log.
func (c *Client) WithLogf(f func(string, ...any)) *Client { c.logf = f; return c }

func (c *Client) log(format string, args ...any) {
	if c.logf != nil {
		c.logf(format, args...)
	}
}

// List returns up to limit releases, newest first.
//
// It prefers `GET /releases`, which judges deployability against the registry.
// A portal that predates that endpoint answers 404, and the fallback derives the
// same list from `GET /builds` — every release amp-build recorded, which is the
// same set minus the registry cross-check.
func (c *Client) List(ctx context.Context, limit int) ([]Release, error) {
	if c.base == "" {
		return nil, fmt.Errorf("no report portal URL configured, so the list of real releases cannot be read")
	}
	if limit <= 0 {
		limit = 20
	}
	var body struct {
		Items []Release `json:"items"`
	}
	url := fmt.Sprintf("%s/api/v1/releases?limit=%d&require=%s,%s", c.base, limit, ControllerRepo, EndpointRepo)
	status, err := c.get(ctx, url, &body)
	switch {
	case err != nil && status != http.StatusNotFound:
		return nil, err
	case status == http.StatusNotFound:
		c.log("releases: portal %s has no /releases endpoint; deriving the list from /builds", c.base)
		return c.fromBuilds(ctx, limit)
	}
	if len(body.Items) == 0 {
		return nil, fmt.Errorf("the portal at %s lists no releases", c.base)
	}
	return body.Items, nil
}

// fromBuilds derives releases from build provenance: every tag amp-build
// recorded that is shaped like a release, newest version first.
func (c *Client) fromBuilds(ctx context.Context, limit int) ([]Release, error) {
	var body struct {
		Items []struct {
			BuildTag  string `json:"build_tag"`
			Branch    string `json:"branch"`
			CommitSHA string `json:"commit_sha"`
			SeenAt    string `json:"seen_at"`
		} `json:"items"`
	}
	if _, err := c.get(ctx, c.base+"/api/v1/builds?limit=1000", &body); err != nil {
		return nil, err
	}
	byVersion := map[string]Release{}
	for _, it := range body.Items {
		version, sha, ok := ParseTag(it.BuildTag)
		if !ok {
			continue
		}
		r := Release{Version: version, Tag: it.BuildTag, Sha: sha, Branch: it.Branch, Date: it.SeenAt}
		if cur, dup := byVersion[version]; dup && !newer(r, cur) {
			continue
		}
		byVersion[version] = r
	}
	out := make([]Release, 0, len(byVersion))
	for _, r := range byVersion {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return newer(out[i], out[j]) })
	if len(out) == 0 {
		return nil, fmt.Errorf("the portal at %s recorded no release builds, only branch builds", c.base)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, url string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return resp.StatusCode, fmt.Errorf("%s answered %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("decode %s: %w", url, err)
	}
	return resp.StatusCode, nil
}

// ParseTag splits a release tag into version and sha, and reports whether the
// tag is a release at all.
func ParseTag(tag string) (version, sha string, ok bool) {
	t := strings.TrimPrefix(strings.TrimSpace(tag), "v")
	if i := strings.Index(t, "-"); i >= 0 {
		sha, t = t[i+1:], t[:i]
		if len(sha) < 7 || !isHex(sha) {
			return "", "", false
		}
	}
	parts := strings.Split(t, ".")
	if len(parts) != 3 {
		return "", "", false
	}
	for _, p := range parts {
		if _, err := strconv.Atoi(p); err != nil {
			return "", "", false
		}
	}
	return t, sha, true
}

// Compare orders two dotted numeric versions: >0 when a is newer. String order
// is wrong the moment a patch reaches 10 (0.260812.10 sorts below 0.260812.2).
func Compare(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		av, bv := component(as, i), component(bs, i)
		if av != bv {
			if av > bv {
				return 1
			}
			return -1
		}
	}
	return 0
}

func component(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n, _ := strconv.Atoi(parts[i])
	return n
}

func newer(a, b Release) bool {
	if c := Compare(a.Version, b.Version); c != 0 {
		return c > 0
	}
	if a.Date != b.Date {
		return a.Date > b.Date
	}
	return a.Tag > b.Tag
}

func isHex(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return s != ""
}
