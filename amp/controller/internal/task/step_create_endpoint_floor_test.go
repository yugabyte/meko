package task

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/node"
)

const testVerifier = "SCRAM-SHA-256$4096:c2FsdA==$c3RvcmVk:c2VydmVy"

// fakeRemoteAuth answers GET /v1/remote-auth from a script, call by call.
type fakeRemoteAuth struct {
	answer func(ctx context.Context, call int) (node.RemoteAuthStatus, error)

	mu   sync.Mutex
	urls []string
}

func (f *fakeRemoteAuth) RemoteAuth(ctx context.Context, base string) (node.RemoteAuthStatus, error) {
	f.mu.Lock()
	f.urls = append(f.urls, base)
	call := len(f.urls)
	f.mu.Unlock()
	return f.answer(ctx, call)
}

func (f *fakeRemoteAuth) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.urls)
}

func answers(status node.RemoteAuthStatus, err error) func(context.Context, int) (node.RemoteAuthStatus, error) {
	return func(context.Context, int) (node.RemoteAuthStatus, error) { return status, err }
}

// noSupervisorDeployer is a deployer with no way to reach an endpoint's
// supervisor, as the local driver is.
type noSupervisorDeployer struct{ *fakeDeployer }

func (noSupervisorDeployer) EndpointSupervisorURL(string) string { return "" }

type floorFixture struct {
	state     *fakeStateStore
	dep       *fakeDeployer
	auth      *fakeRemoteAuth
	args      CreateEndpointArgs
	clusterID uuid.UUID
}

// newFloorFixture is a writer create with a password on a root that has no
// floor yet, on an endpoint whose supervisor reports the password applied.
func newFloorFixture(t *testing.T) *floorFixture {
	t.Helper()
	oldPoll, oldBudget := remoteAuthPoll, remoteAuthBudget
	remoteAuthPoll, remoteAuthBudget = time.Millisecond, time.Minute
	t.Cleanup(func() { remoteAuthPoll, remoteAuthBudget = oldPoll, oldBudget })

	f := &floorFixture{
		state:     newFakeStateStore(),
		dep:       &fakeDeployer{},
		auth:      &fakeRemoteAuth{answer: answers(node.RemoteAuthStatus{State: node.RemoteAuthApplied, LSN: "0/1A2B3C0"}, nil)},
		clusterID: uuid.New(),
	}
	f.args = CreateEndpointArgs{EndpointID: uuid.New(), ClusterID: f.clusterID, BranchID: uuid.New(),
		Role: "read_write", PgVersion: 18, PasswordVerifier: testVerifier}
	f.state.seedBranch(f.clusterID, f.args.BranchID)
	return f
}

func (f *floorFixture) execute(ctx context.Context) StepResult {
	return NewCreateEndpointStep(f.args, Deps{State: f.state, Deployer: f.dep, ControllerURL: "http://sc:50051", RemoteAuth: f.auth}).Execute(ctx)
}

func (f *floorFixture) floor() (string, bool) {
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	lsn, ok := f.state.rootForkLSN[f.args.BranchID]
	return lsn, ok
}

func TestCreateEndpointStepRecordsTheRootFloorWhenThePasswordIsApplied(t *testing.T) {
	f := newFloorFixture(t)
	res := f.execute(t.Context())
	if res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if got := decodeResult(t, res.Output)["compute_password"]; got != "applied" {
		t.Errorf("result compute_password = %v, want applied", got)
	}
	want := f.dep.EndpointSupervisorURL(EndpointDeployName(f.args.EndpointID))
	if len(f.auth.urls) != 1 || f.auth.urls[0] != want {
		t.Errorf("asked %v, want [%s]", f.auth.urls, want)
	}
	if lsn, _ := f.floor(); lsn != "0/1A2B3C0" {
		t.Errorf("root floor = %q, want 0/1A2B3C0", lsn)
	}
}

// The step is called right after the pod is up, usually before the supervisor
// has decided, and a fresh endpoint's Service can refuse for a moment.
func TestCreateEndpointStepPollsUntilTheSupervisorDecides(t *testing.T) {
	f := newFloorFixture(t)
	f.auth.answer = func(_ context.Context, call int) (node.RemoteAuthStatus, error) {
		switch call {
		case 1:
			return node.RemoteAuthStatus{}, node.ErrUnreachable
		case 2, 3:
			return node.RemoteAuthStatus{State: node.RemoteAuthPending}, nil
		default:
			return node.RemoteAuthStatus{State: node.RemoteAuthApplied, LSN: "0/2000028"}, nil
		}
	}
	res := f.execute(t.Context())
	if res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	if n := f.auth.calls(); n != 4 {
		t.Errorf("asked %d time(s), want 4", n)
	}
	if lsn, _ := f.floor(); lsn != "0/2000028" {
		t.Errorf("root floor = %q, want 0/2000028", lsn)
	}
	if got := decodeResult(t, res.Output)["compute_password"]; got != "applied" {
		t.Errorf("result compute_password = %v, want applied", got)
	}
}

// The outcomes that mean the catalog has a password but not where it was set
// record 0/0, so the root stops requiring a password the supervisor would not
// apply.
func TestCreateEndpointStepRecordsZeroFloorForAPasswordOfUnknownLSN(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*floorFixture)
		want  string
	}{
		{"applied with no lsn", func(f *floorFixture) {
			f.auth.answer = answers(node.RemoteAuthStatus{State: node.RemoteAuthApplied}, nil)
		}, "applied_no_lsn"},
		{"applied with an lsn that does not parse", func(f *floorFixture) {
			f.auth.answer = answers(node.RemoteAuthStatus{State: node.RemoteAuthApplied, LSN: "not-an-lsn"}, nil)
		}, "applied_no_lsn"},
		{"already set", func(f *floorFixture) {
			f.auth.answer = answers(node.RemoteAuthStatus{State: node.RemoteAuthAlreadySet}, nil)
		}, "already_set"},
		{"an endpoint image older than the route", func(f *floorFixture) {
			f.auth.answer = answers(node.RemoteAuthStatus{State: node.RemoteAuthUnsupported}, nil)
		}, "unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFloorFixture(t)
			tc.setup(f)
			res := f.execute(t.Context())
			if res.Failed() {
				t.Fatalf("Execute: %v", res.Err)
			}
			if got := decodeResult(t, res.Output)["compute_password"]; got != tc.want {
				t.Errorf("result compute_password = %v, want %s", got, tc.want)
			}
			if lsn, ok := f.floor(); !ok || lsn != "0/0" {
				t.Errorf("root floor = %q (set %v), want 0/0", lsn, ok)
			}
		})
	}
}

// Outcomes that may leave the catalog with no password, and any failure along
// the way, record nothing and still create the endpoint: the floor is never
// worth a failed create. The result says which, and BackfillRootForkLSN reads it.
func TestCreateEndpointStepRecordsNoFloorWithoutEvidenceOfAPassword(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*floorFixture)
		want  string
	}{
		{"unset", func(f *floorFixture) {
			f.auth.answer = answers(node.RemoteAuthStatus{State: node.RemoteAuthUnset}, nil)
		}, "unset"},
		{"failed", func(f *floorFixture) {
			f.auth.answer = answers(node.RemoteAuthStatus{State: node.RemoteAuthFailed}, nil)
		}, "failed"},
		{"a state this controller does not know", func(f *floorFixture) {
			f.auth.answer = answers(node.RemoteAuthStatus{State: "deferred", LSN: "0/10"}, nil)
		}, "unknown"},
		{"still pending at the budget", func(f *floorFixture) {
			remoteAuthBudget = 20 * time.Millisecond
			f.auth.answer = answers(node.RemoteAuthStatus{State: node.RemoteAuthPending}, nil)
		}, "unknown"},
		{"unreachable until the budget", func(f *floorFixture) {
			remoteAuthBudget = 20 * time.Millisecond
			f.auth.answer = answers(node.RemoteAuthStatus{}, node.ErrUnreachable)
		}, "unknown"},
		{"pending read error", func(f *floorFixture) { f.state.errRootForkPending = errors.New("db down") }, "unknown"},
		{"floor write error", func(f *floorFixture) { f.state.errSetRootForkLSN = errors.New("db down") }, "applied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFloorFixture(t)
			tc.setup(f)
			res := f.execute(t.Context())
			if res.Failed() {
				t.Fatalf("Execute failed over the floor: %v", res.Err)
			}
			if got := decodeResult(t, res.Output)["compute_password"]; got != tc.want {
				t.Errorf("result compute_password = %v, want %s", got, tc.want)
			}
			if lsn, ok := f.floor(); ok {
				t.Errorf("root floor recorded as %q, want none", lsn)
			}
			if f.state.endpointAddresses[f.args.EndpointID] == "" {
				t.Error("endpoint address not recorded")
			}
		})
	}
}

// Each of these has no floor to record, or no way to learn one, so the step
// must not spend a wait on the supervisor.
func TestCreateEndpointStepSkipsTheWaitWithNoFloorToRecord(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*floorFixture, *Deps)
	}{
		{"fork", func(f *floorFixture, _ *Deps) {
			f.state.seedBranchFork(f.clusterID, f.args.BranchID, uuid.New())
		}},
		{"root with a floor", func(f *floorFixture, _ *Deps) {
			f.state.rootForkLSN = map[uuid.UUID]string{f.args.BranchID: "0/0"}
		}},
		{"no verifier", func(f *floorFixture, _ *Deps) { f.args.PasswordVerifier = "" }},
		{"read_only", func(f *floorFixture, _ *Deps) { f.args.Role, f.args.PasswordVerifier = "read_only", "" }},
		{"no supervisor url", func(f *floorFixture, d *Deps) { d.Deployer = noSupervisorDeployer{f.dep} }},
		{"no client", func(_ *floorFixture, d *Deps) { d.RemoteAuth = nil }},
		// No deployer opens the supervisor's API without a controller URL.
		{"no controller url", func(_ *floorFixture, d *Deps) { d.ControllerURL = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFloorFixture(t)
			deps := Deps{State: f.state, Deployer: f.dep, ControllerURL: "http://sc:50051", RemoteAuth: f.auth}
			tc.setup(f, &deps)
			wantFloor, wantSet := f.floor()
			res := NewCreateEndpointStep(f.args, deps).Execute(t.Context())
			if res.Failed() {
				t.Fatalf("Execute: %v", res.Err)
			}
			if n := f.auth.calls(); n != 0 {
				t.Errorf("asked the supervisor %d time(s), want 0", n)
			}
			if got, ok := decodeResult(t, res.Output)["compute_password"]; ok {
				t.Errorf("result compute_password = %v with no wait, want it absent", got)
			}
			if got, set := f.floor(); got != wantFloor || set != wantSet {
				t.Errorf("root floor = %q (set %v), want %q (set %v)", got, set, wantFloor, wantSet)
			}
		})
	}
}

// A replay finds the floor its first run recorded and neither asks again nor
// moves it.
func TestCreateEndpointStepReplayKeepsTheFloorWithoutWaiting(t *testing.T) {
	f := newFloorFixture(t)
	if res := f.execute(t.Context()); res.Failed() {
		t.Fatalf("Execute: %v", res.Err)
	}
	f.auth.answer = answers(node.RemoteAuthStatus{State: node.RemoteAuthApplied, LSN: "0/2000000"}, nil)
	if res := f.execute(t.Context()); res.Failed() {
		t.Fatalf("replay Execute: %v", res.Err)
	}
	if n := f.auth.calls(); n != 1 {
		t.Errorf("asked %d time(s) across a run and its replay, want 1", n)
	}
	if lsn, _ := f.floor(); lsn != "0/1A2B3C0" {
		t.Errorf("root floor = %q after the replay, want the first run's 0/1A2B3C0", lsn)
	}
}

// A shutdown during the wait interrupts the step rather than completing it
// without a floor, so the controller that resumes it records one.
func TestCreateEndpointStepShutdownDuringTheWaitInterrupts(t *testing.T) {
	f := newFloorFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	f.auth.answer = func(context.Context, int) (node.RemoteAuthStatus, error) {
		cancel()
		return node.RemoteAuthStatus{State: node.RemoteAuthPending}, nil
	}
	res := f.execute(ctx)
	if !res.Failed() || !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("Execute = %v, want an interruption carrying context.Canceled", res.Err)
	}
	if lsn, ok := f.floor(); ok {
		t.Errorf("root floor recorded as %q, want none", lsn)
	}
}
