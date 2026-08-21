package features

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	// A `user`-context setting: a config reload puts it in force in the running
	// server, so changing it must not cost the endpoint its connections.
	settingsReloadName  = "work_mem"
	settingsReloadValue = "123MB"

	// A `postmaster`-context setting: fixed at server start, so this one has to
	// restart the compute. Chosen for being harmless — `shared_buffers` risks a
	// compute that cannot start again on a value the pod has no memory for,
	// which would fail the test for a reason that is not the feature.
	settingsRestartName  = "track_commit_timestamp"
	settingsRestartValue = "on"

	// A name Postgres does not report. Dotted, so Postgres accepts it in the
	// config file as a placeholder rather than rejecting the whole file — the
	// endpoint has to survive this step for the later assertions to mean
	// anything. Nothing can put it in force, so nothing should be restarted for
	// it.
	settingsProbeName = "amp_probe.nothing_applies_this"

	// The branch-wide baseline. Two names on purpose: one the endpoint also sets,
	// to catch an inverted merge, and one only the branch sets, because that is
	// the only one whose arrival proves the branch row was read at all.
	settingsBranchValue     = "77MB"
	settingsBranchOnly      = "maintenance_work_mem"
	settingsBranchOnlyValue = "97MB"

	// How long the compute may take to accept connections again after a restart
	// forced by a settings change. It is one process coming back inside a pod
	// that is already running, not a reschedule.
	settingsReconnectTimeout = 3 * time.Minute
)

func init() {
	core.Register(core.Test{
		Name:       "features/amp_endpoint_settings",
		Suite:      "features",
		Category:   core.Stress,
		Env:        core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:       []string{"regression", "nightly"},
		OwningTeam: core.TeamCompute,
		Description: "Settings reach a running compute the cheapest way that applies them: " +
			"a reloadable setting keeps its connections, one fixed at server start restarts and says so, " +
			"one nothing can apply restarts nothing, and an identical write costs nothing.",
		Params: []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:    runAmpEndpointSettings,
	})
}

// runAmpEndpointSettings proves the reload path is real. A settings change used
// to restart the compute unconditionally, which drops every connection; the
// claim now is that most changes apply in place and only the ones Postgres fixes
// at server start cost an interruption. Both halves of that claim are only
// observable against a running endpoint: whether the value is in force, and
// whether a connection opened before the change survived it.
func runAmpEndpointSettings(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"endpoint_settings", a.endpointSettings}}
	})
}

func (a *ampFeatureRun) endpointSettings(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	return endpointSettingsSteps(ctx, cl, a.proj, a.f.endpointSQL)
}

// pinner is a connection that can hand out one backend and keep it. The live
// client does (`pg.Client.Pin`); the fake in the tests does too, so the
// survival assertion is exercised both ways.
type pinner interface {
	Pin(ctx context.Context) (pg.Session, error)
}

// pgSettings is the request for one Postgres-settings surface. The other
// surfaces stay nil: the controller reads an omitted one as "leave it alone",
// and this test has no business touching the write lease.
func pgSettings(kv map[string]string) controller.UpdateSettingsRequest {
	return controller.UpdateSettingsRequest{PgSettings: &kv}
}

// showSetting reads what Postgres currently has in force.
func showSetting(ctx context.Context, c pg.Session, name string) (string, error) {
	return c.QueryString(ctx, "SHOW "+name)
}

// startedAt identifies the running postmaster. It moves if and only if Postgres
// restarted, which is what separates "applied in place" from "applied at a
// cost" — the reply says which happened, and this is the independent check.
func startedAt(ctx context.Context, c pg.Session) (string, error) {
	return c.QueryString(ctx, "SELECT pg_postmaster_start_time()::text")
}

// reconnect waits out a restart. Bounded and polling, because the endpoint's
// address does not change (the supervisor restarts one process inside a running
// pod) but the listener is gone for a moment.
func reconnect(ctx context.Context, connect endpointConnector, ep controller.Endpoint) (sqlConn, error) {
	deadline := time.Now().Add(settingsReconnectTimeout)
	var lastErr error
	for {
		// Per attempt, not just between them. `connect` on the self-provisioning
		// path waits out its own readiness budgets — 20 minutes for the workload,
		// 5 more for Postgres — so an unbounded attempt would overshoot this
		// deadline by twenty-odd minutes, make the message below a lie, and turn a
		// clear assertion failure into an unattributed suite timeout.
		attempt, cancel := context.WithDeadline(ctx, deadline)
		c, err := connect(attempt, ep)
		cancel()
		if err == nil {
			return c, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("compute did not accept connections within %s: %w", settingsReconnectTimeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// only returns the single endpoint outcome, so a reply that reached a different
// number of endpoints than the one this test created fails loudly rather than
// being indexed into.
func only(r controller.SettingsUpdateResult) (controller.EndpointSettingsOutcome, error) {
	if len(r.Endpoints) != 1 {
		return controller.EndpointSettingsOutcome{}, fmt.Errorf("want 1 endpoint outcome, got %d", len(r.Endpoints))
	}
	o := r.Endpoints[0]
	if o.Unreachable != "" {
		return o, fmt.Errorf("the endpoint could not be told: %s", o.Unreachable)
	}
	if !r.Applied {
		return o, fmt.Errorf("applied is false: the settings row was not written")
	}
	return o, nil
}

// endpointSettingsSteps drives the assertions over the ampCtl and connector
// seams, so the ordering and the claims are unit-testable with fakes.
func endpointSettingsSteps(ctx context.Context, cl ampCtl, proj ampCluster, connect endpointConnector) error {
	ep, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create endpoint: %w", err)
	}
	conn, err := connect(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	// Nil-checked, and never assigned a nil: a failed reconnect returns one, and
	// unwinding through Close() on it panics — which would replace the error
	// saying the compute never came back with a nil dereference, and take the
	// diagnostics with it (a panic leaves runLifecycle's named return nil, so
	// collection is skipped and teardown deletes the namespace).
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()

	born, err := startedAt(ctx, conn)
	if err != nil {
		return fmt.Errorf("read postmaster start time: %w", err)
	}

	// One backend, held across the reload. Closed before the restart step, which
	// is expected to kill it.
	pinnable, ok := conn.(pinner)
	if !ok {
		return fmt.Errorf("this connection cannot pin a backend, so the reload's promise cannot be tested")
	}
	session, err := pinnable.Pin(ctx)
	if err != nil {
		return fmt.Errorf("pin a backend: %w", err)
	}
	defer func() {
		if session != nil {
			session.Close()
		}
	}()
	// A session that is not really pinned would make the assertion below
	// meaningless, so check the premise: a temp table is visible only to the
	// backend that made it.
	if _, err := session.Exec(ctx, "CREATE TEMP TABLE amp_settings_probe (x int)"); err != nil {
		return fmt.Errorf("mark the pinned session: %w", err)
	}

	// 1. A reloadable setting. The headline claim: in force, and the connection
	// that was already open is still usable.
	live := map[string]string{settingsReloadName: settingsReloadValue}
	reply, err := cl.UpdateEndpointSettings(ctx, proj.poolID, proj.projID, proj.branchID, ep.ID, pgSettings(live))
	if err != nil {
		return fmt.Errorf("set %s: %w", settingsReloadName, err)
	}
	out, err := only(reply)
	if err != nil {
		return fmt.Errorf("setting %s: %w", settingsReloadName, err)
	}
	if len(out.Restarted) != 0 {
		return fmt.Errorf("%s is reloadable but restarted %v (forced by %v)", settingsReloadName, out.Restarted, out.RestartForcedBy)
	}
	if len(out.Reloaded) == 0 {
		return fmt.Errorf("%s reported neither a reload nor a restart: %+v", settingsReloadName, out)
	}
	// On the pinned backend, not through the pool. A pool replaces a connection
	// whose server went away without telling anyone, so reading through one
	// cannot tell "the reload kept my session" from "the compute bounced and I
	// was quietly reconnected" — and that difference is the whole promise of a
	// reload.
	got, err := showSetting(ctx, session, settingsReloadName)
	if err != nil {
		return fmt.Errorf("the session open before the reload did not survive it: %w", err)
	}
	if got != settingsReloadValue {
		return fmt.Errorf("%s = %s after a reported reload, want %s", settingsReloadName, got, settingsReloadValue)
	}
	// The temp table proves it is the same backend rather than a reconnect that
	// happened to succeed.
	if _, err := session.QueryInt(ctx, "SELECT count(*) FROM amp_settings_probe"); err != nil {
		return fmt.Errorf("the session did not survive the reload (its temp table is gone): %w", err)
	}
	if now, err := startedAt(ctx, conn); err != nil || now != born {
		return fmt.Errorf("a reload restarted Postgres (%s -> %s, err %v)", born, now, err)
	}
	// The restart step below takes it down; releasing it first keeps the failure
	// that follows about the restart.
	session.Close()
	session = nil

	// 2. A setting fixed at server start. Carried alongside the first, so the
	// only thing that moved is the new one.
	live[settingsRestartName] = settingsRestartValue
	reply, err = cl.UpdateEndpointSettings(ctx, proj.poolID, proj.projID, proj.branchID, ep.ID, pgSettings(live))
	if err != nil {
		return fmt.Errorf("set %s: %w", settingsRestartName, err)
	}
	if out, err = only(reply); err != nil {
		return fmt.Errorf("setting %s: %w", settingsRestartName, err)
	}
	if len(out.Restarted) == 0 {
		return fmt.Errorf("%s can only change at server start, but nothing was restarted: %+v", settingsRestartName, out)
	}
	// Naming it is what turns "we restarted your database" into a cost the
	// operator can predict, so an unnamed restart is a failure of its own.
	if !contains(out.RestartForcedBy, settingsRestartName) {
		return fmt.Errorf("restart_forced_by = %v, want it to name %s", out.RestartForcedBy, settingsRestartName)
	}
	conn.Close()
	conn = nil
	fresh, err := reconnect(ctx, connect, ep)
	if err != nil {
		return fmt.Errorf("after the restart forced by %s: %w", settingsRestartName, err)
	}
	conn = fresh
	if got, err = showSetting(ctx, conn, settingsRestartName); err != nil {
		return fmt.Errorf("read %s: %w", settingsRestartName, err)
	}
	if got != settingsRestartValue {
		return fmt.Errorf("%s = %s after the restart it forced, want %s", settingsRestartName, got, settingsRestartValue)
	}
	restarted, err := startedAt(ctx, conn)
	if err != nil {
		return fmt.Errorf("read postmaster start time: %w", err)
	}
	if restarted == born {
		return fmt.Errorf("a restart was reported but Postgres was not restarted (still %s)", born)
	}
	// The reloadable value has to survive the restart: it is in the same stored
	// set, and a restart that rendered only the new setting would silently drop
	// it.
	if got, err = showSetting(ctx, conn, settingsReloadName); err != nil || got != settingsReloadValue {
		return fmt.Errorf("%s = %s after the restart, want %s (err %v)", settingsReloadName, got, settingsReloadValue, err)
	}

	// 3. A setting nothing can apply. It must be reported, and it must not cost
	// an interruption — a restart would not put it in force either.
	live[settingsProbeName] = "1"
	reply, err = cl.UpdateEndpointSettings(ctx, proj.poolID, proj.projID, proj.branchID, ep.ID, pgSettings(live))
	if err != nil {
		return fmt.Errorf("set %s: %w", settingsProbeName, err)
	}
	if out, err = only(reply); err != nil {
		return fmt.Errorf("setting %s: %w", settingsProbeName, err)
	}
	if !contains(out.NotApplied, settingsProbeName) {
		return fmt.Errorf("not_applied = %v, want it to name %s — otherwise a setting that will never take effect reports success", out.NotApplied, settingsProbeName)
	}
	if len(out.Restarted) != 0 {
		return fmt.Errorf("restarted %v for %s, which a restart cannot apply either", out.Restarted, settingsProbeName)
	}
	if now, err := startedAt(ctx, conn); err != nil || now != restarted {
		return fmt.Errorf("Postgres was restarted for a setting nothing can apply (%s -> %s, err %v)", restarted, now, err)
	}

	// 4. The same settings again. Identical rendered config, so there is nothing
	// to apply and nothing to bounce.
	reply, err = cl.UpdateEndpointSettings(ctx, proj.poolID, proj.projID, proj.branchID, ep.ID, pgSettings(live))
	if err != nil {
		return fmt.Errorf("re-send the same settings: %w", err)
	}
	if out, err = only(reply); err != nil {
		return fmt.Errorf("re-sending the same settings: %w", err)
	}
	if len(out.Restarted) != 0 || len(out.Reloaded) != 0 {
		return fmt.Errorf("an identical settings write restarted %v and reloaded %v, want neither", out.Restarted, out.Reloaded)
	}

	// 5. The branch baseline reaches the endpoint, and loses to the endpoint's own
	// value for a name both set. Both halves matter and only one of them is about
	// precedence: the two rows are merged into a single key before rendering, so a
	// branch value for a name the endpoint already sets renders byte-identically
	// and would pass whether or not the branch row was read. The branch-only name
	// is what shows it was.
	branch := map[string]string{
		settingsReloadName: settingsBranchValue,
		settingsBranchOnly: settingsBranchOnlyValue,
	}
	if _, err := cl.UpdateBranchSettings(ctx, proj.poolID, proj.projID, proj.branchID, pgSettings(branch)); err != nil {
		return fmt.Errorf("set the branch baseline: %w", err)
	}
	// Reconnect rather than reuse: the branch-only name is a setting the compute
	// did not have, so this write does move the rendered spec. Whether it costs a
	// restart is not what this step is about, so tolerate either and assert the
	// values.
	conn.Close()
	conn = nil
	fresh, err = reconnect(ctx, connect, ep)
	if err != nil {
		return fmt.Errorf("after the branch baseline write: %w", err)
	}
	conn = fresh
	if got, err = showSetting(ctx, conn, settingsReloadName); err != nil {
		return fmt.Errorf("read %s after the branch write: %w", settingsReloadName, err)
	}
	if got != settingsReloadValue {
		return fmt.Errorf("%s = %s: the branch baseline beat the endpoint's own value (want %s)", settingsReloadName, got, settingsReloadValue)
	}
	// And the name only the branch sets has to arrive, or nothing above shows the
	// branch row is read at all.
	if got, err = showSetting(ctx, conn, settingsBranchOnly); err != nil {
		return fmt.Errorf("read %s after the branch write: %w", settingsBranchOnly, err)
	}
	if got != settingsBranchOnlyValue {
		return fmt.Errorf("%s = %s: the branch baseline did not reach the endpoint (want %s)", settingsBranchOnly, got, settingsBranchOnlyValue)
	}
	return nil
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if strings.EqualFold(s, needle) {
			return true
		}
	}
	return false
}
