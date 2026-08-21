package features

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
)

// settingsCtl is a scripted controller: it hands back one endpoint and answers
// each settings PATCH from a queue, so the assertions in
// endpointSettingsSteps are exercised without a cluster.
type settingsCtl struct {
	ampCtl
	replies []controller.SettingsUpdateResult
	sent    []map[string]string
	branch  []map[string]string
	// Fired after each PATCH is recorded, on the caller's goroutine, so a test
	// can make the fake compute do what the reply claims (bounce, lose a
	// connection) without a second goroutine racing on the same fields.
	onPatch  func(n int)
	onBranch func()
}

func (s *settingsCtl) CreateEndpointAndWait(context.Context, string, string, string, controller.CreateEndpointRequest) (controller.Task, error) {
	return controller.Task{}, nil
}

func (s *settingsCtl) ListAllEndpoints(context.Context, string, string, string) ([]controller.Endpoint, error) {
	return []controller.Endpoint{{ID: "ep-1"}}, nil
}

func (s *settingsCtl) UpdateEndpointSettings(_ context.Context, _, _, _, _ string, req controller.UpdateSettingsRequest) (controller.SettingsUpdateResult, error) {
	if req.PgSettings != nil {
		copied := map[string]string{}
		for k, v := range *req.PgSettings {
			copied[k] = v
		}
		s.sent = append(s.sent, copied)
	}
	if s.onPatch != nil {
		s.onPatch(len(s.sent))
	}
	if len(s.replies) == 0 {
		return controller.SettingsUpdateResult{}, errors.New("no scripted reply left")
	}
	out := s.replies[0]
	s.replies = s.replies[1:]
	return out, nil
}

func (s *settingsCtl) UpdateBranchSettings(_ context.Context, _, _, _ string, req controller.UpdateSettingsRequest) (controller.SettingsUpdateResult, error) {
	if req.PgSettings != nil {
		s.branch = append(s.branch, *req.PgSettings)
	}
	if s.onBranch != nil {
		s.onBranch()
	}
	return controller.SettingsUpdateResult{Applied: true, Endpoints: []controller.EndpointSettingsOutcome{{EndpointID: "ep-1"}}}, nil
}

// settingsConn is a scripted Postgres: `SHOW <name>` reads from `values`, and
// the postmaster start time is whatever `born` currently says, so a test can
// move it to simulate a restart (or refuse to, to simulate one that was
// reported and did not happen). `dead` makes every query fail, which is what a
// dropped connection looks like from here.
type settingsConn struct {
	values map[string]string
	born   *string
	dead   *bool
	closed int
}

func (c *settingsConn) Exec(context.Context, string, ...any) (int64, error) { return 0, nil }
func (c *settingsConn) QueryInt(context.Context, string, ...any) (int64, error) {
	return 0, nil
}

func (c *settingsConn) QueryString(_ context.Context, sql string, _ ...any) (string, error) {
	if c.dead != nil && *c.dead {
		return "", errors.New("connection reset by peer")
	}
	if strings.HasPrefix(sql, "SELECT pg_postmaster_start_time") {
		return *c.born, nil
	}
	name := strings.TrimPrefix(sql, "SHOW ")
	v, ok := c.values[name]
	if !ok {
		return "", errors.New("unrecognized configuration parameter " + name)
	}
	return v, nil
}

func (c *settingsConn) Close() { c.closed++ }

// Pin models a real pinned backend: one that dies when the server it is on goes
// away, rather than being silently replaced the way a pool would. The snapshot
// is what makes that observable — a moved start time means a restart happened,
// so this session is gone even though a pooled read would have succeeded.
func (c *settingsConn) Pin(context.Context) (pg.Session, error) {
	return &settingsSession{conn: c, bornAt: *c.born}, nil
}

type settingsSession struct {
	conn   *settingsConn
	bornAt string
	marked bool
}

func (s *settingsSession) alive() error {
	if s.conn.dead != nil && *s.conn.dead {
		return errors.New("connection reset by peer")
	}
	if *s.conn.born != s.bornAt {
		return errors.New("server closed the connection unexpectedly")
	}
	return nil
}

func (s *settingsSession) Exec(_ context.Context, sql string, _ ...any) (int64, error) {
	if err := s.alive(); err != nil {
		return 0, err
	}
	if strings.Contains(sql, "CREATE TEMP TABLE") {
		s.marked = true
	}
	return 0, nil
}

func (s *settingsSession) QueryInt(_ context.Context, _ string, _ ...any) (int64, error) {
	if err := s.alive(); err != nil {
		return 0, err
	}
	if !s.marked {
		return 0, errors.New("relation \"amp_settings_probe\" does not exist")
	}
	return 0, nil
}

func (s *settingsSession) QueryString(ctx context.Context, sql string, args ...any) (string, error) {
	if err := s.alive(); err != nil {
		return "", err
	}
	return s.conn.QueryString(ctx, sql, args...)
}

func (s *settingsSession) Close() {}

// scenario wires a fake compute whose state the replies are consistent with:
// applying a value writes it into `values`, and a restart moves `born`.
type scenario struct {
	ctl       *settingsCtl
	conn      *settingsConn
	born      string
	dead      bool
	noConnect bool
	values    map[string]string
}

func newScenario(replies ...controller.SettingsUpdateResult) *scenario {
	s := &scenario{
		ctl:  &settingsCtl{replies: replies},
		born: "2026-08-21 10:00:00+00",
		// Seeded with its default: a real `SHOW` of a setting nobody has changed
		// answers with the default rather than failing, so a branch write that
		// never arrives has to look like the default and not like an error.
		values: map[string]string{settingsBranchOnly: "64MB"},
	}
	s.conn = &settingsConn{values: s.values, born: &s.born, dead: &s.dead}
	return s
}

// connectFails makes every later connect attempt fail, standing in for a compute
// that does not come back.
func (s *scenario) connectFails() { s.noConnect = true }

// connect hands out the same fake connection every time; a reconnect after a
// simulated restart revives it.
func (s *scenario) connect(context.Context, controller.Endpoint) (sqlConn, error) {
	if s.noConnect {
		return nil, errors.New("dial tcp: connection refused")
	}
	s.dead = false
	return s.conn, nil
}

func (s *scenario) run() error { return s.runCtx(context.Background()) }

func (s *scenario) runCtx(ctx context.Context) error {
	return endpointSettingsSteps(ctx, s.ctl,
		ampCluster{poolID: "p", projID: "c", branchID: "b"}, s.connect)
}

func reloaded() controller.SettingsUpdateResult {
	return controller.SettingsUpdateResult{Applied: true, Endpoints: []controller.EndpointSettingsOutcome{
		{EndpointID: "ep-1", Reloaded: []string{"compute_ctl"}},
	}}
}

func restarted(because ...string) controller.SettingsUpdateResult {
	return controller.SettingsUpdateResult{Applied: true, Endpoints: []controller.EndpointSettingsOutcome{
		{EndpointID: "ep-1", Restarted: []string{"compute_ctl"}, RestartForcedBy: because},
	}}
}

func notApplied(names ...string) controller.SettingsUpdateResult {
	return controller.SettingsUpdateResult{Applied: true, Endpoints: []controller.EndpointSettingsOutcome{
		{EndpointID: "ep-1", NotApplied: names},
	}}
}

func nothing() controller.SettingsUpdateResult {
	return controller.SettingsUpdateResult{Applied: true, Endpoints: []controller.EndpointSettingsOutcome{
		{EndpointID: "ep-1"},
	}}
}

// The shape of a healthy run: reload, restart, nothing-applies-it, an identical
// write, then the branch baseline losing to the endpoint's own value.
func happy() *scenario {
	s := newScenario(reloaded(), restarted(settingsRestartName), notApplied(settingsProbeName), nothing())
	s.values[settingsReloadName] = settingsReloadValue
	s.values[settingsRestartName] = settingsRestartValue
	// The branch write lands its own name; the compute reports it from then on.
	s.ctl.onBranch = func() { s.values[settingsBranchOnly] = settingsBranchOnlyValue }
	return s
}

// restartOn makes the fake compute bounce when the nth PATCH is sent, which is
// what makes a "restarted" reply true rather than merely claimed.
func (s *scenario) restartOn(n int) {
	s.ctl.onPatch = func(got int) {
		if got == n {
			s.born = "2026-08-21 10:05:00+00"
		}
	}
}

func TestEndpointSettingsStepsAcceptsAHealthyEndpoint(t *testing.T) {
	s := happy()
	s.restartOn(2)
	if err := s.run(); err != nil {
		t.Fatalf("a healthy endpoint was reported as a failure: %v", err)
	}
	if len(s.ctl.sent) != 4 {
		t.Errorf("sent %d endpoint PATCHes, want 4", len(s.ctl.sent))
	}
	if len(s.ctl.branch) != 1 {
		t.Errorf("sent %d branch PATCHes, want 1", len(s.ctl.branch))
	}
	// The reloadable setting must be carried in every later write, or a restart
	// would render it away.
	for i, sent := range s.ctl.sent {
		if sent[settingsReloadName] != settingsReloadValue {
			t.Errorf("PATCH %d dropped %s: %v", i+1, settingsReloadName, sent)
		}
	}
	if s.conn.closed == 0 {
		t.Error("the connection open across the restart was never closed")
	}
}

// The failure a pooled read cannot see. The reply says reloaded and the compute
// restarted anyway — a pool would reconnect and answer the next query as though
// nothing happened, so only the pinned session catches it.
func TestEndpointSettingsStepsCatchesASilentBounceUnderAReload(t *testing.T) {
	s := newScenario(reloaded())
	s.values[settingsReloadName] = settingsReloadValue
	// Postgres restarts while the reply claims a reload.
	s.ctl.onPatch = func(int) { s.born = "2026-08-21 10:05:00+00" }
	err := s.run()
	if err == nil {
		t.Fatal("a reload that restarted Postgres was accepted")
	}
	if !strings.Contains(err.Error(), "did not survive") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// The branch baseline has to arrive, not merely fail to win. A branch value for
// a name the endpoint already sets renders identically, so it would pass even if
// the branch row were never read.
func TestEndpointSettingsStepsRequiresTheBranchBaselineToArrive(t *testing.T) {
	s := happy()
	s.restartOn(2)
	// The branch write is accepted and changes nothing on the compute.
	s.ctl.onBranch = func() {}
	err := s.run()
	if err == nil {
		t.Fatal("a branch baseline that never reached the endpoint was accepted")
	}
	if !strings.Contains(err.Error(), "did not reach the endpoint") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// A compute that never comes back has to be reported as exactly that. The
// reconnect returns a nil connection with its error, and the deferred Close must
// not be handed that nil: unwinding through it panics, which replaces this error
// with a nil dereference and — because a panic leaves the lifecycle's named
// return nil — skips the diagnostics collection that would have captured the pod
// logs, right before teardown deletes the namespace.
func TestEndpointSettingsStepsReportsAComputeThatDoesNotComeBack(t *testing.T) {
	s := newScenario(reloaded(), restarted(settingsRestartName))
	s.values[settingsReloadName] = settingsReloadValue
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.ctl.onPatch = func(n int) {
		if n == 2 {
			s.born = "2026-08-21 10:05:00+00"
			s.connectFails()
			// Keeps the retry loop from waiting out its real 3-minute budget.
			cancel()
		}
	}
	err := s.runCtx(ctx)
	if err == nil {
		t.Fatal("a compute that never accepted a connection again was accepted")
	}
	if !strings.Contains(err.Error(), "after the restart forced by") {
		t.Errorf("the failure was not attributed to the restart: %v", err)
	}
}

// A reload that bounced the compute is the regression this whole change is
// about: it must not pass as success.
func TestEndpointSettingsStepsRejectsARestartForAReloadableSetting(t *testing.T) {
	s := newScenario(restarted(settingsReloadName))
	s.values[settingsReloadName] = settingsReloadValue
	err := s.run()
	if err == nil {
		t.Fatal("a restart for a reloadable setting was accepted")
	}
	if !strings.Contains(err.Error(), "reloadable but restarted") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// The claim is not just that it reloaded, but that connections opened before
// the change survived it. A dropped connection has to fail the test.
func TestEndpointSettingsStepsRejectsALostConnection(t *testing.T) {
	s := newScenario(reloaded())
	s.values[settingsReloadName] = settingsReloadValue
	// Alive for the opening reads, gone once the reload lands: the reply says
	// reloaded, and the connection did not survive it.
	s.ctl.onPatch = func(int) { s.dead = true }
	err := s.run()
	if err == nil {
		t.Fatal("a reload that dropped the connection was accepted")
	}
	if !strings.Contains(err.Error(), "did not survive") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// A restart nobody explained is a cost the operator cannot predict next time.
func TestEndpointSettingsStepsRequiresTheRestartToBeExplained(t *testing.T) {
	s := newScenario(reloaded(), restarted())
	s.values[settingsReloadName] = settingsReloadValue
	s.values[settingsRestartName] = settingsRestartValue
	s.restartOn(2)
	err := s.run()
	if err == nil {
		t.Fatal("an unexplained restart was accepted")
	}
	if !strings.Contains(err.Error(), "restart_forced_by") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// Restarting for a setting a restart cannot apply is the exact cost this rule
// exists to avoid.
func TestEndpointSettingsStepsRejectsARestartForAnInapplicableSetting(t *testing.T) {
	s := newScenario(reloaded(), restarted(settingsRestartName), restarted(settingsProbeName))
	s.values[settingsReloadName] = settingsReloadValue
	s.values[settingsRestartName] = settingsRestartValue
	s.restartOn(2)
	err := s.run()
	if err == nil {
		t.Fatal("a restart for a setting nothing can apply was accepted")
	}
	if !strings.Contains(err.Error(), "not_applied") && !strings.Contains(err.Error(), "cannot apply") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// An identical write that bounces the compute is a real bug: it costs an
// interruption for a change that is not a change.
func TestEndpointSettingsStepsRejectsAPointlessRestart(t *testing.T) {
	s := newScenario(reloaded(), restarted(settingsRestartName), notApplied(settingsProbeName),
		restarted(settingsRestartName))
	s.values[settingsReloadName] = settingsReloadValue
	s.values[settingsRestartName] = settingsRestartValue
	s.restartOn(2)
	err := s.run()
	if err == nil {
		t.Fatal("an identical write that restarted the compute was accepted")
	}
	if !strings.Contains(err.Error(), "identical") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// The endpoint's own value beats the branch baseline. If the branch write won,
// the merge precedence is inverted and every read replica would take its
// writer's tuning.
func TestEndpointSettingsStepsRejectsABranchBaselineThatWins(t *testing.T) {
	s := happy()
	s.restartOn(2)
	// The branch write lands and the compute starts reporting the branch's
	// value: the endpoint's own setting lost.
	s.ctl.onBranch = func() { s.values[settingsReloadName] = settingsBranchValue }
	err := s.run()
	if err == nil {
		t.Fatal("a branch baseline overriding the endpoint's value was accepted")
	}
	if !strings.Contains(err.Error(), "branch baseline beat") {
		t.Errorf("unhelpful error: %v", err)
	}
}
