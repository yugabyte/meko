package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yugabyte/meko/meko/test/functional/framework/env/fake"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/reap"
)

type planned struct{ p *reap.Plan }

func (x planned) ReapPlan() *reap.Plan { return x.p }

// The file is the whole mechanism: a run that is killed before its teardown
// leaves it behind, and a separate process reclaims what it names.
func TestReapPlanFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "reap-plan.json")
	want := &reap.Plan{Namespaces: []reap.NSReap{
		{Kind: reap.KindYBAStack, Namespace: "mekoqa-yba-abc", CRBs: []string{"x-mekoqa-yba-abc"}, HelmRelease: "r"},
		{Kind: reap.KindYBK8s, Namespace: "mekoqa-yb-abc"},
	}}
	writeReapPlan(path, planned{want}, func(string, ...any) {})

	got, err := loadReapPlanFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Namespaces) != 2 || got.Namespaces[0].Namespace != "mekoqa-yba-abc" ||
		got.Namespaces[0].CRBs[0] != "x-mekoqa-yba-abc" || got.Namespaces[1].Kind != reap.KindYBK8s {
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

// A failed bring-up tears its stack down before the retry builds another, so
// the plan must stop naming it rather than keep the last live state.
func TestReapPlanFileRemovedWhenNothingIsLiveAnymore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reap-plan.json")
	writeReapPlan(path, planned{&reap.Plan{Namespaces: []reap.NSReap{{Kind: reap.KindYBK8s, Namespace: "mekoqa-st-a"}}}}, func(string, ...any) {})
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no plan written for a live stack: %v", err)
	}
	writeReapPlan(path, planned{nil}, func(string, ...any) {})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the plan still names a stack that was torn down")
	}
}

type plannedProvider struct {
	*fake.Provider
	plan *reap.Plan
}

func (p *plannedProvider) ReapPlan() *reap.Plan { return p.plan }

// A provider closed after a failed bring-up can leave its stack behind. The
// provider built to carry on must not erase that from the file.
func TestReapPlansKeepWhatAClosedProviderLeft(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reap-plan.json")
	plans := &reapPlans{path: path, logf: func(string, ...any) {}}
	old := &plannedProvider{Provider: &fake.Provider{}, plan: &reap.Plan{Namespaces: []reap.NSReap{{Kind: reap.KindYBK8s, Namespace: "mekoqa-st-a"}}}}
	plans.wire(old)
	plans.wire(&plannedProvider{Provider: &fake.Provider{}})
	plans.write()

	got, err := loadReapPlanFile(path)
	if err != nil {
		t.Fatalf("the rebuilt provider dropped the plan file: %v", err)
	}
	if len(got.Namespaces) != 1 || got.Namespaces[0].Namespace != "mekoqa-st-a" {
		t.Errorf("plan = %+v, want the closed provider's mekoqa-st-a", got.Namespaces)
	}
}

func TestReapPlansDropTheFileWhenTheClosedProviderCleanedUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reap-plan.json")
	plans := &reapPlans{path: path, logf: func(string, ...any) {}}
	old := &plannedProvider{Provider: &fake.Provider{}, plan: &reap.Plan{Namespaces: []reap.NSReap{{Kind: reap.KindYBK8s, Namespace: "mekoqa-st-a"}}}}
	plans.wire(old)
	old.plan = nil
	plans.wire(&plannedProvider{Provider: &fake.Provider{}})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the plan still names a stack its provider tore down")
	}
}
