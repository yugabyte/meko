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
}

var guardActive = struct {
	sync.Mutex
	open map[string][]*guardWindow
}{open: map[string][]*guardWindow{}}

func openWindow(env Environment, tolerates []string) *guardWindow {
	h := env.Handles()
	w := &guardWindow{key: h.Cluster + "/" + h.Namespace, tolerates: tolerates, excused: map[string]bool{}}
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

func closeWindow(w *guardWindow) map[string]bool {
	guardActive.Lock()
	defer guardActive.Unlock()
	open := guardActive.open[w.key]
	for i, o := range open {
		if o == w {
			guardActive.open[w.key] = append(open[:i], open[i+1:]...)
			break
		}
	}
	return w.excused
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
		closeWindow(st.window)
	}
}

func guardMark(env Environment, cfg *Config, test Test) *guardState {
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
	return &guardState{g: g, mark: mark, window: openWindow(env, test.Tolerates)}
}

// guardCheck runs on its own deadline, like Collect, so a body that spent its
// timeout is still checked. A fatal finding is recorded as concurrent instead
// of failing the test only when an overlapping body on the same stack
// tolerates that kind, because that body may have caused it.
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
	findings, err := st.g.GuardCheck(ctx, st.mark)
	excused := closeWindow(st.window)
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
