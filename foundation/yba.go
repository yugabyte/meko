package foundation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// helmRepoName and helmRepoURL are the public chart repo, which carries
// released versions only.
const (
	helmRepoName = "yugabytedb"
	helmRepoURL  = "https://charts.yugabyte.com"
)

// YBA's own customer count. Unauthenticated by design — there is no API token
// on this path, and creating one to ask this question would defeat the point —
// which is what lets it be read over the Service from another pod as well as
// from inside the yugaware container.
const (
	customerCountPath   = "/api/v1/customer_count"
	customerCountPodURL = "http://localhost:9000" + customerCountPath
)

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
	if err := d.clearWedgedRelease(ctx, release); err != nil {
		return err
	}
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
	return d.checkYBACustomer(ctx)
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
//
// It runs immediately before the universe is applied rather than at the end of
// installYBA, so a Release entry earlier in the spec has already had its
// chance to import one.
func (d *Deployer) checkDBRelease(ctx context.Context) error {
	want := universeString(d.spec.universe(), "ybSoftwareVersion")
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
	msg := fmt.Sprintf("this YBA has no DB release %q — it bundles %s. YBA needs a helm package "+
		"for the release, not just the version, and rejects the universe asynchronously with "+
		"\"Helm Package path not found\" otherwise. Note a published chart version is MUTABLE: "+
		"chart %s can be re-cut with a new appVersion, which moves the yugaware image and the "+
		"release it carries",
		want, strings.Join(have, ", "), d.spec.ChartVersion)

	// A Release entry is there precisely to import a release YBA does not
	// have, so this probe has stopped being definitive: it lists what the
	// yugaware IMAGE bundles, while a Release registers one in YBA's own
	// metadata from a URL, with no guarantee it lands in that directory.
	// Failing here would be a false negative on a correct spec, and the rule
	// in this file is that only a definitive answer fails a deploy.
	//
	// The cost, worth naming: a spec importing an UNRELATED release — a later
	// one staged for an upgrade — loses drift detection on the version it is
	// actually running.
	if d.spec.DeclaresRelease() {
		d.log("warning: %s. The spec declares a Release, which is what imports it, so this is "+
			"not fatal — but if the import does not take, the universe will come back "+
			"\"Error Creating\"", msg)
		return nil
	}
	return fmt.Errorf("%s. Set ybSoftwareVersion to one of the above, or declare a Release that "+
		"imports the one you want", msg)
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

	body, where, ok := d.readCustomerCount(ctx, "statefulset/"+names[0])
	// An unreachable endpoint is inconclusive, not proof of a bad install — a
	// hard failure here would turn a probe we cannot run into a broken deploy.
	// A definitive wrong ANSWER below is a different matter.
	if !ok {
		d.log("warning: could not read the customer count from %s; skipping the check", where)
		return nil
	}
	var doc struct {
		Count *int `json:"count"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &doc); err != nil || doc.Count == nil {
		d.log("warning: could not parse a customer count from %q; skipping the check",
			strings.TrimSpace(body))
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

// clearWedgedRelease uninstalls a release that `helm upgrade --install` cannot
// recover, and does nothing otherwise.
//
// A first install that times out leaves the release `failed`; one whose process
// is killed part way leaves it `pending-install`. In both cases the release
// RECORD exists, so `--install` no longer applies and helm takes the upgrade
// path — which refuses, because there is no deployed revision to upgrade from.
// Every later converge then fails the same way, with an error that has nothing
// to do with whatever went wrong the first time. A pod makes this likely: the
// install runs for tens of minutes and any rollout lands inside it.
//
// THE PREDICATE IS THE WHOLE SAFETY PROPERTY. It is not "the status looks bad":
// uninstalling a working release removes the operator that holds every CR's
// finalizer, which is the one thing `down` is carefully ordered to avoid. It is
// "no revision has ever reached deployed", which is exactly the condition under
// which an upgrade cannot work and an uninstall loses nothing — there is no
// working release to lose. A `superseded` revision counts as deployed, because
// that is what a previously-deployed revision becomes.
//
// Within a converge it fires once. Across converges a cause that persists will
// see it fire again; the caller's backoff is what stops that becoming a tight
// uninstall/reinstall loop.
func (d *Deployer) clearWedgedRelease(ctx context.Context, release string) error {
	res, err := d.helm(ctx, "history", release, "--namespace", d.spec.Namespace, "-o", "json")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		// Overwhelmingly "release: not found", which is a fresh install and the
		// common case. Anything else is a helm we cannot interrogate, and the
		// install below will report it better than a guess here would.
		return nil
	}
	var revisions []struct {
		Revision int    `json:"revision"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &revisions); err != nil || len(revisions) == 0 {
		return nil
	}
	// The newest revision that actually deployed, which is what a rollback
	// returns to. helm history is ascending, so the last one wins.
	good := 0
	for _, r := range revisions {
		if r.Status == "deployed" || r.Status == "superseded" {
			good = r.Revision
		}
	}
	last := revisions[len(revisions)-1].Status

	// A release caught mid-operation holds a lock that `helm upgrade` refuses
	// with "another operation (install/upgrade/rollback) is in progress", and
	// nothing clears it on its own — so without this a converge fails forever.
	//
	// It is reached by the pod being KILLED during a helm call: an eviction, a
	// node scaled down, or a SIGKILL at the end of terminationGracePeriodSeconds
	// on an install that ran longer. That is not hypothetical; it is how this
	// was found.
	//
	// Rolled back rather than uninstalled, and only when a good revision exists
	// to return to: uninstalling here would take the operator that holds every
	// CR's finalizer, which is the one thing worse than being stuck. Rollback
	// restores a revision that was already running and releases the lock.
	if good > 0 && strings.HasPrefix(last, "pending-") {
		d.log("release %s is stuck at %q, which `helm upgrade` refuses — rolling back to "+
			"revision %d, the newest that deployed", release, last, good)
		rb, err := d.helm(ctx, "rollback", release, strconv.Itoa(good),
			"--namespace", d.spec.Namespace, "--wait", "--timeout", d.ybaTimeout.String())
		if err != nil {
			return err
		}
		if rb.ExitCode != 0 {
			return fmt.Errorf("release %s is stuck at %q and `helm rollback` to revision %d "+
				"failed, so the install cannot proceed: %s",
				release, last, good, strings.TrimSpace(rb.Stderr))
		}
		return nil
	}
	if good > 0 {
		// A failed revision over a deployed one is ordinary: `helm upgrade`
		// handles it, and uninstalling would be far more destructive than the
		// problem.
		return nil
	}

	d.log("release %s has %d revision(s) and none ever deployed (last: %s), so "+
		"`helm upgrade --install` would refuse it — uninstalling to start clean",
		release, len(revisions), last)
	un, err := d.helm(ctx, "uninstall", release, "--namespace", d.spec.Namespace, "--ignore-not-found")
	if err != nil {
		return err
	}
	if un.ExitCode != 0 {
		return fmt.Errorf("release %s is wedged at %q and `helm uninstall` failed, so the install "+
			"cannot proceed: %s", release, last, strings.TrimSpace(un.Stderr))
	}
	return nil
}

// readCustomerCount asks YBA for its customer count, over whichever transport
// can reach it.
//
// In a pod it is an ordinary GET of the yugaware Service: the endpoint is
// unauthenticated by design, so there is nothing to arrange, and it means the
// deployer needs no pods/exec permission for this check. Outside the cluster
// that Service name resolves to nothing, so the CLI keeps exec'ing curl in the
// yugaware container.
// customerCountAttempts is how many times the probe is retried before it is
// called inconclusive.
//
// The retry is on the TRANSPORT, not on the answer. A count of zero is a real,
// permanent answer and fails immediately — the hook that would have created the
// customer has already run or will never run. What is retried is yugaware not
// answering yet: the rollout check above only proves the pod is Ready, and a
// Play application serves its first request some way after that. Without this,
// that window reads as "cannot check" and the install proceeds to have every CR
// refused, which is the exact outcome this probe exists to prevent.
const customerCountAttempts = 5

func (d *Deployer) readCustomerCount(ctx context.Context, workload string) (body, where string, ok bool) {
	for i := range customerCountAttempts {
		if i > 0 {
			select {
			case <-ctx.Done():
				return "", where, false
			case <-time.After(d.pollInterval):
			}
		}
		body, where, ok = d.readCustomerCountOnce(ctx, workload)
		if ok {
			return body, where, true
		}
	}
	return "", where, false
}

func (d *Deployer) readCustomerCountOnce(ctx context.Context, workload string) (body, where string, ok bool) {
	if d.inCluster {
		url := d.customerCountURL()
		code, body, err := d.httpGet(ctx, url)
		if err != nil || code != http.StatusOK {
			return "", url, false
		}
		return body, url, true
	}
	res, err := d.kubectl(ctx, nil, "exec", workload, "-c", "yugaware", "--",
		"curl", "-s", "-m", "15", customerCountPodURL)
	if err != nil || res.ExitCode != 0 {
		return "", workload, false
	}
	return res.Stdout, workload, true
}

// customerCountURL addresses the chart's own yugaware-ui Service, which is what
// the chart's customer-create hook posts to. Port 80 unless yugaware TLS is on,
// in which case the chart serves 443 and 80 does not exist.
func (d *Deployer) customerCountURL() string {
	host := fmt.Sprintf("%s-yugaware-ui.%s.svc.cluster.local", d.spec.HelmRelease(), d.spec.Namespace)
	if tls, _ := d.spec.YBA.Values["tls"].(map[string]any); tls != nil {
		if on, _ := tls["enabled"].(bool); on {
			return "https://" + host + customerCountPath
		}
	}
	return "http://" + host + customerCountPath
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

	case ChartPath:
		return d.spec.YBA.Chart, nil
	}
	return "", fmt.Errorf("unknown chartSource %q", d.spec.ChartSource)
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

// yugawareLogHint is the command that reads the operator's log on THIS install.
//
// The StatefulSet is looked up rather than named, for the same reason
// yugawareStatefulSets exists: the chart has renamed it across versions, so a
// hint that guesses `<release>-yugaware` sends the reader to a pod that is not
// there. If the lookup fails, the label selector is still correct and finds it.
func (d *Deployer) yugawareLogHint(ctx context.Context) string {
	target := "-l " + d.yugawareSelector()
	if names, err := d.yugawareStatefulSets(ctx); err == nil && len(names) > 0 {
		target = "statefulset/" + names[0]
	}
	return fmt.Sprintf("read the yugaware log: kubectl -n %s logs %s -c yugaware --tail=200",
		d.spec.Namespace, target)
}
