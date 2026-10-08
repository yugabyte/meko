package core

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	GuardPodRestart = "pod_restart"
	GuardCrashLog   = "crash_log"
	GuardHTTP5xx    = "http_5xx"
	GuardErrorLogs  = "error_logs"
)

// GuardFinding is something a service of the environment did wrong while a test
// ran: a container restarted, a panic or traceback in its log, a 5xx it served.
// Fatal findings fail the test unless it tolerates their kind.
type GuardFinding struct {
	Kind     string
	Workload string
	Detail   string
	Fatal    bool
	// Count and Matches identify an http_5xx series: how many answers it
	// counted, and whether a call (method, path) a body noted is one of them.
	Count   int
	Matches func(method, path string) bool
}

func (f GuardFinding) String() string { return f.Kind + " " + f.Workload + ": " + f.Detail }

// Guarded is an environment that watches its own services. GuardMark records
// where they stand before a test body, and GuardCheck reports what went wrong
// since. On a shared environment the window covers every test running in it,
// so a finding is reported on each of them.
type Guarded interface {
	GuardMark(ctx context.Context) (any, error)
	GuardCheck(ctx context.Context, mark any) ([]GuardFinding, error)
}

const defaultGuardTimeout = 90 * time.Second

func guardEnabled(cfg *Config) bool {
	return cfg == nil || cfg.GetBool("guard.enabled", true)
}

// guardWindow is one test body's stretch on an environment. Bodies on the same
// stack (same cluster and namespace) that overlap mark each other, because a
// finding then cannot be pinned on either.
type guardWindow struct {
	key       string
	tolerates []string
	// excused are the finding kinds some overlapping body tolerates, which is
	// what makes one of them unattributable.
	excused map[string]bool
	opened  time.Time
}

type served5xx struct {
	at     time.Time
	method string
	path   string
	by     *guardWindow
}

var guardActive = struct {
	sync.Mutex
	open   map[string][]*guardWindow
	served map[string][]served5xx
}{open: map[string][]*guardWindow{}, served: map[string][]served5xx{}}

func openWindow(env Environment, tolerates []string, opened time.Time) *guardWindow {
	h := env.Handles()
	w := &guardWindow{key: h.Cluster + "/" + h.Namespace, tolerates: tolerates, excused: map[string]bool{}, opened: opened}
	guardActive.Lock()
	defer guardActive.Unlock()
	for _, o := range guardActive.open[w.key] {
		for _, k := range o.tolerates {
			w.excused[k] = true
		}
		for _, k := range w.tolerates {
			o.excused[k] = true
		}
	}
	guardActive.open[w.key] = append(guardActive.open[w.key], w)
	return w
}

// closeWindow returns the kinds overlapping bodies tolerate, and the 5xx
// answers other bodies' own calls received between w opening and cutoff.
func closeWindow(w *guardWindow, cutoff time.Time) (map[string]bool, []served5xx) {
	guardActive.Lock()
	defer guardActive.Unlock()
	open := guardActive.open[w.key]
	for i, o := range open {
		if o == w {
			open = append(open[:i], open[i+1:]...)
			guardActive.open[w.key] = open
			break
		}
	}
	var others []served5xx
	for _, s := range guardActive.served[w.key] {
		if s.by != w && !s.at.Before(w.opened) && !s.at.After(cutoff) {
			others = append(others, s)
		}
	}
	keep := guardActive.served[w.key][:0]
	for _, s := range guardActive.served[w.key] {
		for _, o := range open {
			if !s.at.Before(o.opened) {
				keep = append(keep, s)
				break
			}
		}
	}
	guardActive.served[w.key] = keep
	return w.excused, others
}

// NoteServerError pins a 5xx that a body's own call (made with ctx) got on that
// body, so the guard does not fail the bodies beside it on the stack.
func NoteServerError(ctx context.Context, method, path string) {
	t, _ := ctx.Value(tKey{}).(*T)
	if t == nil {
		return
	}
	t.mu.Lock()
	w := t.guard
	t.mu.Unlock()
	if w == nil {
		return
	}
	guardActive.Lock()
	defer guardActive.Unlock()
	if !slices.Contains(guardActive.open[w.key], w) {
		return
	}
	guardActive.served[w.key] = append(guardActive.served[w.key], served5xx{at: time.Now(), method: method, path: path, by: w})
}

func servedElsewhere(f GuardFinding, others []served5xx) bool {
	if f.Kind != GuardHTTP5xx || f.Matches == nil || f.Count <= 0 {
		return false
	}
	n := 0
	for _, s := range others {
		if f.Matches(s.method, s.path) {
			n++
		}
	}
	return n >= f.Count
}

type guardState struct {
	g       Guarded
	mark    any
	markErr error
	window  *guardWindow
}

// release closes the window of a body that never reached guardCheck: a Skip,
// FailNow or panic. Left open, it would excuse every later body on the stack.
func (st *guardState) release() {
	if st != nil && st.window != nil {
		closeWindow(st.window, time.Now())
	}
}

func guardMark(t *T, env Environment, cfg *Config, test Test) *guardState {
	g, ok := env.(Guarded)
	if !ok || test.Unguarded || !guardEnabled(cfg) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultGuardTimeout)
	defer cancel()
	mark, err := g.GuardMark(ctx)
	if err != nil {
		return &guardState{g: g, markErr: err}
	}
	// The window opens after the mark's counter read and closes (cutoff in
	// guardCheck) before the check's, so a note excuses only an answer the
	// counter delta includes.
	w := openWindow(env, test.Tolerates, time.Now())
	t.mu.Lock()
	t.guard = w
	t.mu.Unlock()
	return &guardState{g: g, mark: mark, window: w}
}

// guardCheck runs on its own deadline, like Collect, so a body that spent its
// timeout is still checked. A fatal finding is recorded as concurrent instead
// of failing the test only when an overlapping body on the same stack
// tolerates that kind, because that body may have caused it, or when the 5xx
// answers were all received by overlapping bodies' own calls.
func guardCheck(t *T, test Test, st *guardState) error {
	if st == nil {
		return nil
	}
	if st.markErr != nil {
		return t.Step("service_guard", func(*StepHandle) error {
			return fmt.Errorf("guard could not mark the services: %w", st.markErr)
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultGuardTimeout)
	defer cancel()
	cutoff := time.Now()
	findings, err := st.g.GuardCheck(ctx, st.mark)
	excused, others := closeWindow(st.window, cutoff)
	return t.Step("service_guard", func(h *StepHandle) error {
		if err != nil {
			return fmt.Errorf("guard could not check the services: %w", err)
		}
		var fatal, concurrent, other []string
		for _, f := range findings {
			switch {
			case !f.Fatal || slices.Contains(test.Tolerates, f.Kind):
				other = append(other, f.String())
			case excused[f.Kind]:
				concurrent = append(concurrent, f.String())
			case servedElsewhere(f, others):
				concurrent = append(concurrent, f.String()+", all to an overlapping test's own calls")
			default:
				fatal = append(fatal, f.String())
			}
		}
		if len(other) > 0 {
			h.Set("findings", strings.Join(other, "\n"))
		}
		if len(concurrent) > 0 {
			h.Set("concurrent", strings.Join(concurrent, "\n"))
		}
		if len(fatal) > 0 {
			h.Set("fatal", strings.Join(fatal, "\n"))
			return fmt.Errorf("services misbehaved during the test: %s", strings.Join(fatal, "; "))
		}
		return nil
	})
}
