package perf

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// Live baseline: the same workload runs concurrently on stacks from a prior amp release and the verdict is current-vs-that under identical conditions; the portal only says which release tags exist.

const liveBaselineArtifact = "live-baseline.txt"

type liveBaselineConfig struct {
	Enabled      bool
	ImageVersion string
	// AllowSameImage keeps the arm up when the baseline resolves to the image
	// under test. That is an A/A run: the two arms differ in nothing, so every
	// delta it reports is this harness measuring its own noise.
	AllowSameImage bool
	// AA pins the baseline arm to the image under test: the run measures the
	// harness's own arm-to-arm noise, which the A/B gate then uses as its band.
	AA bool
}

func liveBaselineFromConfig(c *core.Config) liveBaselineConfig {
	return liveBaselineConfig{
		Enabled:        c.GetBool("perf.baseline.live", true),
		ImageVersion:   strings.TrimSpace(c.GetString("perf.baseline.image_version", "")),
		AllowSameImage: c.GetBool("perf.baseline.allow_same_image", false),
		AA:             c.GetBool("perf.baseline.aa", false),
	}
}

type portalBuild struct {
	BuildTag string `json:"build_tag"`
	Branch   string `json:"branch"`
}

// Pinned perf.baseline.image_version, else the release before the current tag, else (custom/unknown tag) the newest release; "" = none resolvable.
func resolveBaselineTag(ctx context.Context, lb liveBaselineConfig, portalURL, currentTag string) (string, error) {
	if lb.AA {
		return currentTag, nil
	}
	if lb.ImageVersion != "" {
		return lb.ImageVersion, nil
	}
	if portalURL == "" {
		return "", fmt.Errorf("no report.portal.url to resolve the previous release from")
	}
	cctx, cancel := context.WithTimeout(ctx, portalGateTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, strings.TrimRight(portalURL, "/")+"/api/v1/builds", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("portal builds: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Items []portalBuild `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return pickBaselineTag(out.Items, currentTag), nil
}

// Newest-first: a listed release gets the next older one, anything else gets the newest release.
func pickBaselineTag(items []portalBuild, currentTag string) string {
	var releases []string
	for _, b := range items {
		if isReleaseTag(b.BuildTag) {
			releases = append(releases, b.BuildTag)
		}
	}
	for i, tag := range releases {
		if tag == currentTag {
			if i+1 < len(releases) {
				return releases[i+1]
			}
			return ""
		}
	}
	for _, tag := range releases {
		if tag != currentTag {
			return tag
		}
	}
	return ""
}

func isReleaseTag(tag string) bool {
	return tag != "" && tag[0] >= '0' && tag[0] <= '9'
}

// The amp image tag this run deploys, off the resolved controller image.
func currentAmpTag(c *core.Config) string {
	img := ampstack.FromConfig(c).ControllerImage
	if i := strings.LastIndex(img, ":"); i >= 0 {
		return img[i+1:]
	}
	return ""
}

// Same stack, amp images swapped to the baseline release.
func baselineOverrides(tag string) map[string]string {
	return map[string]string{
		variantControllerImageKey: ampImageRepo + "controller:" + tag,
		variantEndpointImageKey:   ampImageRepo + "endpoint:" + tag,
	}
}

type float64Thresholds struct {
	DropPct float64
	RisePct float64
}
