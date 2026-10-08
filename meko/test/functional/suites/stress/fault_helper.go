package stress

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoload"
	"github.com/yugabyte/meko/meko/test/functional/framework/chaos"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	execk8s "github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/loadtest"
)

// workload names a component the way a fault finds it: the pods a selector
// lists whose name starts with prefix (the rag workers share one selector), and
// the Deployment or StatefulSet that brings them back.
type workload struct {
	name     string
	kube     *execk8s.Kubectl
	selector string
	prefix   string
	owner    string // deployment/<name> or statefulset/<name>
	settle   time.Duration
}

func (w workload) pods(ctx context.Context) ([]string, error) {
	raw, err := w.kube.GetSelector(ctx, "pods", w.selector, `{range .items[*]}{.metadata.name}{"\n"}{end}`)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range strings.Fields(raw) {
		if strings.HasPrefix(p, w.prefix) {
			out = append(out, p)
		}
	}
	return out, nil
}

// ready says every replica the owner wants is ready and gone is not among them.
func (w workload) ready(ctx context.Context, gone string) (bool, error) {
	kind, name, _ := strings.Cut(w.owner, "/")
	want, err := w.kube.Get(ctx, kind, name, "{.spec.replicas}")
	if err != nil {
		return false, err
	}
	ready, err := w.kube.Get(ctx, kind, name, "{.status.readyReplicas}")
	if err != nil {
		return false, err
	}
	pods, err := w.pods(ctx)
	if err != nil {
		return false, err
	}
	for _, p := range pods {
		if p == gone {
			return false, nil
		}
	}
	return strings.TrimSpace(want) != "" && strings.TrimSpace(want) == strings.TrimSpace(ready), nil
}

// fault is one thing done to the stack while it is under load. inject breaks
// it and returns how to undo it; undo is nil for a fault that heals itself
// (a deleted pod comes back).
type fault struct {
	name   string
	inject func(t *core.T, n int) (target string, undo func(context.Context) error, err error)
	// settled waits until the stack has healed after the undo.
	settled func(t *core.T, target string) error
}

// event is a fault as it happened.
type event struct {
	name, target string
	start, end   time.Time
	settleErr    error
}

func podDelete(o *core.Config, w workload) fault {
	return fault{
		name: "delete " + w.name + " pod",
		inject: func(t *core.T, n int) (string, func(context.Context) error, error) {
			if err := chaosAllowed(o, w.kube); err != nil {
				return "", nil, err
			}
			pods, err := w.pods(t)
			if err != nil || len(pods) == 0 {
				return "", nil, fmt.Errorf("find a %s pod (%s, %s*): %v", w.name, w.selector, w.prefix, err)
			}
			victim := pods[n%len(pods)]
			action, err := chaos.DefaultRegistry().Build(chaos.ActionPodDelete, chaos.FromConfig(o))
			if err != nil {
				return "", nil, err
			}
			return victim, nil, action.Inject(t, chaos.NewPodTarget(w.name, w.kube, victim, ""))
		},
		settled: func(t *core.T, victim string) error {
			return t.WaitFor(fmt.Sprintf("%s to have every replica back after %s was deleted", w.owner, victim), cmp.Or(w.settle, 5*time.Minute), 3*time.Second, func() (bool, error) {
				return w.ready(t, victim)
			})
		},
	}
}

func podFreeze(o *core.Config, w workload) fault {
	return fault{
		name: "freeze " + w.name,
		inject: func(t *core.T, n int) (string, func(context.Context) error, error) {
			if err := chaosAllowed(o, w.kube); err != nil {
				return "", nil, err
			}
			pods, err := w.pods(t)
			if err != nil || len(pods) == 0 {
				return "", nil, fmt.Errorf("find a %s pod: %v", w.name, err)
			}
			victim := pods[n%len(pods)]
			action, err := chaos.DefaultRegistry().Build(chaos.ActionPodFreeze, chaos.FromConfig(o))
			if err != nil {
				return "", nil, err
			}
			target := chaos.NewPodTarget(w.name, w.kube, victim, "")
			if err := action.Inject(t, target); err != nil {
				return victim, nil, err
			}
			return victim, func(ctx context.Context) error { return action.Recover(ctx, target) }, nil
		},
		settled: func(t *core.T, victim string) error {
			return t.WaitFor(w.name+" to be ready after the thaw", 3*time.Minute, 3*time.Second, func() (bool, error) {
				return w.ready(t, "")
			})
		},
	}
}

func llmFault(name string, llm *llmmock.Client, f llmmock.Fault) fault {
	return fault{
		name: name,
		inject: func(t *core.T, _ int) (string, func(context.Context) error, error) {
			id, err := llm.AddFault(t, f)
			if err != nil {
				return "", nil, err
			}
			return "llmmock " + f.Route, func(ctx context.Context) error { return llm.RemoveFault(ctx, id) }, nil
		},
	}
}

func chaosAllowed(o *core.Config, kube *execk8s.Kubectl) error {
	if owner := chaos.FromConfig(o).OwnerPrefix; !strings.Contains(kube.Namespace(), owner) {
		return fmt.Errorf("namespace %s does not carry %q, the chaos guard refuses to touch it", kube.Namespace(), owner)
	}
	return nil
}

// faultSchedule injects the faults in turn, one every interval from firstAfter,
// holds each for hold, undoes it and waits for the stack to settle. It stops
// leaving room at the end for the last one to be seen recovering.
type faultSchedule struct {
	faults     []fault
	firstAfter time.Duration
	interval   time.Duration
	hold       time.Duration
	recover    time.Duration
}

func (s faultSchedule) run(t *core.T, rig *loadtest.Rig, started time.Time, duration time.Duration) ([]event, error) {
	var events []event
	stop := started.Add(duration - s.recover - s.hold - recoveryWindow*time.Second)
	next := started.Add(s.firstAfter)
	for n := 0; next.Before(stop); n++ {
		if err := sleepUntil(t, next); err != nil {
			return events, err
		}
		f := s.faults[n%len(s.faults)]
		ev := event{name: f.name, start: time.Now()}
		target, undo, err := f.inject(t, n/len(s.faults))
		ev.target = target
		if err != nil {
			return events, fmt.Errorf("%s: %w", f.name, err)
		}
		if undo != nil {
			if err := sleepUntil(t, ev.start.Add(s.hold)); err != nil {
				_ = undo(context.Background())
				return events, err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			err := undo(ctx)
			cancel()
			if err != nil {
				return events, fmt.Errorf("undo %s on %s: %w", f.name, target, err)
			}
		}
		ev.end = time.Now()
		if f.settled != nil {
			ev.settleErr = f.settled(t, target)
		}
		events = append(events, ev)
		rig.Logf("stress: %s on %s from %s to %s, settled: %v", f.name, target, ev.start.Format("15:04:05"), ev.end.Format("15:04:05"), ev.settleErr)
		next = ev.start.Add(s.interval)
	}
	return events, nil
}

// judgeFaults reads the per-second timeline once the load has ended. After
// each fault ends the load must be healthy again within recoverWithin, the
// stack must have settled, and outside the fault windows (fault plus recovery)
// the error rate stays under quietErr.
func judgeFaults(t *core.T, load *mekoload.Load, events []event, end time.Time, recoverWithin time.Duration, recoveryErr, quietErr float64) []string {
	if len(events) == 0 {
		return []string{"no fault was injected, the duration is too short for the schedule"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tls, err := load.Timelines(ctx)
	if err != nil {
		return []string{"read the load timeline: " + err.Error()}
	}
	sec := loadtest.Seconds(tls)
	var problems []string
	var windows [][2]time.Time
	for i, ev := range events {
		until := end
		if i+1 < len(events) {
			until = events[i+1].start
		}
		label := fmt.Sprintf("fault.%d.%s", i, strings.ReplaceAll(ev.name, " ", "_"))
		rate, errs := loadtest.Damage(sec, ev.start)
		t.Metric(label+".error_rate_30s", rate*100, "%")
		t.Metric(label+".errors_30s", float64(errs), "calls")
		t.Metric(label+".held_s", ev.end.Sub(ev.start).Seconds(), "s")
		if ev.settleErr != nil {
			problems = append(problems, fmt.Sprintf("%s on %s: %v", ev.name, ev.target, ev.settleErr))
		}
		took, ok := loadtest.Recovered(sec, ev.end, until, recoveryWindow, recoveryErr)
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("after %s on %s ended at %s the load was not healthy again (%d seconds with calls under %.0f%% errors) before the next fault or the end",
				ev.name, ev.target, ev.end.Format("15:04:05"), recoveryWindow, recoveryErr*100))
			windows = append(windows, [2]time.Time{ev.start, until})
		default:
			t.Metric(label+".recovery_seconds", took.Seconds(), "s")
			if took > recoverWithin {
				problems = append(problems, fmt.Sprintf("after %s on %s the load took %s to recover, %s allowed", ev.name, ev.target, took.Round(time.Second), recoverWithin))
			}
			windows = append(windows, [2]time.Time{ev.start, ev.end.Add(took + recoveryWindow*time.Second)})
		}
	}
	ok, bad := quietSeconds(sec, windows)
	if ok+bad > 0 {
		rate := float64(bad) / float64(ok+bad)
		t.Metric("quiet.error_rate", rate*100, "%")
		if rate > quietErr {
			problems = append(problems, fmt.Sprintf("between faults the load failed %.2f%% of %d calls, %.2f%% allowed: the stack did not serve cleanly even with nothing broken",
				rate*100, ok+bad, quietErr*100))
		}
	}
	return problems
}

func quietSeconds(sec map[int64]loadtest.Second, windows [][2]time.Time) (ok, bad int64) {
	for s, b := range sec {
		inside := false
		for _, w := range windows {
			if s >= w[0].Unix() && s <= w[1].Unix() {
				inside = true
				break
			}
		}
		if !inside {
			ok += b.OK
			bad += b.Err
		}
	}
	return ok, bad
}
