package consumer

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"meko/event-consumers/datapack-metrics-consumer/internal/stats"
	"meko/event-consumers/datapack-metrics-consumer/internal/store"
)

func TestOnlyFaultsOfTheEventArePoison(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		poison bool
	}{
		{"handler error", errors.New("bad payload shape"), true},
		{"bad data", &pq.Error{Code: "22P02"}, true},
		{"wrapped bad data", fmt.Errorf("insert: %w", &pq.Error{Code: "22003"}), true},
		{"unique violation", &pq.Error{Code: "23505"}, true},
		{"serialization conflict", &pq.Error{Code: "40001"}, false},
		{"deadlock", &pq.Error{Code: "40P01"}, false},
		{"query canceled", &pq.Error{Code: "57014"}, false},
		{"undefined table", &pq.Error{Code: "42P01"}, false},
		{"permission denied", &pq.Error{Code: "42501"}, false},
		{"connection failure", &pq.Error{Code: "08006"}, false},
		{"raise exception", &pq.Error{Code: "P0001"}, false},
		{"bad conn", driver.ErrBadConn, false},
		{"conn done", sql.ErrConnDone, false},
		{"deadline", context.DeadlineExceeded, false},
		{"canceled", context.Canceled, false},
		{"eof", io.EOF, false},
		{"network", &net.OpError{Op: "read", Err: errors.New("reset")}, false},
	}
	for _, tc := range cases {
		if got := IsPoison(tc.err); got != tc.poison {
			t.Errorf("%s: IsPoison = %v, want %v", tc.name, got, tc.poison)
		}
	}
}

func ev(typ string, n int) store.Event { return store.Event{ID: fmt.Sprint(n), Type: typ} }

func TestDispatchCallsEachHandlerOnceInOrder(t *testing.T) {
	var seen [][]string
	reg := NewRegistry()
	_ = reg.Register("search.completed", func(_ context.Context, _ *sql.Tx, es []store.Event) error {
		var ids []string
		for _, e := range es {
			ids = append(ids, e.ID)
		}
		seen = append(seen, ids)
		return nil
	})
	counts, err := dispatch(context.Background(), reg, nil,
		[]store.Event{ev("search.completed", 0), ev("search.completed", 2), ev("search.completed", 3)})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || fmt.Sprint(seen[0]) != "[0 2 3]" {
		t.Fatalf("handler calls = %v", seen)
	}
	if counts["search.completed"] != 3 {
		t.Fatalf("counts = %v", counts)
	}
}

func TestAnEventTypeWithNoHandlerStopsTheBatchBeforeAnyHandlerRuns(t *testing.T) {
	ran := false
	reg := NewRegistry()
	_ = reg.Register("search.completed", func(context.Context, *sql.Tx, []store.Event) error { ran = true; return nil })
	_, err := dispatch(context.Background(), reg, nil,
		[]store.Event{ev("search.completed", 0), ev("other.thing", 1)})
	if !errors.Is(err, ErrNoHandler) {
		t.Fatalf("err = %v, want ErrNoHandler", err)
	}
	if ran {
		t.Fatal("a handler ran for a batch that cannot be applied whole")
	}
	if IsPoison(err) {
		t.Fatal("an unknown type is not the event's fault: it must not be dead-lettered")
	}
}

func TestDispatchCountsNothingItself(t *testing.T) {
	before := testutil.ToFloat64(stats.Processed.WithLabelValues("search.completed"))
	reg := NewRegistry()
	_ = reg.Register("search.completed", func(context.Context, *sql.Tx, []store.Event) error { return nil })
	if _, err := dispatch(context.Background(), reg, nil, []store.Event{ev("search.completed", 0)}); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(stats.Processed.WithLabelValues("search.completed")); got != before {
		t.Fatalf("dispatch counted %v events before the commit", got-before)
	}
}

func TestAHandlerErrorPropagatesSoTheBatchRollsBack(t *testing.T) {
	reg := NewRegistry()
	boom := errors.New("boom")
	_ = reg.Register("a", func(context.Context, *sql.Tx, []store.Event) error { return boom })
	if _, err := dispatch(context.Background(), reg, nil, []store.Event{ev("a", 0)}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestAHandlerPanicBecomesAPoisonError(t *testing.T) {
	reg := NewRegistry()
	_ = reg.Register("a", func(context.Context, *sql.Tx, []store.Event) error { panic("index out of range") })
	_, err := dispatch(context.Background(), reg, nil, []store.Event{ev("a", 0)})
	if err == nil || !IsPoison(err) {
		t.Fatalf("err = %v, poison = %v", err, IsPoison(err))
	}
}

func TestATypeHasOnlyOneHandler(t *testing.T) {
	reg := NewRegistry()
	h := func(context.Context, *sql.Tx, []store.Event) error { return nil }
	if err := reg.Register("a", h); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register("a", h); err == nil {
		t.Fatal("a second handler for the same type was accepted")
	}
}

func TestErrorTextForLogsNamesTheClassAndStateNotTheMessage(t *testing.T) {
	err := fmt.Errorf("apply: %w", &pq.Error{Code: "22P02", Message: `invalid input syntax: "my secret prompt"`})
	if sqlState(err) != "22P02" || errClass(err) != "*pq.Error" {
		t.Fatalf("state=%q class=%q", sqlState(err), errClass(err))
	}
}
