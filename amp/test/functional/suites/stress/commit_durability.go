// The boundary between "commit acknowledged" and "commit durable".
//
// The other nemesis tests hold storage to a row-count lower bound, which a lost
// acknowledged row can pass whenever a committed-but-unacknowledged row makes up
// the count. This one journals the outcome of every id it issues and afterwards
// demands the exact set: every acked id present, every present id issued.
package stress

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/chaos"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/commit_durability",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane, FaultsSharedInfra: true},
		Tags:        []string{"stress", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Journal every write's outcome while pod_delete faults hit amp's endpoint computes and the backing YugabyteDB, aiming a PostgreSQL cancel request at a seeded share of in-flight inserts plus a forced floor per writer, then recycle one tenant's endpoint and hold storage to the exact set: every acknowledged id present with its value, every present id one that was issued, and writes flowing again. A cancelled insert the server acks anyway is journaled as acked, which makes the meko#473 window reachable, not certain.",
		Params:      []string{"stress.commit_durability", "chaos"},
		Timeout:     90 * time.Minute,
		Run:         runCommitDurability,
	})
}

type commitDurabilityParams struct {
	Tenants          int
	BaselineDuration time.Duration
	Duration         time.Duration
	SettleTimeout    time.Duration
	ResumeDuration   time.Duration
	PayloadBytes     int
	// CancelProbability is the share of inserts a PostgreSQL cancel request is
	// aimed at while they are in flight, the client behaviour of meko#473.
	CancelProbability float64
	CancelDelayMax    time.Duration
}

func commitDurabilityParamsFromConfig(c *core.Config) commitDurabilityParams {
	return commitDurabilityParams{
		Tenants:           c.GetInt("stress.commit_durability.tenants", 3),
		BaselineDuration:  c.GetDuration("stress.commit_durability.baseline_duration", 20*time.Second),
		Duration:          c.GetDuration("stress.commit_durability.duration", 3*time.Minute),
		SettleTimeout:     c.GetDuration("stress.commit_durability.settle_timeout", 10*time.Minute),
		ResumeDuration:    c.GetDuration("stress.commit_durability.resume_duration", 30*time.Second),
		PayloadBytes:      c.GetInt("stress.commit_durability.payload_bytes", 256),
		CancelProbability: c.GetFloat("stress.commit_durability.cancel_probability", 0.1),
		CancelDelayMax:    c.GetDuration("stress.commit_durability.cancel_delay_max", 50*time.Millisecond),
	}
}

func runCommitDurability(t *core.T) error {
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	ampEnv, ok := t.Env.(ampKubeEnv)
	if !ok || ampEnv.AmpKube() == nil {
		t.Skip("requires a kube handle for the control-plane namespace: the endpoint computes cannot be faulted without it")
		return nil
	}
	backingEnv, ok := t.Env.(backingKubeEnv)
	if !ok || backingEnv.BackingKube() == nil {
		t.Skip("requires a kube handle for the backing's namespace: the durable side of the write path cannot be faulted without it")
		return nil
	}
	params := commitDurabilityParamsFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)

	fleet, err := newTenantFleet(t, env, logf)
	if err != nil {
		return err
	}
	defer fleet.Close()

	var journals []*AckJournal

	if err := t.Step("create_tenants", func(s *core.StepHandle) error {
		start := time.Now()
		if err := fleet.Create(t, params.Tenants); err != nil {
			return err
		}
		s.Set("tenants", len(fleet.Tenants))
		s.Set("create_seconds", time.Since(start).Seconds())
		journals = make([]*AckJournal, len(fleet.Tenants))
		for i := range journals {
			journals[i] = NewAckJournal()
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("write_baseline", func(s *core.StepHandle) error {
		runJournaledWriters(t.Context, env, fleet.Tenants, journals,
			t.Seed.Fork("baseline"), params.BaselineDuration, params, nil)
		acked := totalAcked(journals)
		s.Set("acked", acked)
		if acked == 0 {
			return fmt.Errorf("no tenant was acknowledged for a single write in %s before the faults started, so there is no promise to hold the product to",
				params.BaselineDuration)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("chaos_with_cancels", func(s *core.StepHandle) error {
		return commitDurabilityChaos(t, s, env, ampEnv, backingEnv, fleet, journals, params, logf)
	}); err != nil {
		return err
	}

	if err := t.Step("endpoints_serve_again", func(s *core.StepHandle) error {
		if err := assertEndpointsServe(t, env, fleet.Tenants, params.SettleTimeout); err != nil {
			return err
		}
		s.Set("endpoints_recovered", len(fleet.Tenants))
		return nil
	}); err != nil {
		return err
	}

	// A fresh endpoint rebuilds from the storage layer's durable frontier rather
	// than from anything the old compute held.
	if err := t.Step("force_recovery_cycle", func(s *core.StepHandle) error {
		return recycleOneEndpoint(t, s, env, fleet, params)
	}); err != nil {
		return err
	}

	if err := t.Step("verify_exact_sets", func(s *core.StepHandle) error {
		return verifyExactSets(t, s, env, fleet.Tenants, journals, params.PayloadBytes)
	}); err != nil {
		return err
	}

	return t.Step("writes_flow_again", func(s *core.StepHandle) error {
		before := totalAcked(journals)
		runJournaledWriters(t.Context, env, fleet.Tenants, journals,
			t.Seed.Fork("resume"), params.ResumeDuration, params, nil)
		delta := totalAcked(journals) - before
		s.Set("acked_after_recovery", delta)
		if delta == 0 {
			return fmt.Errorf("no tenant was acknowledged for a single write in %s after the faults stopped and every endpoint answered SELECT 1: the endpoints serve reads but no COMMIT completes",
				params.ResumeDuration)
		}
		return verifyExactSets(t, s, env, fleet.Tenants, journals, params.PayloadBytes)
	})
}

// commitDurabilityChaos deletes pods across BOTH sides of the write path — the
// endpoint computes amp deploys and the backing that holds their durable WAL —
// while journaled writers run and cancel a seeded share of inserts in flight.
func commitDurabilityChaos(t *core.T, s *core.StepHandle, env ampTenantEnv, ampEnv ampKubeEnv,
	backingEnv backingKubeEnv, fleet *tenantFleet, journals []*AckJournal,
	params commitDurabilityParams, logf func(string, ...any)) error {
	ampKube, backingKube := ampEnv.AmpKube(), backingEnv.BackingKube()

	epPods, err := endpointPods(t, ampKube, fleet)
	if err != nil {
		return err
	}
	backPods, err := backingPods(t, backingKube)
	if err != nil {
		return err
	}
	if len(epPods) == 0 {
		return fmt.Errorf("the control-plane namespace has no pod for any of this fleet's %d endpoints to aim at",
			len(fleet.Tenants))
	}
	if len(backPods) == 0 {
		return fmt.Errorf("no backing pod matched any known selector in namespace %q", backingKube.Namespace())
	}

	targets := make([]*chaos.Target, 0, len(epPods)+len(backPods))
	names := make([]string, 0, len(epPods)+len(backPods))
	for _, pod := range epPods {
		targets = append(targets, chaos.NewPodTarget(pod, ampKube, pod, ""))
		names = append(names, pod)
	}
	for _, pod := range backPods {
		targets = append(targets, chaos.NewPodTarget(pod, backingKube, pod, ""))
		names = append(names, pod)
	}
	s.Set("targets", strings.Join(names, ","))

	var mu sync.Mutex
	injects := 0
	cfg := chaos.FromConfig(t.Options)
	cfg.Actions = []string{chaos.ActionPodDelete}
	cfg.Observe = func(e chaos.Event) {
		if e.Phase == "inject" {
			mu.Lock()
			injects++
			mu.Unlock()
		}
		if e.Err != nil {
			logf("chaos %s %s on %s: %v", e.Phase, e.Action, e.Target, e.Err)
		}
	}
	runner, err := chaos.NewRunner(t.Seed.Fork("commit-durability"), chaos.DefaultRegistry(), cfg, targets)
	if err != nil {
		return err
	}
	if err := runner.Start(); err != nil {
		return err
	}
	before := totalAcked(journals)
	cancels := &cancelStats{}
	runJournaledWriters(t.Context, env, fleet.Tenants, journals,
		t.Seed.Fork("chaos-writers"), params.Duration, params, cancels)
	runner.Stop()

	mu.Lock()
	defer mu.Unlock()
	attempted, delivered, undelivered := cancels.Attempted.Load(), cancels.Delivered.Load(), cancels.Undelivered.Load()
	s.Set("injects", injects)
	ackedUnderFault := totalAcked(journals) - before
	s.Set("acked_under_fault", ackedUnderFault)
	s.Set("cancelled", totalCancelled(journals))
	s.Set("cancels_attempted", attempted)
	s.Set("cancels_delivered", delivered)
	s.Set("cancels_undelivered", undelivered)
	s.Set("acked_despite_cancel", cancels.AckedDespiteCancel.Load())
	if injects == 0 {
		return fmt.Errorf("the chaos loop injected no fault in %s, so this run proves nothing about durability under fault", params.Duration)
	}
	// A fault window nobody wrote through is a green that means nothing: the
	// exact-set check afterwards would be verifying commits that never met a
	// fault.
	if ackedUnderFault == 0 {
		return fmt.Errorf("%d fault(s) were injected but no commit was acknowledged during the %s window, so no acked commit crossed a fault and the exact-set verification proves nothing",
			injects, params.Duration)
	}
	if params.CancelProbability == 0 {
		return nil
	}
	// Zero attempts is the harness failing to enter its own leg, not a product
	// verdict: every writer forces cancelFloorPerWriter of them.
	if attempted == 0 {
		return core.AsEnvFailure(fmt.Errorf("no cancel request was even attempted in %s (%d attempted, %d undelivered) though cancel_probability is %g and each writer forces %d attempts, so the writers issued almost no insert and the cancel-during-commit window was never entered",
			params.Duration, attempted, undelivered, params.CancelProbability, cancelFloorPerWriter))
	}
	if delivered == 0 {
		return fmt.Errorf("cancel requests were issued in %s and not one was accepted by a backend (%d attempted, %d undelivered): the endpoints answered the inserts but refused every PostgreSQL CancelRequest, so the acked-despite-cancel leg proved nothing",
			params.Duration, attempted, undelivered)
	}
	return nil
}

// recycleOneEndpoint drops one seeded tenant's endpoint and creates a fresh one
// on the same branch, so the tenant is served by a compute that knows only what
// the storage layer reports as durable.
func recycleOneEndpoint(t *core.T, s *core.StepHandle, env ampTenantEnv, fleet *tenantFleet,
	params commitDurabilityParams) error {
	if len(fleet.Tenants) == 0 {
		return fmt.Errorf("no tenant to recycle an endpoint for")
	}
	idx := t.Seed.Fork("recycle-tenant").Rand().IntN(len(fleet.Tenants))
	ten := &fleet.Tenants[idx]
	s.Set("tenant", ten.Index)
	s.Set("endpoint_before", ten.Endpoint.ID)

	if ten.Conn != nil {
		ten.Conn.Close()
		ten.Conn = nil
	}

	start := time.Now()
	if _, err := fleet.cl.DeleteEndpointAndWait(t.Context, fleet.poolID, ten.ClusterID, ten.BranchID, ten.Endpoint.ID); err != nil {
		return fmt.Errorf("tenant %d: delete endpoint %s: %w", ten.Index, ten.Endpoint.ID, err)
	}
	s.Set("endpoint_delete_seconds", time.Since(start).Seconds())

	start = time.Now()
	_, err := fleet.cl.CreateEndpointAndWait(t.Context, fleet.poolID, ten.ClusterID, ten.BranchID,
		controller.CreateEndpointRequest{
			Role:      tenantRole,
			Resources: &controller.EndpointResources{CPU: tenantEndpointCPU, Memory: tenantEndpointMemory},
		})
	if err != nil {
		return classifyEndpointRecreate(t.Context, fleet.cl, ten.ClusterID,
			fmt.Errorf("tenant %d: create a fresh endpoint on branch %s after the fault window: %w", ten.Index, ten.BranchID, err))
	}
	s.Set("endpoint_create_seconds", time.Since(start).Seconds())

	eps, err := fleet.cl.ListAllEndpoints(t.Context, fleet.poolID, ten.ClusterID, ten.BranchID)
	if err != nil {
		return fmt.Errorf("tenant %d: list endpoints after recreate: %w", ten.Index, err)
	}
	if len(eps) == 0 {
		return fmt.Errorf("tenant %d: the branch lists no endpoint after a successful create", ten.Index)
	}
	ten.Endpoint = eps[len(eps)-1]
	s.Set("endpoint_after", ten.Endpoint.ID)

	start = time.Now()
	if err := assertEndpointsServe(t, env, fleet.Tenants[idx:idx+1], params.SettleTimeout); err != nil {
		return fmt.Errorf("tenant %d's FRESH endpoint %s never became usable within %s, though its create_endpoint task succeeded: a compute that rebuilds from the storage layer cannot start (a durable frontier advertised at an LSN the pageserver never receives looks exactly like this): %w",
			ten.Index, ten.Endpoint.ID, params.SettleTimeout, err)
	}
	conn, err := env.ConnectEndpoint(t.Context, ten.Endpoint)
	if err != nil {
		return fmt.Errorf("tenant %d: connect the fresh endpoint %s: %w", ten.Index, ten.Endpoint.ID, err)
	}
	ten.Conn = conn
	s.Set("endpoint_usable_seconds", time.Since(start).Seconds())
	return nil
}

// classifyEndpointRecreate separates the product refusing to bring the endpoint
// up (a create_endpoint task that FAILED) from a pod that was never placed
// before the wait ran out, which is the cluster being slow.
func classifyEndpointRecreate(ctx context.Context, cl *controller.Client, clusterID string, err error) error {
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	failed, lerr := cl.ListTasks(lctx, controller.ListTasksOptions{
		ClusterID: clusterID, Kind: "create_endpoint", Status: controller.TaskFailed, Limit: 10,
	})
	if lerr == nil && failed.Total > 0 {
		return fmt.Errorf("%w — a create_endpoint task for this cluster is FAILED, so the product refused to bring the fresh endpoint up", err)
	}
	return core.AsEnvFailure(fmt.Errorf("%w — no create_endpoint task failed, so the endpoint was still being provisioned when this wait ran out; this is the cluster being slow, not the product refusing", err))
}

// verifyExactSets reads every tenant's rows back over a FRESH connection and
// holds the product to its promises id by id.
func verifyExactSets(t *core.T, s *core.StepHandle, env ampTenantEnv, tenants []tenant,
	journals []*AckJournal, payloadBytes int) error {
	var stored, acked int
	for i := range tenants {
		ten := &tenants[i]
		present, badPad, err := readStoredRows(t, env, ten, payloadBytes)
		if err != nil {
			return err
		}
		v := journals[i].Verify(present)
		stored += v.Present
		acked += v.Counts.Acked

		if len(v.Lost) > 0 {
			return fmt.Errorf("tenant %d (%s) lost an ACKNOWLEDGED commit: id %d was acknowledged as committed to the client and is absent from %s read over a fresh connection. %d acked id(s) missing in total: %s. Journal: %s",
				ten.Index, ten.Name, v.Lost[0], ten.Table, len(v.Lost), formatIDs(v.Lost, 10), v.Summary())
		}
		if len(v.Unexpected) > 0 {
			return fmt.Errorf("tenant %d (%s) holds %d row(s) in %s whose id was never issued by any writer: %s. Journal: %s",
				ten.Index, ten.Name, len(v.Unexpected), ten.Table, formatIDs(v.Unexpected, 10), v.Summary())
		}
		if len(v.Mismatched) > 0 {
			m := v.Mismatched[0]
			return fmt.Errorf("tenant %d (%s) holds %d row(s) in %s whose value is not the one written: id %d stores v=%d, the writer sent v=%d (outcome %s)",
				ten.Index, ten.Name, len(v.Mismatched), ten.Table, m.ID, m.Got, m.Want, m.State)
		}
		if badPad > 0 {
			return fmt.Errorf("tenant %d (%s) holds %d row(s) in %s whose pad is not %d bytes long",
				ten.Index, ten.Name, badPad, ten.Table, payloadBytes)
		}
	}
	s.Set("rows_verified", stored)
	s.Set("acked_verified", acked)
	return nil
}

// readStoredRows returns id → v for every row of the tenant's table, plus how
// many rows carry a pad of the wrong length.
func readStoredRows(t *core.T, env ampTenantEnv, ten *tenant, payloadBytes int) (map[int64]int64, int, error) {
	conn, err := env.ConnectEndpoint(t.Context, ten.Endpoint)
	if err != nil {
		return nil, 0, fmt.Errorf("tenant %d: fresh connection for verification: %w", ten.Index, err)
	}
	defer conn.Close()

	present := map[int64]int64{}
	badPad := 0
	err = conn.Query(t.Context, func(r pgx.Rows) error {
		var id, v int64
		var padLen int
		if err := r.Scan(&id, &v, &padLen); err != nil {
			return err
		}
		present[id] = v
		if padLen != payloadBytes {
			badPad++
		}
		return nil
	}, fmt.Sprintf("SELECT id, v, length(pad) FROM %s", ten.Table))
	if err != nil {
		return nil, 0, fmt.Errorf("tenant %d: read %s back: %w", ten.Index, ten.Table, err)
	}
	return present, badPad, nil
}

// runJournaledWriters drives every tenant concurrently for d, recording every
// id's outcome in that tenant's journal. Each writer's randomness forks from the
// test seed, so --seed replays the same cancel decisions, delays and targets. A
// nil cancels disables the cancel leg for that phase.
func runJournaledWriters(ctx context.Context, env ampTenantEnv, tenants []tenant, journals []*AckJournal,
	seed *core.Seed, d time.Duration, params commitDurabilityParams, cancels *cancelStats) {
	loadCtx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var wg sync.WaitGroup
	for i := range tenants {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rnd := seed.Fork(fmt.Sprintf("writer-%d", i)).Rand()
			journaledWriteFor(loadCtx, env, &tenants[i], journals[i], rnd, params, cancels)
		}(i)
	}
	wg.Wait()
}

// journaledWriteFor is the writer of multitenant_helper with the outcome of every
// id recorded, and with a cancel request aimed at a seeded share of inserts.
func journaledWriteFor(ctx context.Context, env ampTenantEnv, ten *tenant, j *AckJournal,
	rnd *rand.Rand, params commitDurabilityParams, cancels *cancelStats) {
	base := int64(ten.Index) * 1_000_000_000
	const reconnectAfterErrors = 5
	consecutive := 0
	dec := newCancelDecider(rnd, params.CancelProbability, cancelFloorPerWriter, cancelFloorWithin)
	for i := 0; ctx.Err() == nil; i++ {
		id := base + ten.nextID
		ten.nextID++
		ten.Attempted++
		value := id * 7

		if cancels != nil && dec.want(i) {
			delivered, ierr := issueInsertWithCancel(ctx, ten, id, value, params, rnd)
			cancels.recordDelivery(delivered)
			if ctx.Err() != nil {
				j.RecordUnknown(id, value)
				return
			}
			v := classifyCancelledInsert(ierr, delivered)
			switch v.Outcome {
			case OutcomeAcked:
				ten.Written++
				j.RecordAcked(id, value)
			case OutcomeCancelled:
				j.RecordCancelled(id, value)
			default:
				j.RecordUnknown(id, value)
			}
			if v.AckedDespiteCancel {
				cancels.AckedDespiteCancel.Add(1)
			}
			continue
		}

		err := issueInsert(ctx, ten, id, value, params)
		if err == nil {
			consecutive = 0
			ten.Written++
			j.RecordAcked(id, value)
			continue
		}
		if ctx.Err() != nil {
			// The phase ended around this id, so its fate is genuinely unknown.
			j.RecordUnknown(id, value)
			return
		}
		j.RecordUnknown(id, value)
		ten.WriteErrors++
		consecutive++
		if consecutive < reconnectAfterErrors {
			continue
		}
		conn, rerr := env.ConnectEndpoint(ctx, ten.Endpoint)
		if rerr != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		ten.Conn.Close()
		ten.Conn = conn
		ten.Reconnects++
		consecutive = 0
	}
}

const (
	// cancelFloorPerWriter is how many inserts per writer take the cancel path
	// whatever the probability draw says. A fault window can leave a writer with
	// only a handful of completed inserts, and a plain 0.1 draw then enters the
	// cancel leg with no better than even odds.
	cancelFloorPerWriter = 2
	// cancelFloorWithin bounds where those forced inserts sit, so the leg is
	// entered early rather than after an insert count the window may never reach.
	cancelFloorWithin = 8
)

// cancelDecider chooses which inserts a cancel request is aimed at: the seeded
// share, plus a floor of forced ones. Every decision comes from the seed, so
// --seed replays the same inserts. Insert 0 is always forced: a writer whose
// first insert hangs on a black-holed connection completes nothing else.
type cancelDecider struct {
	rnd         *rand.Rand
	probability float64
	forced      map[int]bool
}

func newCancelDecider(rnd *rand.Rand, probability float64, floor, within int) *cancelDecider {
	d := &cancelDecider{rnd: rnd, probability: probability, forced: map[int]bool{}}
	if probability <= 0 || floor <= 0 {
		return d
	}
	d.forced[0] = true
	if within < floor {
		within = floor
	}
	for _, p := range rnd.Perm(within) {
		if len(d.forced) >= floor {
			break
		}
		d.forced[p] = true
	}
	return d
}

// want reports whether the insert at index i is issued with a cancel aimed at it.
func (d *cancelDecider) want(i int) bool {
	if d.probability <= 0 {
		return false
	}
	if d.forced[i] {
		return true
	}
	return d.rnd.Float64() < d.probability
}

// insertSQL casts $3 explicitly: a bare parameter arrives as "unknown", which
// repeat(text, integer) cannot resolve against.
func insertSQL(table string) string {
	return fmt.Sprintf(
		"INSERT INTO %s (id, v, pad) VALUES ($1, $2, repeat('t', $3::int)) ON CONFLICT (id) DO NOTHING", table)
}

func issueInsert(ctx context.Context, ten *tenant, id, value int64, params commitDurabilityParams) error {
	_, err := ten.Conn.Exec(ctx, insertSQL(ten.Table), id, value, params.PayloadBytes)
	return err
}

// cancelRequestTimeout bounds the second connection the cancel request opens.
const cancelRequestTimeout = 5 * time.Second

// issueInsertWithCancel runs the insert on a dedicated pooled connection so the
// cancel reaches that backend, and fires a protocol-level CancelRequest at it
// after a seeded delay. The insert itself carries no per-statement cancellable
// context, so the driver keeps reading and an acknowledgement that arrives
// despite the cancel is observed rather than swallowed as context.Canceled.
func issueInsertWithCancel(ctx context.Context, ten *tenant, id, value int64,
	params commitDurabilityParams, rnd *rand.Rand) (bool, error) {
	pool := ten.Conn.Pool()
	if pool == nil {
		return false, fmt.Errorf("tenant %d: no pool to acquire a cancellable connection from", ten.Index)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("tenant %d: acquire a dedicated connection to cancel: %w", ten.Index, err)
	}
	defer conn.Release()

	pgConn := conn.Conn().PgConn()
	var delivered atomic.Bool
	fired := make(chan struct{})
	timer := time.AfterFunc(cancelDelay(params.CancelDelayMax, rnd), func() {
		defer close(fired)
		cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), cancelRequestTimeout)
		defer ccancel()
		if err := pgConn.CancelRequest(cctx); err == nil {
			delivered.Store(true)
		}
	})
	_, execErr := conn.Exec(ctx, insertSQL(ten.Table), id, value, params.PayloadBytes)
	if !timer.Stop() {
		<-fired
	}
	return delivered.Load(), execErr
}

func cancelDelay(max time.Duration, rnd *rand.Rand) time.Duration {
	if max <= 0 {
		max = time.Millisecond
	}
	return time.Duration(rnd.Int64N(int64(max)) + 1)
}

// cancelStats is the reach of the cancel leg. A window that delivered no cancel
// exercised nothing, so the counts are step properties rather than internals.
type cancelStats struct {
	Attempted          atomic.Int64
	Delivered          atomic.Int64
	Undelivered        atomic.Int64
	AckedDespiteCancel atomic.Int64
}

func (c *cancelStats) recordDelivery(delivered bool) {
	c.Attempted.Add(1)
	if delivered {
		c.Delivered.Add(1)
		return
	}
	c.Undelivered.Add(1)
}

// cancelVerdict is what an insert that a cancel was aimed at owes the journal.
type cancelVerdict struct {
	Outcome            AckOutcome
	AckedDespiteCancel bool
}

// classifyCancelledInsert reads the insert's own return. A success is an
// acknowledgement the client was given whatever the cancel did, so the id
// becomes a durability obligation; an error leaves its fate legitimately open.
func classifyCancelledInsert(execErr error, cancelDelivered bool) cancelVerdict {
	if execErr == nil {
		return cancelVerdict{Outcome: OutcomeAcked, AckedDespiteCancel: cancelDelivered}
	}
	if cancelDelivered {
		return cancelVerdict{Outcome: OutcomeCancelled}
	}
	return cancelVerdict{Outcome: OutcomeUnknown}
}

func totalAcked(journals []*AckJournal) int {
	n := 0
	for _, j := range journals {
		n += j.Counts().Acked
	}
	return n
}

func totalCancelled(journals []*AckJournal) int {
	n := 0
	for _, j := range journals {
		n += j.Counts().Cancelled
	}
	return n
}
