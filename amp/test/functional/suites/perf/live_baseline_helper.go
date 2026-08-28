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
}

func liveBaselineFromConfig(c *core.Config) liveBaselineConfig {
	return liveBaselineConfig{
		Enabled:      c.GetBool("perf.baseline.live", true),
		ImageVersion: strings.TrimSpace(c.GetString("perf.baseline.image_version", "")),
	}
}

type portalBuild struct {
	BuildTag string `json:"build_tag"`
	Branch   string `json:"branch"`
}

// Pinned perf.baseline.image_version, else the release before the current tag, else (custom/unknown tag) the newest release; "" = none resolvable.
func resolveBaselineTag(ctx context.Context, lb liveBaselineConfig, portalURL, currentTag string) (string, error) {
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

// Asserts current vs the baseline arm (throughput_drop_pct / latency_rise_pct); returns the live-baseline.txt table.
func liveBaselineGate(current, base BenchResult, thr float64Thresholds, baselineTag string) (string, error) {
	var b strings.Builder
	var violations []string
	fmt.Fprintf(&b, "live baseline: this run vs release %s (same workload, concurrent stacks)\n", baselineTag)
	fmt.Fprintf(&b, "%-14s %14s %14s %9s %11s\n", "metric", "current", "baseline", "delta%", "allowed%")
	for _, m := range comparisonMetrics() {
		cur, ref := m.stats(current).Median, m.stats(base).Median
		if cur == 0 || ref == 0 {
			continue
		}
		d, _ := regressionPct(ref, cur, m.dir)
		allowed := thr.RisePct
		if m.dir == core.HigherIsBetter {
			allowed = thr.DropPct
		}
		fmt.Fprintf(&b, "%-14s %14.3f %14.3f %+8.1f%% %10.1f%%\n", m.name, cur, ref, d, allowed)
		if allowed > 0 && d > allowed {
			violations = append(violations, fmt.Sprintf("%s %+.1f%% vs the live %s baseline (allowed %.1f%%: current %.4g, baseline %.4g %s)",
				m.name, d, baselineTag, allowed, cur, ref, m.unit))
		}
	}
	if len(violations) > 0 {
		return b.String(), fmt.Errorf("perf regression vs the live baseline: %s", strings.Join(violations, "; "))
	}
	return b.String(), nil
}

type float64Thresholds struct {
	DropPct float64
	RisePct float64
}

// The baseline arm's medians, .baseline-suffixed, ungated.
func baselineDatapoints(spec benchWorkloadSpec, base BenchResult) []core.Datapoint {
	gated, _ := spec.datapoints(base)
	out := make([]core.Datapoint, len(gated))
	for i, dp := range gated {
		dp.Name += ".baseline"
		out[i] = dp
	}
	return out
}
