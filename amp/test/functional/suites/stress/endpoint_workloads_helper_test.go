package stress

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps"
)

// stubLoad is an endpointWorkload with scripted liveness and log.
func stubLoad(name string, tenant int, alive bool, tail string) *endpointWorkload {
	return &endpointWorkload{
		Name:          name,
		Tenant:        tenant,
		exitIsFinding: true,
		start:         func(context.Context) error { return nil },
		alive:         func(context.Context) (bool, error) { return alive, nil },
		logTail:       func(context.Context) string { return tail },
		collect:       func(context.Context, apps.ArtifactSink) error { return nil },
	}
}

func TestWorkloadCheckPassesWhenAllAreRunningAndClean(t *testing.T) {
	w := &workloadSet{logf: func(string, ...any) {}, workloads: []*endpointWorkload{
		stubLoad("SqlTransactions", 0, true, "some ops/sec chatter"),
		stubLoad("SqlBankTransfer", 1, true, "balances fine"),
	}}
	if err := w.Check(context.Background(), "software_upgrade"); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

// The bank workload exits on a violation (--bank_exit_on_fail), so a gone workload is a finding.
func TestWorkloadCheckFailsOnAnExitedWorkload(t *testing.T) {
	w := &workloadSet{logf: func(string, ...any) {}, workloads: []*endpointWorkload{
		stubLoad("SqlBankTransfer", 1, false, "ERROR Failed to compare sums: Expected 200000 - actual 199000"),
	}}
	err := w.Check(context.Background(), "software_upgrade")
	if err == nil {
		t.Fatal("a workload that exited must fail the check")
	}
	for _, want := range []string{"software_upgrade", "SqlBankTransfer", "Failed to compare sums"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q", err, want)
		}
	}
}

// Without --bank_exit_on_fail the app keeps going after logging a violation, so a still-running workload must be caught too.
func TestWorkloadCheckFailsOnAViolationInTheLog(t *testing.T) {
	w := &workloadSet{logf: func(string, ...any) {}, workloads: []*endpointWorkload{
		stubLoad("SqlBankTransfer", 1, true, "ERROR Failed to compare sums: Expected 5 - actual 4"),
	}}
	if err := w.Check(context.Background(), "gflag_change"); err == nil {
		t.Fatal("a logged consistency violation must fail the check even while the workload runs")
	}
}

func TestWorkloadCheckCatchesForbiddenExceptions(t *testing.T) {
	w := &workloadSet{logf: func(string, ...any) {}, workloads: []*endpointWorkload{
		stubLoad("SqlTransactions", 0, true, "ERROR Forbidden exception detected"),
	}}
	if err := w.Check(context.Background(), "data_move"); err == nil {
		t.Fatal("a forbidden exception must fail the check")
	}
}

// A maintenance run is full of connection resets by design; those must not read as violations.
func TestWorkloadCheckIgnoresRetryableNoise(t *testing.T) {
	w := &workloadSet{logf: func(string, ...any) {}, workloads: []*endpointWorkload{
		stubLoad("SqlTransactions", 0, true,
			"WARN connection reset by peer\nERROR could not connect, retrying\nINFO reconnected"),
	}}
	if err := w.Check(context.Background(), "rolling_restart"); err != nil {
		t.Fatalf("retryable errors must not be read as consistency violations: %v", err)
	}
}

// A workload that never generated load makes every later assertion vacuous.
func TestWorkloadSetStartFailsFastOnAWorkloadThatWontRun(t *testing.T) {
	bad := stubLoad("tpcc", 2, true, "")
	bad.start = func(context.Context) error { return errors.New("connection refused") }
	w := &workloadSet{logf: func(string, ...any) {}, workloads: []*endpointWorkload{bad}}
	err := w.Start(context.Background())
	if err == nil {
		t.Fatal("a workload that will not start must fail the run")
	}
	if !strings.Contains(err.Error(), "tpcc") {
		t.Errorf("error %q should name the workload", err)
	}
}

type countingSink struct{ n int }

func (c *countingSink) Add(string, string, []byte) error { c.n++; return nil }

func TestWorkloadCollectAllIsIdempotent(t *testing.T) {
	collected := 0
	wl := stubLoad("SqlTransactions", 0, true, "")
	wl.collect = func(context.Context, apps.ArtifactSink) error { collected++; return nil }
	w := &workloadSet{logf: func(string, ...any) {}, workloads: []*endpointWorkload{wl}}
	sink := &countingSink{}
	w.CollectAll(sink)
	w.CollectAll(sink)
	if collected != 1 {
		t.Errorf("collected %d times, want exactly 1", collected)
	}
}

// Omitting the password failed a real run with `password authentication failed for user "cloud_admin"`.
func TestEndpointPropsCarryTheAmpCredentials(t *testing.T) {
	props := endpointProps("cloud_admin", "s3cret", "postgres")
	if props["username"] != "cloud_admin" || props["default_postgres_database"] != "postgres" {
		t.Errorf("props = %v, want amp's compute role and database", props)
	}
	if props["password"] != "s3cret" {
		t.Errorf("props carry no password: %v", props)
	}
}

// amp compute is vanilla PostgreSQL, so the jar's YB-only "SPLIT INTO n TABLETS" DDL must be suppressed.
func TestBankOnAmpPropsSuppressYBOnlyDDLAndExitOnViolation(t *testing.T) {
	props := bankOnAmpProps(200)
	if props["bank_number_of_tablets"] != "0" {
		t.Errorf("bank_number_of_tablets = %q, want 0 so the YB-only SPLIT INTO clause is omitted", props["bank_number_of_tablets"])
	}
	if props["bank_exit_on_fail"] != "true" {
		t.Errorf("bank_exit_on_fail = %q, want true so a broken total stops the workload", props["bank_exit_on_fail"])
	}
	if props["bank_number_of_accounts"] != "200" {
		t.Errorf("bank_number_of_accounts = %q", props["bank_number_of_accounts"])
	}
}

// TPC-C's one connection per worker has no connection-death retry, so a rolling op aborting it is the driver's limitation, not amp's.
func TestWorkloadCheckToleratesTpccLosingItsConnection(t *testing.T) {
	tpcc := stubLoad("tpcc", 2, false, "Caused by: com.yugabyte.util.PSQLException: This connection has been closed.")
	tpcc.exitIsFinding = false
	w := &workloadSet{logf: func(string, ...any) {}, workloads: []*endpointWorkload{tpcc}}
	if err := w.Check(context.Background(), "software_upgrade"); err != nil {
		t.Fatalf("a connection-death abort of TPC-C must be tolerated: %v", err)
	}
}

// Tolerating the exit must not tolerate a violation: the log stays authoritative regardless.
func TestWorkloadCheckStillFailsOnAViolationFromATolerantWorkload(t *testing.T) {
	tpcc := stubLoad("tpcc", 2, false, "ERROR Failed to compare sums: Expected 9 - actual 8")
	tpcc.exitIsFinding = false
	w := &workloadSet{logf: func(string, ...any) {}, workloads: []*endpointWorkload{tpcc}}
	if err := w.Check(context.Background(), "software_upgrade"); err == nil {
		t.Fatal("a consistency violation stays fatal even for a workload allowed to exit")
	}
}

func TestWorkloadCheckDoesNotTolerateASampleAppExit(t *testing.T) {
	w := &workloadSet{logf: func(string, ...any) {}, workloads: []*endpointWorkload{
		stubLoad("SqlTransactions", 0, false, "no violation here"),
	}}
	if err := w.Check(context.Background(), "software_upgrade"); err == nil {
		t.Fatal("a retrying workload that died must fail the run")
	}
}

// The tolerance in TestWorkloadCheckToleratesTpccLosingItsConnection belongs to the public build alone; the stress fork reconnects.
func TestTpccExitIsAFindingWithTheReconnectingFork(t *testing.T) {
	tpcc := stubLoad("tpcc", 2, false, "")
	tpcc.exitIsFinding = true
	w := &workloadSet{logf: func(string, ...any) {}, workloads: []*endpointWorkload{tpcc}}
	if err := w.Check(context.Background(), "software_upgrade"); err == nil {
		t.Fatal("with the reconnecting fork, TPC-C stopping must fail the run")
	}
}
