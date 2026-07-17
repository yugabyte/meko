package core_test

import (
	"testing"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
)

func mkTest(name, suite string, tags ...string) core.Test {
	return core.Test{Name: name, Suite: suite, Tags: tags, Run: func(*core.T) error { return nil }}
}

func TestRegistryAddAndDuplicate(t *testing.T) {
	r := core.NewRegistry()
	if err := r.Add(mkTest("functional/a", "functional")); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := r.Add(mkTest("functional/a", "functional")); err == nil {
		t.Errorf("duplicate name should error")
	}
	if err := r.Add(core.Test{Name: ""}); err == nil {
		t.Errorf("empty name should error")
	}
	if _, ok := r.Get("functional/a"); !ok {
		t.Errorf("registered test not found")
	}
}

func TestRegistryFilter(t *testing.T) {
	r := core.NewRegistry()
	_ = r.Add(mkTest("functional/branch", "functional", "smoke"))
	_ = r.Add(mkTest("functional/ddl", "functional", "nightly"))
	_ = r.Add(mkTest("stress/podkill", "stress", "nightly", "chaos"))

	all, err := r.Filter("", "", nil)
	if err != nil {
		t.Fatalf("filter all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("filter all = %d, want 3", len(all))
	}

	fn, _ := r.Filter("functional", "", nil)
	if len(fn) != 2 {
		t.Errorf("suite filter = %d, want 2", len(fn))
	}

	nightly, _ := r.Filter("", "", []string{"nightly"})
	if len(nightly) != 2 {
		t.Errorf("tag filter nightly = %d, want 2", len(nightly))
	}

	chaosStress, _ := r.Filter("stress", "", []string{"nightly", "chaos"})
	if len(chaosStress) != 1 || chaosStress[0].Name != "stress/podkill" {
		t.Errorf("combined filter wrong: %v", chaosStress)
	}

	byName, _ := r.Filter("", "ddl$", nil)
	if len(byName) != 1 || byName[0].Name != "functional/ddl" {
		t.Errorf("name regex filter wrong: %v", byName)
	}

	if _, err := r.Filter("[", "", nil); err == nil {
		t.Errorf("invalid regex should error")
	}
}

func TestRegistryListSorted(t *testing.T) {
	r := core.NewRegistry()
	_ = r.Add(mkTest("z", "s"))
	_ = r.Add(mkTest("a", "s"))
	_ = r.Add(mkTest("m", "s"))
	got := r.List()
	if len(got) != 3 || got[0].Name != "a" || got[2].Name != "z" {
		t.Errorf("List not sorted: %v", got)
	}
}
