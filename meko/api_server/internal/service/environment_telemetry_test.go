package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/repository"
)

// The door this control plane issues. The install hint prints this same value, so
// anything else an agent reports is a deployment that was edited after the hint was
// pasted — which is exactly what the check exists to catch.
const testDoor = "https://byoc.cloud.yugabyte.com/api/v1/otlp/metrics"

func doorPolicy() TelemetryPolicy {
	return TelemetryPolicy{MetricsURL: testDoor}
}

func goodTelemetry() *Telemetry {
	return &Telemetry{OtelEnabled: true, ByocEnabled: true, MetricsEndpoint: testDoor, LogsSink: "datadog"}
}

// The happy path, and the shapes that must pass untouched: a meko agent on a chart
// predating its collector (nothing to attest) and a control plane with no door
// configured (nothing to compare against).
func TestTelemetryPolicy_Accepts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy TelemetryPolicy
		kind   string
		tel    *Telemetry
	}{
		{"a correctly configured amp deployment", doorPolicy(), KindAmp, goodTelemetry()},
		{"a correctly configured meko deployment", doorPolicy(), KindMeko, goodTelemetry()},
		{"a meko agent on a chart with no collector", doorPolicy(), KindMeko, nil},
		{"any environment when no door is configured", TelemetryPolicy{}, KindAmp, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if degraded := tc.policy.check(tc.kind, tc.tel); degraded != "" {
				t.Errorf("check degraded %s: %s", tc.name, degraded)
			}
		})
	}
}

// Each of these is a different operator mistake with a different fix, so each has
// to name itself rather than share one "telemetry is wrong". None of them refuses —
// see TelemetryPolicy — so the reason is the entire output.
func TestTelemetryPolicy_DegradesWithAReasonThatNamesTheFix(t *testing.T) {
	for _, tc := range []struct {
		name string
		tel  *Telemetry
		want string
	}{
		{
			"collectors disabled",
			&Telemetry{ByocEnabled: true, MetricsEndpoint: testDoor},
			"otel.enabled is false",
		},
		{
			"collecting but shipping nowhere",
			&Telemetry{OtelEnabled: true, MetricsEndpoint: testDoor},
			"otel.byoc.enabled is false",
		},
		{
			"shipping to an endpoint this control plane did not issue",
			&Telemetry{OtelEnabled: true, ByocEnabled: true, MetricsEndpoint: "https://collector.acme.internal/otlp"},
			`ships metrics to "https://collector.acme.internal/otlp"`,
		},
		{
			"shipping nowhere named at all",
			&Telemetry{OtelEnabled: true, ByocEnabled: true},
			`but this control plane issued`,
		},
	} {
		// A meko agent that reports at all is held to exactly amp's checks.
		for _, kind := range []string{KindAmp, KindMeko} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				degraded := doorPolicy().check(kind, tc.tel)
				if degraded == "" {
					t.Fatalf("check accepted %s without comment", tc.name)
				}
				if !strings.Contains(degraded, tc.want) {
					t.Errorf("reason does not name the fix; want %q in:\n%s", tc.want, degraded)
				}
			})
		}
	}
}

// An agent that reports NOTHING is an old image, not a misconfiguration. Refusing
// it would fail every environment enrolled before attestation existed on nothing
// worse than a stale tag, so it enrolls and says so instead.
func TestTelemetryPolicy_AnAgentThatCannotAttestIsDegradedNotRefused(t *testing.T) {
	degraded := doorPolicy().check(KindAmp, nil)
	if !strings.Contains(degraded, "redeploy") {
		t.Errorf("degraded reason does not say what to do about it: %q", degraded)
	}
}

// Telemetry never refuses a registration or an enrolment, which is what keeps an
// environment that cannot ship metrics VISIBLE rather than turned away. The asymmetry
// this replaced (refuse a first enrolment, degrade a re-enrolment) also carried a
// restart-time lockout: enrolment repeats at every agent startup, so a refusal would
// have killed a running environment's convergence on its next pod recycle.
func TestTelemetryPolicy_NeverRefuses(t *testing.T) {
	for _, tel := range []*Telemetry{
		nil,
		{OtelEnabled: true, ByocEnabled: true, MetricsEndpoint: "https://elsewhere.example/otlp"},
		{OtelEnabled: false},
		{OtelEnabled: true, ByocEnabled: false},
		{OtelEnabled: true, ByocEnabled: true, StalledSeconds: 9000},
	} {
		// check returns a reason and nothing else — there is no error path to take.
		if degraded := doorPolicy().check(KindAmp, tel); degraded == "" && tel != nil && !tel.OtelEnabled {
			t.Errorf("a broken configuration produced no reason: %+v", tel)
		}
	}
}

// The other half of the problem: a deployment can pass every configuration check
// and still ship nothing. The install-time preflight proves it worked once; this is
// what notices it stopped.
func TestTelemetryPolicy_RuntimeStallDegrades(t *testing.T) {
	p := TelemetryPolicy{MetricsURL: testDoor, StaleAfter: 10 * time.Minute}
	cfg := func() *Telemetry {
		return &Telemetry{OtelEnabled: true, ByocEnabled: true, MetricsEndpoint: testDoor}
	}

	// Shipping: the counter moved recently.
	fresh := cfg()
	fresh.StalledSeconds = 30
	if got := p.checkRuntime(KindAmp, fresh); got != "" {
		t.Errorf("a shipping environment was degraded: %q", got)
	}

	// Stalled with an empty queue: nothing is reaching the exporter, so the fault is
	// upstream of it.
	up := cfg()
	up.StalledSeconds = 900
	got := p.checkRuntime(KindAmp, up)
	if !strings.Contains(got, "scraping nothing") {
		t.Errorf("an empty queue did not point upstream: %q", got)
	}

	// Stalled with a full queue: the door is not taking what we have.
	down := cfg()
	down.StalledSeconds, down.MetricsQueued, down.MetricsSendFailed = 900, 512, 40
	got = p.checkRuntime(KindAmp, down)
	if !strings.Contains(got, "not accepting them") {
		t.Errorf("a non-empty queue did not point at the door: %q", got)
	}
	if !strings.Contains(got, "512") || !strings.Contains(got, "40") {
		t.Errorf("the diagnostic counters did not reach the operator: %q", got)
	}
}

// "The agent cannot see its collector" is a different statement from "metrics have
// stopped", and must not be reported as a confirmed outage.
func TestTelemetryPolicy_ProbeErrorReadsAsUnknown(t *testing.T) {
	p := TelemetryPolicy{MetricsURL: testDoor, StaleAfter: 10 * time.Minute}
	got := p.checkRuntime(KindAmp, &Telemetry{
		OtelEnabled: true, ByocEnabled: true, MetricsEndpoint: testDoor,
		ProbeError: "connection refused",
	})
	if !strings.Contains(got, "unknown") {
		t.Errorf("an unreadable collector was reported as a confirmed stall: %q", got)
	}
}

// The runtime check NEVER refuses, even for a first enrolment: a door outage is not
// the operator's misconfiguration and must not stop a new environment coming up.
// It also applies only when a threshold and a door are configured.
func TestTelemetryPolicy_RuntimeIsDegradeOnlyAndScoped(t *testing.T) {
	stalled := &Telemetry{OtelEnabled: true, ByocEnabled: true, MetricsEndpoint: testDoor, StalledSeconds: 9000}

	// check() looks at configuration alone, so a stall is invisible to it — the two
	// checks answer different questions and must not be conflated.
	if degraded := (TelemetryPolicy{MetricsURL: testDoor, StaleAfter: time.Minute}).check(KindAmp, stalled); degraded != "" {
		t.Errorf("the configuration check reacted to a runtime stall: %q", degraded)
	}

	for _, tc := range []struct {
		name   string
		policy TelemetryPolicy
		kind   string
	}{
		{"no threshold configured", TelemetryPolicy{MetricsURL: testDoor}, KindAmp},
		{"no door configured", TelemetryPolicy{StaleAfter: time.Minute}, KindAmp},
	} {
		if got := tc.policy.checkRuntime(tc.kind, stalled); got != "" {
			t.Errorf("%s was degraded by the runtime check: %q", tc.name, got)
		}
	}
}

// A meko agent that reports counters is watched the same way an amp one is.
func TestTelemetryPolicy_RuntimeStallDegradesMeko(t *testing.T) {
	p := TelemetryPolicy{MetricsURL: testDoor, StaleAfter: time.Minute}
	stalled := &Telemetry{OtelEnabled: true, ByocEnabled: true, MetricsEndpoint: testDoor, StalledSeconds: 9000}
	if got := p.checkRuntime(KindMeko, stalled); !strings.Contains(got, "shipped no metrics") {
		t.Errorf("a stalled meko environment was not degraded: %q", got)
	}
	if got := p.checkRuntime(KindMeko, nil); got != "" {
		t.Errorf("a meko agent reporting nothing was degraded: %q", got)
	}
}

// The synthetic component is how a telemetry problem reaches an operator: the
// console already renders a component's Error, so the reason lands where they are
// already looking and needs no new column.
func TestWithTelemetryComponent(t *testing.T) {
	reported := []EnvComponent{{Name: "api-server", Healthy: 1, Total: 1}}

	if got := withTelemetryComponent(reported, ""); len(got) != 1 {
		t.Errorf("a healthy environment grew an extra component row: %+v", got)
	}

	got := withTelemetryComponent(reported, "shipping nowhere")
	if len(got) != 2 || got[1].Name != telemetryComponentName || got[1].Healthy != 0 || got[1].Total != 1 {
		t.Fatalf("telemetry component not appended as 0/1: %+v", got)
	}
	if got[1].Error != "shipping nowhere" {
		t.Errorf("reason did not reach the component: %q", got[1].Error)
	}

	// An agent that somehow reported its own "telemetry" row must not produce two.
	twice := withTelemetryComponent(got, "still shipping nowhere")
	if n := len(twice); n != 2 {
		t.Errorf("duplicate telemetry rows: %d\n%+v", n, twice)
	}
}

// A deployment shipping somewhere this control plane did not issue still ENROLS —
// registration is not failed over telemetry. It lands DEGRADED with the reason on a
// `telemetry` component instead, which is what keeps it visible in the console: a
// refusal would hide the very environment whose telemetry we are complaining about.
func TestEnrollByKey_EnrolsButDegradesAnEnvironmentThatShipsElsewhere(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), repository.NewEnvironmentTaskRepo(db))
	svc.SetTelemetryPolicy(doorPolicy())

	mock.ExpectQuery(`FROM meko_system\.environment_api_keys WHERE api_key_id = \$1`).
		WithArgs(keyA).
		WillReturnRows(envRow("amp-acme-prod", KindAmp))

	// $4 is the state column. DEGRADED from the moment it checks in, not from its
	// first heartbeat — an operator reading the console straight after registering
	// should see the problem rather than an ATTESTING row that looks fine.
	mock.ExpectExec(`UPDATE meko_system\.environments SET`).
		WithArgs(
			envUUID,
			sqlmock.AnyArg(), sqlmock.AnyArg(),
			StateDegraded,
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	e, err := svc.EnrollByKey(keyA, "onprem", KindAmp, ClusterFacts{},
		&Telemetry{OtelEnabled: true, ByocEnabled: true, MetricsEndpoint: "https://elsewhere.example/otlp"})
	if err != nil {
		t.Fatalf("enrolment was refused over telemetry: %v", err)
	}
	if e == nil || !e.Live {
		t.Fatalf("the environment did not enrol: %+v", e)
	}
	// The reason has to reach the console, not just the state.
	var comps []EnvComponent
	if uerr := json.Unmarshal(e.Components, &comps); uerr != nil {
		t.Fatalf("components are not readable JSON: %v", uerr)
	}
	var found string
	for _, c := range comps {
		if c.Name == telemetryComponentName {
			found = c.Error
		}
	}
	if !strings.Contains(found, "elsewhere.example") {
		t.Errorf("the telemetry component does not name the wrong endpoint: %q", found)
	}
	if merr := mock.ExpectationsWereMet(); merr != nil {
		t.Errorf("unmet expectations: %v", merr)
	}
}

// The same deployment enrolls fine when the endpoint is the one this control plane
// issued — the pairing that makes the install hint's printed value load-bearing.
func TestEnrollByKey_AcceptsTheIssuedDoor(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), repository.NewEnvironmentTaskRepo(db))
	svc.SetTelemetryPolicy(doorPolicy())

	mock.ExpectQuery(`FROM meko_system\.environment_api_keys WHERE api_key_id = \$1`).
		WithArgs(keyA).
		WillReturnRows(envRow("amp-acme-prod", KindAmp))
	mock.ExpectExec(`UPDATE meko_system\.environments SET`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	e, err := svc.EnrollByKey(keyA, "onprem", KindAmp, ClusterFacts{}, goodTelemetry())
	if err != nil {
		t.Fatalf("EnrollByKey: %v", err)
	}
	if e == nil || !e.Live {
		t.Fatalf("a correctly configured environment did not enroll: %+v", e)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The telemetry row is FOLDED into what is already stored, never written over it.
// An amp agent reports no components at all, so overwriting would erase the last
// good report every tick — which is the failure the nil-Components guard in
// ApplyStatus exists to prevent. Both rules have to hold at once.
func TestWithTelemetryComponent_PreservesWhatWasAlreadyStored(t *testing.T) {
	stored := []byte(`[{"name":"amp-controller","healthy":1,"total":1}]`)

	got := withTelemetryComponent(storedComponents(stored), "shipping nowhere")
	if len(got) != 2 {
		t.Fatalf("stored components were lost: %+v", got)
	}
	if got[0].Name != "amp-controller" || got[0].Healthy != 1 {
		t.Errorf("the stored component did not survive: %+v", got[0])
	}
	if got[1].Name != telemetryComponentName || got[1].Error != "shipping nowhere" {
		t.Errorf("the telemetry row was not appended: %+v", got[1])
	}
}

// Unreadable stored JSON must not fail the whole status write — it degrades to
// recording the telemetry row alone.
func TestStoredComponents_TolerantOfJunk(t *testing.T) {
	for _, raw := range []string{"", "null", "not json", "{}"} {
		if got := storedComponents([]byte(raw)); got != nil {
			t.Errorf("storedComponents(%q) = %+v, want nil", raw, got)
		}
	}
}

// Drift after enroll: a `helm upgrade --set otel.enabled=false` would otherwise
// show up as nothing worse than a gradually emptier dashboard. It cannot un-enroll
// a running environment, so it degrades rather than erroring.
func TestApplyStatus_DegradesAnEnvironmentThatStoppedShipping(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), repository.NewEnvironmentTaskRepo(db))
	svc.SetTelemetryPolicy(doorPolicy())

	mock.ExpectQuery(`FROM meko_system\.environments e WHERE e\.uuid = \$1`).
		WithArgs(envUUID).
		WillReturnRows(envRow("amp-acme-prod", KindAmp))

	// $4 is the state column (see TestEnrollByKey_ChecksIn's positional args). The
	// agent reported READY; the stored state must be DEGRADED regardless, because
	// the agent is reporting on its own convergence and not on whether anyone can
	// see it.
	mock.ExpectExec(`UPDATE meko_system\.environments SET`).
		WithArgs(
			envUUID,
			sqlmock.AnyArg(), sqlmock.AnyArg(),
			StateDegraded, // $4 state
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := svc.ApplyStatus(envUUID, StatusReport{
		EnvironmentState: "READY",
		Telemetry:        &Telemetry{OtelEnabled: false},
	})
	if err != nil {
		t.Fatalf("ApplyStatus: %v", err)
	}
	if !res.Found {
		t.Fatal("status was not applied to the environment")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("an environment that stopped shipping was not degraded: %v", err)
	}
}
