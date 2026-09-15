package stress

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestStallWatchMeasuresASpanWithNoAcknowledgedCommit(t *testing.T) {
	var written atomic.Int64
	frozen := make(chan struct{})
	w := newStallWatch(context.Background(), 5*time.Millisecond, func() int64 {
		select {
		case <-frozen:
		default:
			written.Add(1)
		}
		return written.Load()
	})
	time.Sleep(50 * time.Millisecond)
	close(frozen)
	time.Sleep(300 * time.Millisecond)
	if got := w.Longest(); got < 200*time.Millisecond {
		t.Errorf("longest commit-free span = %s, want at least most of the 300ms freeze", got)
	}
}

func TestStallWatchStaysNearTheSamplingIntervalWhileCommitsLand(t *testing.T) {
	var written atomic.Int64
	w := newStallWatch(context.Background(), 5*time.Millisecond, func() int64 { return written.Add(1) })
	time.Sleep(400 * time.Millisecond)
	if got := w.Longest(); got > 200*time.Millisecond {
		t.Errorf("longest commit-free span = %s, want it near the 5ms sampling interval", got)
	}
}
