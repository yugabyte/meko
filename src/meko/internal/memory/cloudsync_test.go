package memory

import (
	"errors"
	"testing"
	"time"
)

// A sync starts only once the writes already under way have finished; after that, writes are refused.
func TestBeginSyncWaitsForWritesUnderWay(t *testing.T) {
	s := &Service{}
	const dp = "7c1e2f30-9a4b-4c5d-8e6f-0a1b2c3d4a90"
	done, err := s.writing(dp)
	if err != nil {
		t.Fatal(err)
	}
	began := make(chan error)
	go func() { began <- s.BeginSync(dp) }()
	select {
	case <-began:
		t.Fatal("the sync began while a write was under way")
	case <-time.After(50 * time.Millisecond):
	}
	done()
	if err = <-began; err != nil {
		t.Fatal(err)
	}
	if _, err = s.writing(dp); !errors.Is(err, ErrDatapackSyncing) {
		t.Errorf("a write after the sync began: %v", err)
	}
	if err = s.BeginSync(dp); !errors.Is(err, ErrDatapackSyncing) {
		t.Errorf("a second sync: %v", err)
	}
	s.EndSync(dp)
	if done, err = s.writing(dp); err != nil {
		t.Errorf("a write after the sync ended: %v", err)
	} else {
		done()
	}
}
