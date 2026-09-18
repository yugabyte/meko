package stress

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// The tracker times recovery from when it is started — after the region is
// already down — and reports the first acknowledgement separately from the
// longest gap, because a slow first commit and a mid-window hiccup are
// different findings.
func TestStallTrackerReportsFirstCommitAndLongestGap(t *testing.T) {
	tenants := []tenant{{Index: 0}}
	tr := newStallTracker(tenants)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr.Start(ctx)

	// Nothing acknowledged for a beat, then a row lands.
	time.Sleep(1200 * time.Millisecond)
	atomic.AddInt64(&tenants[0].Written, 1)
	time.Sleep(1200 * time.Millisecond)

	max, first, resumed := tr.Stop()
	if !resumed {
		t.Fatal("a row was acknowledged; resumed must be true")
	}
	if first < time.Second {
		t.Errorf("first commit = %s, want at least the ~1.2s it took", first)
	}
	if max <= 0 {
		t.Errorf("longest gap = %s, want a positive measurement", max)
	}
}

// A window in which nothing is ever acknowledged reports resumed=false rather
// than a zero first-commit, which would read as instant recovery.
func TestStallTrackerNeverResumed(t *testing.T) {
	tenants := []tenant{{Index: 0}}
	tr := newStallTracker(tenants)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr.Start(ctx)
	time.Sleep(1200 * time.Millisecond)

	_, first, resumed := tr.Stop()
	if resumed {
		t.Error("no row was acknowledged; resumed must be false")
	}
	if first != 0 {
		t.Errorf("first = %s, want zero when nothing resumed", first)
	}
}
