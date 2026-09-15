package sched

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// Defaults for the scheduler knobs.
const (
	DefaultCapacity          = 8
	DefaultPerEnvParallelism = 4
)

// Options configures a scheduled run. It embeds core.RunOptions (Parallel, Seed,
// Config, Provider, Sink, DetectedVersions, timeouts, ProviderUnavailableReason)
// and adds the packing knobs.
type Options struct {
	core.RunOptions
	Capacity          int // total lease units (concurrent environments)
	PerEnvParallelism int // co-tenant tests per SHARED environment
}

// shapeDefaultWeight is the lease weight a shape costs when a test does not set
// EnvSpec.Weight explicitly and config carries no override.
func shapeDefaultWeight(s core.Shape) int {
	switch s {
	case core.DedicatedStack:
		return 2
	default:
		return 1
	}
}

func weightFor(spec core.EnvSpec, cfg *core.Config) int {
	if spec.Weight > 0 {
		return spec.Weight
	}
	def := shapeDefaultWeight(spec.Shape)
	if cfg != nil {
		return cfg.GetInt("sched.weights."+spec.Shape.String(), def)
	}
	return def
}

// Run drives tests through the packing scheduler: SHARED tests pack onto a
// bounded set of shared environments (up to PerEnvParallelism co-tenants each);
// EXCLUSIVE tests each take their own lease + environment. Both kinds run
// concurrently, bounded by the lease capacity (environments) and the overall
// Parallel slot count (running test bodies). Results are returned in input
// order.
func Run(ctx context.Context, tests []core.Test, opts Options) (core.RunSummary, []*core.Result) {
	if opts.Seed == nil {
		opts.Seed = core.NewRunSeed(0, false)
	}
	parallel := opts.Parallel
	if parallel < 1 {
		parallel = 1
	}
	capacity := opts.Capacity
	if capacity < 1 {
		capacity = DefaultCapacity
	}
	perEnv := opts.PerEnvParallelism
	if perEnv < 1 {
		perEnv = DefaultPerEnvParallelism
	}

	started := time.Now()
	if opts.Sink != nil {
		_ = opts.Sink.RunStarted(core.RunInfo{
			Seed:      opts.Seed.Value(),
			Total:     len(tests),
			Parallel:  parallel,
			StartedAt: started,
		})
	}

	s := &scheduler{
		tests:    tests,
		results:  make([]*core.Result, len(tests)),
		opts:     opts,
		mgr:      NewManager(capacity),
		slots:    make(chan struct{}, parallel),
		capacity: capacity,
		perEnv:   perEnv,
	}
	// Arming costs every test an uncontended RLock; only pay it when there is
	// shared infrastructure and a test that faults it.
	if core.ProviderSharesInfra(opts.Provider) {
		for i := range tests {
			if tests[i].Env.FaultsShared() {
				s.infraGateArmed = true
				break
			}
		}
	}

	var shared, exclusive []int
	for i := range tests {
		if tests[i].Env.Sharing == core.Shared {
			shared = append(shared, i)
		} else {
			exclusive = append(exclusive, i)
		}
	}

	var wg sync.WaitGroup
	for _, idx := range exclusive {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.results[i] = s.runExclusive(ctx, i)
		}(idx)
	}
	if len(shared) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.runShared(ctx, shared)
		}()
	}
	wg.Wait()

	summary := core.RunSummary{
		Counts:    map[core.Status]int{},
		Total:     len(s.results),
		StartedAt: started,
		EndedAt:   time.Now(),
	}
	for _, r := range s.results {
		if r != nil {
			summary.Counts[r.Status]++
		}
	}
	if opts.Sink != nil {
		_ = opts.Sink.RunFinished(summary)
	}
	return summary, s.results
}

type scheduler struct {
	tests    []core.Test
	results  []*core.Result
	opts     Options
	mgr      *Manager
	slots    chan struct{}
	capacity int
	perEnv   int

	// infraGate: a test that faults shared infrastructure takes the write side,
	// every other test the read side.
	infraGate      sync.RWMutex
	infraGateArmed bool

	// poison: a faulting test left the shared stack unserving past its recovery
	// budget; later tests fail fast with the culprit's name.
	poisonMu  sync.Mutex
	poisonBy  string
	poisonErr error

	// sharedLocks serialize the tests that take the same control-plane-global
	// resource, one mutex per EnvSpec.SharedLock key.
	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

// enterSharedLock serializes tests declaring the same SharedLock key.
//
// Taken after the infra gate and before the lease and the slot, for the same
// reason: a waiter holding either would deadlock the gate's writer.
func (s *scheduler) enterSharedLock(test core.Test) func() {
	key := test.Env.Locks()
	if key == "" {
		return func() {}
	}
	s.locksMu.Lock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	mu, ok := s.locks[key]
	if !ok {
		mu = &sync.Mutex{}
		s.locks[key] = mu
	}
	s.locksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// enterInfraGate takes this test's side of the gate and returns the release.
//
// Must be taken BEFORE the lease and the slot: a waiter holding either while
// blocked here deadlocks the writer, which needs both.
func (s *scheduler) enterInfraGate(test core.Test) func() {
	if !s.infraGateArmed || test.Env.Dedicated() {
		return func() {}
	}
	if test.Env.FaultsShared() {
		s.infraGate.Lock()
		return s.infraGate.Unlock
	}
	s.infraGate.RLock()
	return s.infraGate.RUnlock
}

// leaseWeight resolves a spec's lease weight, clamped to total capacity so a
// single environment can always eventually be admitted.
func (s *scheduler) leaseWeight(spec core.EnvSpec) int {
	w := weightFor(spec, s.opts.Config)
	if w > s.capacity {
		w = s.capacity
	}
	return w
}

func (s *scheduler) provisionTimeout() time.Duration {
	if s.opts.ProvisionTimeout > 0 {
		return s.opts.ProvisionTimeout
	}
	return core.ResolveProvisionTimeout(s.opts.Config)
}

func (s *scheduler) teardownTimeout() time.Duration {
	if s.opts.TeardownTimeout > 0 {
		return s.opts.TeardownTimeout
	}
	return core.ResolveTeardownTimeout(s.opts.Config)
}

// acquireSlot takes an overall-concurrency slot for a running body. It is only
// taken once an environment is available (never while blocked on a lease), so a
// slot held while waiting cannot deadlock the lease manager.
func (s *scheduler) acquireSlot(ctx context.Context) bool {
	select {
	case s.slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *scheduler) releaseSlot() { <-s.slots }

// runExclusive acquires a lease, provisions its own environment (inside
// core.RunOne), and runs the test alone.
func (s *scheduler) runExclusive(ctx context.Context, i int) *core.Result {
	test := s.tests[i]
	defer s.enterInfraGate(test)()
	defer s.enterSharedLock(test)()
	if res := s.failIfInfraPoisoned(test); res != nil {
		return res
	}
	lease, err := s.mgr.Acquire(ctx, s.leaseWeight(test.Env))
	if err == nil {
		defer lease.Release()
	}
	// If Acquire failed it was ctx cancellation; RunOne then surfaces the dead
	// context as an env_failed during provisioning — no separate path needed.
	if s.acquireSlot(ctx) {
		defer s.releaseSlot()
	}
	res := core.RunWithEnvRetry(ctx, test, s.opts.RunOptions, core.RunOne)
	s.noteInfraOutcome(ctx, test, res)
	return res
}

// noteInfraOutcome, still under the write gate, poisons the run when a failed
// faulting test's shared stack does not serve within the recovery budget; a
// passed result needs no probe, verifySharedInfra was its final step.
func (s *scheduler) noteInfraOutcome(ctx context.Context, test core.Test, res *core.Result) {
	if !s.infraGateArmed || !test.Env.FaultsShared() || ctx.Err() != nil {
		return
	}
	if res != nil && res.Status == core.StatusPassed {
		return
	}
	hc, ok := s.opts.Provider.(core.SharedInfraHealthChecker)
	if !ok {
		return
	}
	if err := hc.SharedInfraHealthy(context.Background()); err != nil {
		s.poisonMu.Lock()
		s.poisonBy, s.poisonErr = test.Name, err
		s.poisonMu.Unlock()
	}
}

func (s *scheduler) failIfInfraPoisoned(test core.Test) *core.Result {
	if test.Env.Dedicated() {
		return nil
	}
	s.poisonMu.Lock()
	by, err := s.poisonBy, s.poisonErr
	s.poisonMu.Unlock()
	if by == "" {
		return nil
	}
	return core.ReportResult(test, s.opts.RunOptions, core.StatusEnvFailed,
		fmt.Sprintf("shared control plane did not recover after %s: %v", by, err))
}

// runShared packs the shared tests onto at most ceil(N/perEnv) environments.
// Each environment worker lazily provisions on its first pulled item, runs up to
// perEnv co-tenants concurrently on that one environment, drains more work when
// its batch finishes, and tears the environment down exactly once when the queue
// is empty.
func (s *scheduler) runShared(ctx context.Context, idxs []int) {
	if s.opts.Provider == nil {
		for _, i := range idxs {
			s.results[i] = s.noProviderResult(s.tests[i])
		}
		return
	}

	queue := make(chan int, len(idxs))
	for _, i := range idxs {
		queue <- i
	}
	close(queue)

	numEnvs := (len(idxs) + s.perEnv - 1) / s.perEnv
	if numEnvs > len(idxs) {
		numEnvs = len(idxs)
	}

	var wg sync.WaitGroup
	for e := 0; e < numEnvs; e++ {
		wg.Add(1)
		go func(envN int) {
			defer wg.Done()
			s.sharedEnvWorker(ctx, envN, queue)
		}(e)
	}
	wg.Wait()
}

func (s *scheduler) sharedEnvWorker(ctx context.Context, envN int, queue <-chan int) {
	var (
		lease    *Lease
		env      core.Environment
		provErr  error
		provided bool
	)
	defer func() {
		if env != nil {
			tctx, tcancel := context.WithTimeout(context.Background(), s.teardownTimeout())
			_ = s.opts.Provider.Teardown(tctx, env)
			tcancel()
		}
		if lease != nil {
			lease.Release()
		}
	}()

	var batch sync.WaitGroup
	inFlight := make(chan struct{}, s.perEnv) // co-tenant concurrency within this env

	// Every co-tenant reports the shared provision's duration: they all waited
	// for it, and the portal wants the distribution per test.
	runOpts := s.opts.RunOptions
	for i := range queue {
		if !provided {
			provided = true
			start := time.Now()
			lease, env, provErr = s.provisionShared(ctx, envN, s.tests[i].Env)
			runOpts.ProvisionDuration = time.Since(start)
		}
		if env == nil {
			var shape *core.UnsupportedShapeError
			if errors.As(provErr, &shape) {
				s.results[i] = core.ReportResult(s.tests[i], s.opts.RunOptions, core.StatusSkipped, provErr.Error())
				continue
			}
			msg := "shared environment unavailable"
			if provErr != nil {
				msg = provErr.Error()
			}
			s.results[i] = core.ReportResult(s.tests[i], s.opts.RunOptions, core.StatusEnvFailed, msg)
			continue
		}

		inFlight <- struct{}{}
		batch.Add(1)
		go func(idx int) {
			defer batch.Done()
			defer func() { <-inFlight }()
			defer s.enterInfraGate(s.tests[idx])()
			defer s.enterSharedLock(s.tests[idx])()
			if res := s.failIfInfraPoisoned(s.tests[idx]); res != nil {
				s.results[idx] = res
				return
			}
			// The shared env is not re-provisioned, but a body's env failure retries.
			onEnv := func(c context.Context, t core.Test, o core.RunOptions) *core.Result {
				return core.RunOneOnEnv(c, t, env, o)
			}
			if !s.acquireSlot(ctx) {
				s.results[idx] = core.RunWithEnvRetry(ctx, s.tests[idx], runOpts, onEnv)
				return
			}
			defer s.releaseSlot()
			s.results[idx] = core.RunWithEnvRetry(ctx, s.tests[idx], runOpts, onEnv)
		}(i)
	}
	batch.Wait()
}

// provisionShared acquires a lease and provisions one shared environment.
func (s *scheduler) provisionShared(ctx context.Context, envN int, spec core.EnvSpec) (*Lease, core.Environment, error) {
	lease, err := s.mgr.Acquire(ctx, s.leaseWeight(spec))
	if err != nil {
		return nil, nil, err
	}
	pctx, pcancel := context.WithTimeout(ctx, s.provisionTimeout())
	env, err := s.opts.Provider.Provision(pctx, spec, s.opts.Seed.Fork(fmt.Sprintf("shared-env-%d", envN)))
	pcancel()
	if err != nil {
		return lease, nil, err
	}
	return lease, env, nil
}

func (s *scheduler) noProviderResult(test core.Test) *core.Result {
	if s.opts.ProviderUnavailableReason != "" {
		return core.ReportResult(test, s.opts.RunOptions, core.StatusSkipped, s.opts.ProviderUnavailableReason)
	}
	return core.ReportResult(test, s.opts.RunOptions, core.StatusEnvFailed, "no environment provider configured")
}
