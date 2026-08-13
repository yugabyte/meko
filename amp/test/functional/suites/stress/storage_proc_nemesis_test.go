package stress

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// scriptedPod answers each `kubectl exec` with the next canned result, recording
// the shell command it was asked to run. Nothing real runs.
type scriptedPod struct {
	results []execpkg.Result
	cmds    []string
	n       int
}

func (s *scriptedPod) runner() k8s.Runner {
	return func(_ context.Context, _ io.Reader, argv []string) (execpkg.Result, error) {
		s.cmds = append(s.cmds, argv[len(argv)-1])
		if s.n >= len(s.results) {
			return execpkg.Result{ExitCode: 1}, nil
		}
		res := s.results[s.n]
		s.n++
		return res, nil
	}
}

func (s *scriptedPod) kube() *k8s.Kubectl {
	return k8s.New(k8s.Config{Namespace: "ampqa-cp-t"}).WithRunner(s.runner()).ForPod("yb-ep-1-abc", "")
}

func TestDaemonPIDsReadsPgrep(t *testing.T) {
	sp := &scriptedPod{results: []execpkg.Result{{Stdout: "41\n17\n", ExitCode: 0}}}
	pids, err := daemonPIDs(context.Background(), sp.kube(), "pageserver")
	if err != nil {
		t.Fatalf("daemonPIDs: %v", err)
	}
	if joinPIDs(pids, ",") != "17,41" {
		t.Errorf("pids = %v, want sorted 17,41", pids)
	}
	if !strings.Contains(sp.cmds[0], "pgrep -x pageserver") {
		t.Errorf("command = %q, want a pgrep -x", sp.cmds[0])
	}
}

// Exit 1 is pgrep's "nothing matched", which is an answer, not a failure.
func TestDaemonPIDsEmptyOnNoMatch(t *testing.T) {
	sp := &scriptedPod{results: []execpkg.Result{{ExitCode: 1}}}
	pids, err := daemonPIDs(context.Background(), sp.kube(), "safekeeper")
	if err != nil {
		t.Fatalf("daemonPIDs: %v", err)
	}
	if len(pids) != 0 {
		t.Errorf("pids = %v, want none", pids)
	}
	if len(sp.cmds) != 1 {
		t.Errorf("ran %d command(s), want only the pgrep: %v", len(sp.cmds), sp.cmds)
	}
}

// A container without procps must not read as "the daemon is gone".
func TestDaemonPIDsFallsBackToProcScan(t *testing.T) {
	sp := &scriptedPod{results: []execpkg.Result{
		{ExitCode: 127, Stderr: "sh: pgrep: not found"},
		{Stdout: "88\n", ExitCode: 0},
	}}
	pids, err := daemonPIDs(context.Background(), sp.kube(), "pageserver")
	if err != nil {
		t.Fatalf("daemonPIDs: %v", err)
	}
	if joinPIDs(pids, ",") != "88" {
		t.Errorf("pids = %v, want 88", pids)
	}
	if !strings.Contains(sp.cmds[1], "/proc/") {
		t.Errorf("fallback command = %q, want a /proc scan", sp.cmds[1])
	}
}

func TestDaemonPIDsErrorsWhenNeitherToolAnswers(t *testing.T) {
	sp := &scriptedPod{results: []execpkg.Result{
		{ExitCode: 127},
		{ExitCode: 2, Stderr: "permission denied"},
	}}
	if _, err := daemonPIDs(context.Background(), sp.kube(), "pageserver"); err == nil {
		t.Fatal("want an error when neither pgrep nor the /proc scan can list the process")
	}
}

func TestParsePIDsIgnoresJunk(t *testing.T) {
	pids := parsePIDs("12\nnot-a-pid\n0\n-3\n7\n")
	if joinPIDs(pids, ",") != "7,12" {
		t.Errorf("pids = %v, want 7,12", pids)
	}
}

func TestIntersectAndSubtractPIDs(t *testing.T) {
	before, now := []int{10, 20}, []int{20, 30}
	if got := joinPIDs(intersectPIDs(before, now), ","); got != "20" {
		t.Errorf("survivors = %q, want 20", got)
	}
	if got := joinPIDs(subtractPIDs(now, before), ","); got != "30" {
		t.Errorf("new pids = %q, want 30", got)
	}
	if got := intersectPIDs(before, []int{30}); len(got) != 0 {
		t.Errorf("survivors = %v, want none: every killed pid is gone", got)
	}
}

// The plan comes from the seed alone, so --seed replays the same daemon order
// and the same pods (framework rule 9).
func TestStorageKillPlanIsSeedDriven(t *testing.T) {
	daemons := []string{"pageserver", "safekeeper"}
	a := storageKillPlan(core.NewRunSeed(42, true).Fork("x"), 3, daemons)
	b := storageKillPlan(core.NewRunSeed(42, true).Fork("x"), 3, daemons)
	if len(a) != len(daemons) {
		t.Fatalf("plan has %d entries, want one per daemon", len(a))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("same seed produced %v then %v", a, b)
		}
		if !storageDaemonNames[a[i].Daemon] {
			t.Errorf("plan targets %q, which is not a storage daemon", a[i].Daemon)
		}
		if a[i].Tenant < 0 || a[i].Tenant >= 3 {
			t.Errorf("plan targets tenant %d, out of range", a[i].Tenant)
		}
	}
	seen := map[string]bool{}
	for _, tgt := range a {
		if seen[tgt.Daemon] {
			t.Errorf("daemon %s planned twice: each is killed once per pass", tgt.Daemon)
		}
		seen[tgt.Daemon] = true
	}
	if c := storageKillPlan(core.NewRunSeed(43, true).Fork("x"), 3, daemons); c[0] == a[0] && c[1] == a[1] {
		t.Log("seeds 42 and 43 agree on this plan; not a failure, only worth knowing")
	}
	if got := storageKillPlan(core.NewRunSeed(42, true).Fork("x"), 0, daemons); got != nil {
		t.Errorf("plan with no tenant = %v, want none", got)
	}
}

func TestValidateStorageDaemons(t *testing.T) {
	if err := validateStorageDaemons([]string{"pageserver", "safekeeper"}); err != nil {
		t.Errorf("both storage daemons rejected: %v", err)
	}
	if err := validateStorageDaemons(nil); err == nil {
		t.Error("an empty daemon list must fail: nothing would be faulted")
	}
	// compute_ctl is supervised too, but killing it is a compute fault, not this test's.
	if err := validateStorageDaemons([]string{"compute_ctl"}); err == nil {
		t.Error("compute_ctl must be rejected as a target")
	}
}

func TestStorageProcNemesisParamsDeclaredInDefaults(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load shipped defaults: %v", err)
	}
	for _, key := range []string{
		"stress.storage_proc_nemesis.tenants",
		"stress.storage_proc_nemesis.baseline_duration",
		"stress.storage_proc_nemesis.fault_duration",
		"stress.storage_proc_nemesis.daemons",
		"stress.storage_proc_nemesis.kills_per_daemon",
		"stress.storage_proc_nemesis.kill_gap",
		"stress.storage_proc_nemesis.settle_timeout",
		"stress.storage_proc_nemesis.resume_duration",
		"stress.storage_proc_nemesis.payload_bytes",
	} {
		if !cfg.Has(key) {
			t.Errorf("%s is read by the test but not declared in configs/default.yaml", key)
		}
	}
	p := storageProcParamsFromConfig(cfg)
	if err := validateStorageDaemons(p.Daemons); err != nil {
		t.Errorf("shipped daemons list: %v", err)
	}
	if p.KillsPerDaemon < 1 {
		t.Errorf("kills_per_daemon = %d: no kill would land", p.KillsPerDaemon)
	}
	// Past the budget the supervisor exits and the pod is recreated, which is a
	// different contract from the one this test asserts.
	if p.KillsPerDaemon > supervisorMaxRestarts {
		t.Errorf("kills_per_daemon = %d exceeds the supervisor's budget of %d", p.KillsPerDaemon, supervisorMaxRestarts)
	}
	if p.ResumeDuration <= 0 {
		t.Error("resume_duration must be positive: the post-kill write probe is what catches a wedged COMMIT")
	}
}

func TestProcScanCommandMatchesExactComm(t *testing.T) {
	cmd := procScanCommand("pageserver")
	if !strings.Contains(cmd, `= "pageserver"`) {
		t.Errorf("scan %q does not compare /proc/<pid>/comm exactly", cmd)
	}
	if !strings.Contains(cmd, "exit 0") {
		t.Errorf("scan %q must exit 0 when nothing matched, so absence is not read as a broken exec", cmd)
	}
}
