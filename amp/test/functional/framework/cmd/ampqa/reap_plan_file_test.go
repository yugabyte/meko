package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/env/reap"
)

type planned struct{ p *reap.Plan }

func (x planned) ReapPlan() *reap.Plan { return x.p }

// The file is the whole mechanism: a run that is killed before its teardown
// leaves it behind, and a separate process reclaims what it names.
func TestReapPlanFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "reap-plan.json")
	want := &reap.Plan{Namespaces: []reap.NSReap{
		{Kind: reap.KindYBAStack, Namespace: "ampqa-yba-abc", CRBs: []string{"x-ampqa-yba-abc"}, HelmRelease: "r"},
		{Kind: reap.KindAmp, Namespace: "ampqa-cp-abc"},
	}}
	writeReapPlan(path, planned{want}, func(string, ...any) {})

	got, err := loadReapPlanFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Namespaces) != 2 || got.Namespaces[0].Namespace != "ampqa-yba-abc" ||
		got.Namespaces[0].CRBs[0] != "x-ampqa-yba-abc" || got.Namespaces[1].Kind != reap.KindAmp {
		t.Errorf("round-tripped plan lost detail: %+v", got.Namespaces)
	}

	// Teardown succeeded: the file must go, or every later build reaps a
	// namespace that no longer exists and reports a leak that is not one.
	dropReapPlan(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("plan file survived a successful teardown")
	}
}

// A provider with nothing to reclaim writes no file, so its absence stays a
// reliable "nothing was left behind".
func TestReapPlanFileSkippedWhenNothingToReap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reap-plan.json")
	writeReapPlan(path, planned{nil}, func(string, ...any) {})
	writeReapPlan(path, planned{&reap.Plan{}}, func(string, ...any) {})
	writeReapPlan(path, struct{}{}, func(string, ...any) {})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a plan file was written for a provider with nothing to reap")
	}
}
