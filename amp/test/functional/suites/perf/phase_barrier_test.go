package perf

import (
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// Both arms must leave a phase together: neither proceeds until the other has
// arrived, which is what makes the two measurements share cluster weather.
func TestPhaseBarrierPairsBothArms(t *testing.T) {
	b := newPhaseBarrier()
	entered := make(chan struct{})
	released := make(chan struct{})
	go func() {
		close(entered)
		b.sync(1, "load")
		close(released)
	}()
	<-entered
	select {
	case <-released:
		t.Fatal("the first arm to arrive must wait for its partner")
	case <-time.After(50 * time.Millisecond):
	}
	b.sync(1, "load")
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the partner's arrival must release the waiter")
	}
}

// A partner that died or fell behind must not stall the surviving arm for the
// whole pairWaitCap — abandon releases the iteration's waits at once.
func TestPhaseBarrierAbandonReleasesWaiters(t *testing.T) {
	b := newPhaseBarrier()
	done := make(chan struct{})
	go func() {
		b.sync(2, "fork_small")
		b.sync(2, "load") // a later phase must not block either
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	b.abandon(2)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("abandon must release current and future waits for the iteration")
	}
}

// Iterations are independent: abandoning one must not release another's pairing.
func TestPhaseBarrierAbandonIsPerIteration(t *testing.T) {
	b := newPhaseBarrier()
	released := make(chan struct{})
	go func() {
		b.sync(1, "load")
		close(released)
	}()
	time.Sleep(20 * time.Millisecond)
	b.abandon(2)
	select {
	case <-released:
		t.Fatal("abandoning iteration 2 must not release iteration 1's waiter")
	case <-time.After(50 * time.Millisecond):
	}
	b.abandon(1)
	<-released
}

// A nil barrier is the single-arm path and must be a no-op, not a panic.
func TestPhaseBarrierNilIsNoOp(t *testing.T) {
	var b *phaseBarrier
	b.sync(1, "load")
	b.abandon(1)
}

func msDatapoint(name string, v float64) core.Datapoint {
	return core.Datapoint{Name: name, Value: v, Unit: "ms", Direction: core.LowerIsBetter}
}

// The suppression rules must not swallow a real regression: a phase that is
// slower on the median AND on its best iteration still fails.
func TestLiveArmGateFailsWhenTheBestIterationRegressesToo(t *testing.T) {
	thr := float64Thresholds{DropPct: 20, RisePct: 35}
	cur := []core.Datapoint{msDatapoint("x.load", 25000)}
	base := []core.Datapoint{msDatapoint("x.load", 15000)}
	curBest := map[string]float64{"x.load": 24000}
	baseBest := map[string]float64{"x.load": 14000}
	if _, _, err := liveArmGate(cur, base, curBest, baseBest, strictPolicy(thr), "0.1.2-abc"); err == nil {
		t.Fatal("a phase slower on both the median and the best iteration must fail the gate")
	}
}

// A median that lost the node lottery while the best iteration held is weather,
// not a regression.
func TestLiveArmGateSuppressesUnregressedBestIteration(t *testing.T) {
	thr := float64Thresholds{DropPct: 20, RisePct: 35}
	cur := []core.Datapoint{msDatapoint("x.fork_small", 2438)}
	base := []core.Datapoint{msDatapoint("x.fork_small", 1212)}
	curBest := map[string]float64{"x.fork_small": 1250}
	baseBest := map[string]float64{"x.fork_small": 1200}
	if _, _, err := liveArmGate(cur, base, curBest, baseBest, strictPolicy(thr), "0.1.2-abc"); err != nil {
		t.Fatalf("a median-only regression with an unregressed best iteration must be suppressed: %v", err)
	}
}

// Percentages on one-shot ms-scale statements are noise until the absolute
// delta is meaningful.
func TestLiveArmGateNoiseFloorSuppressesTinyMsDeltas(t *testing.T) {
	thr := float64Thresholds{DropPct: 20, RisePct: 35}
	cur := []core.Datapoint{msDatapoint("x.truncate", 82.8)}
	base := []core.Datapoint{msDatapoint("x.truncate", 48.4)}
	if _, _, err := liveArmGate(cur, base, nil, nil, strictPolicy(thr), "0.1.2-abc"); err != nil {
		t.Fatalf("a +71%% violation worth 34ms must be suppressed by the noise floor: %v", err)
	}
	big := []core.Datapoint{msDatapoint("x.truncate", 48.4+liveArmNoiseFloorMS+10)}
	if _, _, err := liveArmGate(big, base, nil, nil, strictPolicy(thr), "0.1.2-abc"); err == nil {
		t.Fatal("a delta past the noise floor must still fail the gate")
	}
}
