// Killing an endpoint compute in the middle of ONE large write.
//
// The other nemesis tests run streams of small inserts through the faults: every
// statement is its own transaction, so the only question they can ask is whether
// an acknowledged row survived. This one asks the atomicity question instead.
// It builds a dataset large enough that a single write takes real time — a COPY,
// an INSERT ... SELECT over generate_series, one transaction of thousands of
// small parameterised INSERTs, a few-statement transaction, an UPDATE rewriting
// the block the round before it loaded — then deletes the pod
// serving it while that write is still streaming, and demands all-or-nothing:
// the range must afterwards hold either every row of the write or none of it
// (for an update, the new value everywhere or nowhere). A partial range is a
// torn transaction, which is corruption whatever the row count says.
//
// The pod goes away both ways: a graceful delete, which is what a rolling update
// does and which lets postgres abort tidily, and a forced one that kills the
// container outright.
//
// Three further things the round holds the product to: a batch the server
// ACKNOWLEDGED must be complete (an ack the kill erased is lost data), the
// baseline loaded before the kill must be untouched (count and checksum), and
// the same batch must load to completion once the endpoint is back — a compute
// that returns but can no longer ingest is its own failure.
//
// Two more oracles sit either side of the rounds. While a write runs, a second
// connection polls its range: every poll is its own snapshot, so it may see none
// of the write or all of it, and a partial count means a reader can see an
// uncommitted bulk write. And after the rounds, one block is loaded to
// completion and the compute killed the instant the server acknowledges it —
// every row must come back, because the ack is the promise, not the write.
package stress

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	opCopy         = "copy"
	opInsertSelect = "insert_select"
	opTxn          = "txn"
	// opBatchInsert is what an application actually does: one transaction holding
	// thousands of small parameterised INSERTs, the client sending rows the whole
	// time. The other loading shapes have the server generate the rows from a
	// single statement, which is an ETL or a restore — far fewer command
	// boundaries, and no client in the loop once the statement is running.
	opBatchInsert = "batch_insert"
	// opUpdate rewrites rows that are already there instead of adding new ones:
	// a large transaction that mutates pages rather than appending to them, which
	// is the other half of "killed in the middle of a big write".
	opUpdate = "update"
)

// computeContainer is the container in an endpoint pod that runs the compute
// supervisor — postgres plus the pageserver and safekeeper it looks after. The
// pod also runs a metrics sidecar, which is why every log fetch names this one:
// asking for --all-containers --previous fails outright when the sidecar has no
// previous instance, and that lost exactly the log that says why the compute
// died (run deebc416).
const computeContainer = "endpoint"

// The pad every row is loaded with, and the one opUpdate rewrites it to. A range
// holding both is an update that was torn: neither the old state nor the new.
const (
	padLoaded  = "x"
	padUpdated = "y"
)

// The two ways the compute goes away. graceful is what a rolling update or a
// drained node does: postgres is asked to shut down, terminates its connections
// and aborts what was in flight. force is the crash — no shutdown, no chance to
// abort anything, which is the half of the fault where a torn write can survive.
const (
	killGraceful = "graceful"
	killForce    = "force"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/bulk_load_kill",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"stress", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Delete a tenant's endpoint compute pod, gracefully and by killing the container outright, while ONE large write is streaming into it — a COPY, a server-side INSERT ... SELECT, an application-shaped transaction of thousands of small parameterised INSERTs, a few-statement transaction, or an UPDATE rewriting rows already there — and hold that write to all-or-nothing, both after the fact and as a concurrent reader sees it while it runs. Then kill the compute the instant a large COPY is acknowledged and demand every row of it back, and hold the whole table to its count, checksum and the same rows through the primary key, the index on v and a full scan.",
		Params:      []string{"stress.bulk_load_kill"},
		Timeout:     120 * time.Minute,
		Run:         runBulkLoadKill,
	})
}

type bulkLoadKillParams struct {
	// BaselineRows is the dataset already committed when the kills start: the
	// data whose survival is not negotiable, and the size that makes the batches
	// below land on a table rather than an empty relation.
	BaselineRows int64
	// BatchRows is one killed write. Large enough that it is still in flight
	// when the pod goes away — a batch that commits before the kubelet acts
	// proves nothing, which is why rounds_killed is asserted.
	BatchRows int64
	Rounds    int
	// Ops are cycled over the rounds: each write shape reaches the storage layer
	// differently (COPY streams, INSERT ... SELECT is one server-side statement,
	// txn spans several statements and an explicit COMMIT).
	Ops          []string
	PayloadBytes int
	// LoadChunkRows bounds one baseline COPY. The baseline is loaded in chunks
	// so a slow cluster does not sit in a single multi-minute statement.
	LoadChunkRows int64
	// KillFraction is how far into the batch the pod is deleted. For a COPY that
	// is exact — the client counts the rows it has handed over. For the ops whose
	// progress the client cannot see (INSERT ... SELECT, txn) it is a delay
	// derived from the rate the baseline loaded at, which is why killing them
	// lands near the same place on a fast cluster and a slow one.
	KillFraction float64
	// KillDelayMin and KillDelayMax bound that derived delay: a batch the client
	// cannot observe must not be killed in its first instant, nor waited on
	// forever if the estimate was far too low.
	KillDelayMin time.Duration
	KillDelayMax time.Duration
	// KillModes are cycled over the rounds independently of Ops, so a graceful
	// delete (postgres is asked to shut down and aborts tidily) and a forced one
	// (the container is killed outright) both reach every write shape.
	KillModes []string
	// EndpointCPU and EndpointMemory size the tenant's compute. The multitenant
	// default (500m/1Gi) is there to fit many tenants on a shared cluster; ONE
	// large write against a compute that small measures the reservation rather
	// than atomicity.
	EndpointCPU    string
	EndpointMemory string
	// RowsPerStatement is how many rows one opBatchInsert statement carries. Small,
	// like an application's batch size, so the transaction is made of thousands of
	// statements rather than a handful.
	RowsPerStatement int64
	// UpdateRows is how many rows an update round rewrites, off the front of the
	// block the round before it loaded. Smaller than a whole block on purpose: an
	// updated row is a new version the later scans all have to read, so a full
	// block doubles the cost of every verification after it.
	UpdateRows int64
	// BaselineProbeRows is the slice of the baseline each round re-reads.
	BaselineProbeRows int64
	// SecondaryIndex adds an index on v. Without it the only index the table has
	// is its primary key, and the index/heap comparison can only ask whether the
	// PK agrees with a full scan — while a bulk load's index maintenance, the
	// part most likely to be left half-done by a kill, is never exercised.
	SecondaryIndex bool
	// CommitKill runs one more block AFTER the rounds, to completion, and kills
	// the compute the instant the server acknowledges it. Every round above tears
	// a write that never committed; this is the other promise, that a large write
	// the client was told about survives the compute dying immediately after.
	CommitKill bool
	// VisibilityProbe polls the range from a SECOND connection while the write is
	// in flight. Every poll is its own snapshot, so it may only ever see none of
	// the write or all of it; anything in between is a reader seeing rows of an
	// uncommitted bulk write.
	VisibilityProbe  bool
	VisibilityPeriod time.Duration
	// VisibilityWindowRows is how many rows at the front of the range each poll
	// counts. Small enough to answer instantly, so the write is sampled often.
	VisibilityWindowRows int64
	// VerifyParallel is how many verification chunks are read at once. The scans
	// are what a big dataset costs, and one connection reads them one at a time.
	VerifyParallel int
	// VerifyChunkRows bounds one verification query. A scan of millions of rows
	// returns nothing for minutes, which the port-forward to the endpoint does
	// not survive.
	VerifyChunkRows int64
	LoadTimeout     time.Duration
	SettleTimeout   time.Duration

	// baselineRowsPerSecond is measured, not configured: the rate the baseline
	// loaded at, which turns KillFraction into a delay for the unobservable ops.
	baselineRowsPerSecond float64
}

func bulkLoadKillParamsFromConfig(c *core.Config) bulkLoadKillParams {
	return bulkLoadKillParams{
		BaselineRows:         int64(c.GetInt("stress.bulk_load_kill.baseline_rows", 4_000_000)),
		BatchRows:            int64(c.GetInt("stress.bulk_load_kill.batch_rows", 1_500_000)),
		Rounds:               c.GetInt("stress.bulk_load_kill.rounds", 10),
		Ops:                  c.GetStringSlice("stress.bulk_load_kill.ops", []string{opCopy, opInsertSelect, opBatchInsert, opTxn, opUpdate}),
		PayloadBytes:         c.GetInt("stress.bulk_load_kill.payload_bytes", 128),
		LoadChunkRows:        int64(c.GetInt("stress.bulk_load_kill.load_chunk_rows", 50_000)),
		KillFraction:         c.GetFloat("stress.bulk_load_kill.kill_fraction", 0.4),
		KillDelayMin:         c.GetDuration("stress.bulk_load_kill.kill_delay_min", 2*time.Second),
		KillDelayMax:         c.GetDuration("stress.bulk_load_kill.kill_delay_max", 2*time.Minute),
		KillModes:            c.GetStringSlice("stress.bulk_load_kill.kill_modes", []string{killGraceful, killForce}),
		EndpointCPU:          c.GetString("stress.bulk_load_kill.endpoint_cpu", "4"),
		EndpointMemory:       c.GetString("stress.bulk_load_kill.endpoint_memory", "8Gi"),
		VerifyChunkRows:      int64(c.GetInt("stress.bulk_load_kill.verify_chunk_rows", 500_000)),
		UpdateRows:           int64(c.GetInt("stress.bulk_load_kill.update_rows", 500_000)),
		RowsPerStatement:     int64(c.GetInt("stress.bulk_load_kill.rows_per_statement", 1_000)),
		BaselineProbeRows:    int64(c.GetInt("stress.bulk_load_kill.baseline_probe_rows", 500_000)),
		SecondaryIndex:       c.GetBool("stress.bulk_load_kill.secondary_index", true),
		CommitKill:           c.GetBool("stress.bulk_load_kill.commit_kill", true),
		VisibilityProbe:      c.GetBool("stress.bulk_load_kill.visibility_probe", true),
		VisibilityPeriod:     c.GetDuration("stress.bulk_load_kill.visibility_period", 500*time.Millisecond),
		VisibilityWindowRows: int64(c.GetInt("stress.bulk_load_kill.visibility_window_rows", 10_000)),
		VerifyParallel:       c.GetInt("stress.bulk_load_kill.verify_parallel", 4),
		LoadTimeout:          c.GetDuration("stress.bulk_load_kill.load_timeout", 20*time.Minute),
		SettleTimeout:        c.GetDuration("stress.bulk_load_kill.settle_timeout", 10*time.Minute),
	}
}

func runBulkLoadKill(t *core.T) error {
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, ok := t.Env.(ampKubeEnv)
	if !ok || kubeEnv.AmpKube() == nil {
		t.Skip("requires a kube context for the control-plane namespace: there is no pod to delete without it")
		return nil
	}
	params := bulkLoadKillParamsFromConfig(t.Options)
	if err := validateBulkParams(params); err != nil {
		return core.AsEnvFailure(err)
	}
	logf := t.Masker().Logf(log.Printf)
	kube := kubeEnv.AmpKube()

	fleet, err := newTenantFleet(t, env, logf)
	if err != nil {
		return err
	}
	defer fleet.Close()

	// The backing's identity at the start, so a compute that later cannot reach it
	// can be told apart from a compute that lost track of one that never moved.
	// Those are an environment event and a product bug respectively, and they
	// print the same error (amp#811 was exactly the second kind).
	backingUID := backingPodUID(t, logf)

	// One tenant: the interesting axis here is the size of a single write and
	// the pod under it, not the number of neighbours. Splitting the same rows
	// across tenants would only make every batch smaller.
	// Every step's failure takes the compute's own log with it: the tenant's
	// cluster is deleted in teardown, so by the time the run collects
	// diagnostics the pod that failed is gone.
	var ten *tenant
	step := func(name string, fn func(*core.StepHandle) error) error {
		err := t.Step(name, fn)
		if ten == nil {
			return err
		}
		// A compute that died on its own is the finding, whatever the step
		// reported: the client only ever sees the dropped connection, which reads
		// like a network problem. Checked after EVERY step, because the step that
		// killed the compute was verify_dataset — reading the table back, with no
		// fault injected at all.
		why := endpointSelfRestart(t, kube, ten)
		if err == nil && why == "" {
			return nil
		}
		diag := attachEndpointDiagnostics(t, kube, ten, name, logf)
		switch {
		case diag.backingUnreachable && backingReplaced(t, backingUID, logf):
			// The compute could not reach the backing AND the backing is not the pod
			// it was: it was rescheduled under the run, so the compute had nothing to
			// talk to. Had the backing been the same pod all along, this would be the
			// compute losing track of a backing that never moved — a product bug, and
			// the branch below reports it as one.
			return core.AsEnvFailure(fmt.Errorf("the backing YugabyteDB pod was replaced during %s, so the compute lost its storage backend (%q in its log): this is the cluster moving the backing, not amp. Step error: %v. Compute: %s",
				name, backingUnreachableSignature, err, why))
		case diag.backingUnreachable:
			return fmt.Errorf("the compute could not reach the backing during %s (%q in its log) though the backing is the same pod it has always been: amp lost track of a backing that never moved. Step error: %w. Compute: %s",
				name, backingUnreachableSignature, err, why)
		case diag.outOfDisk:
			// Env, not product: every write the compute attempted was rejected by
			// the backing, so nothing this step saw says anything about amp.
			return core.AsEnvFailure(fmt.Errorf("the backing YugabyteDB ran out of disk during %s (%q in the compute's log), so its writes were rejected and the compute wedged: this dataset does not fit the backing's volume — raise env.ampstack.yb_storage. Step error: %v. Compute: %s",
				name, backingDiskFullSignature, err, why))
		case why != "" && err != nil:
			return fmt.Errorf("%w — and the compute container died on its own during this step: %s", err, why)
		case why != "":
			return fmt.Errorf("the compute container died on its own during %s, though the step itself passed: %s", name, why)
		}
		return err
	}

	fleet.EndpointCPU, fleet.EndpointMemory = params.EndpointCPU, params.EndpointMemory
	if err := t.Step("create_tenant", func(s *core.StepHandle) error {
		if err := fleet.Create(t, 1); err != nil {
			return err
		}
		s.Set("endpoint", fleet.Tenants[0].Endpoint.ID)
		s.Set("endpoint_cpu", params.EndpointCPU)
		s.Set("endpoint_memory", params.EndpointMemory)
		if !params.SecondaryIndex {
			return nil
		}
		ten := &fleet.Tenants[0]
		if _, err := ten.Conn.Exec(t.Context, fmt.Sprintf(
			"CREATE INDEX %s ON %s (v)", secondaryIndexName(ten.Table), ten.Table)); err != nil {
			return fmt.Errorf("create the secondary index on %s: %w", ten.Table, err)
		}
		s.Set("secondary_index", secondaryIndexName(ten.Table))
		return nil
	}); err != nil {
		return err
	}
	ten = &fleet.Tenants[0]

	// Laid out before the baseline so its size can be stated up front. Ops and
	// kill modes cycle independently, so with the defaults every write shape is
	// torn both by a graceful shutdown and by a killed container.
	plan := bulkPlan(params)

	if err := step("load_baseline", func(s *core.StepHandle) error {
		// Stated before a row is written: the total is baseline + one block per
		// LOADING round + the commit-kill block, so adding an op to Ops grows the
		// dataset even though no size knob changed. That is how a run outgrew the
		// backing's 20Gi with nobody touching a row count (run da7cb2af).
		s.Set("dataset_rows_planned", bulkTotalRows(params, plan))
		s.Set("dataset_bytes_planned", bulkTotalRows(params, plan)*int64(params.PayloadBytes))
		start := time.Now()
		if err := loadBaseline(t.Context, ten, params); err != nil {
			return err
		}
		elapsed := time.Since(start)
		// The rate this cluster ingests at, which is what the unobservable ops
		// turn KillFraction into a delay with.
		params.baselineRowsPerSecond = float64(params.BaselineRows) / elapsed.Seconds()
		s.Set("rows", params.BaselineRows)
		s.Set("seconds", elapsed.Seconds())
		s.Set("rows_per_second", params.baselineRowsPerSecond)
		s.Set("bytes_approx", params.BaselineRows*int64(params.PayloadBytes))
		return verifyBulkRange(t, env, ten, 1, params.BaselineRows, params.BaselineRows, params,
			"the baseline load")
	}); err != nil {
		return err
	}

	killed := map[string]int{}
	// observed counts the rounds where the SERVER reported the write's
	// transaction open at the instant the pod was deleted.
	observed := 0
	for round, r := range plan {
		name := fmt.Sprintf("round_%d_%s_%s", round, r.Op, r.Mode)
		if err := step(name, func(s *core.StepHandle) error {
			landed, sawTxn, err := bulkKillRound(t, s, env, kube, ten, r, params, logf)
			if landed {
				killed[r.Mode]++
			}
			if sawTxn {
				observed++
			}
			return err
		}); err != nil {
			return err
		}
	}

	if err := step("kills_landed_in_flight", func(s *core.StepHandle) error {
		landed := 0
		for _, mode := range params.KillModes {
			s.Set("rounds_killed_"+mode, killed[mode])
			landed += killed[mode]
		}
		s.Set("rounds_killed", landed)
		s.Set("rounds_txn_open_at_kill", observed)
		s.Set("rounds", params.Rounds)
		// Every round asserts atomicity, but a round whose write had already
		// committed when the pod was deleted asserts it about an undisturbed
		// transaction. With none of them landing mid-flight the run has said
		// nothing about torn writes, and passing it green is the failure mode
		// this step exists to prevent.
		if landed == 0 {
			return fmt.Errorf("not one of the %d round(s) had its write still in flight when the pod was deleted (batch_rows=%d): every batch committed before the kill, so this run proves nothing about a torn large transaction — raise stress.bulk_load_kill.batch_rows",
				params.Rounds, params.BatchRows)
		}
		// An unanswered client is not the same as an open transaction: a commit
		// whose acknowledgement is still on the wire looks identical from here, and
		// killing after a commit tests durability rather than atomicity. So at
		// least one round has to have seen the transaction open server-side.
		if observed == 0 {
			return fmt.Errorf("%d round(s) deleted the pod with the write unanswered, and pg_stat_activity showed no open transaction on the table at that moment in ANY of them: this run cannot claim a kill landed inside a transaction, only that the client had no reply yet",
				landed)
		}
		return nil
	}); err != nil {
		return err
	}

	// Every round above tears a write that never committed. This is the opposite
	// promise: a large write the client was TOLD about, with the compute killed
	// the instant it was told.
	if params.CommitKill {
		if err := step("commit_then_kill", func(s *core.StepHandle) error {
			return commitThenKill(t, s, env, kube, ten, params, plan)
		}); err != nil {
			return err
		}
	}

	// A fresh compute knows only what the storage layer reports as durable, so
	// this is where a "committed" row that never left the old pod disappears.
	if err := step("recycle_endpoint", func(s *core.StepHandle) error {
		return recycleOneEndpoint(t, s, env, fleet, commitDurabilityParams{SettleTimeout: params.SettleTimeout})
	}); err != nil {
		return err
	}

	if err := step("verify_dataset", func(s *core.StepHandle) error {
		total := bulkTotalRows(params, plan)
		s.Set("rows_expected", total)
		return verifyBulkRange(t, env, ten, 1, total, total, params,
			"the whole dataset after a fresh endpoint replaced the one the kills hit")
	}); err != nil {
		return err
	}

	if err := step("verify_index_matches_heap", func(s *core.StepHandle) error {
		return verifyIndexMatchesHeap(t, s, ten, 1, bulkTotalRows(params, plan), params)
	}); err != nil {
		return err
	}

	return step("amp_pods_healthy", func(s *core.StepHandle) error {
		// Scoped to this test's own tenants: a co-tenant's endpoints churn while
		// its owner runs, and their health is that owner's assertion.
		summary, err := assertAmpHealthy(t, kube, fleet.Tenants)
		if err != nil {
			return err
		}
		s.Set("pods", summary)
		return nil
	})
}

func validateBulkParams(p bulkLoadKillParams) error {
	if p.BaselineRows <= 0 || p.BatchRows <= 0 || p.Rounds <= 0 || p.LoadChunkRows <= 0 {
		return fmt.Errorf("stress.bulk_load_kill: baseline_rows, batch_rows, rounds and load_chunk_rows must all be positive, got %d/%d/%d/%d",
			p.BaselineRows, p.BatchRows, p.Rounds, p.LoadChunkRows)
	}
	if p.VerifyParallel <= 0 {
		return fmt.Errorf("stress.bulk_load_kill.verify_parallel must be positive, got %d", p.VerifyParallel)
	}
	if p.VisibilityProbe && (p.VisibilityPeriod <= 0 || p.VisibilityWindowRows <= 0) {
		return fmt.Errorf("stress.bulk_load_kill: visibility_period and visibility_window_rows must be positive when the probe is on, got %s/%d",
			p.VisibilityPeriod, p.VisibilityWindowRows)
	}
	if p.RowsPerStatement <= 0 {
		return fmt.Errorf("stress.bulk_load_kill.rows_per_statement must be positive, got %d", p.RowsPerStatement)
	}
	if p.UpdateRows <= 0 || p.BaselineProbeRows <= 0 {
		return fmt.Errorf("stress.bulk_load_kill: update_rows and baseline_probe_rows must be positive, got %d/%d", p.UpdateRows, p.BaselineProbeRows)
	}
	if p.PayloadBytes <= 0 {
		return fmt.Errorf("stress.bulk_load_kill.payload_bytes must be positive, got %d", p.PayloadBytes)
	}
	if p.KillFraction <= 0 || p.KillFraction >= 1 {
		return fmt.Errorf("stress.bulk_load_kill.kill_fraction must be strictly between 0 and 1 (the kill has to land inside the batch), got %g", p.KillFraction)
	}
	if p.KillDelayMin <= 0 || p.KillDelayMax < p.KillDelayMin {
		return fmt.Errorf("stress.bulk_load_kill: kill_delay_min must be positive and kill_delay_max at least as large, got %s/%s", p.KillDelayMin, p.KillDelayMax)
	}
	if len(p.KillModes) == 0 {
		return fmt.Errorf("stress.bulk_load_kill.kill_modes is empty, so there is no way to take the compute down")
	}
	for _, mode := range p.KillModes {
		if mode != killGraceful && mode != killForce {
			return fmt.Errorf("stress.bulk_load_kill.kill_modes: %q is not a way this test takes a pod down (%s, %s)", mode, killGraceful, killForce)
		}
	}
	for _, op := range p.Ops {
		switch op {
		case opCopy, opInsertSelect, opBatchInsert, opTxn, opUpdate:
		default:
			return fmt.Errorf("stress.bulk_load_kill.ops: %q is not a write shape this test knows (%s, %s, %s, %s, %s)",
				op, opCopy, opInsertSelect, opBatchInsert, opTxn, opUpdate)
		}
	}
	if len(p.Ops) == 0 {
		return fmt.Errorf("stress.bulk_load_kill.ops is empty, so there is no write to kill")
	}
	// An update round rewrites the block the round before it loaded, so it
	// cannot be the first thing that runs.
	if p.Ops[0] == opUpdate {
		return fmt.Errorf("stress.bulk_load_kill.ops starts with %q, which rewrites the block the previous round loaded and so cannot run first", opUpdate)
	}
	return nil
}

// bulkRound is one round: a write shape, the way the compute dies under it, and
// the id range it works on.
type bulkRound struct {
	Index int
	Op    string
	Mode  string
	Lo    int64
	Hi    int64
}

// bulkPlan lays the rounds out up front. The op cycles every round and the kill
// mode advances once the ops have all had a turn, so len(Ops) * len(KillModes)
// rounds cover every pair exactly once — which cycling both by the round number
// does not, whenever their lengths share a factor.
//
// The ranges are what the all-or-nothing question needs. A loading round gets
// its OWN block after the baseline, so its rows can be counted on their own. An
// update round has no block: it rewrites the block the previous round loaded —
// always fully present and still carrying the loaded pad, because that round
// verified it. An update round therefore does not advance the id cursor, or the
// next load would leave a hole and every total would be wrong.
func bulkPlan(p bulkLoadKillParams) []bulkRound {
	next := p.BaselineRows + 1
	prevLo, prevHi := int64(1), p.BaselineRows
	plan := make([]bulkRound, 0, p.Rounds)
	for round := 0; round < p.Rounds; round++ {
		r := bulkRound{
			Index: round,
			Op:    p.Ops[round%len(p.Ops)],
			Mode:  p.KillModes[(round/len(p.Ops))%len(p.KillModes)],
		}
		if r.Op == opUpdate {
			r.Lo, r.Hi = prevLo, min(prevLo+p.UpdateRows-1, prevHi)
			plan = append(plan, r)
			continue
		}
		r.Lo, r.Hi = next, next+p.BatchRows-1
		next = r.Hi + 1
		prevLo, prevHi = r.Lo, r.Hi
		plan = append(plan, r)
	}
	return plan
}

// commitThenKill loads one more block to completion and kills the compute the
// moment the server says it is committed. The rows must all be there afterwards:
// an acknowledged large write that a compute death can erase is data loss, and
// the ack is the promise being tested, not the write.
func commitThenKill(t *core.T, s *core.StepHandle, env ampTenantEnv, kube *k8s.Kubectl,
	ten *tenant, p bulkLoadKillParams, plan []bulkRound) error {
	lo, hi := bulkCommitKillRange(p, plan)
	want := hi - lo + 1
	s.Set("id_range", fmt.Sprintf("%d-%d", lo, hi))

	pod, err := endpointPodFor(t, kube, ten.Endpoint)
	if err != nil {
		return err
	}
	uid, _, err := podIdentity(t.Context, kube, pod)
	if err != nil {
		return err
	}
	s.Set("pod", pod)
	s.Set("pod_uid", uid)

	ctx, cancel := context.WithTimeout(t.Context, p.LoadTimeout)
	defer cancel()
	start := time.Now()
	done, _, _ := startBulkLoad(ctx, ten, bulkRound{Op: opCopy, Lo: lo, Hi: hi}, p)
	out := <-done
	if out.Err != nil {
		return fmt.Errorf("the final block %d-%d could not be loaded at all, with no fault injected: %w", lo, hi, out.Err)
	}
	acked := time.Now()
	s.Set("load_seconds", acked.Sub(start).Seconds())
	s.Set("load_rows_reported", out.Rows)

	// Forced, and as soon after the ack as the API call can be made: a graceful
	// shutdown would give the compute time to flush whatever the ack depended on,
	// which is the case the rounds above already cover.
	//
	// The uid is what makes this a test of anything: the load runs for minutes, so
	// the compute could have been replaced meanwhile, and a delete aimed at a pod
	// that is already gone succeeds silently.
	if err := killEndpointPod(t.Context, kube, pod, killForce, uid); err != nil {
		return fmt.Errorf("the block was acknowledged but its compute could not be killed right afterwards, so nothing was tested: %w", err)
	}
	// How long the compute stayed alive past the ack. The smaller this is, the
	// less it had to flush anything the ack depended on.
	s.Set("kill_after_ack_seconds", time.Since(acked).Seconds())

	if err := assertEndpointsServe(t, env, []tenant{*ten}, p.SettleTimeout); err != nil {
		return fmt.Errorf("the endpoint never served SQL again within %s after being killed right after an acknowledged COPY: %w", p.SettleTimeout, err)
	}
	if err := reconnectTenant(t, env, ten); err != nil {
		return err
	}
	return verifyBulkRange(t, env, ten, lo, hi, want, p,
		"the block whose COPY was acknowledged immediately before the compute was killed")
}

// bulkCommitKillRange is the block after everything the plan loaded.
func bulkCommitKillRange(p bulkLoadKillParams, plan []bulkRound) (lo, hi int64) {
	end := p.BaselineRows
	for _, r := range plan {
		if r.Op != opUpdate {
			end = max(end, r.Hi)
		}
	}
	return end + 1, end + p.BatchRows
}

// bulkBaselineProbe is the slice of the baseline round n checks: consecutive
// windows of BaselineProbeRows, wrapping round. Every round reads a different
// part, so over a run the whole baseline is covered without any single round
// paying for all of it.
func bulkBaselineProbe(p bulkLoadKillParams, round int) (lo, hi int64) {
	size := p.BaselineProbeRows
	if size <= 0 || size > p.BaselineRows {
		return 1, p.BaselineRows
	}
	windows := (p.BaselineRows + size - 1) / size
	lo = int64(round)%windows*size + 1
	return lo, min(lo+size-1, p.BaselineRows)
}

// bulkTotalRows is how many rows the table holds once the plan has run: the
// baseline plus one block per loading round. Update rounds add none.
func bulkTotalRows(p bulkLoadKillParams, plan []bulkRound) int64 {
	total := p.BaselineRows
	for _, r := range plan {
		if r.Op != opUpdate {
			total += p.BatchRows
		}
	}
	if p.CommitKill {
		total += p.BatchRows
	}
	return total
}

// bulkKillRound loads one batch, deletes the compute pod under it, and reports
// whether the kill landed while that load was still in flight.
func bulkKillRound(t *core.T, s *core.StepHandle, env ampTenantEnv, kube *k8s.Kubectl,
	ten *tenant, r bulkRound, p bulkLoadKillParams, logf func(string, ...any)) (landedOut, sawTxn bool, err error) {
	op, mode, lo, hi := r.Op, r.Mode, r.Lo, r.Hi
	// An update round rewrites a slice of the previous block, so the number of
	// rows this round is about is its range — not BatchRows.
	want := hi - lo + 1
	pod, err := endpointPodFor(t, kube, ten.Endpoint)
	if err != nil {
		return false, false, err
	}
	uid, _, err := podIdentity(t.Context, kube, pod)
	if err != nil {
		return false, false, err
	}
	s.Set("pod", pod)
	s.Set("pod_uid", uid)
	s.Set("kill_mode", mode)
	s.Set("id_range", fmt.Sprintf("%d-%d", lo, hi))
	if op != opCopy {
		s.Set("kill_delay_seconds", bulkKillDelay(p, want).Seconds())
	}

	loadCtx, cancel := context.WithTimeout(t.Context, p.LoadTimeout)
	defer cancel()

	// A connection for observing, taken BEFORE the write starts. Establishing one
	// mid-COPY costs more than the sample's own budget — the writer's connection
	// is busy streaming, so the pool has to open another against a compute under
	// load, and the sample timed out every COPY round because of it (run
	// 68dbcf72). Now it is opened while the compute is idle.
	// Released the moment the sample is taken, NOT at the end of the round: held
	// to the end it is still checked out when the round's own verification asks
	// the pool for VerifyParallel more connections, and pgxpool's Acquire waits
	// without a deadline — a round that hung for an hour rather than failing.
	var obs *pgxpool.Conn
	if c, err := ten.Conn.Pool().Acquire(loadCtx); err == nil {
		obs = c
	} else {
		s.Set("observer_acquire_error", err.Error())
	}
	releaseObs := func() {
		if obs != nil {
			obs.Release()
			obs = nil
		}
	}
	defer releaseObs()

	// Watch the range from a second connection for as long as the write runs. A
	// poll is its own snapshot, so it may see none of the write or all of it and
	// nothing else.
	probe := startVisibilityProbe(loadCtx, ten, r, p)

	start := time.Now()
	done, trigger, progress := startBulkLoad(loadCtx, ten, r, p)

	// The trigger fires at the kill point, but also when the load ends before
	// reaching it, so the pod is deleted only once the load is known to be
	// unfinished. Otherwise a round could report a mid-flight kill it never
	// performed, and kills_landed_in_flight would pass on nothing.
	var (
		out bulkOutcome
		// finished: the write returned before the kill could be issued.
		// landed: the pod was deleted with the write still unanswered.
		// txnWasOpen: the server reported a transaction of this write open at that
		// moment, which is the claim "killed during the transaction" rests on.
		finished   bool
		landed     bool
		txnWasOpen bool
	)
	select {
	case out = <-done:
		finished = true
	case <-trigger:
		select {
		case out = <-done:
			finished = true
		default:
		}
	}
	if finished {
		releaseObs()
	}
	if !finished {
		// What the SERVER thinks, at the instant before the pod goes away. Without
		// this the round only knows the client had no answer yet, which a commit
		// whose acknowledgement is still on the wire also satisfies — and killing
		// after a commit tests durability, not atomicity.
		open := sampleOpenTxn(t.Context, ten, obs, s)
		if progress != nil {
			s.Set("rows_streamed_at_kill", progress.handedOver())
		}
		releaseObs()
		if err := killEndpointPod(t.Context, kube, pod, mode, uid); err != nil {
			cancel()
			<-done
			return false, false, fmt.Errorf("the %s round could not kill the compute serving it: %w", op, err)
		}
		landed = true
		txnWasOpen = open
		select {
		case out = <-done:
		case <-t.Context.Done():
			return true, txnWasOpen, t.Context.Err()
		}
	}
	s.Set("killed_in_flight", landed)
	s.Set("txn_open_at_kill", txnWasOpen)
	s.Set("load_seconds", time.Since(start).Seconds())
	s.Set("load_rows_reported", out.Rows)
	acked := out.Err == nil
	s.Set("load_acknowledged", acked)
	if out.Err != nil {
		logf("bulk_load_kill: round %s/%s (%d-%d) failed as expected under the kill: %v", op, mode, lo, hi, out.Err)
		s.Set("load_error", out.Err.Error())
	}

	if probe != nil {
		cancel()
		seen := probe.stop()
		s.Set("visibility_polls", seen.polls)
		s.Set("visibility_window", seen.window)
		if seen.partial >= 0 {
			return landed, txnWasOpen, fmt.Errorf("a reader on another connection saw %d of the %d rows in %d-%d while the %s write into %d-%d was still in flight: part of an uncommitted large write is visible to other sessions",
				seen.partial, seen.window, lo, lo+seen.window-1, op, lo, hi)
		}
		// A probe that never got an answer is not evidence of anything; say so
		// rather than letting the round imply it was watched.
		if seen.polls == 0 {
			s.Set("visibility_unobserved", true)
		}
	}

	// Time from the kill to the endpoint answering SQL again. Recorded as a
	// metric, not just a step field, because it is the number that grows with the
	// dataset: a compute whose startup outlasts its liveness probe is restarted
	// by the kubelet, which throws the startup work away and starts it again.
	recovery := time.Now()
	if err := assertEndpointsServe(t, env, []tenant{*ten}, p.SettleTimeout); err != nil {
		return landed, txnWasOpen, fmt.Errorf("the endpoint never served SQL again within %s after its pod was deleted mid-%s: %w", p.SettleTimeout, op, err)
	}
	s.Set("recovery_seconds", time.Since(recovery).Seconds())
	t.Metric("endpoint recovery after kill", time.Since(recovery).Seconds(), "s")
	if err := reconnectTenant(t, env, ten); err != nil {
		return landed, txnWasOpen, err
	}

	batch, retried, err := bulkRangeStats(t, env, ten, lo, hi, p)
	if err != nil {
		return landed, txnWasOpen, err
	}
	stored, bad := batch.Rows, batch.Bad
	s.Set("rows_stored", stored)
	if retried {
		s.Set("range_read_retried", true)
	}
	if op == opUpdate {
		s.Set("rows_updated", batch.Updated)
	}
	switch {
	case bad != 0:
		return landed, txnWasOpen, fmt.Errorf("%d row(s) of the %s batch %d-%d are corrupted (v <> id*7 or pad not %d bytes) after the pod was deleted mid-write",
			bad, op, lo, hi, p.PayloadBytes)

	// An update rewrites rows that are already there, so its all-or-nothing
	// question is about the new pad, not the row count — and losing a row to it
	// is a failure of its own.
	case op == opUpdate && stored != want:
		return landed, txnWasOpen, fmt.Errorf("the update of %d-%d left %d row(s) of %d in place: killing the compute mid-update destroyed rows it was only supposed to rewrite",
			lo, hi, stored, want)
	case op == opUpdate && acked && batch.Updated != want:
		return landed, txnWasOpen, fmt.Errorf("the update of %d rows in %d-%d was ACKNOWLEDGED (%d rows reported) and only %d carry the new value: an acknowledged large update lost its work when the compute pod was deleted",
			want, lo, hi, out.Rows, batch.Updated)
	case op == opUpdate && batch.Updated != 0 && batch.Updated != want:
		return landed, txnWasOpen, fmt.Errorf("the update of %d rows in %d-%d left %d of them carrying the new value and %d the old one after the compute pod was deleted mid-update: the range holds neither state, so a large transaction was torn",
			want, lo, hi, batch.Updated, stored-batch.Updated)
	case op == opUpdate:

	case acked && stored != want:
		return landed, txnWasOpen, fmt.Errorf("the %s of %d rows into %d-%d was ACKNOWLEDGED (%d rows reported) and only %d of them are stored: an acknowledged large write lost data when the compute pod was deleted",
			op, want, lo, hi, out.Rows, stored)
	case stored != 0 && stored != want:
		return landed, txnWasOpen, fmt.Errorf("the %s of %d rows into %d-%d left %d of them visible after the compute pod was deleted mid-write: a large transaction was torn, so rows of an uncommitted write are readable (or rows of a committed one are gone)",
			op, want, lo, hi, stored)
	}

	// The kill must not have cost anything already committed. A ROTATING slice
	// of the baseline, not all of it: reading the whole thing every round costs
	// more than every other check together on a dataset this size, and the round
	// that lost data is still named because the slices move. The whole baseline
	// is covered by verify_dataset at the end.
	probeLo, probeHi := bulkBaselineProbe(p, r.Index)
	s.Set("baseline_probe", fmt.Sprintf("%d-%d", probeLo, probeHi))
	base, _, err := bulkRangeStats(t, env, ten, probeLo, probeHi, p)
	if err != nil {
		return landed, txnWasOpen, err
	}
	if want := probeHi - probeLo + 1; base.Rows != want || base.Bad != 0 {
		return landed, txnWasOpen, fmt.Errorf("the baseline lost or corrupted data when the pod was deleted mid-%s (%s): %d of %d row(s) present in %d-%d, %d corrupted",
			op, mode, base.Rows, want, probeLo, probeHi, base.Bad)
	}
	// The same scan already carries the sum, so holding the window to its
	// checksum costs nothing and catches a row swapped for another inside it —
	// which the count and the per-row predicate both pass.
	if want := bulkExpectedSum(probeLo, probeHi); base.Sum != want {
		return landed, txnWasOpen, fmt.Errorf("the baseline's checksum over %d-%d is %d, expected %d, after the pod was deleted mid-%s (%s): the window has its rows but not the right ones",
			probeLo, probeHi, base.Sum, want, op, mode)
	}

	if bulkRoundComplete(op, batch, want) {
		return landed, txnWasOpen, nil
	}
	// Aborted, so the same write has to run again. This is the probe for a
	// compute that answers SELECT 1 but can no longer ingest.
	retryStart := time.Now()
	if err := retryBulkLoad(t, env, ten, r, p); err != nil {
		return landed, txnWasOpen, err
	}
	s.Set("retry_seconds", time.Since(retryStart).Seconds())
	t.Metric("large write reissued after kill", time.Since(retryStart).Seconds(), "s")
	if err := verifyBulkRange(t, env, ten, lo, hi, want, p,
		fmt.Sprintf("the %s batch reissued after the kill", op)); err != nil {
		return landed, txnWasOpen, err
	}
	if op != opUpdate {
		return landed, txnWasOpen, nil
	}
	after, _, err := bulkRangeStats(t, env, ten, lo, hi, p)
	if err != nil {
		return landed, txnWasOpen, err
	}
	if after.Updated != want {
		return landed, txnWasOpen, fmt.Errorf("the update of %d-%d was reissued after the kill and reported success, and only %d row(s) of %d carry the new value: the endpoint accepts a large update it does not apply",
			lo, hi, after.Updated, want)
	}
	return landed, txnWasOpen, nil
}

// bulkRoundComplete says whether the round's write is fully in place: every row
// present for a load, every row carrying the new pad for an update.
func bulkRoundComplete(op string, st bulkStats, want int64) bool {
	if op == opUpdate {
		return st.Updated == want
	}
	return st.Rows == want
}

// retryBulkLoad loads the batch again, undisturbed, until it is acknowledged or
// the load budget runs out. A dropped connection between attempts is the pod
// still coming back, not a verdict.
func retryBulkLoad(t *core.T, env ampTenantEnv, ten *tenant, r bulkRound, p bulkLoadKillParams) error {
	op, lo, hi := r.Op, r.Lo, r.Hi
	ctx, cancel := context.WithTimeout(t.Context, p.LoadTimeout)
	defer cancel()
	var last error
	for attempt := 1; ; attempt++ {
		// An attempt whose COMMIT landed and whose acknowledgement did not is
		// indistinguishable from one that failed, and reissuing it would fail on
		// the primary key forever. The stored rows are the answer.
		st, _, err := bulkRangeStats(t, env, ten, lo, hi, p)
		if err == nil && bulkRoundComplete(op, st, hi-lo+1) {
			return nil
		}
		done, _, _ := startBulkLoad(ctx, ten, r, p)
		out := <-done
		if out.Err == nil {
			return nil
		}
		last = out.Err
		if ctx.Err() != nil {
			return fmt.Errorf("the %s batch %d-%d never loaded again within %s after the kill, over %d attempt(s): the endpoint serves SQL but no longer ingests a large write: %w",
				op, lo, hi, p.LoadTimeout, attempt, last)
		}
		if err := sleepCtx(ctx, 5*time.Second); err != nil {
			return fmt.Errorf("the %s batch %d-%d never loaded again after the kill: %w (last load error: %w)", op, lo, hi, err, last)
		}
		if err := reconnectTenant(t, env, ten); err != nil {
			return err
		}
	}
}

func reconnectTenant(t *core.T, env ampTenantEnv, ten *tenant) error {
	if ten.Conn != nil {
		ten.Conn.Close()
		ten.Conn = nil
	}
	conn, err := env.ConnectEndpoint(t.Context, ten.Endpoint)
	if err != nil {
		return fmt.Errorf("reconnect to endpoint %s after the kill: %w", ten.Endpoint.ID, err)
	}
	ten.Conn = conn
	return nil
}

type bulkOutcome struct {
	Rows int64
	Err  error
}

// startBulkLoad issues one batch and returns its outcome channel plus a trigger
// that fires when the load is far enough along to be worth killing. The trigger
// is closed rather than signalled, so a caller that never reads it leaks
// nothing, and it always fires eventually — a caller blocked on both channels
// cannot deadlock on a load that ended early.
func startBulkLoad(ctx context.Context, ten *tenant, r bulkRound, p bulkLoadKillParams) (<-chan bulkOutcome, <-chan struct{}, progressReporter) {
	op, lo, hi := r.Op, r.Lo, r.Hi
	done := make(chan bulkOutcome, 1)
	if op == opCopy {
		killAt := int64(float64(hi-lo+1) * p.KillFraction)
		src := newBulkSource(lo, hi, p.PayloadBytes, killAt)
		go func() {
			n, err := ten.Conn.CopyFrom(ctx, ten.Table, []string{"id", "v", "pad"}, src)
			src.release()
			done <- bulkOutcome{Rows: n, Err: err}
		}()
		return done, src.trigger, src
	}

	// batch_insert can say how far it has got — it counts the rows it has sent —
	// so its kill lands at KillFraction of the write exactly, like a COPY's,
	// rather than at an estimated delay.
	if op == opBatchInsert {
		pr := newStmtProgress(int64(float64(hi-lo+1) * p.KillFraction))
		go func() {
			n, err := insertBatched(ctx, ten, lo, hi, p, pr)
			pr.release()
			done <- bulkOutcome{Rows: n, Err: err}
		}()
		return done, pr.trigger, pr
	}

	trigger := make(chan struct{})
	go func() {
		defer close(trigger)
		timer := time.NewTimer(bulkKillDelay(p, hi-lo+1))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
	}()
	go func() {
		var (
			n   int64
			err error
		)
		switch op {
		case opTxn:
			n, err = insertTxn(ctx, ten, lo, hi, p)
		case opUpdate:
			n, err = ten.Conn.Exec(ctx, bulkUpdateSQL(ten.Table, lo, hi, p.PayloadBytes))
		default:
			n, err = ten.Conn.Exec(ctx, bulkInsertSQL(ten.Table, lo, hi, p.PayloadBytes))
		}
		done <- bulkOutcome{Rows: n, Err: err}
	}()
	return done, trigger, nil
}

// attachEndpointDiagnostics saves what the tenant's compute has to say about a
// failed round: the running container's log, the log of the instance before it
// (the one that died, when the container restarted), and the pod description
// with its exit reason. Best effort — a pod that is already gone must not turn
// a real failure into a different error.
// endpointDiagnosis is what the collected logs say about the cause, beyond the
// artifacts themselves.
type endpointDiagnosis struct {
	// outOfDisk: the backing rejected writes for want of space.
	outOfDisk bool
	// backingUnreachable: the compute could not connect to the backing at all.
	backingUnreachable bool
}

func attachEndpointDiagnostics(t *core.T, kube *k8s.Kubectl, ten *tenant, round string,
	logf func(string, ...any)) (diag endpointDiagnosis) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context), 2*time.Minute)
	defer cancel()

	pod, err := endpointPodFor(t, kube, ten.Endpoint)
	if err != nil {
		logf("bulk_load_kill: no endpoint pod to collect diagnostics from for %s: %v", round, err)
		return diag
	}
	// The pageserver logs a line per getpage at INFO, which drowns everything
	// else: a tail of the raw log covers seconds and never reaches postgres's own
	// messages or the supervisor's. So a filtered view goes alongside it.
	if signals, err := endpointLogSignals(ctx, kube, pod); err == nil && signals != "" {
		t.AttachBytes(round+"-endpoint-signals.log", "log", []byte(signals))
		// A backing with no room left fails every write the compute tries and
		// wedges it. That is the test outgrowing its environment, and saying so is
		// the difference between a sized-up rerun and a hunt for a product bug
		// that is not there.
		diag.outOfDisk = strings.Contains(signals, backingDiskFullSignature)
		diag.backingUnreachable = strings.Contains(signals, backingUnreachableSignature)
	}

	for _, item := range []struct {
		name string
		kind string
		read func() (string, error)
	}{
		{round + "-endpoint.log", "log", func() (string, error) {
			return kube.Logs(ctx, "pod/"+pod, computeContainer, 5000)
		}},
		// The instance that DIED, which is the one worth reading. Container-scoped
		// on purpose: the sidecar has no previous instance and asking for all of
		// them fails the whole fetch.
		{round + "-endpoint-previous.log", "log", func() (string, error) {
			return kube.LogsPrevious(ctx, "pod/"+pod, computeContainer, 20000)
		}},
		{round + "-endpoint-describe.txt", "txt", func() (string, error) { return kube.Describe(ctx, "pod", pod) }},
		{round + "-events.txt", "txt", func() (string, error) { return kube.Events(ctx) }},
	} {
		body, err := item.read()
		if err != nil || strings.TrimSpace(body) == "" {
			continue
		}
		t.AttachBytes(item.name, item.kind, []byte(body))
	}
	return diag
}

// backingDiskFullSignature is what a YugabyteDB tserver says when it stops
// accepting writes for want of space (yb/tserver/tablet_service.cc).
const backingDiskFullSignature = "insufficient disk space"

// backingUnreachableSignature is the smart driver finding no tserver to talk to.
const backingUnreachableSignature = "could not find a server to connect to"

// backingPodUID is the backing's pod uid at the start of the run, or "" when the
// env exposes no handle to its namespace. Best effort: without it the compute
// losing the backing cannot be attributed either way, and the failure is
// reported as a product one, which is the safe direction.
func backingPodUID(t *core.T, logf func(string, ...any)) string {
	envB, ok := t.Env.(backingKubeEnv)
	if !ok || envB.BackingKube() == nil {
		return ""
	}
	kube := envB.BackingKube()
	pods, err := backingPods(t, kube)
	if err != nil || len(pods) == 0 {
		logf("bulk_load_kill: no backing pod to identify: %v", err)
		return ""
	}
	uid, err := kube.Get(t.Context, "pod", pods[0], "{.metadata.uid}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(uid)
}

// backingReplaced says whether the backing is a DIFFERENT pod than the one the
// run started against. Unknown counts as not replaced: blaming the cluster
// without evidence is how a real bug gets filed as flakiness.
func backingReplaced(t *core.T, was string, logf func(string, ...any)) bool {
	if was == "" {
		return false
	}
	now := backingPodUID(t, logf)
	return now != "" && now != was
}

// bulkLogSignals are the lines worth reading out of a log the pageserver's
// per-request INFO logging otherwise fills: postgres's own complaints, the
// supervisor's decisions, and anything that names a death.
var bulkLogSignals = []string{
	"FATAL", "PANIC", "ERROR", "WARN",
	"LOG:", "terminating", "out of memory", "Killed",
	"shutting down", "supervisor failed", "basebackup", "liveness",
	"database system", "startup process", "recovery",
}

// endpointLogSignals pulls a long tail of the compute container's log and keeps
// only the lines that say something. The tail is long because the interesting
// lines are minutes back behind a wall of per-request logging, and the result is
// small because almost none of it matches.
func endpointLogSignals(ctx context.Context, kube *k8s.Kubectl, pod string) (string, error) {
	raw, err := kube.Logs(ctx, "pod/"+pod, computeContainer, 200000)
	if err != nil {
		return "", err
	}
	var kept []string
	for _, line := range strings.Split(raw, "\n") {
		for _, sig := range bulkLogSignals {
			if strings.Contains(line, sig) {
				kept = append(kept, line)
				break
			}
		}
	}
	if len(kept) == 0 {
		return "", nil
	}
	return strings.Join(kept, "\n"), nil
}

// endpointSelfRestart reports the compute container dying on its own — the pod
// the test deleted is replaced, and a replacement starts its containers once, so
// any restart count above zero is the endpoint process exiting by itself.
// Returns "" when it is healthy, or when there is nothing to read (a pod being
// replaced right now is not evidence).
func endpointSelfRestart(t *core.T, kube *k8s.Kubectl, ten *tenant) string {
	pod, err := endpointPodFor(t, kube, ten.Endpoint)
	if err != nil {
		return ""
	}
	n, why := endpointContainerRestarts(t.Context, kube, pod)
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%s restarted %d time(s), last exit %s", pod, n, why)
}

// endpointContainerRestarts reports how many times the compute container has
// been restarted in place and why it last died. A pod that k8s has just created
// should be at zero: anything above it is the endpoint process exiting on its
// own, which is a different failure from the pod being deleted.
func endpointContainerRestarts(ctx context.Context, kube *k8s.Kubectl, pod string) (int, string) {
	const container = computeContainer
	count, err := kube.Get(ctx, "pod", pod,
		fmt.Sprintf("{.status.containerStatuses[?(@.name==%q)].restartCount}", container))
	if err != nil {
		return 0, ""
	}
	n, err := strconv.Atoi(strings.TrimSpace(count))
	if err != nil || n == 0 {
		return 0, ""
	}
	why, err := kube.Get(ctx, "pod", pod, fmt.Sprintf(
		"{.status.containerStatuses[?(@.name==%q)].lastState.terminated.reason} exit={.status.containerStatuses[?(@.name==%q)].lastState.terminated.exitCode}",
		container, container))
	if err != nil {
		return n, ""
	}
	return n, strings.TrimSpace(why)
}

// visibilityProbe watches a range from its own connection while a write into it
// is running.
type visibilityProbe struct {
	done    chan struct{}
	window  int64
	mu      sync.Mutex
	polls   int
	partial int64
}

type visibilitySeen struct {
	polls   int
	window  int64
	partial int64
}

// startVisibilityProbe polls the range until ctx ends. It counts what a
// concurrent reader can see: rows for a load, rows carrying the new value for an
// update. Query errors are expected — the pod it is reading from is about to be
// deleted — and are not the probe's business.
func startVisibilityProbe(ctx context.Context, ten *tenant, r bulkRound, p bulkLoadKillParams) *visibilityProbe {
	if !p.VisibilityProbe {
		return nil
	}
	pr := &visibilityProbe{done: make(chan struct{}), partial: -1}
	// A WINDOW at the front of the range, not the whole thing: counting two
	// million rows takes seconds, so a probe that did it managed one or two polls
	// per write and proved nothing. A window of ten thousand is an index range
	// scan and returns at once, so the write is actually sampled — and a torn
	// write's visible rows start at the front, which is where the window is.
	window := min(p.VisibilityWindowRows, r.Hi-r.Lo+1)
	if window < 1 {
		window = 1
	}
	hi := r.Lo + window - 1
	pr.window = window
	sql := fmt.Sprintf("SELECT count(*) FROM %s WHERE id BETWEEN %d AND %d", ten.Table, r.Lo, hi)
	if r.Op == opUpdate {
		sql = fmt.Sprintf("SELECT count(*) FROM %s WHERE id BETWEEN %d AND %d AND pad LIKE '%s%%'",
			ten.Table, r.Lo, hi, padUpdated)
	}
	go func() {
		defer close(pr.done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(p.VisibilityPeriod):
			}
			n, err := ten.Conn.QueryInt(ctx, sql)
			if err != nil {
				continue
			}
			pr.mu.Lock()
			pr.polls++
			// 0 is the write not yet visible, window is all of it: both are whole
			// states of a range this small. Anything between them is part of an
			// uncommitted write showing through.
			if n != 0 && n != window && pr.partial < 0 {
				pr.partial = n
			}
			pr.mu.Unlock()
		}
	}()
	return pr
}

// stop waits for the probe to finish and reports what it saw. partial is -1 when
// every poll saw a whole state.
func (pr *visibilityProbe) stop() visibilitySeen {
	<-pr.done
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return visibilitySeen{polls: pr.polls, window: pr.window, partial: pr.partial}
}

// secondaryIndexName is the index on v, named off the table so two tenants in one
// database cannot collide.
func secondaryIndexName(table string) string { return table + "_v_idx" }

// sampleOpenTxn asks the server whether a transaction touching this tenant's
// table is open right now, on a connection of its own, and records what it saw.
// A backend that is active inside a transaction is the write this round is about
// to kill: the writer's own connection is the only other one doing anything with
// that table, and the sampling connection excludes itself.
//
// Errors are not failures — the pod is about to be deleted and the sample is
// evidence, not a gate. It returns false when it could not see one.
func sampleOpenTxn(ctx context.Context, ten *tenant, obs *pgxpool.Conn, s *core.StepHandle) bool {
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var (
		open        int64
		age         float64
		q, st, wait string
	)
	// An open transaction is xact_start, not state: a COPY waiting for the client
	// to send more rows is parked on ClientRead and reported as idle in
	// transaction, so keying on state = 'active' missed exactly the write whose
	// progress the client can see (run ba7195f8 confirmed six of eight rounds and
	// left both COPY rounds unobserved). The state and wait event are recorded so
	// what the backend was doing is on the record rather than assumed.
	err := queryOn(sctx, ten, obs, func(r pgx.Rows) error {
		return r.Scan(&open, &age, &q, &st, &wait)
	}, fmt.Sprintf(`SELECT count(*),
		       coalesce(max(extract(epoch from (clock_timestamp() - xact_start))), 0),
		       coalesce(max(left(query, 120)), ''),
		       coalesce(max(state), ''),
		       coalesce(max(coalesce(wait_event_type, '') || '/' || coalesce(wait_event, '')), '')
		  FROM pg_stat_activity
		 WHERE pid <> pg_backend_pid()
		   AND xact_start IS NOT NULL
		   AND state <> 'idle'
		   AND query ILIKE '%%%s%%'`, ten.Table))
	if err != nil {
		s.Set("txn_sample_error", err.Error())
		return false
	}
	s.Set("txn_open_backends_at_kill", open)
	if open == 0 {
		// Say what the backends WERE doing rather than leaving the round to be
		// argued about. A COPY has never matched the predicate — batch_insert,
		// which streams from the client the same way, shows up as idle in
		// transaction — and guessing at why has been wrong twice.
		s.Set("txn_backends_seen", describeBackends(sctx, ten, obs))
		return false
	}
	// How long the transaction had been running when the pod was deleted, and
	// what it was doing: the two things that make "during the transaction" a
	// statement about the server rather than about the client's wait.
	s.Set("txn_age_at_kill_seconds", age)
	s.Set("txn_query_at_kill", q)
	s.Set("txn_state_at_kill", st)
	s.Set("txn_wait_at_kill", wait)
	return true
}

// describeBackends lists every other backend mentioning the tenant's table, with
// no predicate beyond that: state, whether it is in a transaction, what it waits
// on, and the head of its query. For when the strict sample finds nothing and the
// question is what the server thinks is happening.
func describeBackends(ctx context.Context, ten *tenant, obs *pgxpool.Conn) string {
	var out []string
	err := queryOn(ctx, ten, obs, func(r pgx.Rows) error {
		var pid int
		var state, wait, q string
		var inTxn bool
		if err := r.Scan(&pid, &state, &inTxn, &wait, &q); err != nil {
			return err
		}
		out = append(out, fmt.Sprintf("pid=%d state=%q in_txn=%v wait=%s query=%q", pid, state, inTxn, wait, q))
		return nil
	}, fmt.Sprintf(`SELECT pid, coalesce(state, ''), xact_start IS NOT NULL,
		       coalesce(wait_event_type, '') || '/' || coalesce(wait_event, ''),
		       left(coalesce(query, ''), 60)
		  FROM pg_stat_activity
		 WHERE pid <> pg_backend_pid() AND query ILIKE '%%%s%%'`, ten.Table))
	if err != nil {
		return "unreadable: " + err.Error()
	}
	if len(out) == 0 {
		return "no backend mentions the table at all"
	}
	return strings.Join(out, " | ")
}

// queryOn runs an observation on the pinned connection when there is one, and
// falls back to the pool otherwise. The pinned one is the point: it was opened
// before the write started, so the observation does not have to wait for a new
// connection to a compute that is busy.
func queryOn(ctx context.Context, ten *tenant, obs *pgxpool.Conn, scan func(pgx.Rows) error, sql string) error {
	if obs == nil {
		return ten.Conn.Query(ctx, scan, sql)
	}
	rows, err := obs.Query(ctx, sql)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// killEndpointPod takes the compute down the way this round asked for, and
// confirms the pod it aimed at actually went away.
//
// Both delete paths pass --ignore-not-found, so deleting a pod that is already
// gone succeeds and says nothing: a round whose compute was replaced during a
// long write would report a kill it never performed. So the uid is checked
// before (it must still be the pod the round measured) and after (it must be
// gone, or replaced by a different pod).
func killEndpointPod(ctx context.Context, kube *k8s.Kubectl, pod, mode, wantUID string) error {
	if before := livePodUID(ctx, kube, pod); before != wantUID {
		return fmt.Errorf("pod %s is %s, not the %s this round measured: its compute was replaced before the kill, so deleting it would prove nothing",
			pod, describeUID(before), describeUID(wantUID))
	}
	var err error
	if mode == killForce {
		err = kube.DeleteNow(ctx, pod)
	} else {
		err = kube.Delete(ctx, "pod", pod)
	}
	if err != nil {
		return err
	}
	// --ignore-not-found means the delete above cannot report this itself.
	if after := livePodUID(ctx, kube, pod); after == wantUID {
		return fmt.Errorf("pod %s still exists with the same uid after a %s delete: nothing was killed", pod, mode)
	}
	return nil
}

// livePodUID is the uid of the pod now, or "" when it is gone.
func livePodUID(ctx context.Context, kube *k8s.Kubectl, pod string) string {
	uid, err := kube.Get(ctx, "pod", pod, "{.metadata.uid}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(uid)
}

func describeUID(uid string) string {
	if uid == "" {
		return "gone"
	}
	return "uid " + uid
}

// bulkKillDelay is KillFraction of how long a write over rows rows is expected
// to take, at the rate the baseline actually loaded at. A fixed delay would land
// at a different point in the write on every cluster — and on a slow one, in the
// first percent of it. rows is the ROUND's row count, so an update over a slice
// is not killed as if it were a whole block.
func bulkKillDelay(p bulkLoadKillParams, rows int64) time.Duration {
	if p.baselineRowsPerSecond <= 0 {
		return p.KillDelayMin
	}
	d := time.Duration(float64(rows) * p.KillFraction / p.baselineRowsPerSecond * float64(time.Second))
	return min(max(d, p.KillDelayMin), p.KillDelayMax)
}

// progressReporter is a write that can say how many rows it has handed to the
// server so far.
type progressReporter interface{ handedOver() int64 }

// stmtProgress counts rows across the statements of one transaction and closes
// its trigger once the kill point is reached.
type stmtProgress struct {
	killAt  int64
	trigger chan struct{}
	once    sync.Once
	mu      sync.Mutex
	sent    int64
}

func newStmtProgress(killAt int64) *stmtProgress {
	if killAt < 1 {
		killAt = 1
	}
	return &stmtProgress{killAt: killAt, trigger: make(chan struct{})}
}

func (p *stmtProgress) add(n int64) {
	p.mu.Lock()
	p.sent += n
	reached := p.sent >= p.killAt
	p.mu.Unlock()
	if reached {
		p.release()
	}
}

func (p *stmtProgress) handedOver() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent
}

func (p *stmtProgress) release() { p.once.Do(func() { close(p.trigger) }) }

// insertBatched writes the range as many small parameterised INSERTs inside ONE
// transaction, the way an application's batch job does. Every row travels from
// the client, and the transaction accumulates thousands of command boundaries
// before it commits — which is what makes this a different write to kill than a
// single server-side statement covering the same rows.
func insertBatched(ctx context.Context, ten *tenant, lo, hi int64, p bulkLoadKillParams, pr *stmtProgress) (int64, error) {
	tx, err := ten.Conn.Pool().Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin the batched insert: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	pad := strings.Repeat(padLoaded, p.PayloadBytes)
	var written int64
	for id := lo; id <= hi; {
		end := min(id+p.RowsPerStatement-1, hi)
		rows := make([][]any, 0, end-id+1)
		for cur := id; cur <= end; cur++ {
			rows = append(rows, []any{cur, cur * 7, pad})
		}
		tag, err := tx.Exec(ctx, batchInsertSQL(ten.Table, len(rows)), flattenRows(rows)...)
		if err != nil {
			return written, fmt.Errorf("insert rows %d-%d of the batched transaction: %w", id, end, err)
		}
		written += tag.RowsAffected()
		pr.add(end - id + 1)
		id = end + 1
	}
	if err := tx.Commit(ctx); err != nil {
		return written, fmt.Errorf("commit the batched insert %d-%d: %w", lo, hi, err)
	}
	return written, nil
}

// batchInsertSQL is a multi-row INSERT with a placeholder per column per row —
// an application's batch, parameters and all, not a server-side generator.
func batchInsertSQL(table string, rows int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "INSERT INTO %s (id, v, pad) VALUES ", table)
	for i := 0; i < rows; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "($%d, $%d, $%d)", i*3+1, i*3+2, i*3+3)
	}
	return b.String()
}

func flattenRows(rows [][]any) []any {
	out := make([]any, 0, len(rows)*3)
	for _, r := range rows {
		out = append(out, r...)
	}
	return out
}

// bulkInsertSQL generates the batch server-side: the client never sends the
// rows, so the whole write is one statement the compute has to make atomic.
func bulkInsertSQL(table string, lo, hi int64, payloadBytes int) string {
	return fmt.Sprintf(
		"INSERT INTO %s (id, v, pad) SELECT g, g * 7, repeat('%s', %d) FROM generate_series(%d::bigint, %d::bigint) g",
		table, padLoaded, payloadBytes, lo, hi)
}

// bulkUpdateSQL rewrites every row of the range in one statement. Idempotent, so
// the reissue after a kill is the same statement rather than a special case.
func bulkUpdateSQL(table string, lo, hi int64, payloadBytes int) string {
	return fmt.Sprintf("UPDATE %s SET pad = repeat('%s', %d) WHERE id BETWEEN %d AND %d",
		table, padUpdated, payloadBytes, lo, hi)
}

// bulkTxnStatements is how many statements opTxn spreads its batch over. More
// than one, so the kill can land between two of them and the COMMIT that would
// have made them all durable never arrives.
const bulkTxnStatements = 4

// insertTxn writes the batch as several statements inside one explicit
// transaction. The deferred rollback is best effort on a context that outlives
// the kill: when the connection is already gone it fails, and the abort is what
// the server does on its own.
func insertTxn(ctx context.Context, ten *tenant, lo, hi int64, p bulkLoadKillParams) (int64, error) {
	tx, err := ten.Conn.Pool().Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin the batch transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var written int64
	for _, chunk := range splitBulkRange(lo, hi, bulkTxnStatements) {
		tag, err := tx.Exec(ctx, bulkInsertSQL(ten.Table, chunk[0], chunk[1], p.PayloadBytes))
		if err != nil {
			return written, fmt.Errorf("insert %d-%d inside the batch transaction: %w", chunk[0], chunk[1], err)
		}
		written += tag.RowsAffected()
	}
	if err := tx.Commit(ctx); err != nil {
		return written, fmt.Errorf("commit the batch transaction %d-%d: %w", lo, hi, err)
	}
	return written, nil
}

// splitBulkRange cuts [lo, hi] into at most n contiguous inclusive ranges.
func splitBulkRange(lo, hi int64, n int64) [][2]int64 {
	if n < 1 {
		n = 1
	}
	total := hi - lo + 1
	if total <= 0 {
		return nil
	}
	if n > total {
		n = total
	}
	per := total / n
	out := make([][2]int64, 0, n)
	for i := int64(0); i < n; i++ {
		from := lo + i*per
		to := from + per - 1
		if i == n-1 {
			to = hi
		}
		out = append(out, [2]int64{from, to})
	}
	return out
}

// bulkSource feeds COPY the batch row by row and closes trigger once killAt of
// them have been handed over, which is the client's only view of how far a COPY
// has got.
type bulkSource struct {
	lo, hi  int64
	pad     string
	next    int64
	killAt  int64
	trigger chan struct{}
	once    sync.Once
	// produced is read by the round at the moment of the kill, while COPY's
	// goroutine is still writing it.
	mu       sync.Mutex
	produced int64
}

func newBulkSource(lo, hi int64, payloadBytes int, killAt int64) *bulkSource {
	return &bulkSource{
		lo: lo, hi: hi, next: lo, killAt: killAt,
		pad:     strings.Repeat(padLoaded, payloadBytes),
		trigger: make(chan struct{}),
	}
}

func (s *bulkSource) Next() bool {
	if s.next > s.hi {
		return false
	}
	s.mu.Lock()
	s.produced++
	reached := s.produced >= s.killAt
	s.mu.Unlock()
	if reached {
		s.release()
	}
	return true
}

func (s *bulkSource) Values() ([]any, error) {
	id := s.next
	s.next++
	return []any{id, id * 7, s.pad}, nil
}

func (s *bulkSource) Err() error { return nil }

// handedOver is how many rows the source has given COPY so far — the client's
// only measure of how far into the stream the kill landed.
func (s *bulkSource) handedOver() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.produced
}

// release unblocks whoever waits on the trigger, whether the kill point was
// reached or the COPY ended first.
func (s *bulkSource) release() { s.once.Do(func() { close(s.trigger) }) }

// loadBaseline commits the starting dataset in chunks, each its own COPY.
func loadBaseline(ctx context.Context, ten *tenant, p bulkLoadKillParams) error {
	for _, chunk := range splitBulkRange(1, p.BaselineRows, (p.BaselineRows+p.LoadChunkRows-1)/p.LoadChunkRows) {
		src := newBulkSource(chunk[0], chunk[1], p.PayloadBytes, chunk[1]-chunk[0]+2)
		n, err := ten.Conn.CopyFrom(ctx, ten.Table, []string{"id", "v", "pad"}, src)
		if err != nil {
			return fmt.Errorf("baseline COPY of %d-%d: %w", chunk[0], chunk[1], err)
		}
		if want := chunk[1] - chunk[0] + 1; n != want {
			return fmt.Errorf("baseline COPY of %d-%d reported %d rows, expected %d", chunk[0], chunk[1], n, want)
		}
	}
	return nil
}

// bulkRangeStats reads [lo, hi] and returns how many rows are stored, how many
// are wrong, and the sum of their v — all three from ONE scan, so the numbers
// describe the same rows.
//
// The range is read in chunks of VerifyChunkRows. A single count over millions
// of rows is a query that sends nothing for minutes, and the kubectl
// port-forward the test reaches the endpoint through does not survive that: two
// earlier runs lost a multi-minute scan to "unexpected EOF" on a compute whose
// pod never restarted. Chunks keep every query short; the aggregate is the same
// number.
//
// A chunk that loses its connection is retried once on a fresh one, and the
// retry recorded — a verification that cannot run is not a verdict about the
// data, but a query that keeps dying says something about the endpoint.
func bulkRangeStats(t *core.T, env ampTenantEnv, ten *tenant, lo, hi int64, p bulkLoadKillParams) (stats bulkStats, retried bool, err error) {
	chunks := splitBulkRange(lo, hi, bulkChunkCount(lo, hi, p.VerifyChunkRows))
	stats, err = scanChunks(t.Context, ten, chunks, p)
	if err == nil || t.Context.Err() != nil {
		return stats, false, err
	}
	// The whole pass failed. Reconnecting covers the case the chunking cannot: a
	// port-forward that died, which no per-chunk retry on the same pool can heal.
	if rerr := reconnectTenant(t, env, ten); rerr != nil {
		return bulkStats{}, true, fmt.Errorf("%w (and reconnecting to retry it failed: %w)", err, rerr)
	}
	stats, err = scanChunks(t.Context, ten, chunks, p)
	return stats, true, err
}

// scanChunks reads the chunks VerifyParallel at a time and adds their numbers up.
// The pool opens a connection per worker, so this is the compute answering
// several scans at once rather than one after another — the difference between a
// verification that costs minutes and one that costs an hour on a big table.
func scanChunks(ctx context.Context, ten *tenant, chunks [][2]int64, p bulkLoadKillParams) (bulkStats, error) {
	workers := min(p.VerifyParallel, len(chunks))
	if workers < 1 {
		workers = 1
	}
	var (
		mu      sync.Mutex
		total   bulkStats
		firstEr error
		wg      sync.WaitGroup
	)
	work := make(chan [2]int64)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunk := range work {
				// Bounded: a chunk read that cannot get a connection would otherwise
				// wait on the pool forever, and a hang says far less than a failure.
				cctx, cancel := context.WithTimeout(ctx, p.SettleTimeout)
				got, err := queryRangeStats(cctx, ten, chunk[0], chunk[1], p.PayloadBytes)
				cancel()
				mu.Lock()
				if err != nil {
					if firstEr == nil {
						firstEr = err
					}
				} else {
					total.Rows += got.Rows
					total.Bad += got.Bad
					total.Sum += got.Sum
					total.Updated += got.Updated
				}
				mu.Unlock()
			}
		}()
	}
	for _, chunk := range chunks {
		select {
		case work <- chunk:
		case <-ctx.Done():
		}
	}
	close(work)
	wg.Wait()
	if firstEr != nil {
		return bulkStats{}, firstEr
	}
	if err := ctx.Err(); err != nil {
		return bulkStats{}, err
	}
	return total, nil
}

// bulkChunkCount is how many chunks of at most chunkRows [lo, hi] splits into.
func bulkChunkCount(lo, hi, chunkRows int64) int64 {
	if chunkRows <= 0 {
		return 1
	}
	return (hi - lo + chunkRows) / chunkRows
}

// bulkStats is what one scan of a range reports.
type bulkStats struct {
	Rows int64
	Bad  int64
	Sum  int64
	// Updated is how many rows carry the pad opUpdate rewrites to. A range where
	// it is neither 0 nor all of the rows is an update that was torn.
	Updated int64
}

func queryRangeStats(ctx context.Context, ten *tenant, lo, hi int64, payloadBytes int) (bulkStats, error) {
	var st bulkStats
	err := ten.Conn.Query(ctx, func(r pgx.Rows) error {
		return r.Scan(&st.Rows, &st.Bad, &st.Sum, &st.Updated)
	}, fmt.Sprintf(
		"SELECT count(*), count(*) FILTER (WHERE v <> id * 7 OR length(pad) <> %d), coalesce(sum(v), 0), count(*) FILTER (WHERE pad LIKE '%s%%') FROM %s WHERE id BETWEEN %d AND %d",
		payloadBytes, padUpdated, ten.Table, lo, hi))
	if err != nil {
		return bulkStats{}, fmt.Errorf("count rows %d-%d of %s: %w", lo, hi, ten.Table, err)
	}
	return st, nil
}

// verifyBulkRange holds [lo, hi] to an exact row count, no corrupted row, and
// the checksum the ids imply. The sum catches a row substituted for another
// inside the range, which a count and a per-row predicate both miss.
func verifyBulkRange(t *core.T, env ampTenantEnv, ten *tenant, lo, hi int64, wantRows int64, p bulkLoadKillParams, what string) error {
	st, _, err := bulkRangeStats(t, env, ten, lo, hi, p)
	if err != nil {
		return err
	}
	if st.Rows != wantRows {
		return fmt.Errorf("%s: %s holds %d row(s) in %d-%d, expected exactly %d",
			what, ten.Table, st.Rows, lo, hi, wantRows)
	}
	if st.Bad != 0 {
		return fmt.Errorf("%s: %d row(s) in %d-%d of %s are corrupted (v <> id*7 or pad not %d bytes)",
			what, st.Bad, lo, hi, ten.Table, p.PayloadBytes)
	}
	// The checksum is only predictable for a range that is entirely present.
	if st.Rows != hi-lo+1 {
		return nil
	}
	if want := bulkExpectedSum(lo, hi); st.Sum != want {
		return fmt.Errorf("%s: the checksum of %d-%d in %s is %d, expected %d: the range has the right number of rows but not the right ones",
			what, lo, hi, ten.Table, st.Sum, want)
	}
	return nil
}

// verifyIndexMatchesHeap counts the range by every path it has — the primary
// key, the index on v, and a full scan — each forced with session GUCs on a
// connection pinned per worker. A torn write that left an index entry without
// its row (or a row an index cannot find) reads as two different counts, which
// every check above would agree on and miss, since they all take whichever plan
// the planner picked.
//
// Chunked for the same reason as bulkRangeStats, and compared per chunk, which
// says where the two disagree rather than only that they do.
func verifyIndexMatchesHeap(t *core.T, s *core.StepHandle, ten *tenant, lo, hi int64, p bulkLoadKillParams) error {
	chunks := splitBulkRange(lo, hi, bulkChunkCount(lo, hi, p.VerifyChunkRows))
	workers := min(max(p.VerifyParallel, 1), len(chunks))

	var (
		mu                              sync.Mutex
		totalPK, totalScan, totalSecond int64
		firstEr                         error
		wg                              sync.WaitGroup
	)
	work := make(chan [2]int64)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// One pinned connection per worker: the plan is forced with session
			// GUCs, so a worker that shared a connection would be changing another
			// worker's plan underneath it.
			conn, err := ten.Conn.Pool().Acquire(t.Context)
			if err != nil {
				mu.Lock()
				if firstEr == nil {
					firstEr = fmt.Errorf("acquire a connection to count %s: %w", ten.Table, err)
				}
				mu.Unlock()
				for range work {
				}
				return
			}
			defer func() {
				if conn != nil {
					conn.Release()
				}
			}()
			for chunk := range work {
				// One retry on a FRESH connection. Forcing the plan through the index
				// on v makes the slowest read in the test, and losing its connection
				// is not a verdict about the index — the pass that has no retry is the
				// one that fails a 36M-row run for a dropped socket (run 02e15568).
				var (
					pk, scan, second int64
					err              error
				)
				for attempt := 0; attempt < 2; attempt++ {
					cctx, cancel := context.WithTimeout(t.Context, p.SettleTimeout)
					if conn == nil {
						if conn, err = ten.Conn.Pool().Acquire(cctx); err != nil {
							cancel()
							break
						}
					}
					pk, scan, second, err = countChunkEveryWay(cctx, conn, ten, chunk, p)
					cancel()
					if err == nil || t.Context.Err() != nil {
						break
					}
					conn.Release()
					conn = nil
				}
				mu.Lock()
				if err != nil {
					if firstEr == nil {
						firstEr = err
					}
				} else {
					totalPK += pk
					totalScan += scan
					totalSecond += second
				}
				mu.Unlock()
			}
		}()
	}
	for _, chunk := range chunks {
		select {
		case work <- chunk:
		case <-t.Context.Done():
		}
	}
	close(work)
	wg.Wait()
	if firstEr != nil {
		return firstEr
	}
	s.Set("rows_via_primary_key", totalPK)
	s.Set("rows_via_scan", totalScan)
	if p.SecondaryIndex {
		s.Set("rows_via_secondary_index", totalSecond)
	}
	return nil
}

// requirePlanUses fails unless the plan for sql names the index. EXPLAIN without
// ANALYZE, so it costs a plan and not a scan.
func requirePlanUses(ctx context.Context, conn *pgxpool.Conn, sql, index string) error {
	rows, err := conn.Query(ctx, "EXPLAIN "+sql)
	if err != nil {
		return fmt.Errorf("explain %q: %w", sql, err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("read the plan of %q: %w", sql, err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the plan of %q: %w", sql, err)
	}
	if !strings.Contains(plan.String(), index) {
		return fmt.Errorf("the count meant to read %s does not use it — the planner chose another path, so this leg would agree with the heap without touching the index:\nquery: %s\nplan:\n%s",
			index, sql, plan.String())
	}
	return nil
}

// countChunkEveryWay counts one chunk through each access path on a pinned
// connection and reports a disagreement between them.
func countChunkEveryWay(ctx context.Context, conn *pgxpool.Conn, ten *tenant, chunk [2]int64,
	p bulkLoadKillParams) (viaPK, viaScan, viaSecondary int64, err error) {
	{
		byID := fmt.Sprintf("SELECT count(*) FROM %s WHERE id BETWEEN %d AND %d", ten.Table, chunk[0], chunk[1])
		// v is id*7 for every row, so a range of v holds exactly the rows the
		// matching range of id does — and reading it forces the index on v.
		byV := fmt.Sprintf("SELECT count(*) FROM %s WHERE v BETWEEN %d AND %d", ten.Table, chunk[0]*7, chunk[1]*7)

		legs := []struct {
			gucs  []string
			query string
			into  *int64
			name  string
		}{
			{[]string{"SET enable_seqscan = off"}, byID, &viaPK, "the primary key"},
			{[]string{"SET enable_seqscan = on", "SET enable_indexscan = off", "SET enable_bitmapscan = off"}, byID, &viaScan, "a full scan"},
		}
		if p.SecondaryIndex {
			legs = append(legs, struct {
				gucs  []string
				query string
				into  *int64
				name  string
			}{[]string{"SET enable_seqscan = off", "SET enable_indexscan = on", "SET enable_bitmapscan = on"}, byV, &viaSecondary, "the index on v"})
		}
		// Turning seqscan off does not choose WHICH index: the planner can answer
		// a range of v from the primary key with v as a filter, which is exactly
		// its fallback when the index on v is missing or damaged — so the leg
		// would agree with the heap without reading the secondary index at all.
		// The plan has to name the index for the comparison to mean anything.
		if p.SecondaryIndex {
			if err := requirePlanUses(ctx, conn, byV, secondaryIndexName(ten.Table)); err != nil {
				return 0, 0, 0, err
			}
		}
		for _, leg := range legs {
			for _, guc := range leg.gucs {
				if _, err := conn.Exec(ctx, guc); err != nil {
					return 0, 0, 0, fmt.Errorf("%q: %w", guc, err)
				}
			}
			if err := conn.QueryRow(ctx, leg.query).Scan(leg.into); err != nil {
				return 0, 0, 0, fmt.Errorf("count %d-%d of %s through %s: %w", chunk[0], chunk[1], ten.Table, leg.name, err)
			}
		}
		if viaPK != viaScan {
			return 0, 0, 0, fmt.Errorf("%s holds %d row(s) in %d-%d read through its primary key and %d read by a full scan: the index and the heap disagree, so a killed write left one of them with rows the other does not have",
				ten.Table, viaPK, chunk[0], chunk[1], viaScan)
		}
		if p.SecondaryIndex && viaSecondary != viaScan {
			return 0, 0, 0, fmt.Errorf("%s holds %d row(s) in %d-%d read through the index on v and %d read by a full scan: a killed write left the secondary index and the heap with different rows, which is index maintenance that did not survive the kill",
				ten.Table, viaSecondary, chunk[0], chunk[1], viaScan)
		}
	}
	return viaPK, viaScan, viaSecondary, nil
}

// bulkExpectedSum is sum(id*7) over the inclusive range, the value a complete
// range must check to.
func bulkExpectedSum(lo, hi int64) int64 {
	n := hi - lo + 1
	return 7 * (lo + hi) * n / 2
}
