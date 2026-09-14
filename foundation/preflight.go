package foundation

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Minimum versions. Both are floors the operator path actually needs, not
// arbitrary currency: server-side apply with --force-conflicts is what every
// apply here uses, and the operator's CRDs and the chart's operator mode want a
// reasonably current control plane. Failing on them here beats failing forty
// minutes into a helm install.
const (
	MinHelmMajor = 3
	MinHelmMinor = 11
	MinK8sMajor  = 1
	MinK8sMinor  = 27
)

// checkHelmVersion rejects a helm too old for the chart.
func (d *Deployer) checkHelmVersion(ctx context.Context) error {
	res, err := d.run(ctx, nil, []string{"helm", "version", "--template", "{{.Version}}"})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		// Not fatal: an unexpected helm build that cannot report a version is
		// still likely to work, and refusing to run on that basis is worse than
		// letting the install speak for itself.
		d.log("warning: could not read the helm version (%s); skipping the version check",
			strings.TrimSpace(res.Stderr))
		return nil
	}
	major, minor, ok := parseVersion(res.Stdout)
	if !ok {
		d.log("warning: could not parse the helm version %q; skipping the version check", strings.TrimSpace(res.Stdout))
		return nil
	}
	if major < MinHelmMajor || (major == MinHelmMajor && minor < MinHelmMinor) {
		return fmt.Errorf("helm %d.%d is too old; the yugaware chart needs >= %d.%d — upgrade helm",
			major, minor, MinHelmMajor, MinHelmMinor)
	}
	return nil
}

// checkK8sVersion rejects a control plane too old for the operator.
func (d *Deployer) checkK8sVersion(ctx context.Context) error {
	res, err := d.run(ctx, nil, append(d.kubectlBase(), "version", "-o", "json"))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		d.log("warning: could not read the cluster version; skipping the version check")
		return nil
	}
	var doc struct {
		ServerVersion struct {
			Major string `json:"major"`
			Minor string `json:"minor"`
		} `json:"serverVersion"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &doc); err != nil {
		d.log("warning: could not parse the cluster version; skipping the version check")
		return nil
	}
	major, majorOK := parseNumber(doc.ServerVersion.Major)
	minor, minorOK := parseNumber(doc.ServerVersion.Minor)
	if !majorOK || !minorOK {
		d.log("warning: cluster reported version %q.%q; skipping the version check",
			doc.ServerVersion.Major, doc.ServerVersion.Minor)
		return nil
	}
	if major < MinK8sMajor || (major == MinK8sMajor && minor < MinK8sMinor) {
		return fmt.Errorf("Kubernetes %d.%d is too old; the YugabyteDB operator needs >= %d.%d",
			major, minor, MinK8sMajor, MinK8sMinor)
	}
	return nil
}

// parseVersion reads the major and minor from a "v3.20.0" style string.
func parseVersion(s string) (int, int, bool) {
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, ok := parseNumber(parts[0])
	if !ok {
		return 0, 0, false
	}
	minor, ok := parseNumber(parts[1])
	if !ok {
		return 0, 0, false
	}
	return major, minor, true
}

// parseNumber reads a leading integer, tolerating the "27+" a managed cluster
// reports for its minor version.
func parseNumber(s string) (int, bool) {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:end])
	return n, err == nil
}
