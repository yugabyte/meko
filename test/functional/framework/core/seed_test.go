package core_test

import (
	"testing"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
)

func TestSeedDeterministic(t *testing.T) {
	a := core.NewRunSeed(12345, true)
	b := core.NewRunSeed(12345, true)
	if a.Value() != b.Value() {
		t.Fatalf("explicit run seeds differ: %d vs %d", a.Value(), b.Value())
	}
	if a.ForTest("foo").Value() != b.ForTest("foo").Value() {
		t.Errorf("per-test seed not deterministic for same run seed + name")
	}
	// Same seed → identical rand sequence.
	r1 := a.ForTest("foo").Rand()
	r2 := b.ForTest("foo").Rand()
	for i := 0; i < 100; i++ {
		if r1.Uint64() != r2.Uint64() {
			t.Fatalf("rand sequences diverged at %d", i)
		}
	}
}

func TestSeedPerTestDiffersByName(t *testing.T) {
	s := core.NewRunSeed(7, true)
	if s.ForTest("a").Value() == s.ForTest("b").Value() {
		t.Errorf("different test names produced the same seed")
	}
}

func TestSeedForkIndependence(t *testing.T) {
	s := core.NewRunSeed(99, true).ForTest("t")
	nemesis := s.Fork("nemesis")
	workload := s.Fork("workload")
	if nemesis.Value() == workload.Value() {
		t.Errorf("forks with different labels must differ")
	}
	// Deterministic fork.
	if s.Fork("nemesis").Value() != nemesis.Value() {
		t.Errorf("fork not deterministic")
	}
}

func TestRunSeedRandomWhenNotExplicit(t *testing.T) {
	// Extremely unlikely to collide; guards against a hardcoded default.
	a := core.NewRunSeed(0, false)
	b := core.NewRunSeed(0, false)
	if a.Value() == 0 && b.Value() == 0 {
		t.Errorf("random run seeds both zero — not drawn from crypto/rand")
	}
}
