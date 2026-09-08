package stress

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The gate must be the only thing that keeps two table creators apart, so the
// body blocks until every goroutine has been admitted: with the gate removed
// they all overlap and maxInside rises above 1.
func TestHoldDDLGateSerialisesTableCreators(t *testing.T) {
	const holders = 8
	r := &combinedRun{ddlGate: make(chan struct{}, 1)}
	var inside, maxInside atomic.Int32
	admitted := make(chan struct{}, holders)
	var wg sync.WaitGroup
	for range holders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := r.holdDDLGate(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			n := inside.Add(1)
			for {
				m := maxInside.Load()
				if n <= m || maxInside.CompareAndSwap(m, n) {
					break
				}
			}
			admitted <- struct{}{}
			// Wait until everyone who can be inside is inside. Serialised, this
			// is one goroutine and the wait times out; unserialised, all eight
			// arrive and every one of them sees the others.
			deadline := time.After(200 * time.Millisecond)
			for i := 0; i < holders-1; i++ {
				select {
				case <-admitted:
				case <-deadline:
					i = holders
				}
			}
			inside.Add(-1)
			release()
		}()
	}
	wg.Wait()
	if got := maxInside.Load(); got != 1 {
		t.Errorf("%d table-creating startups overlapped, want 1 — concurrent DDL is what fails the jar on a catalog version mismatch", got)
	}
}

func TestHoldDDLGateHonoursContext(t *testing.T) {
	r := &combinedRun{ddlGate: make(chan struct{}, 1)}
	held, err := r.holdDDLGate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.holdDDLGate(ctx); err == nil {
		t.Error("a cancelled run must not block forever on the gate")
	}
}

func TestHoldDDLGateReleaseIsIdempotent(t *testing.T) {
	r := &combinedRun{ddlGate: make(chan struct{}, 1)}
	release, err := r.holdDDLGate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	// A second release that drained the slot again would let two holders in.
	if _, err := r.holdDDLGate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.holdDDLGate(ctx); err == nil {
		t.Error("the slot is held, so a second holder must not be admitted")
	}
}
