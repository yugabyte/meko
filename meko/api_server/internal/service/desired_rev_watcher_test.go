package service

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

// fakeRevs serves change tokens from a map and records each batched read.
type fakeRevs struct {
	revs  map[string]int64
	reads [][]string
	mu    sync.Mutex
	fail  int // fail this many reads before serving
}

func (f *fakeRevs) DesiredRevs(envs []string) (map[string]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	got := append([]string(nil), envs...)
	sort.Strings(got)
	f.reads = append(f.reads, got)
	if f.fail > 0 {
		f.fail--
		return nil, errors.New("connection reset")
	}
	out := make(map[string]int64, len(f.revs))
	for k, v := range f.revs {
		out[k] = v
	}
	return out, nil
}

func (f *fakeRevs) set(env string, rev int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revs[env] = rev
}

func (f *fakeRevs) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reads)
}

func fastWatch(t *testing.T) {
	t.Helper()
	old := DesiredPollEvery
	DesiredPollEvery = 5 * time.Millisecond
	t.Cleanup(func() { DesiredPollEvery = old })
}

type waitResult struct {
	err   error
	token int64
}

func startWait(ctx context.Context, w *desiredRevWatcher, env string, since int64, d time.Duration) chan waitResult {
	out := make(chan waitResult, 1)
	go func() {
		token, err := w.wait(ctx, env, since, d)
		out <- waitResult{err: err, token: token}
	}()
	return out
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// Every held read on the replica shares one query per tick, and a write wakes only the
// reads for the environment it moved.
func TestDesiredRevWatcher_OneQueryServesEveryHeldRead(t *testing.T) {
	fastWatch(t)
	revs := &fakeRevs{revs: map[string]int64{"a": 1, "b": 5}}
	w := newDesiredRevWatcher(revs)
	ctx := context.Background()

	a := startWait(ctx, w, "a", 1, time.Minute)
	a2 := startWait(ctx, w, "a", 1, time.Minute)
	b := startWait(ctx, w, "b", 5, time.Minute)
	waitFor(t, func() bool {
		revs.mu.Lock()
		defer revs.mu.Unlock()
		return len(revs.reads) > 0 && len(revs.reads[len(revs.reads)-1]) == 2
	}, "a read covering both environments")

	revs.mu.Lock()
	for i, envs := range revs.reads {
		if len(envs) > 2 {
			t.Errorf("read %d asked for %v: an environment is read once however many reads it holds", i, envs)
		}
	}
	revs.mu.Unlock()

	revs.set("a", 2)
	for _, ch := range []chan waitResult{a, a2} {
		if r := <-ch; r.err != nil || r.token != 2 {
			t.Errorf("a's read = %+v, want token 2", r)
		}
	}
	select {
	case r := <-b:
		t.Fatalf("b was woken by a's write: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}

	revs.set("b", 6)
	if r := <-b; r.err != nil || r.token != 6 {
		t.Errorf("b's read = %+v, want token 6", r)
	}
}

// A failed read does not fail the held requests: they keep waiting and are woken by the
// next read that succeeds.
func TestDesiredRevWatcher_AFailedReadKeepsWaiting(t *testing.T) {
	fastWatch(t)
	revs := &fakeRevs{revs: map[string]int64{"a": 2}, fail: 3}
	w := newDesiredRevWatcher(revs)

	if r := <-startWait(context.Background(), w, "a", 1, time.Minute); r.err != nil || r.token != 2 {
		t.Errorf("read = %+v, want token 2 once the reads recover", r)
	}
	if n := revs.readCount(); n < 4 {
		t.Errorf("%d reads, want the three failures retried", n)
	}
}

// An environment with no counter row reads as 0, like DesiredRev.
func TestDesiredRevWatcher_AnUnwrittenEnvironmentIsZero(t *testing.T) {
	fastWatch(t)
	w := newDesiredRevWatcher(&fakeRevs{revs: map[string]int64{}})
	if r := <-startWait(context.Background(), w, "new", 3, time.Minute); r.err != nil || r.token != 0 {
		t.Errorf("read = %+v, want token 0", r)
	}
}

// A hung-up agent or a wait that runs out leaves nothing behind, and with nothing held
// the loop stops: an idle replica issues no queries.
func TestDesiredRevWatcher_StopsWhenNothingIsHeld(t *testing.T) {
	fastWatch(t)
	revs := &fakeRevs{revs: map[string]int64{"a": 1}}
	w := newDesiredRevWatcher(revs)

	ctx, cancel := context.WithCancel(context.Background())
	hungUp := startWait(ctx, w, "a", 1, time.Minute)
	timedOut := startWait(context.Background(), w, "a", 1, 20*time.Millisecond)
	if r := <-timedOut; r.err != nil || r.token != 1 {
		t.Errorf("timed-out read = %+v, want its since back", r)
	}
	cancel()
	if r := <-hungUp; !errors.Is(r.err, context.Canceled) {
		t.Errorf("hung-up read = %+v, want context.Canceled", r)
	}

	waitFor(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return !w.running && len(w.waiters) == 0
	}, "the loop to stop")
	n := revs.readCount()
	time.Sleep(30 * time.Millisecond)
	if got := revs.readCount(); got != n {
		t.Errorf("%d reads after the last hold ended, want none", got-n)
	}

	// A new hold starts the loop again.
	revs.set("a", 4)
	if r := <-startWait(context.Background(), w, "a", 1, time.Minute); r.token != 4 {
		t.Errorf("read after a restart = %+v, want token 4", r)
	}
}

// gatedRevs blocks its first read until released, so a test can register holds while a
// query is in flight.
type gatedRevs struct {
	*fakeRevs
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

// The first read's answer is what the store held when the query began, as a real
// query's would be, however the store changes while it is blocked.
func (g *gatedRevs) DesiredRevs(envs []string) (map[string]int64, error) {
	first := false
	g.once.Do(func() { first = true })
	revs, err := g.fakeRevs.DesiredRevs(envs)
	if first {
		close(g.entered)
		<-g.release
	}
	return revs, err
}

// A hold registered while a query is in flight is judged on the next tick, not against
// that query: its environment may be missing from the result (reading as 0), or its own
// token read may be newer than the query's. Either would wake it at once with a wrong
// token, costing the agent a spurious reconcile and an immediate re-read.
func TestDesiredRevWatcher_AHoldJoiningMidQueryWaitsForTheNextTick(t *testing.T) {
	fastWatch(t)
	revs := &gatedRevs{
		fakeRevs: &fakeRevs{revs: map[string]int64{"a": 1}},
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	w := newDesiredRevWatcher(revs)
	ctx := context.Background()

	first := startWait(ctx, w, "a", 1, time.Minute)
	<-revs.entered // the first query is in flight, for "a" only

	newEnv := startWait(ctx, w, "b", 7, time.Minute) // "b" is not in the in-flight query
	newer := startWait(ctx, w, "a", 5, time.Minute)  // read a token newer than the query will return
	waitFor(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.waiters["a"]) == 2 && len(w.waiters["b"]) == 1
	}, "both mid-query holds to register")
	// What the mid-query holds read before registering. The in-flight query began earlier
	// and still answers a=1, with no "b" at all.
	revs.set("b", 7)
	revs.set("a", 5)
	close(revs.release)

	if r := <-first; r.err != nil || r.token != 5 {
		t.Errorf("the snapshotted hold = %+v, want token 5", r)
	}
	select {
	case r := <-newEnv:
		t.Fatalf("a hold on an environment missing from the in-flight query woke: %+v", r)
	case r := <-newer:
		t.Fatalf("a hold with a newer token woke with an older one: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
}
