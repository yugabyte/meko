// The other durability tests write at the default synchronous_commit only, so a
// level that acks before the safekeeper has the commit (meko#1564) passes them.
package stress

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/sync_commit_levels",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamCompute,
		Description: "Write at every synchronous_commit level at once (on, local, remote_write, remote_apply, off, and local set per transaction), each from its own session with every outcome journaled and timed. Freeze the endpoint's safekeeper with SIGSTOP and require that no durable level acknowledges a commit issued while it is frozen (meko#1564), then kill the compute while it is still frozen and, after a plain crash under load as well, require every commit a durable level acknowledged to be in storage. off may lose commits but may never invent one, and remote_apply with no replica must not hang (meko#1573).",
		Params:      []string{"stress.sync_commit_levels", "stress.consistency"},
		Timeout:     60 * time.Minute,
		Run:         runSyncCommitLevels,
	})
}

type syncLevel struct {
	Name    string
	Setting string
	PerTxn  bool
	Durable bool
}

const levelConnectBudget = 5 * time.Minute

var allSyncLevels = []syncLevel{
	{Name: "on", Setting: "on", Durable: true},
	{Name: "local", Setting: "local", Durable: true},
	{Name: "remote_write", Setting: "remote_write", Durable: true},
	{Name: "remote_apply", Setting: "remote_apply", Durable: true},
	{Name: "local_txn", Setting: "local", PerTxn: true, Durable: true},
	{Name: "off", Setting: "off"},
}

type syncCommitParams struct {
	Levels           []string
	BaselineDuration time.Duration
	FreezeLead       time.Duration
	FreezeDuration   time.Duration
	FreezeGrace      time.Duration
	KillLead         time.Duration
	CrashLead        time.Duration
	CrashTail        time.Duration
	SettleTimeout    time.Duration
	ResumeDuration   time.Duration
	PayloadBytes     int
}

func syncCommitParamsFromConfig(c *core.Config) syncCommitParams {
	p := "stress.sync_commit_levels."
	return syncCommitParams{
		Levels:           c.GetStringSlice(p+"levels", []string{"on", "local", "remote_write", "remote_apply", "local_txn", "off"}),
		BaselineDuration: c.GetDuration(p+"baseline_duration", 20*time.Second),
		FreezeLead:       c.GetDuration(p+"freeze_lead", 5*time.Second),
		FreezeDuration:   c.GetDuration(p+"freeze_duration", 10*time.Second),
		FreezeGrace:      c.GetDuration(p+"freeze_grace", time.Second),
		KillLead:         c.GetDuration(p+"kill_lead", 3*time.Second),
		CrashLead:        c.GetDuration(p+"crash_lead", 10*time.Second),
		CrashTail:        c.GetDuration(p+"crash_tail", 20*time.Second),
		SettleTimeout:    c.GetDuration(p+"settle_timeout", 10*time.Minute),
		ResumeDuration:   c.GetDuration(p+"resume_duration", 20*time.Second),
		PayloadBytes:     c.GetInt(p+"payload_bytes", 256),
	}
}

func (p syncCommitParams) levels() ([]syncLevel, error) {
	var out []syncLevel
	for _, name := range p.Levels {
		i := slices.IndexFunc(allSyncLevels, func(l syncLevel) bool { return l.Name == name })
		if i < 0 {
			return nil, fmt.Errorf("stress.sync_commit_levels.levels names %q, which is not one of on, local, remote_write, remote_apply, local_txn, off", name)
		}
		out = append(out, allSyncLevels[i])
	}
	if !slices.ContainsFunc(out, func(l syncLevel) bool { return l.Durable }) {
		return nil, fmt.Errorf("stress.sync_commit_levels.levels has no durable level, so there is no promise to hold the product to")
	}
	return out, nil
}

type levelWrite struct {
	ID       int64
	Issued   time.Time
	Returned time.Time
	Acked    bool
}

// Reused across rounds so ids never repeat.
type levelWriter struct {
	Level   syncLevel
	Index   int
	Table   string
	Journal *AckJournal

	mu     sync.Mutex
	writes []levelWrite
	nextID int64
}

func (w *levelWriter) record(lw levelWrite) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, lw)
}

func (w *levelWriter) windowSummary(since time.Time) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	var longest time.Duration
	for _, lw := range w.writes {
		if lw.Issued.Before(since) {
			continue
		}
		n++
		longest = max(longest, lw.Returned.Sub(lw.Issued))
	}
	return fmt.Sprintf("%d writes returned, the longest after %s, last error: %s", n, longest.Round(time.Millisecond), orNone(w.Journal.LastErr()))
}

func (w *levelWriter) snapshot() []levelWrite {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.writes)
}

func runSyncCommitLevels(t *core.T) error {
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	ampEnv, ok := t.Env.(ampKubeEnv)
	if !ok || ampEnv.AmpKube() == nil {
		t.Skip("requires a kube handle for the control-plane namespace: the safekeeper is frozen by signal inside the endpoint pod")
		return nil
	}
	params := syncCommitParamsFromConfig(t.Options)
	levels, err := params.levels()
	if err != nil {
		return err
	}
	logf := t.Masker().Logf(log.Printf)
	kube := ampEnv.AmpKube()

	fleet, err := newTenantFleet(t, env, logf)
	if err != nil {
		return err
	}
	defer fleet.Close()

	var writers []*levelWriter
	if err := t.Step("create_tenant", func(s *core.StepHandle) error {
		if err := fleet.Create(t, 1); err != nil {
			return err
		}
		ten := &fleet.Tenants[0]
		for i, lvl := range levels {
			w := &levelWriter{Level: lvl, Index: i, Table: "sc_" + lvl.Name, Journal: NewAckJournal()}
			if _, err := ten.Conn.Exec(t.Context, fmt.Sprintf(
				"CREATE TABLE IF NOT EXISTS %s (id bigint PRIMARY KEY, v bigint NOT NULL, pad text)", w.Table)); err != nil {
				return fmt.Errorf("create %s: %w", w.Table, err)
			}
			writers = append(writers, w)
		}
		s.Set("levels", strings.Join(params.Levels, ","))
		return nil
	}); err != nil {
		return err
	}
	ten := &fleet.Tenants[0]

	if err := t.Step("baseline_every_level_commits", func(s *core.StepHandle) error {
		conns, err := connectLevelWriters(t.Context, env, ten, writers)
		if err != nil {
			return err
		}
		start := time.Now()
		runLevelWriters(t.Context, env, ten, writers, conns, params.BaselineDuration, params.PayloadBytes)
		var silent []string
		for _, w := range writers {
			ws := w.snapshot()
			acked, p50, p99 := ackLatency(ws)
			s.Set("acked_"+w.Level.Name, acked)
			if acked > 0 {
				t.Metric("commit_p50_ms_"+w.Level.Name, p50, "ms")
				t.Metric("commit_p99_ms_"+w.Level.Name, p99, "ms")
			}
			if acked == 0 {
				silent = append(silent, fmt.Sprintf("%s (%s)", w.Level.Name, w.windowSummary(start)))
			}
		}
		if len(silent) > 0 {
			return fmt.Errorf("no commit was acknowledged in %s at synchronous_commit %s, with the safekeeper and the compute healthy: a level that never returns is a hang, and remote_apply with no replica is meant to behave like on (meko#1573)",
				params.BaselineDuration, strings.Join(silent, ", "))
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("freeze_safekeeper_then_crash", func(s *core.StepHandle) error {
		return freezeThenCrash(t, s, env, kube, ten, writers, params, logf)
	}); err != nil {
		return err
	}

	if err := t.Step("serves_after_frozen_crash", func(s *core.StepHandle) error {
		return assertEndpointsServe(t, env, fleet.Tenants, params.SettleTimeout)
	}); err != nil {
		return err
	}

	if err := t.Step("verify_after_frozen_crash", func(s *core.StepHandle) error {
		return verifyLevels(t, s, env, ten, writers)
	}); err != nil {
		return err
	}

	if err := t.Step("crash_under_load", func(s *core.StepHandle) error {
		return crashUnderLoad(t, s, env, kube, ten, writers, params)
	}); err != nil {
		return err
	}

	if err := t.Step("serves_after_crash", func(s *core.StepHandle) error {
		return assertEndpointsServe(t, env, fleet.Tenants, params.SettleTimeout)
	}); err != nil {
		return err
	}

	if err := t.Step("verify_after_crash", func(s *core.StepHandle) error {
		return verifyLevels(t, s, env, ten, writers)
	}); err != nil {
		return err
	}

	if err := t.Step("every_level_commits_again", func(s *core.StepHandle) error {
		before := make([]int, len(writers))
		for i, w := range writers {
			before[i] = w.Journal.Counts().Acked
		}
		conns, err := connectLevelWriters(t.Context, env, ten, writers)
		if err != nil {
			return err
		}
		start := time.Now()
		runLevelWriters(t.Context, env, ten, writers, conns, params.ResumeDuration, params.PayloadBytes)
		var silent []string
		for i, w := range writers {
			if w.Journal.Counts().Acked == before[i] {
				silent = append(silent, fmt.Sprintf("%s (%s)", w.Level.Name, w.windowSummary(start)))
			}
		}
		if len(silent) > 0 {
			return fmt.Errorf("after both crashes the endpoint serves, but no commit at synchronous_commit %s was acknowledged in %s",
				strings.Join(silent, ", "), params.ResumeDuration)
		}
		return verifyLevels(t, s, env, ten, writers)
	}); err != nil {
		return err
	}

	return t.Step("physical_consistency", func(s *core.StepHandle) error {
		return verifyPhysicalConsistency(t, s, env, fleet.Tenants)
	})
}

// freezeThenCrash kills compute_ctl before the safekeeper is let go, so a
// commit acknowledged from local WAL alone is lost for verification to find.
func freezeThenCrash(t *core.T, s *core.StepHandle, env ampTenantEnv, kube *k8s.Kubectl, ten *tenant,
	writers []*levelWriter, params syncCommitParams, logf func(string, ...any)) error {
	pod, err := endpointPodFor(t, kube, ten.Endpoint)
	if err != nil {
		return err
	}
	ex := kube.ForPod(pod, "")

	conns, err := connectLevelWriters(t.Context, env, ten, writers)
	if err != nil {
		return err
	}
	window := params.FreezeLead + params.FreezeDuration + params.SettleTimeout
	wctx, stopWriters := context.WithTimeout(t.Context, window)
	defer stopWriters()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLevelWriters(wctx, env, ten, writers, conns, window, params.PayloadBytes)
	}()

	if err := sleepCtx(t.Context, params.FreezeLead); err != nil {
		stopWriters()
		<-done
		return err
	}
	pids, err := daemonPIDs(t.Context, ex, daemonProcess("safekeeper"))
	if err != nil || len(pids) == 0 {
		stopWriters()
		<-done
		return fmt.Errorf("no safekeeper process to freeze in pod %s (pids %v): %v", pod, pids, err)
	}
	resume := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := ex.Run(ctx, "kill -CONT "+joinPIDs(pids, " ")+" 2>/dev/null; exit 0"); err != nil {
			logf("sync_commit_levels: resuming safekeeper %s: %v", joinPIDs(pids, ","), err)
		}
	}
	defer resume()
	if _, err := ex.Run(t.Context, "kill -STOP "+joinPIDs(pids, " ")); err != nil {
		stopWriters()
		<-done
		return fmt.Errorf("SIGSTOP safekeeper pid(s) %s: %w", joinPIDs(pids, ","), err)
	}
	if err := t.WaitFor("safekeeper to show as stopped", 30*time.Second, time.Second, func() (bool, error) {
		return processesStopped(t.Context, ex, pids)
	}); err != nil {
		stopWriters()
		<-done
		return fmt.Errorf("SIGSTOP was sent to safekeeper pid(s) %s but they never showed state T, so nothing was frozen: %w", joinPIDs(pids, ","), err)
	}
	frozenAt := time.Now()

	if err := sleepCtx(t.Context, params.FreezeDuration-params.KillLead); err != nil {
		stopWriters()
		<-done
		return err
	}
	killedAt := time.Now()
	rec, kerr := killDaemonOnce(t, ex, "compute_ctl")
	if err := sleepCtx(t.Context, params.KillLead); err != nil {
		stopWriters()
		<-done
		return err
	}
	resume()
	thawedAt := time.Now()
	s.Set("frozen_seconds", thawedAt.Sub(frozenAt).Seconds())
	s.Set("safekeeper_pids", joinPIDs(pids, ","))
	if kerr != nil {
		stopWriters()
		<-done
		return fmt.Errorf("the compute kill while the safekeeper was frozen did not land, so the crash half of this round proves nothing: %w", kerr)
	}
	s.Set("compute_ctl_pids", joinPIDs(rec.PIDs, ","))

	if err := assertEndpointsServe(t, env, []tenant{*ten}, params.SettleTimeout); err != nil {
		stopWriters()
		<-done
		return err
	}
	stopWriters()
	<-done

	cutoff := frozenAt.Add(params.FreezeGrace)
	var violations []string
	for _, w := range writers {
		n, ids := ackedInside(w.snapshot(), cutoff, killedAt)
		s.Set("acked_while_frozen_"+w.Level.Name, n)
		if n > 0 && w.Level.Durable {
			violations = append(violations, fmt.Sprintf("%s acked %d (ids %s)", w.Level.Name, n, formatIDs(ids, 5)))
		}
	}
	if len(violations) > 0 {
		return fmt.Errorf("with the safekeeper frozen (SIGSTOP, state T) from %s to %s, durable synchronous_commit levels acknowledged commits issued after %s: %s. Nothing can be durable while the only path to YugabyteDB is stopped, so each of these was acknowledged from compute-local WAL (meko#1564)",
			frozenAt.Format(time.RFC3339Nano), thawedAt.Format(time.RFC3339Nano), cutoff.Format(time.RFC3339Nano), strings.Join(violations, "; "))
	}
	return nil
}

func crashUnderLoad(t *core.T, s *core.StepHandle, env ampTenantEnv, kube *k8s.Kubectl, ten *tenant,
	writers []*levelWriter, params syncCommitParams) error {
	pod, err := endpointPodFor(t, kube, ten.Endpoint)
	if err != nil {
		return err
	}
	conns, err := connectLevelWriters(t.Context, env, ten, writers)
	if err != nil {
		return err
	}
	window := params.CrashLead + params.CrashTail
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLevelWriters(t.Context, env, ten, writers, conns, window, params.PayloadBytes)
	}()
	if err := sleepCtx(t.Context, params.CrashLead); err != nil {
		<-done
		return err
	}
	rec, kerr := killDaemonOnce(t, kube.ForPod(pod, ""), "compute_ctl")
	<-done
	if kerr != nil {
		return fmt.Errorf("the compute kill under load did not land: %w", kerr)
	}
	s.Set("compute_ctl_pids", joinPIDs(rec.PIDs, ","))
	return nil
}

func verifyLevels(t *core.T, s *core.StepHandle, env ampTenantEnv, ten *tenant, writers []*levelWriter) error {
	conn, err := env.ConnectEndpoint(t.Context, ten.Endpoint)
	if err != nil {
		return fmt.Errorf("fresh connection for verification: %w", err)
	}
	defer conn.Close()
	var lost []string
	for _, w := range writers {
		present, err := readLevelRows(t.Context, conn, w.Table)
		if err != nil {
			return err
		}
		v := w.Journal.Verify(present)
		s.Set("verify_"+w.Level.Name, v.Summary())
		if len(v.Unexpected) > 0 {
			return fmt.Errorf("%s holds %d id(s) no writer issued: %s. Journal: %s",
				w.Table, len(v.Unexpected), formatIDs(v.Unexpected, 10), v.Summary())
		}
		if len(v.Mismatched) > 0 {
			m := v.Mismatched[0]
			return fmt.Errorf("%s holds %d row(s) whose value is not the one written: id %d stores %d, the writer sent %d",
				w.Table, len(v.Mismatched), m.ID, m.Got, m.Want)
		}
		if len(v.Lost) == 0 {
			continue
		}
		if !w.Level.Durable {
			s.Set("lost_allowed_"+w.Level.Name, len(v.Lost))
			continue
		}
		lost = append(lost, fmt.Sprintf("synchronous_commit=%s lost %d acknowledged commit(s) (%s)",
			w.Level.Name, len(v.Lost), formatIDs(v.Lost, 10)))
	}
	if len(lost) > 0 {
		return fmt.Errorf("acknowledged commits are missing from storage read over a fresh connection: %s", strings.Join(lost, "; "))
	}
	return nil
}

func readLevelRows(ctx context.Context, conn *pg.Client, table string) (map[int64]int64, error) {
	present := map[int64]int64{}
	err := conn.Query(ctx, func(r pgx.Rows) error {
		var id, v int64
		if err := r.Scan(&id, &v); err != nil {
			return err
		}
		present[id] = v
		return nil
	}, "SELECT id, v FROM "+table)
	if err != nil {
		return nil, fmt.Errorf("read %s back: %w", table, err)
	}
	return present, nil
}

type levelConn struct {
	client *pg.Client
	sess   pg.Session
}

// Each writer connects before the window starts: a port-forward can take longer
// to open than the whole window, and that is no commit going unacknowledged.
func connectLevelWriters(ctx context.Context, env ampTenantEnv, ten *tenant, writers []*levelWriter) ([]levelConn, error) {
	conns := make([]levelConn, len(writers))
	errs := make([]error, len(writers))
	var wg sync.WaitGroup
	for i, w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, levelConnectBudget)
			defer cancel()
			// A connected client keeps its port-forward until Teardown, so only the
			// pin is retried on it.
			var client *pg.Client
			for {
				var err error
				if client == nil {
					client, err = env.ConnectEndpoint(cctx, ten.Endpoint)
				}
				if err == nil {
					var sess pg.Session
					if sess, err = pinLevelSession(cctx, client, w.Level); err == nil {
						conns[i] = levelConn{client: client, sess: sess}
						return
					}
				}
				if sleepCtx(cctx, time.Second) != nil {
					if client != nil {
						client.Close()
					}
					errs[i] = fmt.Errorf("connect the synchronous_commit %s writer within %s: %w", w.Level.Name, levelConnectBudget, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		for _, c := range conns {
			if c.sess != nil {
				c.sess.Close()
				c.client.Close()
			}
		}
		return nil, err
	}
	return conns, nil
}

func runLevelWriters(ctx context.Context, env ampTenantEnv, ten *tenant, writers []*levelWriter, conns []levelConn, d time.Duration, payload int) {
	wctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var wg sync.WaitGroup
	for i, w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			levelWriteFor(wctx, env, ten, w, conns[i], payload)
		}()
	}
	wg.Wait()
}

// A client per level so the levels never queue on one pool, and a fresh
// session after an error, because a pool that reconnected silently forgot the SET.
func levelWriteFor(ctx context.Context, env ampTenantEnv, ten *tenant, w *levelWriter, conn levelConn, payload int) {
	base := int64(w.Index) * tenantIDSpan
	client, sess := conn.client, conn.sess
	defer func() {
		if sess != nil {
			sess.Close()
		}
		if client != nil {
			client.Close()
		}
	}()
	insert := fmt.Sprintf("INSERT INTO %s (id, v, pad) VALUES ($1, $2, repeat('s', $3::int)) ON CONFLICT (id) DO NOTHING", w.Table)
	for ctx.Err() == nil {
		if sess == nil {
			var err error
			if client == nil {
				client, err = env.ConnectEndpoint(ctx, ten.Endpoint)
			}
			if err == nil {
				sess, err = pinLevelSession(ctx, client, w.Level)
			}
			if err != nil {
				w.Journal.RecordErr(err)
				if client != nil {
					client.Close()
					client = nil
				}
				if sleepCtx(ctx, time.Second) != nil {
					return
				}
				continue
			}
		}
		w.mu.Lock()
		id := base + w.nextID
		w.nextID++
		w.mu.Unlock()
		value := id * 7
		issued := time.Now()
		err := execLevelWrite(ctx, sess, w.Level, insert, id, value, payload)
		w.record(levelWrite{ID: id, Issued: issued, Returned: time.Now(), Acked: err == nil})
		if err == nil {
			w.Journal.RecordAcked(id, value)
			continue
		}
		w.Journal.RecordUnknown(id, value)
		w.Journal.RecordErr(err)
		sess.Close()
		sess = nil
	}
}

func pinLevelSession(ctx context.Context, client *pg.Client, lvl syncLevel) (pg.Session, error) {
	sess, err := client.Pin(ctx)
	if err != nil {
		return nil, err
	}
	if lvl.PerTxn {
		return sess, nil
	}
	if _, err := sess.Exec(ctx, "SET synchronous_commit TO "+lvl.Setting); err != nil {
		sess.Close()
		return nil, fmt.Errorf("SET synchronous_commit TO %s: %w", lvl.Setting, err)
	}
	got, err := sess.QueryString(ctx, "SHOW synchronous_commit")
	if err != nil {
		sess.Close()
		return nil, err
	}
	if got != lvl.Setting {
		sess.Close()
		return nil, fmt.Errorf("SET synchronous_commit TO %s left the session at %q", lvl.Setting, got)
	}
	return sess, nil
}

func execLevelWrite(ctx context.Context, sess pg.Session, lvl syncLevel, insert string, id, value int64, payload int) error {
	if !lvl.PerTxn {
		_, err := sess.Exec(ctx, insert, id, value, payload)
		return err
	}
	for _, stmt := range []string{"BEGIN", "SET LOCAL synchronous_commit TO " + lvl.Setting} {
		if _, err := sess.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	if _, err := sess.Exec(ctx, insert, id, value, payload); err != nil {
		return err
	}
	_, err := sess.Exec(ctx, "COMMIT")
	return err
}

func processesStopped(ctx context.Context, ex *k8s.Kubectl, pids []int) (bool, error) {
	for _, p := range pids {
		res, err := ex.Run(ctx, fmt.Sprintf("cut -d' ' -f3 /proc/%d/stat", p))
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(res.Stdout) != "T" {
			return false, nil
		}
	}
	return true, nil
}

func ackedInside(ws []levelWrite, from, until time.Time) (int, []int64) {
	var ids []int64
	for _, w := range ws {
		if w.Acked && !w.Issued.Before(from) && w.Returned.Before(until) {
			ids = append(ids, w.ID)
		}
	}
	return len(ids), ids
}

func ackLatency(ws []levelWrite) (int, float64, float64) {
	var lat []float64
	for _, w := range ws {
		if w.Acked {
			lat = append(lat, float64(w.Returned.Sub(w.Issued).Microseconds())/1000)
		}
	}
	if len(lat) == 0 {
		return 0, 0, 0
	}
	sort.Float64s(lat)
	at := func(q float64) float64 { return lat[int(q*float64(len(lat)-1))] }
	return len(lat), at(0.50), at(0.99)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
