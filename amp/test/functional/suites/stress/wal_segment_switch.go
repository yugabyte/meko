// WAL shapes the other writers never produce, under safekeeper restarts: a
// segment switch's padding (meko#799) and a record past the anchor scan window
// (meko#800). meko#799 needs an anchor below the switch, which find_anchor never
// picks once a fresh page follows it, so only meko#800 can fire here.
package stress

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/wal_segment_switch",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamCompute,
		Description: "Write one WAL record larger than the safekeeper's 64 MiB anchor scan window as the last WAL of the branch, freeze the pageserver and SIGKILL the safekeeper on top of it, recording whether the pageserver was still below the record (the meko#800 shape); then run journaled writers while a second session calls pg_switch_wal on a timer and SIGKILL the safekeeper right after a switch with acknowledged commits above the padding, kill the compute once so it restarts from a basebackup on the far side of a switch, and split the amp_storage.wals tablets before a final safekeeper kill. After every restart the endpoint must serve and commit again, and every acknowledged id must be in storage.",
		Params:      []string{"stress.wal_segment_switch", "stress.consistency"},
		Timeout:     75 * time.Minute,
		Run:         runWalSegmentSwitch,
	})
}

type walSwitchParams struct {
	Tenants          int
	BaselineDuration time.Duration
	Rounds           int
	RoundDuration    time.Duration
	SwitchInterval   time.Duration
	PostSwitchWrites time.Duration
	LargeRecordBytes int
	SplitWalTablets  bool
	SettleTimeout    time.Duration
	ResumeDuration   time.Duration
	PayloadBytes     int
}

func walSwitchParamsFromConfig(c *core.Config) walSwitchParams {
	p := "stress.wal_segment_switch."
	return walSwitchParams{
		Tenants:          c.GetInt(p+"tenants", 1),
		BaselineDuration: c.GetDuration(p+"baseline_duration", 20*time.Second),
		Rounds:           c.GetInt(p+"rounds", 3),
		RoundDuration:    c.GetDuration(p+"round_duration", 30*time.Second),
		SwitchInterval:   c.GetDuration(p+"switch_interval", 3*time.Second),
		PostSwitchWrites: c.GetDuration(p+"post_switch_writes", 2*time.Second),
		LargeRecordBytes: c.GetInt(p+"large_record_bytes", 80<<20),
		SplitWalTablets:  c.GetBool(p+"split_wal_tablets", true),
		SettleTimeout:    c.GetDuration(p+"settle_timeout", 10*time.Minute),
		ResumeDuration:   c.GetDuration(p+"resume_duration", 30*time.Second),
		PayloadBytes:     c.GetInt(p+"payload_bytes", 256),
	}
}

type walSwitchRound struct {
	Kill  string
	Split bool
}

func (p walSwitchParams) plan() []walSwitchRound {
	var rounds []walSwitchRound
	for i := 0; i < p.Rounds; i++ {
		r := walSwitchRound{Kill: "safekeeper"}
		if i == 1 {
			r.Kill = "compute_ctl"
		}
		if i == p.Rounds-1 && p.SplitWalTablets {
			r.Split = true
		}
		rounds = append(rounds, r)
	}
	return rounds
}

func runWalSegmentSwitch(t *core.T) error {
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	ampEnv, ok := t.Env.(ampKubeEnv)
	if !ok || ampEnv.AmpKube() == nil {
		t.Skip("requires a kube handle for the control-plane namespace: the safekeeper is killed by pid inside the endpoint pod")
		return nil
	}
	params := walSwitchParamsFromConfig(t.Options)
	if params.Rounds < 1 {
		return fmt.Errorf("stress.wal_segment_switch.rounds is %d, so no restart would land on a switched segment", params.Rounds)
	}
	logf := t.Masker().Logf(log.Printf)
	kube := ampEnv.AmpKube()
	var backing *k8s.Kubectl
	if be, ok := t.Env.(backingKubeEnv); ok {
		backing = be.BackingKube()
	}

	fleet, err := newTenantFleet(t, env, logf)
	if err != nil {
		return err
	}
	defer fleet.Close()
	cdp := commitDurabilityParams{PayloadBytes: params.PayloadBytes}

	var journals []*AckJournal
	if err := t.Step("create_tenants", func(s *core.StepHandle) error {
		if err := fleet.Create(t, params.Tenants); err != nil {
			return err
		}
		for range fleet.Tenants {
			journals = append(journals, NewAckJournal())
		}
		s.Set("tenants", len(fleet.Tenants))
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("seed_writes", func(s *core.StepHandle) error {
		runJournaledWriters(t.Context, env, fleet.Tenants, journals, t.Seed.Fork("seed"), params.BaselineDuration, cdp, nil)
		if totalAcked(journals) == 0 {
			return fmt.Errorf("no write was acknowledged in %s before any WAL was switched", params.BaselineDuration)
		}
		return nil
	}); err != nil {
		return err
	}

	if params.LargeRecordBytes > 0 {
		if err := t.Step("large_record_on_top_then_safekeeper_kill", func(s *core.StepHandle) error {
			return largeRecordOnTop(t, s, env, kube, backing, fleet, journals, params)
		}); err != nil {
			return err
		}
	}

	for i, round := range params.plan() {
		name := fmt.Sprintf("round_%d_%s", i, round.Kill)
		if round.Split {
			name += "_after_wal_split"
		}
		if err := t.Step(name, func(s *core.StepHandle) error {
			if round.Split {
				if backing == nil {
					s.Set("wal_split", "skipped: no kube handle for the backing namespace")
				} else if err := splitWalTablets(t, s, backing, logf); err != nil {
					return err
				}
			}
			if err := switchRound(t, s, name, env, kube, fleet, journals, params, round, cdp); err != nil {
				return err
			}
			if err := assertEndpointsServe(t, env, fleet.Tenants, params.SettleTimeout); err != nil {
				return fmt.Errorf("after the %s kill on a switched segment: %w", round.Kill, err)
			}
			return verifyExactSets(t, s, env, fleet.Tenants, journals, params.PayloadBytes)
		}); err != nil {
			return err
		}
	}

	if err := t.Step("writes_flow_again", func(s *core.StepHandle) error {
		before := totalAcked(journals)
		runJournaledWriters(t.Context, env, fleet.Tenants, journals, t.Seed.Fork("resume"), params.ResumeDuration, cdp, nil)
		if totalAcked(journals) == before {
			return fmt.Errorf("no write was acknowledged in %s after the last restart, though every endpoint answers SELECT 1", params.ResumeDuration)
		}
		return verifyExactSets(t, s, env, fleet.Tenants, journals, params.PayloadBytes)
	}); err != nil {
		return err
	}

	return t.Step("physical_consistency", func(s *core.StepHandle) error {
		return verifyPhysicalConsistency(t, s, env, fleet.Tenants)
	})
}

func switchRound(t *core.T, s *core.StepHandle, name string, env ampTenantEnv, kube *k8s.Kubectl, fleet *tenantFleet,
	journals []*AckJournal, params walSwitchParams, round walSwitchRound, cdp commitDurabilityParams) error {
	wctx, stop := context.WithCancel(t.Context)
	defer stop()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runJournaledWriters(wctx, env, fleet.Tenants, journals, t.Seed.Fork(name+"-writers"),
			params.RoundDuration+params.SettleTimeout, cdp, nil)
	}()

	switches, err := switchWAL(t, s, env, fleet.Tenants, params)
	if err != nil {
		stop()
		wg.Wait()
		return err
	}
	s.Set("pg_switch_wal_calls", switches)
	if switches == 0 {
		stop()
		wg.Wait()
		return fmt.Errorf("every pg_switch_wal call failed in %s (last error in the step's last_switch_error), so no restart lands on a switch", params.RoundDuration)
	}

	ackedBefore := totalAcked(journals)
	if err := forceSwitch(t.Context, env, fleet.Tenants); err != nil {
		stop()
		wg.Wait()
		return err
	}
	if err := sleepCtx(t.Context, params.PostSwitchWrites); err != nil {
		stop()
		wg.Wait()
		return err
	}
	if totalAcked(journals) == ackedBefore {
		stop()
		wg.Wait()
		return fmt.Errorf("no write was acknowledged in the %s after the last WAL switch, so the %s kill has no commit above the padding to lose", params.PostSwitchWrites, round.Kill)
	}

	var targets []string
	for i := range fleet.Tenants {
		pod, err := endpointPodFor(t, kube, fleet.Tenants[i].Endpoint)
		if err != nil {
			stop()
			wg.Wait()
			return err
		}
		rec, err := killDaemonOnce(t, kube.ForPod(pod, ""), round.Kill)
		if err != nil {
			stop()
			wg.Wait()
			return fmt.Errorf("the %s kill after the WAL switch did not land: %w", round.Kill, err)
		}
		targets = append(targets, fmt.Sprintf("%s@%s(%s)", round.Kill, pod, joinPIDs(rec.PIDs, ",")))
	}
	s.Set("killed", strings.Join(targets, " "))

	if err := assertEndpointsServe(t, env, fleet.Tenants, params.SettleTimeout); err != nil {
		stop()
		wg.Wait()
		return err
	}
	stop()
	wg.Wait()
	return nil
}

// anchorScanWindow is the safekeeper's MAX_ANCHOR_SCAN_BYTES.
const anchorScanWindow = 64 << 20

// pageserverPosition returns the anchor walk's length: the WAL end minus the
// branch's consistent_lsn, read from YugabyteDB since the pageserver is frozen.
// -1 means unread.
func pageserverPosition(ctx context.Context, backing *k8s.Kubectl, branchID, walEnd string) (string, int64) {
	if backing == nil {
		return "unread: no kube handle for the backing namespace", -1
	}
	end, err := parseLSN(walEnd)
	if err != nil {
		return "unread: " + err.Error(), -1
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pod, err := firstTserverPod(cctx, backing)
	if err != nil {
		return "unread: " + err.Error(), -1
	}
	q := fmt.Sprintf("SELECT coalesce(consistent_lsn, -1) FROM amp_storage.branch_state WHERE branch_id = '%s'", branchID)
	out, err := backing.ForPod(pod, tserverContainer).Run(cctx,
		"ysqlsh -h $(hostname)."+tserverHeadlessOf(pod)+" -U yugabyte -d "+ampStorageDB+" -v ON_ERROR_STOP=1 -tAc "+shellQuote(q))
	if err != nil || out.ExitCode != 0 {
		return fmt.Sprintf("unread: ysqlsh: %v %s", err, strings.TrimSpace(out.Stderr)), -1
	}
	got, err := strconv.ParseInt(strings.TrimSpace(out.Stdout), 10, 64)
	if err != nil || got < 0 {
		return "unread: " + strings.TrimSpace(out.Stdout), -1
	}
	return fmt.Sprintf("consistent_lsn=%d wal_end=%d walk_bytes=%d", got, end, end-got), end - got
}

func parseLSN(s string) (int64, error) {
	hi, lo, ok := strings.Cut(s, "/")
	if !ok {
		return 0, fmt.Errorf("not an LSN: %q", s)
	}
	h, err := strconv.ParseUint(hi, 16, 32)
	if err != nil {
		return 0, err
	}
	l, err := strconv.ParseUint(lo, 16, 32)
	if err != nil {
		return 0, err
	}
	return int64(h<<32 | l), nil
}

func switchWAL(t *core.T, s *core.StepHandle, env ampTenantEnv, tenants []tenant, params walSwitchParams) (int, error) {
	deadline := time.Now().Add(params.RoundDuration)
	switches := 0
	for time.Now().Before(deadline) {
		if err := forceSwitch(t.Context, env, tenants); err == nil {
			switches++
		} else {
			s.Set("last_switch_error", err.Error())
		}
		if err := sleepCtx(t.Context, params.SwitchInterval); err != nil {
			return switches, err
		}
	}
	return switches, nil
}

// The pageserver is frozen the moment the record commits, while it is still
// ingesting it; freezing it before the emit stalls the commit.
func largeRecordOnTop(t *core.T, s *core.StepHandle, env ampTenantEnv, kube, backing *k8s.Kubectl, fleet *tenantFleet,
	journals []*AckJournal, params walSwitchParams) error {
	var targets []string
	var maxWalk int64
	for i := range fleet.Tenants {
		ten := &fleet.Tenants[i]
		pod, err := endpointPodFor(t, kube, ten.Endpoint)
		if err != nil {
			return err
		}
		ex := kube.ForPod(pod, "")
		ps, err := daemonPIDs(t.Context, ex, daemonProcess("pageserver"))
		if err != nil || len(ps) == 0 {
			return fmt.Errorf("no pageserver process to freeze in pod %s (pids %v): %v", pod, ps, err)
		}
		thaw := func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, _ = ex.Run(ctx, "kill -CONT "+joinPIDs(ps, " ")+" 2>/dev/null; exit 0")
		}
		start, err := ten.Conn.QueryString(t.Context, "SELECT pg_current_wal_insert_lsn()::text")
		if err != nil {
			return err
		}
		if err := emitLargeRecord(t.Context, env, ten, params.LargeRecordBytes); err != nil {
			return err
		}
		end, err := ten.Conn.QueryString(t.Context, "SELECT pg_current_wal_insert_lsn()::text")
		if err != nil {
			return err
		}
		if _, err := ex.Run(t.Context, "kill -STOP "+joinPIDs(ps, " ")); err != nil {
			thaw()
			return fmt.Errorf("SIGSTOP pageserver pid(s) %s: %w", joinPIDs(ps, ","), err)
		}
		position, walk := pageserverPosition(t.Context, backing, ten.BranchID, end)
		if walk > maxWalk {
			maxWalk = walk
		}
		rec, err := killDaemonOnce(t, ex, "safekeeper")
		thaw()
		if err != nil {
			return fmt.Errorf("the safekeeper kill on top of the large record, with the pageserver frozen, did not land: %w", err)
		}
		lsn, err := ten.Conn.QueryString(t.Context, "SELECT pg_current_wal_flush_lsn()::text")
		if err != nil {
			return err
		}
		targets = append(targets, fmt.Sprintf("safekeeper@%s(%s) at %s, pageserver %s frozen", pod, joinPIDs(rec.PIDs, ","), lsn, joinPIDs(ps, ",")))
		s.Set(fmt.Sprintf("pageserver_position_%d", i), position)
		s.Set(fmt.Sprintf("large_record_start_%d", i), start)
		s.Set(fmt.Sprintf("large_record_end_%d", i), end)
	}
	s.Set("killed", strings.Join(targets, " "))
	s.Set("large_record_mib", params.LargeRecordBytes>>20)
	reached := maxWalk > anchorScanWindow
	s.Set("anchor_walk_bytes", maxWalk)
	s.Set("reached_meko800_shape", reached)
	shape := "the anchor walk did not have to cross the whole record"
	if reached {
		shape = fmt.Sprintf("the anchor walk had to cross %d MiB of one record, more than its %d MiB window (the meko#800 shape)", maxWalk>>20, anchorScanWindow>>20)
	}
	if err := assertEndpointsServe(t, env, fleet.Tenants, params.SettleTimeout); err != nil {
		return fmt.Errorf("after a safekeeper restart on top of a %d MiB record, where %s: %w", params.LargeRecordBytes>>20, shape, err)
	}
	// The whole settle window: the thawed pageserver takes a while to ingest
	// 80 MiB, and meko#800 never recovers however long it is given.
	waited, err := awaitWritePath(t.Context, env, fleet.Tenants, journals, params.SettleTimeout, params.PayloadBytes)
	if err != nil {
		return fmt.Errorf("the endpoint answers SELECT 1 after the safekeeper restarted on top of a %d MiB record, where %s, but no commit was acknowledged in %s: %w",
			params.LargeRecordBytes>>20, shape, params.SettleTimeout, err)
	}
	s.Set("first_commit_after_restart_seconds", waited.Seconds())
	return verifyExactSets(t, s, env, fleet.Tenants, journals, params.PayloadBytes)
}

func forceSwitch(ctx context.Context, env ampTenantEnv, tenants []tenant) error {
	var errs []error
	for i := range tenants {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := tenants[i].Conn.QueryString(cctx, "SELECT pg_switch_wal()::text")
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %d: pg_switch_wal: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

// A logical message is one XLOG record whatever its size; TOAST would split a
// wide row into many.
func emitLargeRecord(ctx context.Context, env ampTenantEnv, ten *tenant, n int) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if _, err := ten.Conn.QueryString(cctx,
		"SELECT pg_logical_emit_message(true, 'ampqa_wal_segment_switch', repeat('w', $1::int))::text", n); err != nil {
		return fmt.Errorf("tenant %d: emit a %d MiB WAL record: %w", ten.Index, n>>20, err)
	}
	return nil
}

func splitWalTablets(t *core.T, s *core.StepHandle, backing *k8s.Kubectl, logf func(string, ...any)) error {
	admin, err := backingAdmin(t.Context, backing, logf)
	if err != nil {
		return fmt.Errorf("reach yb-admin on the backing: %w", err)
	}
	tbl, err := admin.FindTable(t.Context, ampStorageDB, "wals")
	if err != nil {
		return err
	}
	tablets, err := admin.ListTablets(t.Context, tbl.ID, 0)
	if err != nil {
		return err
	}
	if _, err := admin.Run(t.Context, "flush_table_by_id", tbl.ID, "60"); err != nil {
		logf("wal_segment_switch: flush wals before the split: %v", err)
	}
	var split, refused []string
	for _, tab := range tablets {
		if err := admin.SplitTablet(t.Context, tab.ID); err != nil {
			refused = append(refused, tab.ID)
			continue
		}
		split = append(split, tab.ID)
	}
	s.Set("wal_tablets_before", len(tablets))
	s.Set("wal_tablets_split", len(split))
	if len(split) == 0 {
		return fmt.Errorf("yb-admin refused to split every one of the %d amp_storage.wals tablets (%s), so the safekeeper never reads WAL across a split", len(tablets), strings.Join(refused, ", "))
	}
	return t.WaitFor("amp_storage.wals to show the split tablets", 10*time.Minute, 10*time.Second, func() (bool, error) {
		now, err := admin.ListTablets(t.Context, tbl.ID, 0)
		if err != nil {
			return false, nil
		}
		s.Set("wal_tablets_after", len(now))
		return len(now) > len(tablets), nil
	})
}
