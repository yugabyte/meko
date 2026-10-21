package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/store"
)

func TestCallLogRecordsAndPrunes(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "meko.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	svc := New(st, nil, Options{})
	old := store.Call{At: time.Now().Add(-CallRetention - time.Hour), Tool: "memory_add", OK: true}
	recent := store.Call{At: time.Now(), Tool: "context_search", OK: true}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	svc.RecordCall(canceled, old) // recorded even when the request is already over
	svc.RecordCall(ctx, recent)

	run, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.RunCallPruner(run)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		calls, err := st.Calls(ctx, "", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) == 1 && calls[0].Tool == "context_search" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not pruned: %+v", calls)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	<-done
}
