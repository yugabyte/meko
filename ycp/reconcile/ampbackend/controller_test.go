package ampbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"meko/ycp/internal/api"
	"meko/ycp/reconcile"
)

const (
	ctlRelease       = "yb-amp-controller"
	ctlNamespace     = "amp"
	ctlChart         = "oci://quay.io/yugabyte/charts/yb-amp-controller"
	ctlVersionTarget = "0.260902.0-def5678"
	ctlChartVersion  = "0.260902.1"
)

// fakeHelm stands in for the helm CLI. It answers `get values` / `get metadata`
// from its own state and records every invocation, so the tests can assert on the
// exact argv — which is the whole interface to helm, and where the --wait hazard
// lives.
type fakeHelm struct {
	values       map[string]any
	chartVersion string
	err          error
	calls        [][]string
	// status is what `helm status` reports; empty means deployed. stdins records
	// what each call was handed, so a test can assert the values document an
	// upgrade sent rather than only its argv.
	status string
	stdins [][]byte
}

func (f *fakeHelm) Run(_ context.Context, stdin []byte, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	f.stdins = append(f.stdins, stdin)
	if f.err != nil {
		return nil, f.err
	}
	switch {
	case args[0] == "status":
		st := f.status
		if st == "" {
			st = "deployed"
		}
		return []byte(`{"info":{"status":"` + st + `","description":"fixture"}}`), nil
	case len(args) > 1 && args[0] == "get" && args[1] == "values":
		return json.Marshal(f.values)
	case len(args) > 1 && args[0] == "get" && args[1] == "metadata":
		return []byte(`{"version":"` + f.chartVersion + `"}`), nil
	case args[0] == "upgrade":
		f.applyUpgrade(args)
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected helm invocation %v", args)
}

// upgradeStdin is the values document the Nth upgrade was handed.
func (f *fakeHelm) upgradeStdin(n int) []byte {
	seen := 0
	for i, c := range f.calls {
		if c[0] != "upgrade" {
			continue
		}
		if seen == n {
			return f.stdins[i]
		}
		seen++
	}
	return nil
}

// applyUpgrade folds an upgrade's overrides into the deployed values the way helm
// does, so a second tick reads back what the first one asked for — the loop the
// mid-rollout hazard is about. Two details are the point of the model rather than
// decoration: a --set key is a PATH into the values tree, and the re-sent values MERGE
// an override into what the release already carries instead of replacing it. A fake
// that stored either flat would hide a comparison that can never match.
func (f *fakeHelm) applyUpgrade(args []string) {
	if f.values == nil {
		f.values = map[string]any{}
	}
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "--set":
			if k, v, ok := strings.Cut(args[i+1], "="); ok {
				mergeInto(f.values, strings.Split(k, "."), v)
			}
		case "--set-json":
			if k, v, ok := strings.Cut(args[i+1], "="); ok {
				var val any
				if json.Unmarshal([]byte(v), &val) == nil {
					mergeInto(f.values, strings.Split(k, "."), val)
				}
			}
		case "--version":
			f.chartVersion = args[i+1]
		}
	}
}

func mergeInto(dst map[string]any, path []string, val any) {
	for _, seg := range path[:len(path)-1] {
		next, _ := dst[seg].(map[string]any)
		if next == nil {
			next = map[string]any{}
			dst[seg] = next
		}
		dst = next
	}
	leaf := path[len(path)-1]
	if m, ok := val.(map[string]any); ok {
		if into, ok := dst[leaf].(map[string]any); ok {
			for k, v := range m {
				mergeInto(into, []string{k}, v)
			}
			return
		}
	}
	dst[leaf] = val
}

func (f *fakeHelm) upgrades() [][]string {
	var out [][]string
	for _, c := range f.calls {
		if c[0] == "upgrade" {
			out = append(out, c)
		}
	}
	return out
}

func ctlRes(gen int64, spec string) api.DesiredResource {
	return api.DesiredResource{Kind: KindController, Name: controllerName, Generation: gen,
		Spec: json.RawMessage(spec)}
}

func ctlSpec(version string) string {
	return `{"version":"` + version + `","chart_version":"` + ctlChartVersion + `"}`
}

// deployedTag is the tag the release carries, and so the one helm would have put
// on the pod template.
func (h *fakeHelm) deployedTag() string {
	if h == nil {
		return ""
	}
	ctl, _ := h.values["controller"].(map[string]any)
	img, _ := ctl["image"].(map[string]any)
	tag, _ := img["tag"].(string)
	return tag
}

// deployedAt is the release as helm reports it once it runs version: the per-image
// tags every real install pins, not the chart-global imageTag fallback.
func deployedAt(version string) *fakeHelm {
	return &fakeHelm{
		values: map[string]any{
			// All three tags a controller release moves, including the endpoint image
			// — that tag IS the pin the fleet is rolled onto, so a release deployed at
			// a version carries it like the other two.
			"controller": map[string]any{
				"image":    map[string]any{"tag": version},
				"endpoint": map[string]any{"image": map[string]any{"tag": version}},
			},
			"agent": map[string]any{"image": map[string]any{"tag": version}, "enabled": true},
		},
		chartVersion: ctlChartVersion,
	}
}

// The release already carries what the spec declares and the controller reports
// it. Nothing is re-run: returning nil is what writes the engine's last-applied
// cache, and re-running helm on a converged release bumps the revision for
// nothing — the tick that lands mid-rollout is exactly when that happens.
func TestControllerUpdateConvergedRunsNoUpgrade(t *testing.T) {
	f := &ctlServer{helm: deployedAt(ctlVersionTarget), ctlVersion: ctlVersionTarget}
	b := f.start(t)

	if err := b.Update(t.Context(), ctlRes(1, ctlSpec(ctlVersionTarget))); err != nil {
		t.Fatalf("Update on a converged controller = %v, want nil", err)
	}
	if got := f.helm.upgrades(); len(got) != 0 {
		t.Errorf("helm upgrades = %v, want none", got)
	}
}

// The declared version is not the deployed one, so the release moves. The argv is
// the assertion: --wait is absent deliberately — this agent is a member of the
// release it is upgrading, so its own rollout kills the helm client mid-wait and
// leaves the release pending-upgrade, blocking every later upgrade.
func TestControllerUpdateIssuesOneUpgradeWithoutWait(t *testing.T) {
	f := &ctlServer{helm: deployedAt("0.260801.0-old"), ctlVersion: "0.260801.0-old"}
	b := f.start(t)

	err := b.Update(t.Context(), ctlRes(1, `{"version":"`+ctlVersionTarget+`","chart_version":"`+ctlChartVersion+
		`","values":{"imagePullSecrets":[{"name":"quay"}]}}`))
	mustPending(t, err)

	ups := f.helm.upgrades()
	if len(ups) != 1 {
		t.Fatalf("helm upgrades = %v, want exactly 1", ups)
	}
	argv := strings.Join(ups[0], " ")
	for _, want := range []string{
		"upgrade " + ctlRelease + " " + ctlChart,
		"--namespace " + ctlNamespace,
		"--version " + ctlChartVersion,
		"-f", "-",
		"--set controller.image.tag=" + ctlVersionTarget,
		"--set agent.image.tag=" + ctlVersionTarget,
		`--set-json imagePullSecrets=[{"name":"quay"}]`,
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("helm argv %q is missing %q", argv, want)
		}
	}
	if strings.Contains(argv, "--wait") {
		t.Errorf("helm argv %q passes --wait: the agent's own rollout would kill the client and wedge the release", argv)
	}
	// The chart-global imageTag is only the fallback for an image with no tag of
	// its own, so on an install that pins one it moves nothing — and it is also the
	// fallback for the endpoint image, which this kind deliberately does not move.
	if strings.Contains(argv, "--set imageTag=") {
		t.Errorf("helm argv %q sets the chart-global imageTag instead of the per-image tags", argv)
	}

	// The next tick finds its own upgrade deployed and waits for the rollout
	// instead of bumping the revision again.
	f.ctlRolling = true
	mustPending(t, b.Update(t.Context(), ctlRes(1, ctlSpec(ctlVersionTarget))))
	if got := f.helm.upgrades(); len(got) != 1 {
		t.Errorf("helm upgrades after a mid-rollout tick = %d, want still 1", len(got))
	}
}

// A chart version change with the same image tag still moves the release: the
// chart is where the pod spec lives, so a values-only comparison would leave it
// behind on a chart-only release.
func TestControllerUpdateFollowsTheChartVersion(t *testing.T) {
	f := &ctlServer{helm: deployedAt(ctlVersionTarget), ctlVersion: ctlVersionTarget}
	f.helm.chartVersion = "0.260714.4"
	b := f.start(t)

	mustPending(t, b.Update(t.Context(), ctlRes(1, ctlSpec(ctlVersionTarget))))
	if got := f.helm.upgrades(); len(got) != 1 {
		t.Fatalf("helm upgrades = %v, want 1 for the chart version change", got)
	}
}

// The upgrade restarts the controller, so /version is unreachable at exactly the
// moment this kind is converging. It must never fail List: a List error aborts
// the tick for every resource in the environment, freezing all reconciliation for
// the length of every controller upgrade.
func TestControllerListDegradesWhenVersionIsUnreachable(t *testing.T) {
	f := &ctlServer{helm: deployedAt(ctlVersionTarget)} // ctlVersion "" => /version 503
	b := f.start(t)

	actuals, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("List with an unreachable controller = %v, want it to degrade rather than fail the tick", err)
	}
	var row reconcile.Actual
	var found bool
	for _, a := range actuals {
		if a.Key.Kind == KindController {
			row, found = a, true
		}
		if a.DiscoveryError != nil {
			t.Errorf("row %s reports a discovery error, but only /version failed: an unreachable "+
				"controller must cost its own readiness, not the tree's", a.Key)
		}
	}
	if !found {
		t.Fatal("no controller row: Parent gating reads the actual map, so a missing row defers anything gated on it forever")
	}
	if row.Ready || row.State != controllerStateUpgrading {
		t.Errorf("controller row = {Ready:%v State:%q}, want not-ready in %q", row.Ready, row.State, controllerStateUpgrading)
	}
}

// The controller is the release this agent runs in; withdrawing the declaration
// means stopping managing its version, not deleting anything.
func TestControllerDeleteIsPermanent(t *testing.T) {
	f := &ctlServer{helm: deployedAt(ctlVersionTarget), ctlVersion: ctlVersionTarget}
	b := f.start(t)

	err := b.Delete(t.Context(), reconcile.ResourceKey{Kind: KindController, Name: controllerName})
	var perm *reconcile.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("Delete = %v (%T), want a PermanentError", err, err)
	}
}

// Two names would be two declarations driving one helm release, with no way to
// say which wins. Permanent from Update and from Create, so it never reaches the
// generic retryable "unknown kind" error.
func TestControllerSecondNameIsPermanent(t *testing.T) {
	f := &ctlServer{helm: deployedAt(ctlVersionTarget), ctlVersion: ctlVersionTarget}
	b := f.start(t)

	for _, verb := range []struct {
		name string
		call func(api.DesiredResource) error
	}{
		{"Update", func(r api.DesiredResource) error { return b.Update(t.Context(), r) }},
		{"Create", func(r api.DesiredResource) error { return b.Create(t.Context(), r) }},
	} {
		r := api.DesiredResource{Kind: KindController, Name: "other", Generation: 1,
			Spec: json.RawMessage(ctlSpec(ctlVersionTarget))}
		var perm *reconcile.PermanentError
		if err := verb.call(r); !errors.As(err, &perm) {
			t.Errorf("%s on a second name = %v (%T), want a PermanentError", verb.name, err, err)
		}
	}
}

// Chart overrides are compared by containment, not equality. --set-json merges a
// declared subtree into what the release already carries (the operator's own
// registry, storage class and Secret names, re-sent from the release), so a declared
// subset can never equal what `helm get values` reports back — and an equality
// test would re-run helm on every tick, bumping the release revision forever and
// leaving the row pinned un-Ready.
func TestControllerValuesAreComparedByContainment(t *testing.T) {
	f := &ctlServer{helm: deployedAt(ctlVersionTarget), ctlVersion: ctlVersionTarget}
	// Added to what the release already carries, not replacing it: the endpoint
	// subtree also holds the image tag a deployed release is pinned at.
	ep := f.helm.values["controller"].(map[string]any)["endpoint"].(map[string]any)
	ep["storageClass"], ep["ybHostPorts"] = "premium-rwo", "5433"
	b := f.start(t)

	declared := `{"version":"` + ctlVersionTarget + `","chart_version":"` + ctlChartVersion +
		`","values":{"controller":{"endpoint":{"storageClass":"premium-rwo"}}}}`
	if err := b.Update(t.Context(), ctlRes(1, declared)); err != nil {
		t.Fatalf("Update = %v, want nil: the release already carries the declared override", err)
	}
	if got := f.helm.upgrades(); len(got) != 0 {
		t.Fatalf("helm upgrades = %v, want none", got)
	}

	// A value the release does not carry still moves it, and the next tick then
	// finds its own write and stops.
	changed := strings.Replace(declared, "premium-rwo", "standard-rwo", 1)
	mustPending(t, b.Update(t.Context(), ctlRes(2, changed)))
	if got := f.helm.upgrades(); len(got) != 1 {
		t.Fatalf("helm upgrades = %v, want 1 for the changed override", got)
	}
	f.ctlRolling = true
	mustPending(t, b.Update(t.Context(), ctlRes(2, changed)))
	if got := f.helm.upgrades(); len(got) != 1 {
		t.Errorf("helm upgrades after a mid-rollout tick = %d, want still 1", len(got))
	}
}

// The controller is on the remote whether or not anyone declared it, so it is
// always listed — and the row says so, which is what stops the engine collecting
// it. Without Preexisting the ordinary GC rule sees in-actual-not-in-desired,
// plans a Delete, this kind's Delete refuses as it must, and the refusal latches a
// permanent drift row on an environment where nothing is wrong.
func TestUndeclaredControllerIsReportedButNeverCollected(t *testing.T) {
	f := &ctlServer{helm: deployedAt(ctlVersionTarget), ctlVersion: ctlVersionTarget}
	b := f.start(t)

	row, ok := ctlRow(t, b)
	if !ok {
		t.Fatal("no controller row: anything gated on the controller reads the actual map, " +
			"so a missing row defers its children forever and silently")
	}
	if !row.Preexisting {
		t.Error("the controller row is not marked Preexisting, so the engine would try to collect it")
	}

	// The whole point, through the engine: desired declares nothing.
	res, err := reconcile.New(b, reconcile.Options{}).Reconcile(t.Context(), nil, true)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Deleted != 0 {
		t.Errorf("Deleted = %d; the controller is the release this agent runs in", res.Deleted)
	}
	if len(res.Permanent) != 0 {
		t.Errorf("Permanent = %v; a refused Delete latches drift on an environment "+
			"that simply never asked YCP to manage its controller", res.Permanent)
	}
	if len(res.Failed) != 0 {
		t.Errorf("Failed = %v, want nothing attempted", res.Failed)
	}
}

// A declared controller is converged normally: Preexisting suppresses the delete
// and nothing else, so an operator who does declare one still gets the upgrade.
func TestDeclaredControllerStillConverges(t *testing.T) {
	f := &ctlServer{helm: deployedAt("0.260801.0-old"), ctlVersion: "0.260801.0-old"}
	b := f.start(t)

	res, err := reconcile.New(b, reconcile.Options{}).Reconcile(t.Context(),
		[]api.DesiredResource{ctlRes(1, ctlSpec(ctlVersionTarget))}, true)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Updating) != 1 {
		t.Fatalf("Updating = %v, want the controller in flight", res.Updating)
	}
	if got := f.helm.upgrades(); len(got) != 1 {
		t.Fatalf("helm upgrades = %v, want exactly one", got)
	}
}

// The recovery path for the one thing that can break the controller's REST API: a
// controller version this agent deployed. The controller kind converges on helm
// and the k8s rollout, so it can still run — but only if a List that cannot reach
// the controller reports what it did read instead of aborting the tick, which
// would leave the rollback unappliable and recovery to a human with helm.
func TestControllerUpgradeRunsWhileTheControllerIsUnreachable(t *testing.T) {
	f := &ctlServer{helm: deployedAt("0.260801.0-old"), ctlVersion: "0.260801.0-old", pools: []string{"pool-a"}}
	b := f.start(t)
	// One good tick first: an agent mid-upgrade has been running, so it knows what
	// the environment held before the controller went down. That is what lets the
	// failed read NAME what it could not reach instead of returning a short list.
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("seed List: %v", err)
	}
	f.treeDown, f.ctlVersion = true, "" // the controller restarts mid-upgrade
	eng := reconcile.New(b, reconcile.Options{})

	res, err := eng.Reconcile(t.Context(), []api.DesiredResource{ctlRes(1, ctlSpec(ctlVersionTarget))}, true)
	if err != nil {
		t.Fatalf("Reconcile with an unreachable controller = %v, want a partial tick rather than an aborted one", err)
	}
	if len(res.Unreadable) == 0 {
		t.Error("Result.Unreadable is empty, so the environment would report READY while most of it went unread")
	}
	if got := f.helm.upgrades(); len(got) != 1 {
		t.Fatalf("helm upgrades = %v, want 1: the declared version must still be deployable", got)
	}
}

// The controller's own /version reports the binary's stamp
// ("0.260714.0-199ec1a"), which is never the image tag an operator declares
// ("custom-28386c402c60"). Comparing the two never converges: the row would never
// go Ready, so nothing gated on the controller being at its declared version
// could ever proceed. The pod template's tag is the comparand; /version only has
// to answer.
func TestControllerConvergesOnTheImageTagNotTheReportedVersion(t *testing.T) {
	f := &ctlServer{helm: deployedAt(ctlVersionTarget), ctlVersion: "0.260714.0-199ec1a"}
	b := f.start(t)

	if err := b.Update(t.Context(), ctlRes(1, ctlSpec(ctlVersionTarget))); err != nil {
		t.Fatalf("Update = %v, want nil: the release carries the declared tag", err)
	}
	if got := f.helm.upgrades(); len(got) != 0 {
		t.Errorf("helm upgrades = %v, want none", got)
	}

	actuals, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var ctl reconcile.Actual
	var ok bool
	for _, a := range actuals {
		if a.Key.Kind == KindController {
			ctl, ok = a, true
		}
	}
	if !ok {
		t.Fatal("no controller row listed")
	}
	if !ctl.Ready {
		t.Errorf("controller Ready = false (state %q), want true", ctl.State)
	}
	if ctl.Attrs["version"] != "0.260714.0-199ec1a" || ctl.Attrs["image_tag"] != ctlVersionTarget {
		t.Errorf("attrs = %v, want the reported version and the image tag reported separately", ctl.Attrs)
	}
}

// ctlServer is the controller kind's HTTP fixture: GET /version, and enough of
// the resource tree for List to complete. Purpose-built rather than shared with
// the other backend tests, because this kind is the one that has to behave while
// the REST API it would otherwise be read through is down.
type ctlServer struct {
	// ctlVersion is what GET /version reports; empty makes the route fail, which
	// is what a controller restarting mid-upgrade looks like. ctlImageTag is the
	// tag the pod template carries, which is what a declared version is compared
	// against — deliberately a different string, because in a cluster they are.
	// ctlRolling holds the Deployment rollout open.
	helm        *fakeHelm
	ctlVersion  string
	ctlImageTag string
	ctlRolling  bool

	// treeDown makes the pool traversal fail the way an unreachable controller
	// does, which is the state a controller upgrade itself produces.
	treeDown bool
	// chart overrides the chart reference, so a test can use the packaged-archive
	// form the agent is pointed at before the chart is published.
	chart string
	// pools are the storage pools the tree read returns, so a tick has something
	// to remember and a later failed read has something to name.
	pools []string
}

func poolRows(names []string) string {
	rows := make([]string, 0, len(names))
	for i, n := range names {
		rows = append(rows, fmt.Sprintf(`{"id":"00000000-0000-0000-0000-00000000000%d","name":%q}`, i+1, n))
	}
	return strings.Join(rows, ",")
}

func (f *ctlServer) start(t *testing.T) *Backend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch p := r.URL.Path; {
		case p == "/version":
			if f.ctlVersion == "" {
				w.WriteHeader(http.StatusServiceUnavailable)
				io.WriteString(w, `{"error":{"code":"unavailable","message":"restarting"}}`)
				return
			}
			io.WriteString(w, `{"version":"`+f.ctlVersion+`","git_commit":"abc1234","build_date":"2026-01-01T00:00:00Z"}`)
		case p == "/storage-pools":
			if f.treeDown {
				w.WriteHeader(http.StatusServiceUnavailable)
				io.WriteString(w, `{"error":{"code":"unavailable","message":"restarting"}}`)
				return
			}
			io.WriteString(w, `{"storage_pools":[`+poolRows(f.pools)+`]}`)
		case strings.HasSuffix(p, "/clusters"):
			io.WriteString(w, `{"clusters":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":"not_found","message":"unknown route"}}`)
		}
	}))
	t.Cleanup(srv.Close)

	if f.helm == nil {
		f.helm = &fakeHelm{}
	}
	chart := f.chart
	if chart == "" {
		chart = ctlChart
	}
	b, err := New(srv.URL, "", nil, ControllerConfig{
		Helm: f.helm,
		Rollout: func(context.Context, string) (bool, string, error) {
			tag := f.ctlImageTag
			if tag == "" {
				tag = f.helm.deployedTag()
			}
			return !f.ctlRolling, tag, nil
		},
		Release:    ctlRelease,
		Namespace:  ctlNamespace,
		Chart:      chart,
		Deployment: "amp-controller",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}
