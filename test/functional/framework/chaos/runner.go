package chaos

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
)

// Runner is the NemesisLoop: it drives a seed-derived, reproducible sequence of
// chaos actions against the test's targets in the background while a workload
// runs, and guarantees every injected fault is recovered — after its hold, on
// Stop, on a panic, and even if the run is cancelled mid-inject.
//
// Lifecycle: NewRunner (build + validate) → Start (launch background loop) →
// Stop (halt and recover everything). The loop is rooted in context.Background()
// so Stop can cancel in-flight injections while recovery still runs to
// completion under its own background-rooted, time-bounded context — mirroring
// the framework's teardown discipline (AGENTS.md §5).
type Runner struct {
	seed    *core.Seed
	cfg     Config
	actions []ChaosAction
	targets []*Target

	mu      sync.Mutex
	started bool
	active  map[*fault]struct{}

	injCtx    context.Context
	cancelInj context.CancelFunc
	stopCh    chan struct{}
	doneCh    chan struct{}
}

// fault is one injected-but-not-yet-recovered disruption. recovered is guarded
// by Runner.mu so the loop and the Stop sweep never recover it twice.
type fault struct {
	action    ChaosAction
	target    *Target
	recovered bool
}

// Planned is one entry of a previewed chaos sequence (names + timing), used to
// verify seed reproducibility without executing anything.
type Planned struct {
	Action string
	Target string
	Hold   time.Duration
	Gap    time.Duration
}

// NewRunner builds a runner: it resolves cfg defaults, materializes the selected
// actions (cfg.Actions, or every registered action in stable order), and checks
// there is at least one action and one target. reg may be nil (DefaultRegistry).
func NewRunner(seed *core.Seed, reg *Registry, cfg Config, targets []*Target) (*Runner, error) {
	if seed == nil {
		return nil, fmt.Errorf("chaos: nil seed")
	}
	if reg == nil {
		reg = DefaultRegistry()
	}
	cfg = cfg.withDefaults()

	names := cfg.Actions
	if len(names) == 0 {
		names = reg.Names()
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("chaos: no actions configured")
	}
	actions := make([]ChaosAction, 0, len(names))
	for _, n := range names {
		a, err := reg.Build(n, cfg)
		if err != nil {
			return nil, err
		}
		actions = append(actions, a)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("chaos: no targets")
	}
	return &Runner{
		seed:    seed,
		cfg:     cfg,
		actions: actions,
		targets: targets,
		active:  map[*fault]struct{}{},
	}, nil
}

// Plan previews the first count steps of the loop's action sequence. It draws
// from a fresh RNG seeded exactly as the live loop does, so it reflects what
// Start would run — same seed ⇒ same sequence.
func (r *Runner) Plan(count int) []Planned {
	rng := r.rng()
	out := make([]Planned, 0, count)
	for i := 0; i < count; i++ {
		p := r.draw(rng)
		out = append(out, Planned{
			Action: r.actions[p.actionIdx].Name(),
			Target: r.targets[p.targetIdx].name,
			Hold:   p.hold,
			Gap:    p.gap,
		})
	}
	return out
}

// Start validates target ownership and launches the background loop. It errors
// (injecting nothing) if any target is not owned by this test.
func (r *Runner) Start() error {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return fmt.Errorf("chaos: runner already started")
	}
	r.mu.Unlock()

	if err := r.validate(); err != nil {
		return err
	}

	r.injCtx, r.cancelInj = context.WithCancel(context.Background())
	r.stopCh = make(chan struct{})
	r.doneCh = make(chan struct{})

	r.mu.Lock()
	r.started = true
	r.mu.Unlock()

	go r.loop()
	return nil
}

// Stop halts the loop and guarantees every still-applied fault is recovered.
// It cancels in-flight injections, waits for the loop to exit (which recovers
// its own current fault), then sweeps any residual faults as a backstop. Safe to
// call more than once. Idempotent recovery means the sweep never double-reverts.
func (r *Runner) Stop() {
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return
	}
	r.started = false
	r.mu.Unlock()

	close(r.stopCh)
	r.cancelInj()
	<-r.doneCh

	r.mu.Lock()
	remaining := make([]*fault, 0, len(r.active))
	for f := range r.active {
		remaining = append(remaining, f)
	}
	r.mu.Unlock()
	for _, f := range remaining {
		r.recoverFault(f)
	}
}

// validate enforces the ownership guard: pod targets must live in a namespace
// matching the owner prefix. Host targets are caller-asserted owned (there is no
// namespace to check), matching how the SSH transport is only ever handed a
// test-provisioned host.
func (r *Runner) validate() error {
	for _, t := range r.targets {
		if t.kind == KindPod {
			if t.namespace == "" || !strings.HasPrefix(t.namespace, r.cfg.OwnerPrefix) {
				return fmt.Errorf("chaos: refusing pod target %q in namespace %q (must match owner prefix %q)",
					t.name, t.namespace, r.cfg.OwnerPrefix)
			}
		}
	}
	return nil
}

// loop is the background driver. It draws each step from the seeded RNG (fixed
// consumption order → reproducible), runs a cycle, and rests for the gap,
// checking for Stop at every boundary.
func (r *Runner) loop() {
	defer close(r.doneCh)
	rng := r.rng()
	for {
		if r.stopping() {
			return
		}
		p := r.draw(rng)
		r.runCycle(p)
		if r.stopping() {
			return
		}
		r.sleepOrStop(p.gap)
	}
}

// runCycle injects one fault, holds it, and recovers it. Recovery is deferred so
// it runs on every exit path — normal return, a panicking action, or a Stop that
// cut the hold short. A panic in Inject is contained so the loop survives.
func (r *Runner) runCycle(p drawn) {
	action := r.actions[p.actionIdx]
	target := r.targets[p.targetIdx]
	f := r.trackFault(action, target)
	defer r.recoverFault(f)

	func() {
		defer func() {
			if rec := recover(); rec != nil {
				r.observe(action.Name(), target.name, "inject", fmt.Errorf("panic: %v", rec))
			}
		}()
		err := action.Inject(r.injCtx, target)
		r.observe(action.Name(), target.name, "inject", err)
	}()

	r.sleepOrStop(p.hold)
}

// trackFault records an injected fault as active so Stop's backstop can recover
// it. Registered before Inject runs, so even a failed or partial injection is
// still guaranteed a Recover.
func (r *Runner) trackFault(action ChaosAction, target *Target) *fault {
	f := &fault{action: action, target: target}
	r.mu.Lock()
	r.active[f] = struct{}{}
	r.mu.Unlock()
	return f
}

// recoverFault reverts a fault exactly once, under a fresh background-rooted,
// time-bounded context so recovery completes even after the run was cancelled.
func (r *Runner) recoverFault(f *fault) {
	r.mu.Lock()
	if f.recovered {
		r.mu.Unlock()
		return
	}
	f.recovered = true
	delete(r.active, f)
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.RecoverTimeout)
	defer cancel()
	err := f.action.Recover(ctx, f.target)
	r.observe(f.action.Name(), f.target.name, "recover", err)
}

// drawn is one RNG draw: which action, which target, and the two durations.
type drawn struct {
	actionIdx int
	targetIdx int
	hold      time.Duration
	gap       time.Duration
}

// rng returns the loop's RNG, forked from the run seed under a stable label so
// the chaos sequence is reproducible and independent of other seed consumers.
func (r *Runner) rng() *rand.Rand { return r.seed.Fork("chaos").Rand() }

// draw consumes the RNG in a fixed order (action, target, hold, gap) so a given
// seed always yields the same sequence.
func (r *Runner) draw(rng *rand.Rand) drawn {
	return drawn{
		actionIdx: rng.IntN(len(r.actions)),
		targetIdx: rng.IntN(len(r.targets)),
		hold:      randDur(rng, r.cfg.HoldMin, r.cfg.HoldMax),
		gap:       randDur(rng, r.cfg.GapMin, r.cfg.GapMax),
	}
}

func (r *Runner) observe(action, target, phase string, err error) {
	if r.cfg.Observe != nil {
		r.cfg.Observe(Event{Action: action, Target: target, Phase: phase, Err: err, At: time.Now()})
	}
}

func (r *Runner) stopping() bool {
	select {
	case <-r.stopCh:
		return true
	default:
		return false
	}
}

// sleepOrStop waits d, returning early if Stop is signalled.
func (r *Runner) sleepOrStop(d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-r.stopCh:
	}
}

// randDur returns a duration uniformly in [min, max]. If max <= min it returns
// min.
func randDur(rng *rand.Rand, min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(rng.Int64N(int64(max-min)+1))
}
