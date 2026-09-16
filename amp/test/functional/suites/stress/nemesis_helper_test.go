package stress

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybadmin"
	"github.com/yugabyte/amp/test/functional/framework/core"
	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// The selector list is what makes backing chaos work against EVERY backing
// this framework deploys. Each shape is load-bearing and was read off live
// pods: ybk8s labels tservers app=yb-tserver, a YBA-provisioned universe
// labels them app.kubernetes.io/name=yb-tserver, and ampstack's built-in
// yugabyted is app=yugabyted (one pod, both roles).
func TestBackingPodSelectorsCoverEveryBacking(t *testing.T) {
	want := map[string]bool{
		"app=yb-tserver":                    false,
		"app.kubernetes.io/name=yb-tserver": false,
		"app=yugabyted":                     false,
	}
	for _, sel := range backingPodSelectors {
		if _, ok := want[sel]; !ok {
			t.Errorf("unexpected selector %q — masters and non-serving pods must not be targeted", sel)
		}
		want[sel] = true
	}
	for sel, seen := range want {
		if !seen {
			t.Errorf("selector %q missing: that backing's pods would never be targeted", sel)
		}
	}
}

func TestBackingNemesisParamsDeclaredInDefaults(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load shipped defaults: %v", err)
	}
	for _, key := range []string{
		"stress.backing_nemesis.tenants",
		"stress.backing_nemesis.duration",
		"stress.backing_nemesis.settle_timeout",
		"stress.backing_nemesis.payload_bytes",
		"stress.backing_nemesis.resume_duration",
		"stress.amp_nemesis.tenants",
		"stress.amp_nemesis.duration",
	} {
		if !cfg.Has(key) {
			t.Errorf("%s is read by the test but not declared in configs/default.yaml", key)
		}
	}
	p := backingNemesisParamsFromConfig(cfg)
	if p.Tenants < 2 {
		t.Errorf("default tenants = %d: with one tenant, isolation and fairness mean nothing", p.Tenants)
	}
	if p.ResumeDuration <= 0 {
		t.Error("resume_duration must be positive — the post-chaos write probe is the point of the test (meko#727)")
	}
}

// A prefix-only match would turn amp_nemesis into every concurrent test's
// nemesis, since all their endpoints share the namespace.
func TestFleetEndpointPodsScopesToOwnFleet(t *testing.T) {
	pods := []string{
		"yb-ep-aaaa1111-2222-3333-4444-555566667777-96b45f5cb-dk2j5", // ours
		"yb-ep-bbbb1111-2222-3333-4444-555566667777-85847c58d7-gkdl", // another test's
		"yb-ep-aaaa1111-2222-3333-4444-555566667777-96b45f5cb-x9z8q", // ours, second pod
		"amp-controller-5b9dcc4c44-f5hqm",
		"ampqa-http-relay",
	}
	tenants := []tenant{
		{Endpoint: controller.Endpoint{ID: "aaaa1111-2222-3333-4444-555566667777"}},
		{Endpoint: controller.Endpoint{}}, // endpoint never created: must match nothing
	}
	got := fleetEndpointPods(pods, tenants)
	want := []string{
		"yb-ep-aaaa1111-2222-3333-4444-555566667777-96b45f5cb-dk2j5",
		"yb-ep-aaaa1111-2222-3333-4444-555566667777-96b45f5cb-x9z8q",
	}
	if !slices.Equal(got, want) {
		t.Errorf("fleetEndpointPods = %v, want %v", got, want)
	}
	if got := fleetEndpointPods(pods, nil); len(got) != 0 {
		t.Errorf("no tenants must select no pods, got %v", got)
	}
}

// A foreign endpoint mid-churn must not fail us; shared pods still must.
func TestForeignEndpointPod(t *testing.T) {
	tenants := []tenant{{Endpoint: controller.Endpoint{ID: "aaaa1111-2222-3333-4444-555566667777"}}}
	for pod, foreign := range map[string]bool{
		"yb-ep-aaaa1111-2222-3333-4444-555566667777-96b45f5cb-dk2j5": false, // ours: asserted
		"yb-ep-bbbb1111-2222-3333-4444-555566667777-85847c58d7-gkdl": true,  // another fleet's
		"amp-controller-5b9dcc4c44-f5hqm":                            false, // shared: asserted
		"yb-amp-controller-logs-collector-4d6vt":                     false, // shared: asserted
	} {
		if got := foreignEndpointPod(pod, tenants); got != foreign {
			t.Errorf("foreignEndpointPod(%s) = %v, want %v", pod, got, foreign)
		}
	}
}

type scriptedAdmin struct {
	answers []string
	cmds    []string
}

func (s *scriptedAdmin) Run(_ context.Context, cmd string) (execpkg.Result, error) {
	s.cmds = append(s.cmds, cmd)
	if len(s.answers) == 0 {
		return execpkg.Result{ExitCode: 1}, nil
	}
	out := s.answers[0]
	s.answers = s.answers[1:]
	return execpkg.Result{Stdout: out}, nil
}

func (s *scriptedAdmin) Copy(context.Context, string, string) error { return nil }

const ampTables = `Table-Name  Table-ID
yb_storage.endpoints_name_per_branch [ysql_schema=amp_registry]  000040000000300080000000000040b3
yb_storage.meta [ysql_schema=amp_storage]  000040000000300080000000000040cf
other_db.pages [ysql_schema=public]  000040000000300080000000000040ff`

const mixedTablets = `Tablet-UUID                       Range                                         Leader-IP        Leader-UUID
8cc256fe5b9d4b199b3b8bd8f4497578  partition_key_start: "" partition_key_end: ""
a1b2c3d4e5f60718293a4b5c6d7e8f90  partition_key_start: "" partition_key_end: ""  10.4.0.11:9100   c9a3b1de4f5061728394a5b6c7d8e9f0`

const ledTablets = `Tablet-UUID                       Range                                         Leader-IP        Leader-UUID
2e74036a9f6c44dd89040a0deeb40471  partition_key_start: "" partition_key_end: ""  10.4.0.12:9100   d9a3b1de4f5061728394a5b6c7d8e9f1`

func TestLeaderlessTabletsNamesOnlyTheLeaderlessOnesInTheDatabase(t *testing.T) {
	ex := &scriptedAdmin{answers: []string{ampTables, mixedTablets, ledTablets}}
	got, raw, err := leaderlessTablets(context.Background(), ybadmin.New(ex, "m:7100"), "yb_storage")
	if err != nil {
		t.Fatalf("leaderlessTablets: %v", err)
	}
	want := "yb_storage.amp_registry.endpoints_name_per_branch/8cc256fe5b9d4b199b3b8bd8f4497578"
	if len(got) != 1 || got[0] != want {
		t.Errorf("leaderless = %v, want only %s", got, want)
	}
	if !strings.Contains(raw, "no leader") {
		t.Errorf("raw listing does not record the leaderless tablet: %q", raw)
	}
	for _, c := range ex.cmds {
		if strings.Contains(c, "000040000000300080000000000040ff") {
			t.Errorf("listed tablets of a table outside yb_storage: %s", c)
		}
	}
}

func TestLeaderlessTabletsIsEmptyWhenEveryTabletIsLed(t *testing.T) {
	ex := &scriptedAdmin{answers: []string{ampTables, ledTablets, ledTablets}}
	got, _, err := leaderlessTablets(context.Background(), ybadmin.New(ex, "m:7100"), "yb_storage")
	if err != nil {
		t.Fatalf("leaderlessTablets: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("leaderless = %v, want none", got)
	}
}

func TestLeaderlessTabletsMatchesTheKeyspaceBySubstring(t *testing.T) {
	const prefixed = `Table-Name  Table-ID
ysql.yb_storage.meta [ysql_schema=amp_storage]  000040000000300080000000000040cf`
	ex := &scriptedAdmin{answers: []string{prefixed, mixedTablets}}
	got, _, err := leaderlessTablets(context.Background(), ybadmin.New(ex, "m:7100"), "yb_storage")
	if err != nil {
		t.Fatalf("a keyspace carrying a db-type prefix was not matched: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("leaderless = %v, want the one leaderless tablet", got)
	}
}

func TestLeaderlessTabletsFailsWhenNoTableMatched(t *testing.T) {
	const noAmpTables = `Table-Name  Table-ID
other_db.pages [ysql_schema=public]  000040000000300080000000000040ff`
	_, raw, err := leaderlessTablets(context.Background(), ybadmin.New(&scriptedAdmin{answers: []string{noAmpTables}}, "m:7100"), "yb_storage")
	if err == nil {
		t.Fatal("a listing with no yb_storage table read as a led universe")
	}
	if !strings.Contains(raw, "other_db.pages") {
		t.Errorf("the raw listing is not carried back for the record: %q", raw)
	}
}

func TestBackingAdminReportsABackingItCannotInspect(t *testing.T) {
	if !slices.Contains(backingPodSelectors, "app=yugabyted") {
		t.Fatal("backingPodSelectors no longer covers the built-in yugabyted")
	}
	// Every tserver selector matches nothing, as on a yugabyted backing.
	kube := k8s.New(k8s.Config{Namespace: "yb-dev-t"}).WithRunner(
		func(context.Context, io.Reader, []string) (execpkg.Result, error) {
			return execpkg.Result{}, nil
		})
	_, err := backingAdmin(context.Background(), kube, func(string, ...any) {})
	if !errors.Is(err, errNoTabletView) {
		t.Fatalf("backingAdmin on a backing with no tserver pod = %v, want errNoTabletView", err)
	}
}
