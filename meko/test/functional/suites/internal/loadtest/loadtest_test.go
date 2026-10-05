package loadtest

import (
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoload"
)

func TestPlanKeepsUnderThePATCap(t *testing.T) {
	if err := (Plan{Pods: 2, Credentials: 3, PATBudget: 2}).Check(); err != nil {
		t.Errorf("3 + 4 PATs fit: %v", err)
	}
	err := (Plan{Pods: 3, Credentials: 3, PATBudget: 2}).Check()
	if err == nil || !strings.Contains(err.Error(), "9 active PATs") {
		t.Errorf("err = %v", err)
	}
	if (Plan{Pods: 0, Credentials: 1}).Check() == nil {
		t.Error("no pods was accepted")
	}
}

func TestSplit(t *testing.T) {
	got := Split(10, 3)
	if got[0] != 4 || got[1] != 3 || got[2] != 3 {
		t.Errorf("%v", got)
	}
	if g := Split(1, 3); g[0] != 1 || g[1] != 0 || g[2] != 0 {
		t.Errorf("%v", g)
	}
}

func agg(total mekoload.OpAgg, mod func(*mekoload.Aggregate)) mekoload.Aggregate {
	a := mekoload.Aggregate{Total: total, Ops: map[string]mekoload.OpAgg{}, Invariants: map[string]mekoload.Invariant{}, ErrorSamples: map[string][]string{}}
	if mod != nil {
		mod(&a)
	}
	return a
}

func TestJudge(t *testing.T) {
	good := agg(mekoload.OpAgg{Count: 1000, OK: 999, Errors: 1, ErrorRate: 0.001}, nil)
	if got := Judge(good, Bounds{MaxErrorRate: 0.02}); len(got) != 0 {
		t.Errorf("a clean run was judged: %v", got)
	}

	bad := agg(mekoload.OpAgg{Count: 1000, OK: 900, Errors: 100, ErrorRate: 0.1}, func(a *mekoload.Aggregate) {
		a.Http5xx = 3
		a.Ops["datapack_create"] = mekoload.OpAgg{ErrorsByClass: map[string]int64{"http_5xx": 3, "timeout": 97}, Statuses: map[string]int64{"500": 2, "502": 1}}
		a.ErrorSamples["datapack_create/timeout"] = []string{"POST /datapacks: Client.Timeout"}
		a.Invariants["gone_after_delete"] = mekoload.Invariant{Checked: 10, Violated: 2, Samples: []string{"dp-1 answers 200"}}
		a.Pods = []mekoload.Stats{
			{State: "failed", ID: "r-1", FailError: "setup: boom", Cleanup: &mekoload.CleanupResult{LeakedDatapacks: []string{"dp-9"}}},
		}
	})
	got := strings.Join(Judge(bad, Bounds{MaxErrorRate: 0.02}), "\n")
	for _, want := range []string{"r-1: setup: boom", "error rate 10.00% is over the bound of 2.00%", "datapack_create timeout x97 (POST /datapacks: Client.Timeout)",
		"3 calls answered 5xx, 0 allowed", "datapack_create HTTP 500 x2", "invariant gone_after_delete broke 2 times in 10 checks", "left datapacks [dp-9]"} {
		if !strings.Contains(got, want) {
			t.Errorf("verdict lacks %q:\n%s", want, got)
		}
	}
	if got := Judge(agg(mekoload.OpAgg{}, nil), Bounds{}); len(got) != 1 || !strings.Contains(got[0], "no calls") {
		t.Errorf("an empty run: %v", got)
	}
	if got := Judge(bad, Bounds{MaxErrorRate: 1, Max5xx: 5}); strings.Contains(strings.Join(got, ""), "5xx") {
		t.Errorf("5xx within its allowance was reported: %v", got)
	}
}

func series(from, to int64, ok, err int64) map[int64]Second {
	m := map[int64]Second{}
	for s := from; s < to; s++ {
		m[s] = Second{OK: ok, Err: err}
	}
	return m
}

func merge(parts ...map[int64]Second) map[int64]Second {
	out := map[int64]Second{}
	for _, p := range parts {
		for k, v := range p {
			out[k] = v
		}
	}
	return out
}

func TestRecoveredFindsTheFirstHealthyWindow(t *testing.T) {
	kill := time.Unix(1000, 0)
	sec := merge(
		series(900, 1000, 100, 0),
		series(1000, 1005, 10, 90), // the kill: mostly errors
		series(1005, 1007, 0, 50),  // nothing succeeded
		series(1007, 1100, 100, 1),
	)
	d, ok := Recovered(sec, kill, time.Unix(1100, 0), 3, 0.1)
	if !ok || d != 7*time.Second {
		t.Errorf("recovered after %v ok=%v, want 7s", d, ok)
	}
}

func TestRecoveredWantsEverySecondToCarryTraffic(t *testing.T) {
	kill := time.Unix(1000, 0)
	sec := merge(series(1000, 1010, 100, 0))
	delete(sec, 1001)
	d, ok := Recovered(sec, kill, time.Unix(1010, 0), 3, 0.1)
	if !ok || d != 2*time.Second {
		t.Errorf("a missing second must break a window: %v %v", d, ok)
	}
}

func TestRecoveredFailsWhenTheNextKillComesFirst(t *testing.T) {
	kill := time.Unix(1000, 0)
	sec := merge(series(1000, 1040, 5, 95), series(1040, 1100, 100, 0))
	if _, ok := Recovered(sec, kill, time.Unix(1030, 0), 3, 0.1); ok {
		t.Error("recovery was reported while the errors went on")
	}
	if d, ok := Recovered(sec, kill, time.Unix(1100, 0), 3, 0.1); !ok || d != 40*time.Second {
		t.Errorf("%v %v", d, ok)
	}
}

func TestRecoveredImmediatelyWhenTheLoadNeverNoticed(t *testing.T) {
	kill := time.Unix(1000, 0)
	d, ok := Recovered(series(990, 1100, 100, 0), kill, time.Unix(1100, 0), 3, 0.1)
	if !ok || d != 0 {
		t.Errorf("%v %v", d, ok)
	}
}

func TestSecondsSumsPods(t *testing.T) {
	s := Seconds([]mekoload.PodTimeline{
		{Buckets: []mekoload.Bucket{{Unix: 5, OK: 3, Err: 1}}},
		{Buckets: []mekoload.Bucket{{Unix: 5, OK: 2}, {Unix: 6, OK: 1}}},
	})
	if s[5].OK != 5 || s[5].Err != 1 || s[6].OK != 1 {
		t.Errorf("%v", s)
	}
}

func TestDamage(t *testing.T) {
	sec := merge(series(1000, 1010, 10, 10), series(1010, 1030, 20, 0))
	rate, n := Damage(sec, time.Unix(1000, 0))
	if n != 100 || rate < 0.16 || rate > 0.17 {
		t.Errorf("rate %v errors %d", rate, n)
	}
}
