package yba

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/core"
)

// DB release management on the newer /ybdb_release API. A freshly-deployed YBA
// carries no DB release artifact for our version+architecture, so universe-create
// 500s with "no artifact found for architecture <arch>". EnsureRelease imports
// the release from a public tarball URL (the YBA pulls it) before create.

const (
	// URL artifact extraction states (UrlArtifactStatus in the YBA release UI).
	urlArtifactSuccess = "success"
	urlArtifactFailure = "failure"

	// DefaultReleaseExtractTimeout bounds the extract-metadata poll (the YBA
	// downloads the tarball to read its version_metadata.json).
	DefaultReleaseExtractTimeout = 12 * time.Minute
)

// Release is one entry of the /ybdb_release catalog (ResponseRelease).
type Release struct {
	ReleaseUUID string            `json:"release_uuid"`
	Version     string            `json:"version"`
	Artifacts   []ReleaseArtifact `json:"artifacts"`
}

// ReleaseArtifact is one platform+architecture package of a release. Existing
// artifacts carry either a package_url or a package_file_id (bundled local
// file); both are echoed when updating a release so they are preserved.
type ReleaseArtifact struct {
	Platform      string `json:"platform"`
	Architecture  string `json:"architecture"`
	PackageURL    string `json:"package_url,omitempty"`
	PackageFileID string `json:"package_file_id,omitempty"`
}

// extractHandle is the POST /ybdb_release/extract_metadata response — a
// YBPCreateSuccess carrying the resource UUID to poll (extraction runs async).
type extractHandle struct {
	ResourceUUID string `json:"resourceUUID"`
}

// extractedArtifact is the GET /ybdb_release/extract_metadata/{uuid} response
// (ResponseExtractMetadata): the metadata YBA reads off the tarball plus the
// async extraction status (waiting/running/success/failure).
type extractedArtifact struct {
	MetadataUUID  string `json:"metadata_uuid"`
	Version       string `json:"version"`
	Platform      string `json:"platform"`
	Architecture  string `json:"architecture"`
	Sha256        string `json:"sha256"`
	YBType        string `json:"yb_type"`
	ReleaseType   string `json:"release_type"`
	ReleaseDateMs int64  `json:"release_date_msecs"`
	Status        string `json:"status"`
}

// GetReleases lists the DB release catalog (an array of ResponseRelease); a
// legacy map-keyed-by-uuid response is also tolerated.
func (c *Client) GetReleases(ctx context.Context) ([]Release, error) {
	url := c.custURL("/ybdb_release")
	var asList []Release
	if _, err := c.doJSON(ctx, http.MethodGet, url, nil, &asList); err == nil {
		return asList, nil
	}
	asMap := map[string]Release{}
	if _, err := c.doJSON(ctx, http.MethodGet, url, nil, &asMap); err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}
	out := make([]Release, 0, len(asMap))
	for _, r := range asMap {
		out = append(out, r)
	}
	return out, nil
}

// findRelease returns the catalog entry for version (and whether it exists).
func findRelease(releases []Release, version string) (Release, bool) {
	for _, r := range releases {
		if r.Version == version {
			return r, true
		}
	}
	return Release{}, false
}

// hasArch reports whether a release already carries an artifact for arch.
func hasArch(r Release, arch string) bool {
	for _, a := range r.Artifacts {
		if strings.EqualFold(a.Architecture, arch) {
			return true
		}
	}
	return false
}

// releasesHaveArtifact reports whether the catalog already carries version+arch.
func releasesHaveArtifact(releases []Release, version, arch string) bool {
	r, ok := findRelease(releases, version)
	return ok && hasArch(r, arch)
}

// summarizeReleases renders "version[arch,arch] …" for a diagnostic log line.
func summarizeReleases(releases []Release) string {
	if len(releases) == 0 {
		return "(empty)"
	}
	parts := make([]string, 0, len(releases))
	for _, r := range releases {
		arches := make([]string, 0, len(r.Artifacts))
		for _, a := range r.Artifacts {
			arches = append(arches, a.Architecture)
		}
		parts = append(parts, fmt.Sprintf("%s[%s]", r.Version, strings.Join(arches, ",")))
	}
	return strings.Join(parts, " ")
}

// EnsureRelease imports the DB release for version+arch from tarballURL when the
// catalog does not already carry it, so universe-create can find the artifact.
// It logs the catalog it found (the diagnostic for "what releases does YBA
// have") and is idempotent — an already-present artifact is a no-op.
func (c *Client) EnsureRelease(ctx context.Context, version, arch, tarballURL string) error {
	if version == "" || arch == "" || tarballURL == "" {
		return fmt.Errorf("ensure release: version, arch and tarball url are all required")
	}
	ybArch := ybaArch(arch)
	releases, err := c.GetReleases(ctx)
	if err != nil {
		return err
	}
	c.log("yba: release catalog: %s", summarizeReleases(releases))
	existing, versionExists := findRelease(releases, version)
	if versionExists && hasArch(existing, ybArch) {
		return nil // idempotent: the version+arch artifact is already present.
	}
	c.log("yba: importing %s release for %s from %s", ybArch, version, tarballURL)
	meta, err := c.extractReleaseMetadata(ctx, tarballURL)
	if err != nil {
		return fmt.Errorf("extract metadata for %s: %w", tarballURL, err)
	}
	newArtifact := map[string]any{
		"package_url":  tarballURL,
		"sha256":       meta.Sha256,
		"platform":     firstNonEmpty(meta.Platform, "LINUX"),
		"architecture": firstNonEmpty(meta.Architecture, ybArch),
	}

	if versionExists {
		// The version exists (with other arches) — YBA 400s a POST-create, so add
		// the missing artifact to the existing release. The PUT artifacts list is
		// the full desired set, so echo the existing artifacts to preserve them.
		artifacts := make([]any, 0, len(existing.Artifacts)+1)
		for _, a := range existing.Artifacts {
			m := map[string]any{"platform": a.Platform, "architecture": a.Architecture}
			if a.PackageURL != "" {
				m["package_url"] = a.PackageURL
			}
			if a.PackageFileID != "" {
				m["package_file_id"] = a.PackageFileID
			}
			artifacts = append(artifacts, m)
		}
		artifacts = append(artifacts, newArtifact)
		var out map[string]any
		if _, err := c.doJSON(ctx, http.MethodPut, c.custURL("/ybdb_release/"+existing.ReleaseUUID), map[string]any{"artifacts": artifacts}, &out); err != nil {
			return fmt.Errorf("add %s artifact to release %s: %w", ybArch, version, err)
		}
	} else {
		releaseDate := meta.ReleaseDateMs
		if releaseDate == 0 {
			releaseDate = time.Now().UnixMilli()
		}
		payload := map[string]any{
			"version":            firstNonEmpty(meta.Version, version),
			"release_tag":        "",
			"yb_type":            firstNonEmpty(meta.YBType, "YBDB"),
			"release_type":       meta.ReleaseType,
			"release_date_msecs": releaseDate,
			"artifacts":          []any{newArtifact},
		}
		var out map[string]any
		if _, err := c.doJSON(ctx, http.MethodPost, c.custURL("/ybdb_release"), payload, &out); err != nil {
			return fmt.Errorf("create release %s (%s): %w", version, arch, err)
		}
	}
	// The release download runs async; wait until the artifact actually appears
	// in the catalog so universe-create can resolve it.
	return c.waitReleaseArtifact(ctx, version, ybArch)
}

// waitReleaseArtifact polls the catalog until version+arch is present.
func (c *Client) waitReleaseArtifact(ctx context.Context, version, ybArch string) error {
	deadline := time.Now().Add(DefaultReleaseExtractTimeout)
	backoff := 2 * time.Second
	for {
		releases, err := c.GetReleases(ctx)
		if err != nil {
			return err
		}
		if releasesHaveArtifact(releases, version, ybArch) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("release %s (%s) did not become available after import", version, ybArch)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff += 2 * time.Second
		}
	}
}

// extractReleaseMetadata POSTs the tarball URL to extract_metadata (which returns
// a resource UUID; extraction runs async) and polls GET
// extract_metadata/{uuid} until the YBA has read version_metadata.json.
func (c *Client) extractReleaseMetadata(ctx context.Context, url string) (extractedArtifact, error) {
	var handle extractHandle
	if _, err := c.doJSON(ctx, http.MethodPost, c.custURL("/ybdb_release/extract_metadata"), map[string]any{"url": url}, &handle); err != nil {
		return extractedArtifact{}, err
	}
	if handle.ResourceUUID == "" {
		return extractedArtifact{}, fmt.Errorf("extract_metadata returned no resource uuid")
	}
	deadline := time.Now().Add(DefaultReleaseExtractTimeout)
	backoff := 2 * time.Second
	for {
		var st extractedArtifact
		if _, err := c.doJSON(ctx, http.MethodGet, c.custURL("/ybdb_release/extract_metadata/"+handle.ResourceUUID), nil, &st); err != nil {
			return extractedArtifact{}, err
		}
		switch strings.ToLower(st.Status) {
		case urlArtifactSuccess:
			return st, nil
		case urlArtifactFailure:
			return extractedArtifact{}, fmt.Errorf("metadata extraction failed for the release url")
		}
		if time.Now().After(deadline) {
			return extractedArtifact{}, fmt.Errorf("metadata extraction timed out after %s", DefaultReleaseExtractTimeout)
		}
		select {
		case <-ctx.Done():
			return extractedArtifact{}, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff += 2 * time.Second
		}
	}
}

// releaseURLFromConfig returns the DB-release tarball URL to import from:
// apps.yba.release_url when set, else derived from versions.yugabytedb and the
// provider arch (apps.yba.aws.arch).
func releaseURLFromConfig(c ConfigGetter) string {
	if u := c.GetString("apps.yba.release_url", ""); u != "" {
		return u
	}
	return ReleaseTarballURL(c.GetString("versions.yugabytedb", ""), c.GetString("apps.yba.aws.arch", DefaultArch))
}

// ReleaseTarballURL builds the public software.yugabyte.com el8 tarball URL for
// a release: the base version (no build) is the directory, the full
// build-suffixed version the file, arch mapped to YBA's enum. Matches the
// framework's artifacts.yugabytedb pattern (yugabyte-<v>-b<n>-el8-<arch>.tar.gz).
func ReleaseTarballURL(version, arch string) string {
	v, _ := core.ParseVersion(version)
	return fmt.Sprintf("https://software.yugabyte.com/releases/%s/yugabyte-%s-el8-%s.tar.gz", v.Base(), v.String(), ybaArch(arch))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
