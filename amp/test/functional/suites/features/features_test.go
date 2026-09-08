package features

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pageserver"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybbacking"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/reap"
	"github.com/yugabyte/amp/test/functional/framework/env/sharedk8s"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// A shapeProvisioner stands REAL infrastructure up: env failure, not a defect.
func TestProvisionBackingFailureIsEnvFailure(t *testing.T) {
	boom := errors.New("provision yb in k8s: helm install exited 1: context deadline exceeded")
	f := &flow{
		td:   &teardownStack{},
		logf: func(string, ...any) {},
		prov: func(context.Context, *core.T, *teardownStack, func(string, ...any)) (ybbacking.BackingStore, *reap.Plan, error) {
			return nil, nil, boom
		},
	}
	err := f.provisionBacking(context.Background())
	if !core.IsEnvFailure(err) {
		t.Errorf("provisionBacking error = %v, want it marked an env failure", err)
	}
	if !errors.Is(err, boom) || err.Error() != boom.Error() {
		t.Errorf("marking changed the error: %q", err)
	}
}

// The fork-below-cutoff verdict rests on comparing two LSNs, so equality must not
// read as "the cutoff passed the fork point": a fork AT the cutoff is legal.
func TestCutoffPassed(t *testing.T) {
	for _, c := range []struct {
		cutoff, fork string
		want         bool
	}{
		{"0/3000000", "0/2000000", true},
		{"1/0", "0/FFFFFFF", true},
		{"0/2000000", "0/2000000", false},
		{"0/1000000", "0/2000000", false},
	} {
		got, err := cutoffPassed(c.cutoff, c.fork)
		if err != nil {
			t.Fatalf("cutoffPassed(%s, %s): %v", c.cutoff, c.fork, err)
		}
		if got != c.want {
			t.Errorf("cutoffPassed(cutoff %s, fork %s) = %v, want %v", c.cutoff, c.fork, got, c.want)
		}
	}
	if _, err := cutoffPassed("not-an-lsn", "0/1"); err == nil {
		t.Error("an unreadable cutoff must error rather than decide the verdict")
	}
}

// A missing counter must not read as "the drain is running": the two lead to
// opposite verdicts (an environment skip versus a product failure).
func TestGCDrainDisabledDistinguishesAbsenceFromZero(t *testing.T) {
	off := pageserver.ParseSamples(gcDrainTicksMetric + `{outcome="disabled"} 4`)
	if ticks, known := gcDrainDisabled(off); !known || ticks != 4 {
		t.Errorf("disabled ticks = (%v, %v), want (4, true)", ticks, known)
	}
	running := pageserver.ParseSamples(gcDrainTicksMetric + `{outcome="drained"} 9`)
	if _, known := gcDrainDisabled(running); known {
		t.Error("a scrape without the disabled outcome must report the counter absent")
	}
}

// Only a rejection naming the reason proves the fork-LSN validation ran.
func TestMentionsGCCutoff(t *testing.T) {
	for _, msg := range []string{
		"create_branch failed: fork point 0/2 is below ancestor b1's GC cutoff (history already reclaimed)",
		"fork point 0/2 is below the ancestor's retention",
	} {
		if !mentionsGCCutoff(errors.New(msg)) {
			t.Errorf("rejection not recognised as naming the cutoff: %s", msg)
		}
	}
	for _, msg := range []string{
		"create_branch failed: context deadline exceeded",
		"branch name already in use",
	} {
		if mentionsGCCutoff(errors.New(msg)) {
			t.Errorf("unrelated rejection accepted as a cutoff rejection: %s", msg)
		}
	}
}

// Both halves are load-bearing: a controller that is denied everything is broken
// in the opposite direction from one that still holds cluster-scoped rights.
func TestByocPermissionStepsNeedsBothHalves(t *testing.T) {
	answer := func(m map[string]string, fallback string) canIFunc {
		return func(_ context.Context, p rbacProbe) (string, error) {
			if ans, ok := m[p.String()]; ok {
				return ans, nil
			}
			return fallback, nil
		}
	}
	deny := map[string]string{}
	for _, p := range byocDeniedProbes {
		deny[p.String()] = "no"
	}
	if _, err := byocPermissionSteps(context.Background(), answer(deny, "yes")); err != nil {
		t.Errorf("a BYOC-shaped controller must pass: %v", err)
	}

	leaked := rbacProbe{verb: "list", resource: "pods", cluster: true}
	granted := map[string]string{}
	for k, v := range deny {
		granted[k] = v
	}
	granted[leaked.String()] = "yes"
	_, err := byocPermissionSteps(context.Background(), answer(granted, "yes"))
	if err == nil || !strings.Contains(err.Error(), leaked.String()) {
		t.Errorf("a cluster-scoped grant must fail and name the probe, got %v", err)
	}

	starved := map[string]string{}
	for k, v := range deny {
		starved[k] = v
	}
	_, err = byocPermissionSteps(context.Background(), answer(starved, "no"))
	if err == nil || !strings.Contains(err.Error(), byocAllowedClusterProbes[0].String()) {
		t.Errorf("a controller denied the node read must fail and name the probe, got %v", err)
	}

	noRole := map[string]string{}
	for k, v := range deny {
		noRole[k] = v
	}
	for _, p := range byocAllowedClusterProbes {
		noRole[p.String()] = "yes"
	}
	for _, p := range byocAllowedProbes {
		noRole[p.String()] = "no"
	}
	_, err = byocPermissionSteps(context.Background(), answer(noRole, "no"))
	if err == nil || !strings.Contains(err.Error(), byocAllowedProbes[0].String()) {
		t.Errorf("a controller denied its own Role must fail and name the probe, got %v", err)
	}
}

// rbac.nodeRead is on by default, so node reads are expected and node writes are
// not. Getting this backwards is what made the test red on 0.260907.1.
func TestByocNodeProbesSplitReadFromWrite(t *testing.T) {
	for _, p := range byocDeniedProbes {
		if p.resource == "nodes" && (p.verb == "get" || p.verb == "list") {
			t.Errorf("%q is denied, but rbac.nodeRead grants it by default", p)
		}
	}
	for _, p := range byocAllowedClusterProbes {
		if p.resource != "nodes" || !p.cluster {
			t.Errorf("%q is allowed at cluster scope, but nodeRead is the chart's only cluster grant", p)
		}
	}
}

// The impersonation error must reach the caller intact — it is what selects the
// binding-level fallback instead of failing the run.
func TestByocPermissionStepsPropagatesImpersonationFailure(t *testing.T) {
	canI := func(context.Context, rbacProbe) (string, error) {
		return "", fmt.Errorf("probe: %w: forbidden", errByocNoImpersonation)
	}
	if _, err := byocPermissionSteps(context.Background(), canI); !errors.Is(err, errByocNoImpersonation) {
		t.Errorf("error = %v, want it to wrap errByocNoImpersonation", err)
	}
}

// The fallback has the same two halves, and must say it is binding-level.
func TestByocBindingSteps(t *testing.T) {
	const ns, sa = "ampqa-fnd-x", "amp-controller"
	subject := "ServiceAccount/" + ns + "/" + sa
	list := func(crbs, rbs []byocBinding) byocBindingLister {
		return func(_ context.Context, cluster bool, _ string) ([]byocBinding, error) {
			if cluster {
				return crbs, nil
			}
			return rbs, nil
		}
	}
	nodeRead := byocNodeReadBinding(ns, sa)
	ok := []byocBinding{{name: "amp-controller", roleRef: "Role/amp-controller", subjects: []string{subject}}}
	if _, err := byocBindingSteps(context.Background(), list(nil, ok), ns, sa); err != nil {
		t.Errorf("a namespace-scoped controller must pass the fallback: %v", err)
	}

	chartNodes := []byocBinding{{name: nodeRead, roleRef: "ClusterRole/" + nodeRead, subjects: []string{subject}}}
	if _, err := byocBindingSteps(context.Background(), list(chartNodes, ok), ns, sa); err != nil {
		t.Errorf("the chart's nodeRead pair must pass the fallback: %v", err)
	}

	impostor := []byocBinding{{name: nodeRead, roleRef: "ClusterRole/cluster-admin", subjects: []string{subject}}}
	if _, err := byocBindingSteps(context.Background(), list(impostor, ok), ns, sa); err == nil {
		t.Error("a nodeRead-named binding pointing at another ClusterRole must fail")
	}

	stray := []byocBinding{{name: "legacy-admin", roleRef: "ClusterRole/cluster-admin", subjects: []string{subject}}}
	_, err := byocBindingSteps(context.Background(), list(stray, ok), ns, sa)
	if err == nil || !strings.Contains(err.Error(), "legacy-admin") || !strings.Contains(err.Error(), "binding-level check") {
		t.Errorf("a stray ClusterRoleBinding must fail, naming it and the check's weakness, got %v", err)
	}

	_, err = byocBindingSteps(context.Background(), list(nil, nil), ns, sa)
	if err == nil || !strings.Contains(err.Error(), "no RoleBinding") {
		t.Errorf("a controller with no namespaced binding must fail, got %v", err)
	}
}

// byocRunnerDouble records every argv it is handed and answers with one canned
// result, so the exact kubectl invocation can be pinned.
type byocRunnerDouble struct {
	argvs [][]string
	res   exec.Result
}

func (d *byocRunnerDouble) run(_ context.Context, _ io.Reader, argv []string) (exec.Result, error) {
	d.argvs = append(d.argvs, append([]string(nil), argv...))
	return d.res, nil
}

// kubectl parses only its own global flags before a subcommand, so a scope flag
// placed ahead of `auth can-i` sends the whole invocation down plugin dispatch
// ("flags cannot be placed before plugin name") and no probe ever gets an answer.
func TestByocCanIArgvOrder(t *testing.T) {
	const ns, sa = "ampqa-fnd-x", "amp-controller"
	for _, c := range []struct {
		probe rbacProbe
		want  []string
	}{
		{
			probe: rbacProbe{verb: "list", resource: "namespaces", cluster: true},
			want: []string{"kubectl", "--context", "itest-dev", "auth", "can-i", "list", "namespaces",
				"--as", "system:serviceaccount:" + ns + ":" + sa, "--all-namespaces"},
		},
		{
			probe: rbacProbe{verb: "create", resource: "pods"},
			want: []string{"kubectl", "--context", "itest-dev", "auth", "can-i", "create", "pods",
				"--as", "system:serviceaccount:" + ns + ":" + sa, "-n", ns},
		},
	} {
		d := &byocRunnerDouble{res: exec.Result{Stdout: "no\n", ExitCode: 1}}
		p := &byocKube{run: d.run, bin: byocKubectlBin, kubeContext: "itest-dev"}
		ans, err := p.canI(context.Background(), c.probe, ns, sa)
		if err != nil || ans != "no" {
			t.Fatalf("canI(%s) = (%q, %v), want (\"no\", nil): exit 1 with \"no\" is an answer, not a failure", c.probe, ans, err)
		}
		if len(d.argvs) != 1 || !slices.Equal(d.argvs[0], c.want) {
			t.Errorf("argv = %v, want %v", d.argvs, c.want)
		}
	}
}

func TestByocParseBindingsAndLoadBalancers(t *testing.T) {
	bindings := byocParseBindings("amp-controller|Role/amp-controller|ServiceAccount/ns/amp-controller \n\nbroken|\n")
	if len(bindings) != 1 || !bindings[0].references("ServiceAccount/ns/amp-controller") {
		t.Fatalf("parsed bindings = %+v, want the one complete row", bindings)
	}
	if bindings[0].references("ServiceAccount/ns/other") {
		t.Error("a binding must not match a subject it does not list")
	}
	lbs := byocParseLoadBalancers("yb-master-ui|LoadBalancer\nyb-masters|ClusterIP\nyb-tserver-service|LoadBalancer\n")
	if !slices.Equal(lbs, []string{"yb-master-ui", "yb-tserver-service"}) {
		t.Errorf("LoadBalancer services = %v, want the two LoadBalancer rows", lbs)
	}
}

// The capability check must key on the CRD the chart's own rendering gate reads,
// and a cluster that CAN render collectors must never produce a skip reason.
func TestOtelCRDSkipReason(t *testing.T) {
	if reason := otelCRDSkipReason(nil); !strings.Contains(reason, otelCollectorCRD) {
		t.Errorf("an absent CRD must skip naming %s, got %q", otelCollectorCRD, reason)
	}
	if reason := otelCRDSkipReason([]string{"v1alpha1"}); !strings.Contains(reason, otelCollectorAPIVersion) {
		t.Errorf("a CRD without %s must skip naming the version it lacks, got %q", otelCollectorAPIVersion, reason)
	}
	for _, versions := range [][]string{{"v1beta1"}, {"v1alpha1", "v1beta1"}} {
		if reason := otelCRDSkipReason(versions); reason != "" {
			t.Errorf("versions %v can render collectors, so no skip is allowed, got %q", versions, reason)
		}
	}
}

// A cluster that renders collectors and does not run them is a failure, not a
// skip — the two must not share a branch.
func TestOtelCollectorVerdict(t *testing.T) {
	pods := otelParsePods("amp-controller-otel-controller-collector-abc|Running|otc-container,true,0;\n" +
		"amp-controller-otel-compute-collector-def|Running|otc-container,true,0;\n" +
		"amp-controller-7f8|Running|controller,true,0;\n")
	crs := []string{"amp-controller-otel-controller", "amp-controller-otel-compute"}
	if err := otelCollectorVerdict(crs, pods, DefaultOtelCollectors, 0); err != nil {
		t.Errorf("healthy collectors must pass: %v", err)
	}
	if err := otelCollectorVerdict([]string{crs[0]}, pods, DefaultOtelCollectors, 0); err == nil ||
		!strings.Contains(err.Error(), "*-compute") {
		t.Errorf("a missing collector CR must fail naming it, got %v", err)
	}
	if err := otelCollectorVerdict(crs, nil, DefaultOtelCollectors, 0); err == nil ||
		!strings.Contains(err.Error(), "no pod named") {
		t.Errorf("a CR the operator reconciled no workload for must fail, got %v", err)
	}

	crashing := otelParsePods("amp-controller-otel-controller-collector-abc|Running|otc-container,false,4;\n" +
		"amp-controller-otel-compute-collector-def|Running|otc-container,true,0;\n")
	err := otelCollectorVerdict(crs, crashing, DefaultOtelCollectors, 0)
	if err == nil || !strings.Contains(err.Error(), "restarts=4") {
		t.Errorf("an unready collector pod must fail, reporting its container state, got %v", err)
	}
	restarted := otelParsePods("amp-controller-otel-controller-collector-abc|Running|otc-container,true,2;\n" +
		"amp-controller-otel-compute-collector-def|Running|otc-container,true,0;\n")
	err = otelCollectorVerdict(crs, restarted, DefaultOtelCollectors, 0)
	if err == nil || !strings.Contains(err.Error(), "crash-looping") {
		t.Errorf("restarts over budget must fail, got %v", err)
	}
	if err := otelCollectorVerdict(crs, restarted, DefaultOtelCollectors, 2); err != nil {
		t.Errorf("restarts within budget must pass: %v", err)
	}
}

// Serving is what is being asserted, so an exposition that answers without
// reaching Postgres, or without the series the dashboards read, must fail.
func TestOtelExporterVerdict(t *testing.T) {
	full := pageserver.ParseSamples("pg_up 1\npg_stat_database_numbackends{datname=\"postgres\"} 3\n")
	if err := otelExporterVerdict(full, otelExporterSeries); err != nil {
		t.Errorf("a serving exporter must pass: %v", err)
	}
	if err := otelExporterVerdict(nil, otelExporterSeries); err == nil ||
		!strings.Contains(err.Error(), "no parseable prometheus samples") {
		t.Errorf("an empty scrape must fail, got %v", err)
	}
	down := pageserver.ParseSamples("pg_up 0\npg_stat_database_numbackends 1\n")
	if err := otelExporterVerdict(down, otelExporterSeries); err == nil ||
		!strings.Contains(err.Error(), "cannot reach") {
		t.Errorf("pg_up 0 must fail, got %v", err)
	}
	other := pageserver.ParseSamples("go_goroutines 12\n")
	if err := otelExporterVerdict(other, otelExporterSeries); err == nil ||
		!strings.Contains(err.Error(), "pg_up") {
		t.Errorf("a non-exporter exposition must fail naming pg_up, got %v", err)
	}
	thin := pageserver.ParseSamples("pg_up 1\n")
	if err := otelExporterVerdict(thin, otelExporterSeries); err == nil ||
		!strings.Contains(err.Error(), "pg_stat_database_numbackends") {
		t.Errorf("a scrape missing a dashboard series must fail naming it, got %v", err)
	}
}

// The sidecar's absence and its crash-looping are different failures, and both
// must name the container.
func TestOtelExporterContainerVerdict(t *testing.T) {
	ready := otelParsePods("yb-ep-1-abc|Running|endpoint,true,0;postgres-exporter,true,0;\n")
	if err := otelExporterContainerVerdict(ready, "yb-ep-1-abc", 0); err != nil {
		t.Errorf("a ready sidecar must pass: %v", err)
	}
	if err := otelExporterContainerVerdict(ready, "yb-ep-2-xyz", 0); err == nil ||
		!strings.Contains(err.Error(), "yb-ep-2-xyz") {
		t.Errorf("an unknown pod must fail naming it, got %v", err)
	}
	missing := otelParsePods("yb-ep-1-abc|Running|endpoint,true,0;\n")
	if err := otelExporterContainerVerdict(missing, "yb-ep-1-abc", 0); err == nil ||
		!strings.Contains(err.Error(), otelExporterContainer) {
		t.Errorf("a pod without the sidecar must fail naming it, got %v", err)
	}
	looping := otelParsePods("yb-ep-1-abc|Running|endpoint,true,0;postgres-exporter,true,3;\n")
	if err := otelExporterContainerVerdict(looping, "yb-ep-1-abc", 0); err == nil ||
		!strings.Contains(err.Error(), "crash-looping") {
		t.Errorf("restarts over budget must fail, got %v", err)
	}
}

// The collector discovers the exporter through these annotations, so the port is
// read from them rather than assumed.
func TestOtelParseScrapeAnnotations(t *testing.T) {
	port, err := otelParseScrapeAnnotations("true|9187|/metrics")
	if err != nil || port != 9187 {
		t.Errorf("annotations = (%d, %v), want (9187, nil)", port, err)
	}
	for _, out := range []string{"false|9187|/metrics", "true||/metrics", "true|9187|/", "true|9187"} {
		if _, err := otelParseScrapeAnnotations(out); err == nil {
			t.Errorf("annotations %q must be rejected", out)
		}
	}
}

// upgradeEnvDouble is an amp environment that owns its control plane and can
// name its backing, which is what env/sharedk8s in deploy mode is on CI. It also
// stands in for attach mode, where YBStorage fails.
type upgradeEnvDouble struct{ storageErr error }

func (upgradeEnvDouble) AmpController(context.Context) (*controller.Client, error) {
	return nil, errors.New("not used by the gate")
}
func (upgradeEnvDouble) AmpPoolID() string { return "pool-1" }
func (upgradeEnvDouble) ConnectEndpoint(context.Context, controller.Endpoint) (*pg.Client, error) {
	return nil, errors.New("not used by the gate")
}
func (upgradeEnvDouble) AmpKube() *k8s.Kubectl                            { return nil }
func (upgradeEnvDouble) ControllerURL() string                            { return "http://127.0.0.1:8080" }
func (upgradeEnvDouble) Versions() core.DetectedVersions                  { return core.DetectedVersions{} }
func (upgradeEnvDouble) Exec() exec.RemoteExecutor                        { return nil }
func (upgradeEnvDouble) Handles() core.EnvHandles                         { return core.EnvHandles{} }
func (upgradeEnvDouble) Collect(context.Context, core.ArtifactSink) error { return nil }
func (e upgradeEnvDouble) YBStorage(context.Context) (pg.ConnInfo, error) {
	if e.storageErr != nil {
		return pg.ConnInfo{}, e.storageErr
	}
	return pg.ConnInfo{Host: "127.0.0.1", Port: 15433, User: "yugabyte", Database: "yb_storage"}, nil
}

// The upgrade test skipped on every CI run because it treated any amp-shaped env
// as an attached standing control plane, and the ordinary CI env (sharedk8s in
// deploy mode) is amp-shaped too. An env that can name its backing must never
// reach a skip path.
func TestGateUpgradeBackingDoesNotSkipAnEnvThatNamesItsBacking(t *testing.T) {
	if reason, ok := gateUpgradeBacking(context.Background(), upgradeEnvDouble{}); !ok {
		t.Errorf("an env exposing a backing took the skip path: %s", reason)
	}
	// A non-amp env (fake, universe) is the plain self-provision path.
	if reason, ok := gateUpgradeBacking(context.Background(), nil); !ok {
		t.Errorf("a non-amp env took the skip path: %s", reason)
	}
}

// Attach mode is the only legitimate skip: the standing control plane's backing
// is not ours to name, so a second control plane cannot be given storage.
func TestGateUpgradeBackingSkipsWhenTheBackingCannotBeNamed(t *testing.T) {
	reason, ok := gateUpgradeBacking(context.Background(),
		upgradeEnvDouble{storageErr: errors.New("env.sharedk8s in attach mode does not know the standing control plane's backing YugabyteDB")})
	if ok {
		t.Fatal("an env that cannot name its backing must skip, not proceed")
	}
	for _, want := range []string{"not ours to name", "cannot be given storage", "YBStorage"} {
		if !strings.Contains(reason, want) {
			t.Errorf("skip reason %q does not mention %q", reason, want)
		}
	}
}

// sharedk8s.Env losing YBStorage would silently turn this test back into a
// permanent skip, so the capability is pinned at compile time.
var _ upgradeBackingEnv = (*sharedk8s.Env)(nil)

func TestAssertResizeTasksNeverOverlapped(t *testing.T) {
	base := time.Date(2026, 8, 20, 6, 30, 48, 0, time.UTC)
	task := func(id string, startMS, endMS int) controller.Task {
		return controller.Task{
			ID:        id,
			CreatedAt: base.Add(time.Duration(startMS) * time.Millisecond),
			UpdatedAt: base.Add(time.Duration(endMS) * time.Millisecond),
		}
	}

	// The 0.260819.0 shape: terminal at +325ms, next accepted at +699ms.
	if err := assertResizeTasksNeverOverlapped([]controller.Task{
		task("first", 0, 325), task("second", 699, 2416),
	}); err != nil {
		t.Errorf("two resizes that never overlapped must pass, got %v", err)
	}

	// Handover, not an overlap.
	if err := assertResizeTasksNeverOverlapped([]controller.Task{
		task("first", 0, 325), task("second", 325, 700),
	}); err != nil {
		t.Errorf("touching windows must pass, got %v", err)
	}

	// A real lock violation, in either argument order.
	for _, order := range [][]controller.Task{
		{task("first", 0, 900), task("second", 300, 1200)},
		{task("second", 300, 1200), task("first", 0, 900)},
	} {
		err := assertResizeTasksNeverOverlapped(order)
		if err == nil {
			t.Fatalf("overlapping non-terminal windows must fail, got nil")
		}
		if !strings.Contains(err.Error(), "both non-terminal at once") ||
			!strings.Contains(err.Error(), "tasks_one_active_resize_per_endpoint") {
			t.Errorf("the overlap failure must name the invariant it broke, got %q", err)
		}
	}

	// Missing stamps must not read as "no overlap".
	if err := assertResizeTasksNeverOverlapped([]controller.Task{
		{ID: "a"}, {ID: "b"},
	}); err == nil || !strings.Contains(err.Error(), "created_at/updated_at") {
		t.Errorf("absent timestamps must fail loudly, got %v", err)
	}

	// One task cannot overlap anything.
	if err := assertResizeTasksNeverOverlapped([]controller.Task{task("only", 0, 325)}); err != nil {
		t.Errorf("a single task must pass, got %v", err)
	}
}

// Contenders are indexed by construction order, not by the order the lock
// admitted them, so the winner has to be decided by the controller's own task
// stamps. Run 691ccf01 admitted contender 0 (500m) after contender 5 (1750m)
// settled, and the endpoint kept 500m.
func TestLastResizeToHoldTheLock(t *testing.T) {
	at := func(cpu string, createdAt time.Time) resizeAttempt {
		return resizeAttempt{cpu: cpu, task: controller.Task{ID: cpu}, settled: controller.Task{ID: cpu, CreatedAt: createdAt}}
	}
	base := time.Date(2026, 9, 8, 7, 0, 20, 0, time.UTC)
	accepted := []resizeAttempt{
		at("500m", base.Add(2*time.Second)), // index 0, admitted last
		at("1750m", base),                   // index 5, admitted first
	}
	got, err := lastResizeToHoldTheLock(accepted)
	if err != nil {
		t.Fatal(err)
	}
	if got.cpu != "500m" {
		t.Errorf("winner = %s, want 500m — the highest index is not the last to hold the lock", got.cpu)
	}

	if got, err := lastResizeToHoldTheLock(accepted[1:2]); err != nil || got.cpu != "1750m" {
		t.Errorf("a single acceptance is its own winner, got %s (%v)", got.cpu, err)
	}

	// No stamp means no decidable order, and a guess would silently assert the
	// wrong size.
	if _, err := lastResizeToHoldTheLock([]resizeAttempt{at("500m", base), {cpu: "750m"}}); err == nil {
		t.Error("a task with no created_at must fail rather than pick a winner")
	}
	if _, err := lastResizeToHoldTheLock([]resizeAttempt{{cpu: "750m"}}); err == nil {
		t.Error("a lone task with no created_at must fail too")
	}
}
