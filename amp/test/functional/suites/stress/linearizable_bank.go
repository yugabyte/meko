// The only stress test that checks a recorded history rather than the set of ids a writer was acked for.
package stress

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/history"
	"github.com/yugabyte/amp/test/functional/framework/chaos"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/linearizable_bank",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack, FaultsSharedInfra: true},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Run a recorded bank workload against every tenant while pod_delete faults hit amp's endpoint computes and the backing YugabyteDB, then check the history offline for a total that changed, a negative balance under a guarded transfer, a session snapshot that went backwards, an acknowledged transfer the final state does not reflect, and write skew under SERIALIZABLE. Each tenant's branch is then forked at the WAL position of a recorded snapshot, and the fork must hold exactly that snapshot's balances.",
		Params:      []string{"stress.linearizable_bank", "chaos"},
		Timeout:     120 * time.Minute,
		Run:         runLinearizableBank,
	})
}

type linearizableBankParams struct {
	Tenants          int
	Accounts         int
	SkewPairs        int
	Writers          int
	Readers          int
	Isolation        string
	Unguarded        bool
	InitialBalance   int64
	MaxAmount        int64
	FlushOps         int
	ReadInterval     time.Duration
	BaselineDuration time.Duration
	Duration         time.Duration
	SettleTimeout    time.Duration
	ResumeDuration   time.Duration
}

func linearizableBankParamsFromConfig(c *core.Config) linearizableBankParams {
	return linearizableBankParams{
		Tenants:          c.GetInt("stress.linearizable_bank.tenants", 3),
		Accounts:         c.GetInt("stress.linearizable_bank.accounts", 10),
		SkewPairs:        c.GetInt("stress.linearizable_bank.skew_pairs", 4),
		Writers:          c.GetInt("stress.linearizable_bank.writers", 4),
		Readers:          c.GetInt("stress.linearizable_bank.readers", 2),
		Isolation:        c.GetString("stress.linearizable_bank.isolation", "read committed"),
		Unguarded:        c.GetBool("stress.linearizable_bank.unguarded", false),
		InitialBalance:   int64(c.GetInt("stress.linearizable_bank.initial_balance", 2000)),
		MaxAmount:        int64(c.GetInt("stress.linearizable_bank.max_amount", 500)),
		FlushOps:         c.GetInt("stress.linearizable_bank.flush_ops", 200),
		ReadInterval:     c.GetDuration("stress.linearizable_bank.read_interval", 200*time.Millisecond),
		BaselineDuration: c.GetDuration("stress.linearizable_bank.baseline_duration", 20*time.Second),
		Duration:         c.GetDuration("stress.linearizable_bank.duration", 3*time.Minute),
		SettleTimeout:    c.GetDuration("stress.linearizable_bank.settle_timeout", 10*time.Minute),
		ResumeDuration:   c.GetDuration("stress.linearizable_bank.resume_duration", 30*time.Second),
	}
}

type bankTenant struct {
	bank *history.Bank
	rec  *history.Recorder
	path string
}

func runLinearizableBank(t *core.T) error {
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
	params := linearizableBankParamsFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)

	dir, err := os.MkdirTemp("", "ampqa-linbank-")
	if err != nil {
		return core.AsEnvFailure(fmt.Errorf("create a directory for the recorded histories: %w", err))
	}
	defer os.RemoveAll(dir)

	fleet, err := newTenantFleet(t, env, logf)
	if err != nil {
		return err
	}
	defer fleet.Close()

	var banks []bankTenant
	defer func() {
		for _, bt := range banks {
			bt.rec.Close()
		}
	}()

	if err := t.Step("create_tenants", func(s *core.StepHandle) error {
		if err := fleet.Create(t, params.Tenants); err != nil {
			return err
		}
		s.Set("tenants", len(fleet.Tenants))
		banks, err = openBanks(t, fleet, dir, params)
		if err != nil {
			return err
		}
		s.Set("accounts_per_tenant", banks[0].bank.NumAccounts())
		s.Set("isolation", params.Isolation)
		s.Set("guarded", !params.Unguarded)
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("bank_baseline", func(s *core.StepHandle) error {
		runBanks(t.Context, banks, t.Seed.Fork("baseline").Value(), params.BaselineDuration)
		return recordBankStats(s, banks, "no tenant committed a single transfer before the faults started, so there is no history to check")
	}); err != nil {
		return err
	}

	if err := t.Step("chaos_under_load", func(s *core.StepHandle) error {
		return linearizableBankChaos(t, s, ampEnv, backingEnv, fleet, banks, params, logf)
	}); err != nil {
		return err
	}

	if err := t.Step("backing_ready", func(s *core.StepHandle) error {
		waited, err := awaitBackingReady(t, backingEnv.BackingKube(), params.SettleTimeout)
		s.Set("ready_after", waited.Round(time.Second).String())
		return err
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

	if err := t.Step("bank_after_recovery", func(s *core.StepHandle) error {
		before := totalCommitted(banks)
		runBanks(t.Context, banks, t.Seed.Fork("resume").Value(), params.ResumeDuration)
		s.Set("committed_after_recovery", totalCommitted(banks)-before)
		if totalCommitted(banks) == before {
			return fmt.Errorf("no tenant committed a transfer in %s after the faults stopped and every endpoint answered SELECT 1", params.ResumeDuration)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("fork_at_snapshot_lsn", func(s *core.StepHandle) error {
		return forkAtBankSnapshot(t, s, env, fleet, banks)
	}); err != nil {
		return err
	}

	return t.Step("check_history", func(s *core.StepHandle) error {
		return checkBankHistories(t, s, fleet, banks, params)
	})
}

func openBanks(t *core.T, fleet *tenantFleet, dir string, params linearizableBankParams) ([]bankTenant, error) {
	out := make([]bankTenant, 0, len(fleet.Tenants))
	for i := range fleet.Tenants {
		ten := &fleet.Tenants[i]
		path := filepath.Join(dir, fmt.Sprintf("history-tenant-%d.jsonl", ten.Index))
		rec, err := history.NewRecorder(path, params.FlushOps)
		if err != nil {
			return nil, core.AsEnvFailure(err)
		}
		bank, err := history.NewBank(ten.Conn.Pool(), rec, fmt.Sprintf("t%d", ten.Index), history.BankConfig{
			Table:        fmt.Sprintf("bank_%d", ten.Index),
			Accounts:     params.Accounts,
			SkewPairs:    params.SkewPairs,
			Initial:      params.InitialBalance,
			MaxAmount:    params.MaxAmount,
			Writers:      params.Writers,
			Readers:      params.Readers,
			Isolation:    params.Isolation,
			Unguarded:    params.Unguarded,
			ReadInterval: params.ReadInterval,
		})
		if err != nil {
			return nil, err
		}
		if err := bank.Setup(t.Context); err != nil {
			return nil, fmt.Errorf("tenant %d: %w", ten.Index, err)
		}
		out = append(out, bankTenant{bank: bank, rec: rec, path: path})
	}
	return out, nil
}

func runBanks(ctx context.Context, banks []bankTenant, seed int64, d time.Duration) {
	var wg sync.WaitGroup
	for i := range banks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			banks[i].bank.Run(ctx, seed+int64(i), d)
		}(i)
	}
	wg.Wait()
}

func totalCommitted(banks []bankTenant) int64 {
	var n int64
	for i := range banks {
		n += banks[i].bank.Stats().Committed
	}
	return n
}

func recordBankStats(s *core.StepHandle, banks []bankTenant, noCommitMsg string) error {
	var committed, failed, unknown, reads int64
	for i := range banks {
		st := banks[i].bank.Stats()
		committed += st.Committed
		failed += st.Failed
		unknown += st.Unknown
		reads += st.Reads
		if err := banks[i].bank.RecordError(); err != nil {
			return core.AsEnvFailure(fmt.Errorf("tenant %d's history lost an operation, so it can no longer be checked: %w", i, err))
		}
	}
	s.Set("committed", committed)
	s.Set("failed", failed)
	s.Set("unknown", unknown)
	s.Set("reads", reads)
	if committed == 0 {
		return fmt.Errorf("%s", noCommitMsg)
	}
	return nil
}

func linearizableBankChaos(t *core.T, s *core.StepHandle, ampEnv ampKubeEnv, backingEnv backingKubeEnv,
	fleet *tenantFleet, banks []bankTenant, params linearizableBankParams, logf func(string, ...any)) error {
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
		return fmt.Errorf("the control-plane namespace has no pod for any of this fleet's %d endpoints to aim at", len(fleet.Tenants))
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
	runner, err := chaos.NewRunner(t.Seed.Fork("linearizable-bank"), chaos.DefaultRegistry(), cfg, targets)
	if err != nil {
		return err
	}
	if err := runner.Start(); err != nil {
		return err
	}
	before := totalCommitted(banks)
	runBanks(t.Context, banks, t.Seed.Fork("chaos").Value(), params.Duration)
	runner.Stop()

	committedUnderFault := totalCommitted(banks) - before
	if err := recordBankStats(s, banks, "no tenant committed a transfer at any point in the run"); err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	s.Set("injects", injects)
	s.Set("committed_under_fault", committedUnderFault)
	if injects == 0 {
		return fmt.Errorf("the chaos loop injected no fault in %s, so this run proves nothing about the invariants under fault", params.Duration)
	}
	if committedUnderFault == 0 {
		return fmt.Errorf("%d fault(s) were injected but no transfer was acknowledged during the %s window, so no committed operation crossed a fault and the history proves nothing",
			injects, params.Duration)
	}
	return nil
}

func forkAtBankSnapshot(t *core.T, s *core.StepHandle, env ampTenantEnv, fleet *tenantFleet, banks []bankTenant) error {
	type result struct {
		lsn string
		err error
	}
	results := make([]result, len(fleet.Tenants))
	var wg sync.WaitGroup
	for i := range fleet.Tenants {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lsn, err := forkOneBankSnapshot(t, env, fleet, &fleet.Tenants[i], banks[i])
			results[i] = result{lsn: lsn, err: err}
		}(i)
	}
	wg.Wait()

	lsns := make([]string, 0, len(results))
	for i, r := range results {
		if r.err != nil {
			return r.err
		}
		lsns = append(lsns, fmt.Sprintf("%d=%s", fleet.Tenants[i].Index, r.lsn))
	}
	s.Set("forks_verified", len(results))
	s.Set("fork_lsns", strings.Join(lsns, ","))
	return nil
}

func forkOneBankSnapshot(t *core.T, env ampTenantEnv, fleet *tenantFleet, ten *tenant, bt bankTenant) (string, error) {
	snapshot, lsn, err := bt.bank.Snapshot(t.Context, fmt.Sprintf("t%d-fork", ten.Index))
	if err != nil {
		return "", fmt.Errorf("tenant %d: record a snapshot to fork at: %w", ten.Index, err)
	}
	name := fmt.Sprintf("linbank-fork-%d-%s", ten.Index, t.ResourceSuffix())
	if _, err := fleet.cl.CreateBranchAndWait(t.Context, fleet.poolID, ten.ClusterID, controller.CreateBranchRequest{
		Name:           name,
		ParentBranchID: ten.BranchID,
		ForkPointLSN:   lsn,
	}); err != nil {
		return lsn, fmt.Errorf("tenant %d: fork branch %s at LSN %s: %w", ten.Index, ten.BranchID, lsn, err)
	}
	branches, err := fleet.cl.ListAllBranches(t.Context, fleet.poolID, ten.ClusterID)
	if err != nil {
		return lsn, fmt.Errorf("tenant %d: list branches after the fork: %w", ten.Index, err)
	}
	forkID := ""
	for _, b := range branches {
		if b.Name == name {
			forkID = b.BranchID
		}
	}
	if forkID == "" {
		return lsn, fmt.Errorf("tenant %d: branch %q is absent after a successful fork", ten.Index, name)
	}
	if _, err := fleet.cl.CreateEndpointAndWait(t.Context, fleet.poolID, ten.ClusterID, forkID,
		controller.CreateEndpointRequest{Role: tenantRole, Resources: fleet.endpointResources()}); err != nil {
		return lsn, fmt.Errorf("tenant %d: create an endpoint on the fork at LSN %s: %w", ten.Index, lsn, err)
	}
	eps, err := fleet.cl.ListAllEndpoints(t.Context, fleet.poolID, ten.ClusterID, forkID)
	if err != nil {
		return lsn, fmt.Errorf("tenant %d: list the fork's endpoints: %w", ten.Index, err)
	}
	if len(eps) == 0 {
		return lsn, fmt.Errorf("tenant %d: the fork lists no endpoint after a successful create", ten.Index)
	}
	conn, err := env.ConnectEndpoint(t.Context, eps[len(eps)-1])
	if err != nil {
		return lsn, fmt.Errorf("tenant %d: connect the fork's endpoint: %w", ten.Index, err)
	}
	defer conn.Close()

	onFork := map[int]history.Account{}
	table := fmt.Sprintf("bank_%d", ten.Index)
	err = conn.Query(t.Context, func(r pgx.Rows) error {
		var a history.Account
		if err := r.Scan(&a.ID, &a.Pair, &a.Bal, &a.Ver); err != nil {
			return err
		}
		onFork[a.ID] = a
		return nil
	}, fmt.Sprintf("SELECT id, pair, bal, ver FROM %s ORDER BY id", table))
	if err != nil {
		return lsn, fmt.Errorf("tenant %d: read %s on the fork at LSN %s: %w", ten.Index, table, lsn, err)
	}
	if len(onFork) != len(snapshot) {
		return lsn, fmt.Errorf("tenant %d: the fork at LSN %s holds %d account(s), the snapshot recorded at that position holds %d",
			ten.Index, lsn, len(onFork), len(snapshot))
	}
	for _, want := range snapshot {
		got, ok := onFork[want.ID]
		if !ok {
			return lsn, fmt.Errorf("tenant %d: the fork at LSN %s is missing account %d, which the snapshot recorded at that position holds at balance %d",
				ten.Index, lsn, want.ID, want.Bal)
		}
		if got.Bal != want.Bal || got.Ver != want.Ver {
			return lsn, fmt.Errorf("tenant %d: the fork at LSN %s serves account %d at balance %d version %d; the snapshot recorded at that position reads balance %d version %d",
				ten.Index, lsn, want.ID, got.Bal, got.Ver, want.Bal, want.Ver)
		}
	}
	return lsn, nil
}

const historyAttachmentLimit = 8 << 20

func checkBankHistories(t *core.T, s *core.StepHandle, fleet *tenantFleet, banks []bankTenant, params linearizableBankParams) error {
	serializable := strings.EqualFold(strings.TrimSpace(params.Isolation), "serializable")
	var violations []string
	var ops, reads int64
	for i := range banks {
		if err := banks[i].rec.Close(); err != nil {
			return core.AsEnvFailure(fmt.Errorf("tenant %d: close the recorded history: %w", i, err))
		}
		if err := banks[i].bank.RecordError(); err != nil {
			return core.AsEnvFailure(fmt.Errorf("tenant %d's history lost an operation, so it can no longer be checked: %w", i, err))
		}
		attachHistory(t, fmt.Sprintf("history-tenant-%d.jsonl", fleet.Tenants[i].Index), banks[i].path)
		rep, err := history.Check(banks[i].path, history.CheckConfig{
			Initial:      params.InitialBalance,
			Total:        banks[i].bank.Total(),
			Serializable: serializable,
		})
		if err != nil {
			return fmt.Errorf("tenant %d: check the recorded history: %w", i, err)
		}
		ops += rep.Ops
		reads += rep.Reads
		if len(rep.Violations) > 0 {
			violations = append(violations, fmt.Sprintf("tenant %d: %s", fleet.Tenants[i].Index, rep.Summary()))
		}
	}
	s.Set("ops_checked", ops)
	s.Set("snapshots_checked", reads)
	t.Metric("history_ops_checked", float64(ops), "ops")
	t.Metric("history_snapshots_checked", float64(reads), "snapshots")
	if reads == 0 {
		return fmt.Errorf("no tenant recorded a single successful snapshot read, so every invariant was vacuous")
	}
	if len(violations) > 0 {
		return fmt.Errorf("the recorded history breaks %d of the bank's invariants: %s",
			len(violations), strings.Join(violations, " | "))
	}
	return nil
}

func attachHistory(t *core.T, name, path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, historyAttachmentLimit))
	if err != nil || len(body) == 0 {
		return
	}
	t.AttachBytes(name, "application/x-ndjson", body)
}
