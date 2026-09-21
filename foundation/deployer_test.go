package foundation

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// testDeployer builds a Deployer over examples/amp.yaml with the waits
// shortened, so the poll tests run in milliseconds rather than tens of minutes.
func testDeployer(t *testing.T, rec *Recorder) (*Deployer, *Spec) {
	t.Helper()
	spec := loadExample(t, "amp.yaml")
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {})
	return d, spec
}

// ybaServiceName is what the chart REALLY names the Namespaced endpoint under
// YBA: "<commonLabels.app.kubernetes.io/part-of>-<endpoint name>", where YBA
// sets part-of to the universe name it generated — suffix included. Taken from
// a real run, because the name this code once built instead
// ("namespaced-yb-tserver-conn") matched nothing on a healthy universe.
const ybaServiceName = "foundation-1954916679-yb-tserver-conn"

// tserverServices is what `kubectl get services -o json` returns for a
// namespace holding an operator-created universe. The tserver Service's
// selector carries TWO labels — the app label plus release=<helm release> — and
// the release name carries the operator's generated hash, which is why the
// selector has to be read rather than templated.
// tserverServiceJSON is the Service the chart creates for us: a ClusterIP with
// both client ports, and a selector the chart wrote.
const tserverServiceJSON = `{"spec":{"type":"ClusterIP",` +
	`"selector":{"app":"yb-tserver","release":"foundation-dfac"},` +
	`"ports":[{"name":"tcp-ysql-port","port":5433},{"name":"tcp-rpc-port","port":9100}]}}`

// tserverSelectorJSON is what `kubectl get service <ours> -o jsonpath={.spec.selector}`
// returns. The chart writes the selector; we only ever read it, to find a pod to
// run ysqlsh in.
const tserverSelectorJSON = `{"app":"yb-tserver","release":"foundation-dfac"}`

// readyStatus is a CR document whose universe has settled.
func readyStatus() string {
	return `{"status":{"universeState":"Ready","sqlEndpoints":["yb-tserver-0:5433"],"actions":[]}}`
}

// statusWithAction is a CR document in state with one action in flight, which is
// the shape every interesting poll takes.
func statusWithAction(state, actionType, actionStatus, message string) string {
	return fmt.Sprintf(
		`{"status":{"universeState":%q,"actions":[{"action_type":%q,"status":%q,"message":%q,"taskUUID":"t1"}]}}`,
		state, actionType, actionStatus, message)
}

func TestApplyCRDsUsesServerSideApply(t *testing.T) {
	rec := &Recorder{}
	d, spec := testDeployer(t, rec)
	if err := d.applyCRDs(context.Background()); err != nil {
		t.Fatalf("applyCRDs: %v", err)
	}
	got := rec.Argvs()[0]
	// Server-side apply, not create: the bundle is shared, so a second
	// foundation on the same cluster re-applies the same objects.
	if !strings.Contains(got, "apply --server-side -f ") {
		t.Errorf("argv = %q, want a server-side apply", got)
	}
	if !strings.Contains(got, CRDURL(spec.ChartVersion)) {
		t.Errorf("argv = %q, want the pinned CRD URL", got)
	}
	// raw.github.com is a redirect kubectl will not follow.
	if strings.Contains(got, "raw.github.com/") {
		t.Error("argv uses raw.github.com rather than raw.githubusercontent.com")
	}
}

// A cluster with no egress to GitHub needs a local bundle, so crdPath must win.
func TestApplyCRDsHonoursLocalPath(t *testing.T) {
	rec := &Recorder{}
	d, spec := testDeployer(t, rec)
	spec.CRDPath = "/tmp/crds.yaml"
	if err := d.applyCRDs(context.Background()); err != nil {
		t.Fatalf("applyCRDs: %v", err)
	}
	got := rec.Argvs()[0]
	if !strings.Contains(got, "-f /tmp/crds.yaml") {
		t.Errorf("argv = %q, want the local path", got)
	}
	if strings.Contains(got, "http") {
		t.Errorf("argv = %q, want no URL when crdPath is set", got)
	}
}

// TestInstallYBAArgv pins the three settings that make this an operator-driven
// foundation, and the release naming that keeps concurrent foundations from
// clobbering each other's cluster-scoped RBAC.
// repoChartVersion is a published line, which is the only thing the repo
// source can resolve. The example deploys an unpublished build from the
// release bucket, so this test sets the repo source explicitly rather than
// inheriting whatever the example happens to name.
const repoChartVersion = "2026.1.1"

func TestInstallYBAArgv(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get statefulset", Result: Result{Stdout: "amp-yugaware"}},
	}}
	d, spec := testDeployer(t, rec)
	spec.ChartSource = ChartRepo
	spec.ChartVersion = repoChartVersion
	spec.YBA.Chart = DefaultChartRepo
	if err := d.installYBA(context.Background()); err != nil {
		t.Fatalf("installYBA: %v", err)
	}

	var upgrade string
	for _, a := range rec.Argvs() {
		if strings.Contains(a, "upgrade --install") {
			upgrade = a
		}
	}
	if upgrade == "" {
		t.Fatalf("no helm upgrade --install in %v", rec.Argvs())
	}
	// upgrade --install, not install: a converge re-runs this and must be a
	// no-op rather than an error on an unchanged spec.
	for _, want := range []string{
		"upgrade --install amp-yugaware yugabytedb/yugaware",
		"--namespace amp",
		"--wait",
		"--version " + repoChartVersion,
		"--set-string yugaware.defaultUser.enabled=true",
		"--set-string yugaware.kubernetesOperatorEnabled=true",
		"--set-string yugaware.kubernetesOperatorNamespace=amp",
	} {
		if !strings.Contains(upgrade, want) {
			t.Errorf("helm argv is missing %q\n  got: %s", want, upgrade)
		}
	}
	// The release name carries the namespace because the chart's ClusterRole
	// and ClusterRoleBinding are named after the release, and cluster-scoped
	// objects have no namespace to isolate them.
	if spec.HelmRelease() != "amp-yugaware" {
		t.Errorf("HelmRelease = %q, want it to carry the namespace", spec.HelmRelease())
	}
}

// A local chart tarball already IS a version, and passing --version alongside
// one is an error.
func TestInstallYBAOmitsVersionForALocalChart(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get statefulset", Result: Result{Stdout: "amp-yugaware"}},
	}}
	d, spec := testDeployer(t, rec)
	spec.ChartSource = ChartPath
	spec.YBA.Chart = "/tmp/yugaware-2026.1.1.tgz"
	if err := d.installYBA(context.Background()); err != nil {
		t.Fatalf("installYBA: %v", err)
	}
	for _, a := range rec.Argvs() {
		if strings.Contains(a, "upgrade --install") {
			if strings.Contains(a, "--version") {
				t.Errorf("argv passes --version with a local chart: %s", a)
			}
			if !strings.Contains(a, "/tmp/yugaware-2026.1.1.tgz") {
				t.Errorf("argv does not use the local chart: %s", a)
			}
		}
		if strings.Contains(a, "repo add") {
			t.Error("added the chart repo for a local chart")
		}
	}
}

// TestApplyUniverseForcesConflicts: without --force-conflicts, a CR somebody
// once edited by hand comes back as a field-manager conflict on every converge,
// forever.
func TestApplyCRsForcesConflicts(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get ybprovider", Result: Result{Stdout: `{"status":{"state":"Ready","resourceUUID":"p1"}}`}},
	}}
	d, _ := testDeployer(t, rec)
	if err := d.applyCRs(context.Background()); err != nil {
		t.Fatalf("applyCRs: %v", err)
	}
	call := rec.Calls[0]
	got := strings.Join(call.Argv, " ")
	if !strings.Contains(got, "apply --server-side --force-conflicts -f -") {
		t.Errorf("argv = %q", got)
	}
	if !strings.Contains(got, "-n amp") {
		t.Errorf("argv = %q, want the namespace", got)
	}
	// The manifest goes over stdin, so what is applied is exactly what render
	// produced and what the golden test checks.
	var doc map[string]any
	if err := json.Unmarshal([]byte(call.Stdin), &doc); err != nil {
		t.Fatalf("stdin is not the rendered CR: %v", err)
	}
	// The provider goes first: YBA cannot place a universe against a provider
	// that does not exist yet.
	if doc["kind"] != "YBProvider" {
		t.Errorf("first applied kind = %v, want YBProvider", doc["kind"])
	}
	var kinds []string
	for _, c := range rec.Calls {
		var d map[string]any
		if c.Stdin == "" || json.Unmarshal([]byte(c.Stdin), &d) != nil {
			continue
		}
		kinds = append(kinds, fmt.Sprint(d["kind"]))
	}
	if len(kinds) != 2 || kinds[0] != "YBProvider" || kinds[1] != "YBUniverse" {
		t.Errorf("applied %v, want [YBProvider YBUniverse]", kinds)
	}
	// And the provider is waited on before the universe is applied, because YBA
	// rejects a universe naming a provider that has not finished registering.
	var waited, appliedUniverse int
	for i, a := range rec.Argvs() {
		if strings.Contains(a, "get ybprovider") {
			waited = i
		}
		if strings.Contains(a, "apply") && strings.Contains(rec.Calls[i].Stdin, "YBUniverse") {
			appliedUniverse = i
		}
	}
	if waited > appliedUniverse {
		t.Errorf("the universe was applied before the provider wait; argv: %v", rec.Argvs())
	}
}

// TestWaitUniverseBlocksOnQueuedActionsThenSucceeds is the deployer-level
// counterpart to ybuniverse.Settled's unit test: a CR that reads Ready with an
// action still queued must not end the wait, because the CRD has no
// observedGeneration and that Ready belongs to the previous generation.
func TestWaitUniverseBlocksOnQueuedActionsThenSucceeds(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		// Ready, but the operator has enqueued the edit it has not started.
		{Match: "get ybuniverse", Once: true, Result: Result{
			Stdout: statusWithAction("Ready", "EditUniverse", "queued", "")}},
		{Match: "get ybuniverse", Once: true, Result: Result{
			Stdout: statusWithAction("Editing", "EditUniverse", "running", "")}},
		{Match: "get ybuniverse", Result: Result{Stdout: readyStatus()}},
	}}
	d, _ := testDeployer(t, rec)
	if err := d.waitUniverse(context.Background()); err != nil {
		t.Fatalf("waitUniverse: %v", err)
	}
	// Three polls: it did not stop at the stale Ready.
	polls := 0
	for _, a := range rec.Argvs() {
		if strings.Contains(a, "get ybuniverse") {
			polls++
		}
	}
	if polls < 3 {
		t.Errorf("polled %d times, want at least 3 — the first Ready was stale and must not have ended the wait", polls)
	}
}

// A failed action is terminal, so the wait must report the operator's reason
// immediately instead of burning the whole timeout to say nothing.
func TestWaitUniverseFailsFastWithTheOperatorsReason(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get ybuniverse", Result: Result{
			Stdout: statusWithAction("Editing", "EditGFlags", "failed", "preview flag not allowed")}},
	}}
	d, _ := testDeployer(t, rec)
	start := time.Now()
	err := d.waitUniverse(context.Background())
	if err == nil {
		t.Fatal("waitUniverse succeeded on a failed action")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("took %s; a failed action should not wait out the timeout", elapsed)
	}
	if !strings.Contains(err.Error(), "preview flag not allowed") {
		t.Errorf("error %q does not carry the operator's message", err)
	}
}

// On timeout the error has to carry the CR status, the pods and the recent
// events — otherwise it says only that time passed, and the cause stays in the
// cluster.
func TestWaitUniverseTimeoutDiagnoses(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get ybuniverse", Result: Result{Stdout: `{"status":{"universeState":"Creating","actions":[]}}`}},
		{Match: "get pods", Result: Result{Stdout: "yb-master-0 Pending"}},
		{Match: "get events", Result: Result{Stdout: "FailedScheduling insufficient cpu"}},
	}}
	d, _ := testDeployer(t, rec)
	d.universeTimeout = 10 * time.Millisecond
	err := d.waitUniverse(context.Background())
	if err == nil {
		t.Fatal("waitUniverse succeeded despite never settling")
	}
	for _, want := range []string{"did not settle", "Creating", "insufficient cpu"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("timeout error is missing %q:\n%s", want, err)
		}
	}
}

// TestConnectBuildsTheConsumerContract covers exactly what up-gke.sh consumes.
func TestConnectBuildsTheConsumerContract(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get ybuniverse", Result: Result{Stdout: readyStatus()}},
		{Match: "get service -l " + TServerServiceLabels, Result: Result{Stdout: ybaServiceName}},
		{Match: "jsonpath={.spec.selector}", Result: Result{Stdout: tserverSelectorJSON}},
	}}
	d, _ := testDeployer(t, rec)

	info, err := d.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if want := ybaServiceName + ".amp.svc.cluster.local:5433"; info.YSQLHostPorts != want {
		t.Errorf("YSQLHostPorts = %q, want %q", info.YSQLHostPorts, want)
	}
	// The same host on 9100: the direct-RPC lane derives its address from the
	// YSQL host rather than being configured, because the deploy path cannot set
	// tserver_rpc_addrs. Reported for diagnostics only.
	if want := ybaServiceName + ".amp.svc.cluster.local:9100"; info.RPCHostPorts != want {
		t.Errorf("RPCHostPorts = %q, want %q", info.RPCHostPorts, want)
	}
	if !strings.HasSuffix(info.DBURL, "/yb_storage?sslmode=disable") {
		t.Errorf("DBURL = %q, want the first database and sslmode=disable", info.DBURL)
	}
	// status.sqlEndpoints is carried for diagnostics but must NOT become
	// YB_HOST_PORTS: it may enumerate per-pod endpoints.
	if len(info.SQLEndpoints) == 0 {
		t.Error("SQLEndpoints was dropped; it is the CR's own report and useful in diagnostics")
	}
	if strings.Contains(info.YSQLHostPorts, "yb-tserver-0:5433") {
		t.Error("YSQLHostPorts came from status.sqlEndpoints rather than the Service name")
	}

	exports := info.ShellExports()
	for _, want := range []string{"export YB_HOST_PORTS=", "export DB_URL="} {
		if !strings.Contains(exports, want) {
			t.Errorf("ShellExports is missing %q:\n%s", want, exports)
		}
	}
	// No RPC export: nothing could consume one, and a variable no consumer reads
	// is worse than none because it reads as wired.
	if strings.Contains(exports, "RPC") {
		t.Errorf("ShellExports emits an RPC variable nothing consumes:\n%s", exports)
	}
}

// TestEnsureDatabasesIsIdempotent: Postgres has no CREATE DATABASE IF NOT
// EXISTS, so the script probes pg_database and tolerates a racing create.
func TestEnsureDatabasesIsIdempotent(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get service -l " + TServerServiceLabels, Result: Result{Stdout: ybaServiceName}},
		{Match: "jsonpath={.spec.selector}", Result: Result{Stdout: tserverSelectorJSON}},
		{Match: "get pods -l", Result: Result{Stdout: "yb-tserver-0"}},
		{Match: "exec", Result: Result{Stdout: "exists\n"}},
	}}
	d, _ := testDeployer(t, rec)
	if err := d.ensureDatabases(context.Background()); err != nil {
		t.Fatalf("ensureDatabases: %v", err)
	}

	var exec Call
	for _, c := range rec.Calls {
		if strings.Contains(strings.Join(c.Argv, " "), " exec ") {
			exec = c
		}
	}
	if exec.Stdin == "" {
		t.Fatalf("no exec call in %v", rec.Argvs())
	}
	got := strings.Join(exec.Argv, " ")
	// The container has to be named: a tserver pod has sidecars.
	if !strings.Contains(got, "-c yb-tserver") {
		t.Errorf("exec argv = %q, want -c yb-tserver", got)
	}
	for _, want := range []string{
		"SELECT 1 FROM pg_database WHERE datname='yb_storage'",
		"CREATE DATABASE yb_storage",
		"already exists",
		// ysqlsh is looked up, not assumed on PATH: on a YBA-managed universe
		// it lives under the tserver's own Postgres install.
		"/home/yugabyte/tserver/postgres/bin/ysqlsh",
	} {
		if !strings.Contains(exec.Stdin, want) {
			t.Errorf("the create script is missing %q:\n%s", want, exec.Stdin)
		}
	}
	// set -e, so a genuine connection failure aborts rather than being misread
	// as "absent" and triggering a spurious create.
	if !strings.Contains(exec.Stdin, "set -euo pipefail") {
		t.Error("the create script does not set -e; a failed probe would look like an absent database")
	}
}

func TestEnsureDatabasesNamesTheSelectorWhenNoPodMatches(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get service -l " + TServerServiceLabels, Result: Result{Stdout: ybaServiceName}},
		{Match: "jsonpath={.spec.selector}", Result: Result{Stdout: tserverSelectorJSON}},
		{Match: "get pods -l", Result: Result{Stdout: ""}},
	}}
	d, _ := testDeployer(t, rec)
	err := d.ensureDatabases(context.Background())
	if err == nil {
		t.Fatal("ensureDatabases succeeded with no tserver pod")
	}
	if !strings.Contains(err.Error(), "app=yb-tserver,release=foundation-dfac") {
		t.Errorf("error %q does not name the selector that failed to match", err)
	}
}

// TestDownOrderAndClusterScopedCleanup pins the teardown order and the objects
// nothing else reclaims.
func TestDownOrderAndClusterScopedCleanup(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		// Down addresses every object by its manifest on stdin, so one stub
		// answers each finalizer poll: the CR is gone on the first one.
		{Match: "get -f -", Result: Result{ExitCode: 1, Stderr: `Error from server (NotFound)`}},
		// Down also sweeps for objects we own that the spec dropped.
		{Match: "api-resources", Result: Result{Stdout: "ybuniverses\nybproviders\n"}},
		{Match: "-l " + ManagedByLabel, Result: Result{Stdout: `{"items":[]}`}},
	}}
	d, spec := testDeployer(t, rec)
	if err := d.Down(context.Background(), DownOptions{}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	argvs := rec.Argvs()

	idx := func(sub string) int {
		for i, a := range argvs {
			if strings.Contains(a, sub) {
				return i
			}
		}
		return -1
	}
	// The universe goes first: the operator deletes the DB before its own RBAC
	// disappears, so uninstalling the release first would strand the pods. The
	// provider goes last of the CRs, after the universe that references it and
	// before the operator holding its finalizer is uninstalled — stranding
	// that finalizer is what leaves a namespace Terminating with no way back.
	//
	// Neither position is coded: teardown walks the spec backwards, and the
	// spec requires YBProvider above YBUniverse.
	if got, want := deletedObjects(t, rec), []string{"YBUniverse/foundation", "YBProvider/foundation"}; !reflect.DeepEqual(got, want) {
		t.Errorf("deleted %v, want %v", got, want)
	}
	if del, uninstall := idx("delete -f -"), idx("uninstall"); del < 0 || uninstall < 0 || del > uninstall {
		t.Errorf("CR deletes must precede the helm uninstall; argv order: %v", argvs)
	}
	// The chart's ClusterRole/ClusterRoleBinding are named after the release and
	// a namespace delete never reclaims a cluster-scoped object.
	for _, want := range []string{
		"delete clusterrolebinding " + spec.HelmRelease(),
		"delete clusterrole " + spec.HelmRelease(),
	} {
		if idx(want) < 0 {
			t.Errorf("teardown never ran %q; that object leaks and collides with the next foundation", want)
		}
	}
	if crb := idx("delete clusterrolebinding"); crb >= 0 && crb < idx("uninstall") {
		t.Error("cluster RBAC was deleted before the helm uninstall that owns it")
	}
	// Both destructive-beyond-this-foundation steps must be off by default.
	if i := idx("delete namespace"); i >= 0 {
		t.Error("Down deleted the namespace by default; it is shared with the consumer's workloads")
	}
	// Not "delete -f", which the CR deletes now use too: the CRDs are
	// addressed by their bundle source.
	if i := idx("delete -f " + d.crdSource()); i >= 0 {
		t.Error("Down deleted the CRDs by default; they are cluster-scoped and shared")
	}
}

// TestDownFailsOnANonZeroDelete is the regression test for a bug this shipped
// with: the deletes checked only the runner error, and Exec deliberately returns
// a non-zero exit as DATA rather than an error. So a permission denial reported
// success and the teardown silently leaked the cluster-scoped objects nothing
// else reclaims — with --ignore-not-found, absence is already a zero exit, so
// anything non-zero is real.
func TestDownFailsOnANonZeroDelete(t *testing.T) {
	for _, tc := range []struct {
		name  string
		match string
		opts  DownOptions
	}{
		{name: "clusterrolebinding", match: "delete clusterrolebinding"},
		{name: "clusterrole", match: "delete clusterrole"},
		{name: "namespace", match: "delete namespace", opts: DownOptions{DeleteNamespace: true}},
		{name: "crds", match: "delete -f", opts: DownOptions{DeleteCRDs: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &Recorder{Responses: []Response{
				{Match: "get -f -", Result: Result{ExitCode: 1, Stderr: "NotFound"}},
				{Match: "get namespace", Result: Result{Stdout: "meko-foundation"}},
				{Match: tc.match, Result: Result{ExitCode: 1, Stderr: "forbidden: User cannot delete"}},
			}}
			d, _ := testDeployer(t, rec)
			err := d.Down(context.Background(), tc.opts)
			if err == nil {
				t.Fatalf("Down reported success despite %q failing", tc.match)
			}
			if !strings.Contains(err.Error(), "forbidden") {
				t.Errorf("error %q does not carry the reason kubectl gave", err)
			}
		})
	}
}

// The namespace guard exists because the default namespace holds the consumer's
// workloads: without it, a Down against a shared namespace deletes the amp
// controller and its endpoints.
func TestDownRefusesAForeignNamespace(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get -f -", Result: Result{ExitCode: 1, Stderr: "NotFound"}},
		{Match: "get namespace", Result: Result{Stdout: ""}}, // no managed-by label
	}}
	d, _ := testDeployer(t, rec)
	err := d.Down(context.Background(), DownOptions{DeleteNamespace: true})
	if err == nil {
		t.Fatal("Down deleted a namespace it does not manage")
	}
	if !strings.Contains(err.Error(), "refusing to delete namespace") {
		t.Errorf("error = %q, want a refusal", err)
	}
	for _, a := range rec.Argvs() {
		if strings.Contains(a, "delete namespace") {
			t.Error("it issued the delete anyway")
		}
	}
}

// A teardown that stops at the first failure leaks whatever came after it, and
// the cluster-scoped objects are last.
func TestDownContinuesPastAFailureAndReportsAll(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get -f -", Result: Result{ExitCode: 1, Stderr: "NotFound"}},
		{Match: "uninstall", Result: Result{ExitCode: 1, Stderr: "release not loaded"}},
	}}
	d, spec := testDeployer(t, rec)
	err := d.Down(context.Background(), DownOptions{})
	if err == nil {
		t.Fatal("Down hid the helm failure")
	}
	if !strings.Contains(err.Error(), "release not loaded") {
		t.Errorf("error %q does not carry the helm failure", err)
	}
	// The cluster-scoped cleanup still ran despite the earlier failure.
	var sawCRB bool
	for _, a := range rec.Argvs() {
		if strings.Contains(a, "delete clusterrolebinding "+spec.HelmRelease()) {
			sawCRB = true
		}
	}
	if !sawCRB {
		t.Error("the cluster-scoped cleanup was skipped after an earlier failure — that is the leak")
	}
}

// TestKubeContextThreadsThrough: CI passes --kube-context per run while the
// spec file stays checked in, so every command must carry it.
func TestKubeContextThreadsThrough(t *testing.T) {
	rec := &Recorder{}
	d, spec := testDeployer(t, rec)
	spec.KubeContext = "gke_yugabyte_us-west1_itest-dev"

	if _, err := d.kubectl(context.Background(), nil, "get", "pods"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.helm(context.Background(), "list"); err != nil {
		t.Fatal(err)
	}
	if got := rec.Argvs()[0]; !strings.Contains(got, "--context gke_yugabyte_us-west1_itest-dev") {
		t.Errorf("kubectl argv = %q", got)
	}
	// helm spells it differently, which is exactly the kind of thing that is
	// silently wrong against the default context.
	if got := rec.Argvs()[1]; !strings.Contains(got, "--kube-context gke_yugabyte_us-west1_itest-dev") {
		t.Errorf("helm argv = %q", got)
	}
}

// TestAddressIsTheChartsService: the address is the Service the chart created in
// the namespace just deployed into — LOOKED UP, not built, because YBA prefixes
// a Namespaced endpoint with the universe name it generated. There is no
// override, because which address a CONSUMER dials is a property of the
// consumer's topology, not of the universe. Every place a consumer is
// configured already takes it (redeploy.sh's env, the install hint's YB=),
// which is where it belongs.
func TestAddressIsTheChartsService(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get ybuniverse", Result: Result{Stdout: readyStatus()}},
		{Match: "get service -l " + TServerServiceLabels, Result: Result{Stdout: ybaServiceName}},
	}}
	d, _ := testDeployer(t, rec)
	info, err := d.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if want := ybaServiceName + ".amp.svc.cluster.local:5433"; info.YSQLHostPorts != want {
		t.Errorf("YSQLHostPorts = %q, want %q", info.YSQLHostPorts, want)
	}
}

// TestCheckTServerServiceWarnsWhenTheChartDidNotRenderIt: the endpoint renders
// nothing if enableLoadBalancer is off or oldNamingStyle is on, and the latter
// is YBA's choice that the CRD gives us no way to force. A missing Service means
// the direct-RPC address resolves to nothing, and the symptom downstream is
// daemons failing at startup with nothing pointing back here — so it warns
// specifically rather than silently.
func TestCheckTServerServiceWarnsWhenTheChartDidNotRenderIt(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get service -l " + TServerServiceLabels, Result: Result{Stdout: ""}},
	}}
	spec := loadExample(t, "amp.yaml")
	var logged []string
	d := New(spec, rec.Runner()).WithLogger(func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })

	if err := d.checkTServerService(context.Background()); err != nil {
		t.Fatalf("checkTServerService: %v", err)
	}
	joined := strings.Join(logged, "\n")
	for _, want := range []string{"oldNamingStyle", "enableLoadBalancer"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the warning does not mention %q, which is what a reader has to check:\n%s", want, joined)
		}
	}
	// It must never create the Service itself — that is the chart's job now.
	for _, c := range rec.Calls {
		if strings.Contains(strings.Join(c.Argv, " "), "apply") {
			t.Errorf("checkTServerService applied something: %v", c.Argv)
		}
	}
}

// A rendered Service with no endpoints means its selector matched no pods, which
// on a first bring-up is just "not yet" but persistently is a broken hot path.
func TestCheckTServerServiceWarnsOnNoEndpoints(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get service -l " + TServerServiceLabels, Result: Result{Stdout: ybaServiceName}},
		{Match: "get service " + ybaServiceName, Result: Result{Stdout: tserverServiceJSON}},
		{Match: "get endpoints", Result: Result{Stdout: ""}},
	}}
	spec := loadExample(t, "amp.yaml")
	var logged []string
	d := New(spec, rec.Runner()).WithLogger(func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })

	if err := d.checkTServerService(context.Background()); err != nil {
		t.Fatalf("checkTServerService: %v", err)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "no endpoints") {
		t.Errorf("no warning about an endpoint-less Service; logged: %v", logged)
	}
}

// TestDownLeavesAProviderTheSpecDoesNotDeclare: a provider is optional — the
// operator has an auto-provider, and a universe can name one somebody else
// registered. Teardown walks what the spec renders, so a provider it never
// declared is simply not in the list. Deleting a shared one would take every
// other universe on the cluster with it.
func TestDownLeavesAProviderTheSpecDoesNotDeclare(t *testing.T) {
	spec, err := Load([]byte("namespace: amp\n" +
		"YBUniverse:\n  name: foundation\n  spec:\n" + testVersion +
		"    providerName: shared-gke\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rec := &Recorder{Responses: []Response{
		{Match: "get -f -", Result: Result{ExitCode: 1, Stderr: "NotFound"}},
		{Match: "api-resources", Result: Result{Stdout: "ybuniverses\nybproviders\n"}},
		{Match: "-l " + ManagedByLabel, Result: Result{Stdout: `{"items":[]}`}},
	}}
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {})
	if err := d.Down(context.Background(), DownOptions{}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if got := deletedObjects(t, rec); !reflect.DeepEqual(got, []string{"YBUniverse/foundation"}) {
		t.Errorf("deleted %v, want only the universe", got)
	}
}

// TestDownDeletesADeclaredProvider: the other half — one this spec created
// goes, after the universe that references it.
func TestDownDeletesADeclaredProvider(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	rec := &Recorder{Responses: []Response{
		{Match: "get -f -", Result: Result{ExitCode: 1, Stderr: "NotFound"}},
		{Match: "api-resources", Result: Result{Stdout: "ybuniverses\nybproviders\n"}},
		{Match: "-l " + ManagedByLabel, Result: Result{Stdout: `{"items":[]}`}},
	}}
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {})
	if err := d.Down(context.Background(), DownOptions{}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if got := deletedObjects(t, rec); !reflect.DeepEqual(got,
		[]string{"YBUniverse/foundation", "YBProvider/foundation"}) {
		t.Errorf("deleted %v, want the universe then the provider", got)
	}
}

// deletedObjects is every object a run deleted, as "Kind/name", in order.
//
// Read off the manifest each delete sent on stdin, because the argv is
// kind-agnostic now: objects are addressed by `-f -` so that nothing has to
// derive a resource name from a kind string.
func deletedObjects(t *testing.T, rec *Recorder) []string {
	t.Helper()
	var out []string
	for _, c := range rec.Calls {
		if !strings.Contains(strings.Join(c.Argv, " "), "delete -f -") {
			continue
		}
		var cr struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal([]byte(c.Stdin), &cr); err != nil {
			t.Fatalf("a delete sent something that is not a manifest: %v", err)
		}
		out = append(out, cr.Kind+"/"+cr.Metadata.Name)
	}
	return out
}

// TestInstallYBAFailsWhenYBAHasNoCustomer: the operator refuses every CR
// without exactly one, and the chart creates it from a POST-INSTALL hook — so a
// release that already exists without one never gets one, helm reports success
// every time, and the CR sits with an empty status while only the yugaware log
// says why. Left unchecked, our own wait burns the full universe timeout and
// reports nothing useful.
func TestInstallYBAFailsWhenYBAHasNoCustomer(t *testing.T) {
	for _, tc := range []struct {
		name, count, want string
	}{
		{"none", `{"count":0}`, "post-install hook"},
		{"two", `{"count":2}`, "exactly one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &Recorder{Responses: []Response{
				{Match: "get statefulset", Result: Result{Stdout: "ci-yugaware"}},
				{Match: "get pods", Result: Result{Stdout: "ci-yugaware-0"}},
				{Match: "customer_count", Result: Result{Stdout: tc.count}},
			}}
			d, _ := testDeployer(t, rec)
			err := d.installYBA(context.Background())
			if err == nil {
				t.Fatalf("installYBA succeeded with %s", tc.count)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
		})
	}
}

// TestInstallYBAAcceptsExactlyOneCustomer is the other half: the check must not
// fail a healthy install, and an unreadable probe is inconclusive rather than
// fatal — a check we cannot run should not break a deploy.
func TestInstallYBAAcceptsExactlyOneCustomer(t *testing.T) {
	for _, probe := range []Result{
		{Stdout: `{"count":1}`},
		{ExitCode: 1, Stderr: "error: unable to upgrade connection"},
		{Stdout: "<html>not json</html>"},
	} {
		rec := &Recorder{Responses: []Response{
			{Match: "get statefulset", Result: Result{Stdout: "ci-yugaware"}},
			{Match: "get pods", Result: Result{Stdout: "ci-yugaware-0"}},
			{Match: "customer_count", Result: probe},
		}}
		d, _ := testDeployer(t, rec)
		if err := d.installYBA(context.Background()); err != nil {
			t.Errorf("installYBA failed on probe %+v: %v", probe, err)
		}
	}
}

// TestHelmSetsForcesTheCustomerCreateJob: defaultUser.enabled gates the chart's
// customer-create job and defaults OFF, so setting the defaultUser fields alone
// does nothing. Forced with --set so a values file cannot switch it off.
func TestHelmSetsForcesTheCustomerCreateJob(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	if got := spec.HelmSets()["yugaware.defaultUser.enabled"]; got != "true" {
		t.Errorf("yugaware.defaultUser.enabled = %q, want \"true\"; without it YBA comes up with "+
			"zero customers and the operator refuses every CR", got)
	}
}

// TestPreflightFailsWithoutADefaultUserPassword: the chart's job template
// hard-fails on an empty password, but from inside helm tens of minutes in.
func TestPreflightFailsWithoutADefaultUserPassword(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	yw, _ := spec.YBA.Values["yugaware"].(map[string]any)
	user, _ := yw["defaultUser"].(map[string]any)
	delete(user, "password")

	rec := &Recorder{}
	d := New(spec, rec.Runner()).WithLogger(func(string, ...any) {})
	err := d.Preflight(context.Background())
	if err == nil {
		t.Fatal("Preflight passed with no defaultUser password")
	}
	if !strings.Contains(err.Error(), "exactly one customer") {
		t.Errorf("error %q does not say why the password matters", err)
	}
}

// TestYugawareLookupsUseALabelTheChartActuallySets is a regression test for a
// silent failure, not a style check.
//
// The yugaware chart sets only the old Helm labels — on the StatefulSet,
// `app: <fullname>`, `chart`, `heritage`, `release: <release>` — and NO
// app.kubernetes.io/* label at all. A selector naming one matches nothing, and
// `kubectl get -l` answers an empty list rather than an error, so the lookups
// built on it reported "found nothing, skipping" on every real run: the rollout
// check never ran, and neither did the customer check that exists to stop a
// bring-up burning its whole timeout on CRs the operator will not touch.
func TestYugawareLookupsUseALabelTheChartActuallySets(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get statefulset", Result: Result{Stdout: "ci-yugaware-yugaware"}},
		{Match: "customer_count", Result: Result{Stdout: `{"count":1}`}},
	}}
	d, spec := testDeployer(t, rec)
	if err := d.installYBA(context.Background()); err != nil {
		t.Fatalf("installYBA: %v", err)
	}
	for _, argv := range rec.Argvs() {
		if !strings.Contains(argv, "-l ") {
			continue
		}
		if strings.Contains(argv, "app.kubernetes.io/") {
			t.Errorf("selector uses a recommended label the yugaware chart does not set, so it "+
				"matches nothing and the check silently passes: %s", argv)
		}
		if !strings.Contains(argv, "release="+spec.HelmRelease()) {
			t.Errorf("selector is not the chart's `release` label: %s", argv)
		}
	}
	// The pod template carries only `app: <fullname>` — no `release` — so the
	// customer check must address the workload, not reconstruct a pod selector.
	var execd string
	for _, argv := range rec.Argvs() {
		if strings.Contains(argv, "exec") {
			execd = argv
		}
	}
	if !strings.Contains(execd, "exec statefulset/ci-yugaware-yugaware") {
		t.Errorf("the customer check does not exec on the StatefulSet: %q", execd)
	}
}

// TestCustomerCheckFailsWhenItCannotRun: unlike the rollout check, "I could not
// find yugaware" must not pass. It is the only thing between a customer-less
// YBA and a bring-up that times out on CRs whose status is never written.
func TestCustomerCheckFailsWhenItCannotRun(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get statefulset", Result: Result{Stdout: ""}},
	}}
	d, _ := testDeployer(t, rec)
	err := d.checkYBACustomer(context.Background())
	if err == nil {
		t.Fatal("checkYBACustomer passed without finding yugaware at all")
	}
	if !strings.Contains(err.Error(), "cannot be verified") {
		t.Errorf("error %q does not say the check could not run", err)
	}
}

// TestDSNMatchesTheRenderedUniverse. The DSN's sslmode and the CR's
// enableClientToNodeEncrypt have to come from one place: `disable` against a
// TLS universe gives `no pg_hba.conf entry ... no encryption`, and `require`
// against a plaintext one fails just as hard. An earlier version read the raw
// spec while the defaults filled the field in, so a spec that omitted it
// rendered a plaintext universe and a require DSN.
func TestDSNMatchesTheRenderedUniverse(t *testing.T) {
	for _, tc := range []struct {
		name string
		tls  any
	}{
		{"off, as the spec sets it", false},
		{"on", true},
		{"absent, so universeDefaults decides", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &Recorder{Responses: []Response{
				{Match: "get ybuniverse", Result: Result{Stdout: readyStatus()}},
				{Match: "get service -l " + TServerServiceLabels, Result: Result{Stdout: ybaServiceName}},
			}}
			d, spec := testDeployer(t, rec)
			if tc.tls == nil {
				delete(spec.universe(), "enableClientToNodeEncrypt")
			} else {
				spec.universe()["enableClientToNodeEncrypt"] = tc.tls
			}
			info, err := d.Connect(context.Background())
			if err != nil {
				t.Fatalf("Connect: %v", err)
			}
			// The assertion is the agreement, not a fixed string: whatever the
			// CR ends up carrying is what the DSN has to say.
			rendered, _ := spec.universe()["enableClientToNodeEncrypt"].(bool)
			want := "sslmode=disable"
			if rendered {
				want = "sslmode=require"
			}
			if !strings.Contains(info.DBURL, want) {
				t.Errorf("the CR renders enableClientToNodeEncrypt=%v but the DSN is %q, want %s",
					rendered, info.DBURL, want)
			}
		})
	}
}

// TestTLSIsOffByDefaultOnALoadedSpec: both flags are create-time only in the
// CRD and default to TRUE there, so a spec that omits them must still come out
// with TLS off — otherwise the universe has to be rebuilt to undo it, and
// node-to-node in particular breaks the compute hot path, which speaks pggate
// to tserver 9100 with no cert material available.
//
// Loaded rather than poked, because the defaults are applied at parse time now:
// the universe body is the rendered body, which is what stops a reader of the Spec
// disagreeing with the CR.
func TestTLSIsOffByDefaultOnALoadedSpec(t *testing.T) {
	spec, err := Load([]byte(minimalSpec("")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, key := range []string{"enableNodeToNodeEncrypt", "enableClientToNodeEncrypt"} {
		if spec.universe()[key] != false {
			t.Errorf("%s = %v, want false written explicitly (the CRD defaults it true)",
				key, spec.universe()[key])
		}
	}
	// A spec that asks for TLS keeps it.
	withTLS, err := Load([]byte(minimalSpec("    enableClientToNodeEncrypt: true\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if withTLS.universe()["enableClientToNodeEncrypt"] != true {
		t.Errorf("enableClientToNodeEncrypt = %v, want the spec's own true",
			withTLS.universe()["enableClientToNodeEncrypt"])
	}
}

// TestInstallYBAFailsWhenYBALacksTheDBRelease. YBA needs a helm PACKAGE for the
// release, not just the version string, and checks that only at universe-create
// time — asynchronously, with the reason in its own log and nothing but
// "Error Creating" on the CR.
//
// This is not a check on our own input. The releases a yugaware carries come
// from its image, pinned by the chart's appVersion, and a published chart
// version is MUTABLE: 2026.1.1 was re-cut with appVersion 2026.1.1.1-b2 ->
// 2026.1.1.2-b9, and a spec green for days began failing with nothing in it
// changed.
func TestInstallYBAFailsWhenYBALacksTheDBRelease(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get statefulset", Result: Result{Stdout: "ci-yugaware-yugaware"}},
		{Match: "ls /opt/yugabyte/releases", Result: Result{Stdout: "2026.1.1.9-b99\n"}},
	}}
	d, _ := testDeployer(t, rec)
	err := d.checkDBRelease(context.Background())
	if err == nil {
		t.Fatal("the check passed against a YBA without the spec's DB release")
	}
	// Both versions and the cause, or the reader cannot tell drift from a typo.
	for _, want := range []string{"2026.1.1.9-b99", "2026.1.2.0-b137", "MUTABLE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q", err, want)
		}
	}
}

// TestDBReleaseCheckRunsBeforeTheUniverseApply: it used to be the last step of
// installYBA, which is before any CR is applied — and a Release entry exists
// precisely to import a release YBA does not have, so the check would have
// rejected the thing that fixes it. It now runs as late as it can, immediately
// before the universe asks for the release.
func TestDBReleaseCheckRunsBeforeTheUniverseApply(t *testing.T) {
	spec := loadExample(t, "resources.yaml")
	rec := &Recorder{Responses: []Response{
		{Match: "get ybprovider", Result: Result{Stdout: `{"status":{"state":"Ready"}}`}},
		{Match: "get statefulset", Result: Result{Stdout: "ci-yugaware-yugaware"}},
		{Match: "ls /opt/yugabyte/releases", Result: Result{Stdout: "2026.1.1.2-b9\n"}},
	}}
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {})
	if err := d.applyCRs(context.Background()); err != nil {
		t.Fatalf("applyCRs: %v", err)
	}
	check, universeApply, releaseApply := -1, -1, -1
	for i, c := range rec.Calls {
		joined := strings.Join(c.Argv, " ")
		if strings.Contains(joined, "ls /opt/yugabyte/releases") {
			check = i
		}
		if !strings.Contains(joined, "apply --server-side") {
			continue
		}
		if strings.Contains(c.Stdin, `"kind": "YBUniverse"`) {
			universeApply = i
		}
		if strings.Contains(c.Stdin, `"kind": "Release"`) {
			releaseApply = i
		}
	}
	if check < 0 || universeApply < 0 || releaseApply < 0 {
		t.Fatalf("missing a step: check=%d universe=%d release=%d", check, universeApply, releaseApply)
	}
	if !(releaseApply < check && check < universeApply) {
		t.Errorf("the release check must fall between the Release apply and the universe apply; "+
			"release=%d check=%d universe=%d", releaseApply, check, universeApply)
	}
}

// TestDBReleaseCheckWarnsWhenTheSpecImportsARelease: the probe lists what the
// yugaware IMAGE bundles, while a Release CR registers one in YBA's own
// metadata from a URL — so once a spec declares one, a mismatch here is no
// longer definitive and failing on it would reject a correct spec.
func TestDBReleaseCheckWarnsWhenTheSpecImportsARelease(t *testing.T) {
	spec := loadExample(t, "resources.yaml")
	if !spec.DeclaresRelease() {
		t.Fatal("the fixture no longer declares a Release")
	}
	rec := &Recorder{Responses: []Response{
		{Match: "get statefulset", Result: Result{Stdout: "ci-yugaware-yugaware"}},
		{Match: "ls /opt/yugabyte/releases", Result: Result{Stdout: "2026.1.1.9-b99\n"}},
	}}
	var logged []string
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, time.Second, time.Millisecond).
		WithLogger(func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })
	if err := d.checkDBRelease(context.Background()); err != nil {
		t.Fatalf("the check failed a spec that imports its own release: %v", err)
	}
	joined := strings.Join(logged, "\n")
	if !strings.Contains(joined, "warning") || !strings.Contains(joined, "2026.1.1.9-b99") {
		t.Errorf("no warning naming what YBA actually has; logged: %v", logged)
	}
}

// TestDBReleaseCheckStillFailsWithoutAReleaseEntry: the half that must not be
// weakened. This is the case that caught chart 2026.1.1 being re-cut with a
// new appVersion, which moved the bundled release underneath a spec that had
// been green for days.
func TestDBReleaseCheckStillFailsWithoutAReleaseEntry(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	if spec.DeclaresRelease() {
		t.Fatal("the fixture should not declare a Release")
	}
	rec := &Recorder{Responses: []Response{
		{Match: "get statefulset", Result: Result{Stdout: "ci-yugaware-yugaware"}},
		{Match: "ls /opt/yugabyte/releases", Result: Result{Stdout: "2026.1.1.9-b99\n"}},
	}}
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {})
	if err := d.checkDBRelease(context.Background()); err == nil {
		t.Fatal("drift under a spec that imports nothing must still fail the deploy")
	}
}

// The matching release passes, and an unreadable probe is inconclusive rather
// than fatal — a check we cannot run must not break a deploy.
func TestDBReleaseCheckIsInconclusiveWhenItCannotRun(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	version := universeString(spec.universe(), "ybSoftwareVersion")
	for _, probe := range []Result{
		{Stdout: "some-other\n" + version + "\n"},
		{ExitCode: 1, Stderr: "error: unable to upgrade connection"},
	} {
		rec := &Recorder{Responses: []Response{
			{Match: "get statefulset", Result: Result{Stdout: "ci-yugaware-yugaware"}},
			{Match: "customer_count", Result: Result{Stdout: `{"count":1}`}},
			{Match: "ls /opt/yugabyte/releases", Result: probe},
		}}
		d := New(spec, rec.Runner()).WithTimeouts(time.Second, time.Second, time.Millisecond).
			WithLogger(func(string, ...any) {})
		if err := d.installYBA(context.Background()); err != nil {
			t.Errorf("installYBA failed on probe %+v: %v", probe, err)
		}
	}
}

// TestApplyAppliesEveryDeclaredKindInOrder: the apply loop is kind-agnostic,
// so a spec naming a Release and two StorageConfigs applies all of them, in
// the order the file wrote them.
func TestApplyAppliesEveryDeclaredKindInOrder(t *testing.T) {
	spec := loadExample(t, "resources.yaml")
	rec := &Recorder{Responses: []Response{
		{Match: "get ybprovider", Result: Result{Stdout: `{"status":{"state":"Ready"}}`}},
	}}
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {})
	if err := d.applyCRs(context.Background()); err != nil {
		t.Fatalf("applyCRs: %v", err)
	}
	var applied []string
	for _, c := range rec.Calls {
		if !strings.Contains(strings.Join(c.Argv, " "), "apply --server-side") {
			continue
		}
		var cr struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal([]byte(c.Stdin), &cr); err != nil {
			t.Fatalf("an apply sent something that is not a manifest: %v", err)
		}
		applied = append(applied, cr.Kind+"/"+cr.Metadata.Name)
	}
	want := []string{
		"YBProvider/foundation",
		"Release/meko-db",
		"StorageConfig/backups",
		"StorageConfig/archive",
		"KMSConfig/foundation-kms",
		"YBUniverse/foundation",
	}
	if !reflect.DeepEqual(applied, want) {
		t.Errorf("applied %v, want %v", applied, want)
	}
}

// TestDownIsTheSpecReadBackwards: teardown reverses the spec, and no kind name
// appears in Down at all. Moving a key in the file moves it in the teardown.
func TestDownIsTheSpecReadBackwards(t *testing.T) {
	spec := loadExample(t, "resources.yaml")
	rec := &Recorder{Responses: []Response{
		{Match: "get -f -", Result: Result{ExitCode: 1, Stderr: "NotFound"}},
		{Match: "api-resources", Result: Result{Stdout: "ybuniverses\nybproviders\n"}},
		{Match: "-l " + ManagedByLabel, Result: Result{Stdout: `{"items":[]}`}},
	}}
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {})
	if err := d.Down(context.Background(), DownOptions{}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	want := []string{
		"YBUniverse/foundation",
		"KMSConfig/foundation-kms",
		"StorageConfig/archive",
		"StorageConfig/backups",
		"Release/meko-db",
		"YBProvider/foundation",
	}
	if got := deletedObjects(t, rec); !reflect.DeepEqual(got, want) {
		t.Errorf("deleted %v, want the spec read backwards %v", got, want)
	}
}

// TestDownAttemptsEveryCRPastAFailedDelete: one failed delete must not stop
// the others being attempted — that half of the accumulate-all contract still
// holds, and dropping it would leave objects nobody even asked to remove.
//
// What it no longer does is carry on to the uninstall: a refused delete means
// the object is still there holding a finalizer, and removing the operator
// then makes it unclearable. TestDownKeepsTheReleaseWhenACRIsStuck covers
// that half.
func TestDownAttemptsEveryCRPastAFailedDelete(t *testing.T) {
	spec := loadExample(t, "resources.yaml")
	rec := &Recorder{Responses: []Response{
		{Match: "delete -f -", Once: true, Result: Result{ExitCode: 1, Stderr: "webhook denied the request"}},
		{Match: "get -f -", Result: Result{ExitCode: 1, Stderr: "NotFound"}},
		{Match: "api-resources", Result: Result{Stdout: "ybuniverses\nybproviders\n"}},
		{Match: "-l " + ManagedByLabel, Result: Result{Stdout: `{"items":[]}`}},
	}}
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {})
	err := d.Down(context.Background(), DownOptions{})
	if err == nil {
		t.Fatal("Down hid a failed delete")
	}
	if !strings.Contains(err.Error(), "webhook denied") {
		t.Errorf("error %q does not carry the reason", err)
	}
	// Every CR was still attempted, not just the ones before the failure.
	if got, want := deletedObjects(t, rec), len(rendered(t, spec)); len(got) != want {
		t.Errorf("attempted %v, want all %d", got, want)
	}
}

// TestDownRefusesToUninstallWhenItCannotRenderIsNotDestructive: a render
// failure used to be collected and the teardown carried on — deleting nothing,
// then uninstalling the release and its cluster RBAC, which takes away the
// operator holding every CR's finalizer. The namespace is then permanently
// Terminating with nothing left that can clear it.
func TestDownRefusesToUninstallWhenItCannotRender(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	// A body JSON cannot marshal: a non-string map key.
	spec.universe()["gFlags"] = map[any]any{1: "x"}
	rec := &Recorder{}
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {})

	if err := d.Down(context.Background(), DownOptions{}); err == nil {
		t.Fatal("Down hid a render failure")
	}
	for _, a := range rec.Argvs() {
		if strings.Contains(a, "uninstall") || strings.Contains(a, "delete clusterrole") {
			t.Fatalf("Down removed the operator without deleting the CRs first: %q", a)
		}
	}
}

// TestDownReclaimsALabelledStrayTheSpecDropped: teardown cannot delete only
// what the CURRENT spec renders. Remove a key without pruning — `--no-prune`,
// or a prune that failed — and the object is still on the cluster; the helm
// uninstall would then take the operator holding its finalizer and strand the
// namespace.
func TestDownReclaimsALabelledStrayTheSpecDropped(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	rec := &Recorder{Responses: []Response{
		{Match: "get -f -", Result: Result{ExitCode: 1, Stderr: "NotFound"}},
		{Match: "api-resources", Result: Result{Stdout: "ybuniverses\nybproviders\nkmsconfigs\n"}},
		{Match: "-l " + ManagedByLabel, Result: Result{Stdout: ownedList(
			"YBProvider/foundation", "YBUniverse/foundation", "KMSConfig/dropped")}},
	}}
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {})
	if err := d.Down(context.Background(), DownOptions{}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	want := []string{
		"YBUniverse/foundation",
		"YBProvider/foundation",
		"KMSConfig/dropped", // the stray, after everything the spec declares
	}
	if got := deletedObjects(t, rec); !reflect.DeepEqual(got, want) {
		t.Errorf("deleted %v, want %v", got, want)
	}
}

// TestDownDeletesEachRenderedObjectOnce: the stray sweep must not re-delete
// what the reversed spec walk already handled.
func TestDownDeletesEachRenderedObjectOnce(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	rec := &Recorder{Responses: []Response{
		{Match: "get -f -", Result: Result{ExitCode: 1, Stderr: "NotFound"}},
		{Match: "api-resources", Result: Result{Stdout: "ybuniverses\nybproviders\n"}},
		{Match: "-l " + ManagedByLabel, Result: Result{Stdout: ownedList(
			"YBProvider/foundation", "YBUniverse/foundation")}},
	}}
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {})
	if err := d.Down(context.Background(), DownOptions{}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if got := deletedObjects(t, rec); len(got) != 2 {
		t.Errorf("deleted %v, want each object once", got)
	}
}

// TestDownIssuesEveryStrayDeleteBeforeWaiting: the strays are gone from the
// spec, so there is no declared order left and two of them can still
// reference each other. Waiting on one at a time blocks on a live reference
// for the whole FinalizerTimeout and then uninstalls the operator with the
// pair still there — the same reason prune issues all of its deletes first.
func TestDownIssuesEveryStrayDeleteBeforeWaiting(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	rec := &Recorder{Responses: []Response{
		{Match: "get -f -", Result: Result{ExitCode: 1, Stderr: "NotFound"}},
		{Match: "api-resources", Result: Result{Stdout: "ybuniverses\nybproviders\nstorageconfigs\nbackupschedules\n"}},
		{Match: "-l " + ManagedByLabel, Result: Result{Stdout: ownedList(
			"YBProvider/foundation", "YBUniverse/foundation",
			"StorageConfig/backups", "BackupSchedule/nightly")}},
	}}
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {})
	if err := d.Down(context.Background(), DownOptions{}); err != nil {
		t.Fatalf("Down: %v", err)
	}

	// The labelled query is the boundary: everything before it is the
	// declared walk, everything after is the stray sweep. Within the sweep,
	// every delete must precede every wait.
	boundary := -1
	for i, c := range rec.Calls {
		if strings.Contains(strings.Join(c.Argv, " "), "-l "+ManagedByLabel) {
			boundary = i
		}
	}
	if boundary < 0 {
		t.Fatalf("Down never ran the labelled query; argv: %v", rec.Argvs())
	}
	var strayDeletes, strayWaits []int
	for i, c := range rec.Calls {
		if i <= boundary {
			continue
		}
		joined := strings.Join(c.Argv, " ")
		if strings.Contains(joined, "delete -f -") {
			strayDeletes = append(strayDeletes, i)
		}
		if strings.Contains(joined, "get -f -") {
			strayWaits = append(strayWaits, i)
		}
	}
	if len(strayDeletes) != 2 || len(strayWaits) == 0 {
		t.Fatalf("expected two stray deletes and some waits; argv: %v", rec.Argvs())
	}
	if strayWaits[0] < strayDeletes[len(strayDeletes)-1] {
		t.Errorf("a stray was waited on before the others were asked for; argv: %v", rec.Argvs())
	}
}

// TestDownKeepsTheReleaseWhenACRIsStuck: a CR that did not go still holds a
// finalizer only the operator can clear, so uninstalling the release is what
// turns a failed teardown into an unrecoverable namespace — nothing is left
// that can ever release it.
//
// Leaving the release is recoverable by comparison: the operator keeps
// working, and `down` is idempotent. Errors are otherwise collected so a
// partial failure does not strand the release; this is the one case where
// stranding it is the better outcome.
func TestDownKeepsTheReleaseWhenACRIsStuck(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	rec := &Recorder{Responses: []Response{
		// Never goes: the finalizer is stuck.
		{Match: "get -f -", Result: Result{Stdout: "ybuniverse.operator.yugabyte.io/foundation"}},
		{Match: "api-resources", Result: Result{Stdout: "ybuniverses\nybproviders\n"}},
		{Match: "-l " + ManagedByLabel, Result: Result{Stdout: `{"items":[]}`}},
	}}
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {})

	err := d.Down(context.Background(), DownOptions{FinalizerTimeout: 10 * time.Millisecond})
	if err == nil {
		t.Fatal("Down reported success with a CR still holding its finalizer")
	}
	if !strings.Contains(err.Error(), "left the helm release") {
		t.Errorf("error %q does not say the release was left in place", err)
	}
	for _, a := range rec.Argvs() {
		for _, destructive := range []string{"uninstall", "delete clusterrole", "delete namespace"} {
			if strings.Contains(a, destructive) {
				t.Errorf("Down ran %q while a CR still holds its finalizer; "+
					"that leaves nothing able to clear it", a)
			}
		}
	}
}

// TestDownStillUninstallsWhenEveryCRWent: the guard above must not fire on a
// clean teardown, or `down` would stop reclaiming the release at all.
func TestDownStillUninstallsWhenEveryCRWent(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	rec := &Recorder{Responses: []Response{
		{Match: "get -f -", Result: Result{ExitCode: 1, Stderr: "NotFound"}},
		{Match: "api-resources", Result: Result{Stdout: "ybuniverses\nybproviders\n"}},
		{Match: "-l " + ManagedByLabel, Result: Result{Stdout: `{"items":[]}`}},
	}}
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {})
	if err := d.Down(context.Background(), DownOptions{}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	var uninstalled bool
	for _, a := range rec.Argvs() {
		if strings.Contains(a, "uninstall") {
			uninstalled = true
		}
	}
	if !uninstalled {
		t.Error("a clean teardown did not uninstall the release")
	}
}
