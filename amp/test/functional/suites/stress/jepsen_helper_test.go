package stress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/amp/test/functional/framework/apps/jepsen"
	"github.com/yugabyte/amp/test/functional/framework/chaos"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func TestSelectWorkloadsDefaultsToTheYSQLSet(t *testing.T) {
	sel, dropped := selectWorkloads(nil)
	if len(sel) != len(jepsen.YSQL()) {
		t.Errorf("default selection = %d, want the YSQL set (%d)", len(sel), len(jepsen.YSQL()))
	}
	if len(dropped) != 0 {
		t.Errorf("default selection dropped %v", dropped)
	}
}

// A YCQL workload has no target on amp, whose compute speaks Postgres. It is
// dropped WITH ITS REASON rather than silently, so a run that asked for one can
// see why nothing happened.
func TestSelectWorkloadsDropsYCQLWithAReason(t *testing.T) {
	sel, dropped := selectWorkloads([]string{"counter"})
	for _, w := range sel {
		if strings.HasPrefix(string(w), "ycql/") {
			t.Errorf("selected a YCQL workload: %s", w)
		}
	}
	if len(dropped) == 0 || !strings.Contains(strings.Join(dropped, " "), "YCQL") {
		t.Errorf("YCQL drop not reported: %v", dropped)
	}
}

func TestSelectWorkloadsReportsAFilterThatMatchesNothing(t *testing.T) {
	sel, dropped := selectWorkloads([]string{"no-such-workload"})
	if len(sel) != 0 {
		t.Errorf("selection = %v, want none", sel)
	}
	if len(dropped) != 1 || !strings.Contains(dropped[0], "matches no workload") {
		t.Errorf("dropped = %v", dropped)
	}
}

func TestSelectWorkloadsDeduplicates(t *testing.T) {
	sel, _ := selectWorkloads([]string{"sz.bank", "bank-contention"})
	seen := map[jepsen.Workload]bool{}
	for _, w := range sel {
		if seen[w] {
			t.Errorf("duplicate workload %s", w)
		}
		seen[w] = true
	}
}

// The alias Service is what lets jepsen reach an amp endpoint at all: jepsen
// hardcodes port 5433 and an amp endpoint publishes its own. The alias must
// carry the endpoint's OWN selector, or it points at the wrong pods.
func TestAliasManifestRepublishesOnTheHardcodedPort(t *testing.T) {
	m := jepsenAliasManifest("ampqa-jepsen-ep-abc",
		map[string]string{"app": "yb-ep-1", "role": "compute"}, "55433")
	for _, want := range []string{
		"name: ampqa-jepsen-ep-abc",
		`app: "yb-ep-1"`,
		`role: "compute"`,
		"port: 5433",
		"targetPort: 55433",
		"type: ClusterIP",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q:\n%s", want, m)
		}
	}
}

func TestParseJSONStringMap(t *testing.T) {
	got, err := parseJSONStringMap(`{"app":"yb-ep-1","amp.io/role":"compute"}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["app"] != "yb-ep-1" || got["amp.io/role"] != "compute" {
		t.Errorf("parsed = %v", got)
	}
}

// An empty selector must be an ERROR at the call site, not an empty map that
// silently becomes a Service selecting every pod in the namespace.
func TestParseJSONStringMapEmpty(t *testing.T) {
	got, err := parseJSONStringMap("{}")
	if err != nil || len(got) != 0 {
		t.Errorf("parse({}) = %v, %v", got, err)
	}
}

func TestParseJSONStringMapRejectsGarbage(t *testing.T) {
	if _, err := parseJSONStringMap("not json"); err == nil {
		t.Error("parse of a non-object returned no error")
	}
}

// Two fault loops must never share a pod: one loop's recovery would undo the
// other's fault. Fewer pods than loops gives fewer loops, never an empty one.
func TestSplitPodsGivesDisjointNonEmptyShares(t *testing.T) {
	cases := []struct {
		pods []string
		n    int
		want [][]string
	}{
		{[]string{"a", "b", "c"}, 2, [][]string{{"a", "c"}, {"b"}}},
		{[]string{"a"}, 3, [][]string{{"a"}}},
		{[]string{"a", "b"}, 0, [][]string{{"a", "b"}}},
	}
	for _, c := range cases {
		got := splitPods(c.pods, c.n)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("splitPods(%v, %d) = %v, want %v", c.pods, c.n, got, c.want)
		}
	}
}

// The registry's own network actions exec iptables and tc into the target,
// which a YB pod cannot run; the jepsen loops must get the helper-pod ones.
func TestNetnsRegistryReplacesTheNetworkActions(t *testing.T) {
	r := netnsRegistry(nil, nil)
	for _, name := range []string{chaos.ActionNetworkPartition, chaos.ActionNetworkSlowdown, chaos.ActionPacketLoss} {
		a, err := r.Build(name, chaos.DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := a.(*netnsAction); !ok {
			t.Errorf("%s is %T, want the netns helper action", name, a)
		}
	}
	a, err := r.Build(chaos.ActionPodDelete, chaos.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.(*netnsAction); ok {
		t.Errorf("pod_delete was replaced too")
	}
}

// Recover on a target whose inject failed has nothing to heal.
func TestNetnsActionRecoverWithoutInjectIsANoop(t *testing.T) {
	a := &netnsAction{name: chaos.ActionPacketLoss, active: map[string]healer{}}
	if err := a.Recover(context.Background(), chaos.NewPodTarget("p", k8s.New(k8s.Config{Namespace: "ampqa-x"}), "p", "")); err != nil {
		t.Errorf("Recover = %v, want nil", err)
	}
}

// One alias per tenant, under the name a rig's Close deletes.
func TestJepsenAliasName(t *testing.T) {
	if got := jepsenAliasName("abc", 2); got != "ampqa-jepsen-ep-abc-2" {
		t.Errorf("jepsenAliasName = %q", got)
	}
}

// Every tenant collects under the same jepsen/<workload>/ names; the sink must
// keep them apart, and repeats apart too.
func TestTenantSinkKeepsEveryTenantsEvidence(t *testing.T) {
	got := map[string]string{}
	inner := sinkFunc(func(name, _ string, content []byte) error { got[name] = string(content); return nil })
	for i := 0; i < 2; i++ {
		_ = tenantSink(inner, i, 0, 1).Add("jepsen/ysql__sz_set/launch.log", "log", []byte(fmt.Sprint(i)))
	}
	_ = tenantSink(inner, 0, 1, 2).Add("jepsen/ysql__sz_set/launch.log", "log", []byte("r"))
	want := map[string]string{
		"tenant0/jepsen/ysql__sz_set/launch.log":       "0",
		"tenant1/jepsen/ysql__sz_set/launch.log":       "1",
		"tenant0/iter1/jepsen/ysql__sz_set/launch.log": "r",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("artifacts = %v, want %v", got, want)
	}
}

type sinkFunc func(name, kind string, content []byte) error

func (f sinkFunc) Add(name, kind string, content []byte) error { return f(name, kind, content) }

// The chaos loop runs only when there is something to fault: under the lease
// race with no backing actions, the race is the whole nemesis.
func TestHasFaultTargets(t *testing.T) {
	kb := k8s.New(k8s.Config{Namespace: "ampqa-x"})
	for _, c := range []struct {
		ep, bk []string
		kube   *k8s.Kubectl
		want   bool
	}{
		{nil, nil, kb, false},
		{nil, []string{"pod_delete"}, nil, false},
		{nil, []string{"pod_delete"}, kb, true},
		{[]string{"pod_freeze"}, nil, nil, true},
	} {
		if got := hasFaultTargets(jepsenParams{EndpointActions: c.ep, BackingActions: c.bk}, c.kube); got != c.want {
			t.Errorf("hasFaultTargets(ep=%v, backing=%v, kube=%v) = %v, want %v", c.ep, c.bk, c.kube != nil, got, c.want)
		}
	}
}

// Only a stale snapshot or an accepted write on the old pod is a finding, and
// it belongs to the tenant it happened on. An old pod that refused or dropped
// the session fenced itself; one that never answered a read proved nothing.
func TestLeaseProbeFindings(t *testing.T) {
	run := &leaseRun{Probes: []probeResult{
		{Tenant: 0, Cycle: 1, OldServing: false, MarkerVisible: -1},
		{Tenant: 0, Cycle: 2, OldServing: true, MarkerVisible: 2 * time.Second, OldWriteErr: "fenced"},
		{Tenant: 1, Cycle: 3, OldServing: true, MarkerVisible: -1, StaleReads: 4, OldWriteErr: "fenced"},
		{Tenant: 2, Cycle: 4, OldServing: true, MarkerVisible: time.Second, OldWriteAccepted: true},
		{Tenant: 3, Cycle: 5, OldServing: true, MarkerVisible: -1, OldWentDown: true},
	}}
	f := run.Findings()
	if len(f) != 2 || len(f[1]) != 1 || !strings.Contains(f[1][0], "stale snapshot") ||
		len(f[2]) != 1 || !strings.Contains(f[2][0], "split-brain") {
		t.Errorf("Findings = %q, want tenant 1 (stale) and tenant 2 (split-brain write)", f)
	}
}

// A refused takeover is amp's answer; no evidence for any other reason is the
// harness's, and a pod left frozen spoils everything after it.
func TestLeaseRunFailure(t *testing.T) {
	probe := []probeResult{{Cycle: 1}}
	for _, c := range []struct {
		name string
		run  *leaseRun
		want string // "" = no failure
		env  bool
	}{
		{"clean", &leaseRun{Takeovers: 2, Probes: probe}, "", false},
		{"left frozen", &leaseRun{Takeovers: 2, Probes: probe, Thaw: []string{"x"}}, "left a pod frozen", true},
		{"refused", &leaseRun{Refused: []string{"never Ready"}}, "completed no takeover", false},
		{"never got there", &leaseRun{Errors: []string{"no Deployment"}}, "never reached a takeover", true},
		{"no probe", &leaseRun{Takeovers: 1, Errors: []string{"probe: port-forward"}}, "no lease-race cycle produced a probe", true},
	} {
		err := c.run.Failure(time.Minute)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: Failure = %v, want nil", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: Failure = %v, want %q", c.name, err, c.want)
		case c.want != "" && core.IsEnvFailure(err) != c.env:
			t.Errorf("%s: env failure = %v, want %v", c.name, core.IsEnvFailure(err), c.env)
		}
	}
}

// Only Postgres refusing or dropping the session means fenced; kubectl and
// port-forward failures are the probe's own.
func TestSessionRefused(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&pgconn.PgError{Code: "57P01"}, true},
		{fmt.Errorf("ping: %w", &pgconn.PgError{Code: "57P03"}), true},
		{errors.New("dial tcp 127.0.0.1:4: connect: connection refused"), true},
		{errors.New("failed to receive message: unexpected EOF"), true},
		{errors.New("port-forward to pod p: timed out"), false},
		{errors.New("read the pg-compute port of pod p: NotFound"), false},
		{context.DeadlineExceeded, false},
		{&pgconn.PgError{Code: "42P01"}, false},
	} {
		if got := sessionRefused(c.err); got != c.want {
			t.Errorf("sessionRefused(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// Recover on an endpoint target whose inject failed has nothing to undo.
func TestEndpointActionRecoverWithoutInjectIsANoop(t *testing.T) {
	a := &endpointAction{name: chaos.ActionPodDelete, active: map[string]*chaos.Target{}}
	if err := a.Recover(context.Background(), chaos.NewPodTarget("yb-ep-x-", k8s.New(k8s.Config{Namespace: "ampqa-x"}), "yb-ep-x-", "")); err != nil {
		t.Errorf("Recover = %v, want nil", err)
	}
}
