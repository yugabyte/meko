package foundation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// helmRepoName and helmRepoURL are the public chart repo, which carries
// released versions only.
const (
	helmRepoName = "yugabytedb"
	helmRepoURL  = "https://charts.yugabyte.com"
)

// customerCountURL is YBA's own unauthenticated customer count, read from
// inside the yugaware container.
const customerCountURL = "http://localhost:9000/api/v1/customer_count"

// releasesDir is where the yugaware image keeps the DB releases it bundles.
const releasesDir = "/opt/yugabyte/releases"

// installYBA installs or upgrades the yugaware chart with the Kubernetes
// operator enabled.
//
// `helm upgrade --install` rather than `helm install`: a converge re-runs this,
// and on an unchanged spec it is a no-op upgrade rather than an error.
func (d *Deployer) installYBA(ctx context.Context) error {
	chart, err := d.resolveChart(ctx)
	if err != nil {
		return err
	}
	release := d.spec.HelmRelease()
	d.log("installing yugaware (release %s, chart %s) — this takes tens of minutes on a cold cluster", release, chart)

	argv := []string{
		"upgrade", "--install", release, chart,
		"--namespace", d.spec.Namespace,
		"--wait",
		"--timeout", d.ybaTimeout.String(),
	}

	// The operator's values first as a file, then our --set list, because helm
	// gives --set precedence over -f. That ordering is the point: everything in
	// HelmSets is load-bearing, and a values file that quietly disabled the
	// operator or the customer-create job would leave a YBA with no operator, or
	// no customer, and CRs nothing reconciles. rbac.create is deliberately NOT
	// in that list — see HelmSets.
	if len(d.spec.YBA.Values) > 0 {
		path, cleanup, err := writeValuesFile(d.spec.YBA.Values)
		if err != nil {
			return err
		}
		defer cleanup()
		d.log("yba values: %d top-level key(s) from the spec", len(d.spec.YBA.Values))
		argv = append(argv, "-f", path)
	}
	// Pin the version only for the repo source. An S3 tarball or a local path
	// already IS a version, and passing --version alongside one is an error.
	if d.spec.ChartSource == ChartRepo && d.spec.ChartVersion != "" {
		argv = append(argv, "--version", d.spec.ChartVersion)
	}
	argv = append(argv, SortedSetArgs(d.spec.HelmSets())...)

	res, err := d.helm(ctx, argv...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("helm upgrade --install %s: %s", release, strings.TrimSpace(res.Stderr))
	}
	// --wait already blocked on the release's own resources, so this is a
	// cheap confirmation rather than the actual wait.
	if err := d.waitYugawareReady(ctx); err != nil {
		return err
	}
	if err := d.checkYBACustomer(ctx); err != nil {
		return err
	}
	return d.checkDBRelease(ctx)
}

// checkDBRelease confirms YBA has the DB release the spec asks for.
//
// YBA needs a helm package for it, not just a version string, and it checks
// that only when the universe is created — asynchronously, with the reason in
// its own log and nothing but "Error Creating" on the CR:
//
//	PlatformServiceException: Helm Package path not found for release: 2026.1.1.1-b2
//	  at UniverseCRUDHandler.checkHelmChartExists
//
// The releases a yugaware has are whatever its IMAGE bundles, and that image is
// pinned by the chart's appVersion — which moves without the chart version
// moving, because a published chart version is mutable. 2026.1.1 was re-cut
// with appVersion 2026.1.1.1-b2 -> 2026.1.1.2-b9, and a spec that had been
// green for days started failing with nothing in it changed. So this is not a
// belt-and-braces check on our own input; it catches drift underneath us.
func (d *Deployer) checkDBRelease(ctx context.Context) error {
	want := universeString(d.spec.Universe, "ybSoftwareVersion")
	if want == "" {
		return nil // Validate already rejects this; nothing to compare.
	}
	names, err := d.yugawareStatefulSets(ctx)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		d.log("warning: found no yugaware StatefulSet matching %s; skipping the release check",
			d.yugawareSelector())
		return nil
	}
	res, err := d.kubectl(ctx, nil, "exec", "statefulset/"+names[0], "-c", "yugaware",
		"--", "ls", releasesDir)
	if err != nil {
		return err
	}
	// An unreadable probe is inconclusive: the universe apply still reports the
	// real answer, just later and less clearly.
	if res.ExitCode != 0 {
		d.log("warning: could not list %s; skipping the release check", releasesDir)
		return nil
	}
	have := strings.Fields(res.Stdout)
	// Nothing listed is not "YBA has no releases", it is "the probe told us
	// nothing" — an image that keeps them elsewhere would read the same. Same
	// rule as the other probes here: only a definitive answer fails a deploy.
	if len(have) == 0 {
		d.log("warning: %s listed nothing; skipping the release check", releasesDir)
		return nil
	}
	for _, v := range have {
		if v == want {
			d.log("YBA has DB release %s", want)
			return nil
		}
	}
	return fmt.Errorf("this YBA has no DB release %q — it bundles %s. YBA needs a helm package "+
		"for the release, not just the version, and rejects the universe asynchronously with "+
		"\"Helm Package path not found\" otherwise. Note a published chart version is MUTABLE: "+
		"chart %s can be re-cut with a new appVersion, which moves the yugaware image and the "+
		"release it carries. Set universe.ybSoftwareVersion to one of the above",
		want, strings.Join(have, ", "), d.spec.ChartVersion)
}

// checkYBACustomer confirms YBA has exactly one customer, which every CR the
// operator reconciles depends on.
//
// This exists because the failure it catches is both silent and permanent. The
// chart creates that customer from a **post-install** hook, so it runs on the
// first install and never again — and if the release already exists without a
// customer (an earlier run with defaultUser.enabled off, which is the chart
// default), every later `helm upgrade --install` is an upgrade, the hook does
// not run, and nothing ever creates one. helm reports success throughout.
//
// What the operator then does is worse than an error: OperatorUtils
// .getOperatorCustomer throws before the reconciler can write a status, so the
// CR sits with an EMPTY status forever and the only evidence is a stack trace
// in the yugaware log. Our own wait cannot distinguish that from a slow
// bring-up, so it would burn the full universe timeout and report nothing
// useful.
//
// Checked over the unauthenticated customer_count endpoint from inside the pod:
// there is no API token on this path by design, and creating one to ask this
// question would defeat the point.
func (d *Deployer) checkYBACustomer(ctx context.Context) error {
	// Addressed as the StatefulSet, not a pod matched by label: the chart puts
	// only `app: <fullname>` on the pod template — no `release` — so a pod
	// selector would have to reconstruct the fullname, and getting it wrong
	// yields an empty match that reads as "nothing to check" rather than as an
	// error. kubectl resolves the workload to one of its pods itself.
	names, err := d.yugawareStatefulSets(ctx)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		// A hard failure, unlike the rollout check above: this is the only
		// thing standing between a customer-less YBA and a bring-up that burns
		// its whole timeout on CRs the operator will not touch. A check that
		// cannot run must say so rather than pass.
		return fmt.Errorf("found no yugaware StatefulSet matching %s, so the single YBA customer "+
			"the operator requires cannot be verified; check the release installed at all",
			d.yugawareSelector())
	}

	res, err := d.kubectl(ctx, nil, "exec", "statefulset/"+names[0], "-c", "yugaware", "--",
		"curl", "-s", "-m", "15", customerCountURL)
	if err != nil {
		return err
	}
	// An unreachable endpoint is inconclusive, not proof of a bad install — a
	// hard failure here would turn a probe we cannot run into a broken deploy.
	// A definitive wrong ANSWER below is a different matter.
	if res.ExitCode != 0 {
		d.log("warning: could not read %s from statefulset/%s; skipping the customer check",
			customerCountURL, names[0])
		return nil
	}
	var doc struct {
		Count *int `json:"count"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &doc); err != nil || doc.Count == nil {
		d.log("warning: could not parse a customer count from %q; skipping the check",
			strings.TrimSpace(res.Stdout))
		return nil
	}
	switch n := *doc.Count; {
	case n == 1:
		d.log("YBA has exactly one customer, which the operator requires")
		return nil
	case n == 0:
		return fmt.Errorf("YBA has no customer, so the operator will refuse every CR with "+
			"\"Customer list does not have exactly one customer\" and leave its status empty. The "+
			"chart creates one from a post-install hook, which does not run on an upgrade — so a "+
			"release installed before yba.values.yugaware.defaultUser was set will never get one. "+
			"Fix it by registering the customer once (POST /api/register on the yugaware-ui "+
			"Service with the defaultUser fields), or uninstall release %s and let a fresh install "+
			"run the hook", d.spec.HelmRelease())
	default:
		return fmt.Errorf("YBA has %d customers and the operator requires exactly one, so it will "+
			"refuse every CR. Something registered a second customer — the YBA console can — and "+
			"one of them has to be removed", n)
	}
}

// resolveChart returns the chart reference for the configured source.
func (d *Deployer) resolveChart(ctx context.Context) (string, error) {
	switch d.spec.ChartSource {
	case ChartRepo:
		// Adding an existing repo is an error, so tolerate it; the update is
		// what makes a newly published version visible.
		if _, err := d.helm(ctx, "repo", "add", helmRepoName, helmRepoURL); err != nil {
			return "", err
		}
		if _, err := d.helm(ctx, "repo", "update", helmRepoName); err != nil {
			return "", err
		}
		return d.spec.YBA.Chart, nil

	case ChartS3:
		// Unreleased itest builds exist only as release tarballs, which is the
		// path ampqa's ybastack takes. Fetching one needs credentials this
		// package does not manage, so it is fetched out of band and handed over
		// as a path rather than downloaded here.
		return "", fmt.Errorf("chartSource s3 needs the tarball fetched out of band: "+
			"download %s and set chartSource: path with yba.chart pointing at it",
			ChartS3URL(d.spec.ChartVersion))

	case ChartPath:
		return d.spec.YBA.Chart, nil
	}
	return "", fmt.Errorf("unknown chartSource %q", d.spec.ChartSource)
}

// ChartS3URL is where a yugaware release tarball lives, mirroring the layout
// amp/test/functional/framework/apps/ybastack uses.
func ChartS3URL(version string) string {
	return fmt.Sprintf("s3://releases.yugabyte.com/%s/helm/yugaware-%s.tgz", version, ShortVersion(version))
}

// ShortVersion trims a build suffix to the chart's three-part version:
// "2025.2.1.0-b141" names a chart published as "yugaware-2025.2.1.tgz".
func ShortVersion(version string) string {
	parts := strings.Split(strings.SplitN(version, "-", 2)[0], ".")
	if len(parts) < 3 {
		return version
	}
	return strings.Join(parts[:3], ".")
}

// yugawareSelector is the label every object the chart renders carries.
//
// NOT app.kubernetes.io/instance, and this is worth stating because the
// recommended-label form is the obvious guess and it matches NOTHING here: the
// yugaware chart sets only the old Helm labels — `release`, `chart`,
// `heritage`, `app`. A selector that matches nothing does not fail, it returns
// an empty list, so an earlier version of this silently skipped its own
// rollout and customer checks on every real run.
//
// `release` rather than `app`, because `app` is the chart's fullname (subject
// to nameOverride) while `release` is the release name we passed in.
func (d *Deployer) yugawareSelector() string {
	return "release=" + d.spec.HelmRelease()
}

// yugawareStatefulSets finds the workload by label rather than by name, because
// the chart has renamed it across versions and a hardcoded name turns a working
// install into a hang.
func (d *Deployer) yugawareStatefulSets(ctx context.Context) ([]string, error) {
	res, err := d.kubectl(ctx, nil, "get", "statefulset",
		"-l", d.yugawareSelector(),
		"-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return nil, err
	}
	return strings.Fields(res.Stdout), nil
}

// waitYugawareReady confirms the yugaware StatefulSet rolled out.
func (d *Deployer) waitYugawareReady(ctx context.Context) error {
	names, err := d.yugawareStatefulSets(ctx)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		// Not fatal: --wait already gated on the release's resources, and a
		// chart layout we cannot recognise should not fail an install that
		// helm itself reported healthy.
		d.log("warning: found no StatefulSet matching %s; skipping the rollout check", d.yugawareSelector())
		return nil
	}
	for _, name := range names {
		res, err := d.kubectl(ctx, nil, "rollout", "status", "statefulset/"+name,
			"--timeout", d.ybaTimeout.String())
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("statefulset/%s did not roll out: %s", name, strings.TrimSpace(res.Stderr))
		}
	}
	return nil
}

// writeValuesFile writes chart values to a temp file for `helm -f`.
//
// A file rather than a --set list, because --set cannot express the chart's
// nested and list-valued settings without escaping that is easy to get subtly
// wrong (a comma in a value silently becomes a second assignment), and the
// whole point of yba.values is to hand over the chart's real surface.
func writeValuesFile(values map[string]any) (path string, cleanup func(), err error) {
	body, err := yaml.Marshal(values)
	if err != nil {
		return "", func() {}, fmt.Errorf("render yba.values: %w", err)
	}
	f, err := os.CreateTemp("", "foundation-yba-values-*.yaml")
	if err != nil {
		return "", func() {}, err
	}
	cleanup = func() { os.Remove(f.Name()) }
	if _, err := f.Write(body); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return f.Name(), cleanup, nil
}
