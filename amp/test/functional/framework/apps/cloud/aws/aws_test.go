package aws

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
	sshx "github.com/yugabyte/amp/test/functional/framework/exec/ssh"
)

// recorder is a scripted fake aws runner: it records every argv and answers via
// an optional respond hook (call index + argv → result).
type recorder struct {
	mu      sync.Mutex
	calls   [][]string
	respond func(call int, argv []string) (execpkg.Result, error)
}

func (r *recorder) run(_ context.Context, argv []string) (execpkg.Result, error) {
	r.mu.Lock()
	idx := len(r.calls)
	r.calls = append(r.calls, append([]string(nil), argv...))
	respond := r.respond
	r.mu.Unlock()
	if respond != nil {
		return respond(idx, argv)
	}
	return execpkg.Result{ExitCode: 0}, nil
}

func (r *recorder) recorded() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// runInstancesJSON is a canned run-instances output with a per-index id/IP.
func runInstancesJSON(n int) string {
	return fmt.Sprintf(`{"Instances":[{"InstanceId":"i-%03d","PublicIpAddress":"52.0.0.%d","PrivateIpAddress":"10.0.0.%d","State":{"Name":"pending"}}]}`, n, n, n)
}

func subcommand(argv []string) string {
	if len(argv) >= 3 {
		return argv[2]
	}
	return ""
}

func argHas(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

func argVal(argv []string, flag string) string {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func newRunClient(t *testing.T, rec *recorder) *Client {
	t.Helper()
	rec.respond = func(call int, argv []string) (execpkg.Result, error) {
		if subcommand(argv) == "run-instances" {
			return execpkg.Result{ExitCode: 0, Stdout: runInstancesJSON(call)}, nil
		}
		return execpkg.Result{ExitCode: 0}, nil
	}
	return New(Config{}, WithRunner(rec.run), WithLogf(nil))
}

func TestRunInstancesArgvAndHygiene(t *testing.T) {
	rec := &recorder{}
	c := newRunClient(t, rec)

	instances, err := c.RunInstances(context.Background(), RunSpec{Count: 3, RunTag: "abc123"})
	if err != nil {
		t.Fatalf("RunInstances: %v", err)
	}
	if len(instances) != 3 {
		t.Fatalf("got %d instances, want 3", len(instances))
	}
	if instances[0].ID != "i-000" || instances[2].ID != "i-002" {
		t.Errorf("instance ids = %v", instances)
	}
	if instances[0].PublicIP != "52.0.0.0" || instances[0].PrivateIP != "10.0.0.0" {
		t.Errorf("instance[0] IPs = %+v", instances[0])
	}

	calls := rec.recorded()
	if len(calls) != 3 {
		t.Fatalf("got %d aws calls, want 3 (one run-instances per instance)", len(calls))
	}

	for i, argv := range calls {
		if subcommand(argv) != "run-instances" {
			t.Fatalf("call %d not run-instances: %v", i, argv)
		}
		// Hygiene: reuse the shared keypair; NEVER create one.
		if got := argVal(argv, "--key-name"); got != DefaultKeyName {
			t.Errorf("call %d --key-name = %q, want %q", i, got, DefaultKeyName)
		}
		if argHas(argv, "create-key-pair") || argHas(argv, "--create-key-pair") {
			t.Errorf("call %d issued a create-key-pair — hygiene violation: %v", i, argv)
		}
		if !argHas(argv, "--associate-public-ip-address") {
			t.Errorf("call %d missing --associate-public-ip-address", i)
		}
		if got := argVal(argv, "--count"); got != "1" {
			t.Errorf("call %d --count = %q, want 1", i, got)
		}
		if got := argVal(argv, "--region"); got != DefaultRegion {
			t.Errorf("call %d --region = %q, want %q", i, got, DefaultRegion)
		}
		if got := argVal(argv, "--image-id"); got != DefaultAMI {
			t.Errorf("call %d --image-id = %q, want %q", i, got, DefaultAMI)
		}
		if got := argVal(argv, "--subnet-id"); got != DefaultSubnet {
			t.Errorf("call %d --subnet-id = %q, want %q", i, got, DefaultSubnet)
		}
		// All four SGs present as distinct argv elements.
		for _, sg := range DefaultSecurityGroups {
			if !argHas(argv, sg) {
				t.Errorf("call %d missing security group %q", i, sg)
			}
		}

		// Mandatory tags all present with the per-index Name and the run tag.
		tagSpec := argVal(argv, "--tag-specifications")
		wantName := fmt.Sprintf("%s-abc123-%d", DefaultOwner, i)
		for _, want := range []string{
			"{Key=Name,Value=" + wantName + "}",
			"{Key=yb_owner,Value=" + DefaultOwner + "}",
			"{Key=yb_task,Value=stress-dev}",
			"{Key=yb_dept,Value=qa}",
			"{Key=autoclean,Value=true}",
			"{Key=ampqa,Value=1}",
			"{Key=ampqa-run,Value=abc123}",
		} {
			if !strings.Contains(tagSpec, want) {
				t.Errorf("call %d tag-spec missing %q in %q", i, want, tagSpec)
			}
		}
	}
}

// Across the WHOLE recorded history, no create-key-pair is ever issued — the
// core hygiene guarantee of this app.
func TestNeverCreatesKeypair(t *testing.T) {
	rec := &recorder{}
	c := newRunClient(t, rec)
	if _, err := c.RunInstances(context.Background(), RunSpec{Count: 2, RunTag: "t"}); err != nil {
		t.Fatalf("RunInstances: %v", err)
	}
	_ = c.WaitRunning(context.Background(), []string{"i-000"})
	_ = c.Terminate(context.Background(), []string{"i-000"})
	for _, argv := range rec.recorded() {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "create-key-pair") {
			t.Fatalf("create-key-pair issued: %v", argv)
		}
	}
}

func TestRunInstancesRequiresRunTag(t *testing.T) {
	c := New(Config{}, WithRunner((&recorder{}).run), WithLogf(nil))
	if _, err := c.RunInstances(context.Background(), RunSpec{Count: 1}); err == nil {
		t.Fatal("expected error with empty RunTag")
	}
}

func TestRunInstancesRollsBackOnPartialFailure(t *testing.T) {
	rec := &recorder{}
	rec.respond = func(call int, argv []string) (execpkg.Result, error) {
		if subcommand(argv) == "run-instances" {
			if call == 1 {
				return execpkg.Result{ExitCode: 255, Stderr: "boom"}, nil
			}
			return execpkg.Result{ExitCode: 0, Stdout: runInstancesJSON(call)}, nil
		}
		return execpkg.Result{ExitCode: 0}, nil
	}
	c := New(Config{}, WithRunner(rec.run), WithLogf(nil))

	if _, err := c.RunInstances(context.Background(), RunSpec{Count: 3, RunTag: "roll"}); err == nil {
		t.Fatal("expected error on second launch failure")
	}
	// The first instance (i-000) must have been terminated on rollback.
	var terminated bool
	for _, argv := range rec.recorded() {
		if subcommand(argv) == "terminate-instances" && argHas(argv, "i-000") {
			terminated = true
		}
	}
	if !terminated {
		t.Error("first instance was not terminated on rollback — leak")
	}
}

func TestWaitRunningArgv(t *testing.T) {
	rec := &recorder{}
	c := New(Config{}, WithRunner(rec.run), WithLogf(nil))
	if err := c.WaitRunning(context.Background(), []string{"i-1", "i-2"}); err != nil {
		t.Fatalf("WaitRunning: %v", err)
	}
	calls := rec.recorded()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	argv := calls[0]
	if subcommand(argv) != "wait" || argv[3] != "instance-running" {
		t.Errorf("argv = %v, want ec2 wait instance-running", argv)
	}
	if !argHas(argv, "i-1") || !argHas(argv, "i-2") {
		t.Errorf("argv missing ids: %v", argv)
	}
}

func TestWaitRunningEmptyIsNoop(t *testing.T) {
	rec := &recorder{}
	c := New(Config{}, WithRunner(rec.run), WithLogf(nil))
	if err := c.WaitRunning(context.Background(), nil); err != nil {
		t.Fatalf("WaitRunning(nil): %v", err)
	}
	if len(rec.recorded()) != 0 {
		t.Errorf("empty WaitRunning issued a call: %v", rec.recorded())
	}
}

func TestTerminateIdempotent(t *testing.T) {
	// Empty ids → no call.
	rec := &recorder{}
	c := New(Config{}, WithRunner(rec.run), WithLogf(nil))
	if err := c.Terminate(context.Background(), nil); err != nil {
		t.Fatalf("Terminate(nil): %v", err)
	}
	if len(rec.recorded()) != 0 {
		t.Errorf("empty Terminate issued a call")
	}

	// Already-gone instance is not an error.
	rec2 := &recorder{respond: func(_ int, _ []string) (execpkg.Result, error) {
		return execpkg.Result{ExitCode: 254, Stderr: "An error occurred (InvalidInstanceID.NotFound) ..."}, nil
	}}
	c2 := New(Config{}, WithRunner(rec2.run), WithLogf(nil))
	if err := c2.Terminate(context.Background(), []string{"i-gone"}); err != nil {
		t.Errorf("Terminate of gone instance should be idempotent, got %v", err)
	}

	// A genuine failure surfaces.
	rec3 := &recorder{respond: func(_ int, _ []string) (execpkg.Result, error) {
		return execpkg.Result{ExitCode: 1, Stderr: "AccessDenied"}, nil
	}}
	c3 := New(Config{}, WithRunner(rec3.run), WithLogf(nil))
	if err := c3.Terminate(context.Background(), []string{"i-x"}); err == nil {
		t.Error("expected error on genuine terminate failure")
	}
}

func TestReapByTagFiltersAndTerminates(t *testing.T) {
	rec := &recorder{}
	rec.respond = func(_ int, argv []string) (execpkg.Result, error) {
		if subcommand(argv) == "describe-instances" {
			return execpkg.Result{ExitCode: 0, Stdout: `{"Reservations":[{"Instances":[
				{"InstanceId":"i-a","State":{"Name":"running"}},
				{"InstanceId":"i-b","State":{"Name":"running"}}]}]}`}, nil
		}
		return execpkg.Result{ExitCode: 0}, nil
	}
	c := New(Config{}, WithRunner(rec.run), WithLogf(nil))
	if err := c.ReapByTag(context.Background(), "run-42"); err != nil {
		t.Fatalf("ReapByTag: %v", err)
	}
	calls := rec.recorded()
	// First call describes filtered by the run tag.
	if subcommand(calls[0]) != "describe-instances" {
		t.Fatalf("first call = %v, want describe-instances", calls[0])
	}
	if !argHas(calls[0], "tag:ampqa-run,Values=run-42") && !argHas(calls[0], "Name=tag:ampqa-run,Values=run-42") {
		// filter element is "Name=tag:ampqa-run,Values=run-42"
		var found bool
		for _, a := range calls[0] {
			if strings.Contains(a, "tag:ampqa-run") && strings.Contains(a, "run-42") {
				found = true
			}
		}
		if !found {
			t.Errorf("describe not filtered by run tag: %v", calls[0])
		}
	}
	// Second call terminates exactly the described ids.
	last := calls[len(calls)-1]
	if subcommand(last) != "terminate-instances" || !argHas(last, "i-a") || !argHas(last, "i-b") {
		t.Errorf("terminate call = %v, want i-a and i-b", last)
	}
}

func TestReapStaleFiltersByAge(t *testing.T) {
	old := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	fresh := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	rec := &recorder{}
	rec.respond = func(_ int, argv []string) (execpkg.Result, error) {
		if subcommand(argv) == "describe-instances" {
			return execpkg.Result{ExitCode: 0, Stdout: fmt.Sprintf(`{"Reservations":[{"Instances":[
				{"InstanceId":"i-old","LaunchTime":"%s","State":{"Name":"running"}},
				{"InstanceId":"i-new","LaunchTime":"%s","State":{"Name":"running"}}]}]}`, old, fresh)}, nil
		}
		return execpkg.Result{ExitCode: 0}, nil
	}
	c := New(Config{}, WithRunner(rec.run), WithLogf(nil))
	if err := c.ReapStale(context.Background(), time.Hour); err != nil {
		t.Fatalf("ReapStale: %v", err)
	}
	last := rec.recorded()[len(rec.recorded())-1]
	if subcommand(last) != "terminate-instances" {
		t.Fatalf("last call = %v, want terminate-instances", last)
	}
	if !argHas(last, "i-old") {
		t.Error("stale instance i-old not terminated")
	}
	if argHas(last, "i-new") {
		t.Error("fresh instance i-new should not be terminated")
	}
}

// fakeConn is a fake SSH connection.
type fakeConn struct{ closed bool }

func (f *fakeConn) Run(context.Context, string) (execpkg.Result, error) {
	return execpkg.Result{}, nil
}
func (f *fakeConn) Copy(context.Context, string, string) error { return nil }
func (f *fakeConn) Close() error                               { f.closed = true; return nil }

// TestConnectWiring is the core reach-bug regression: with the default config
// (reach unset → "private"), Connect MUST dial the PRIVATE IP, never the
// public one — the dev VPC's security groups only allow inbound from the
// tailnet/VPC, so dialing the public IP hangs until it times out.
func TestConnectWiring(t *testing.T) {
	var gotCfg sshx.Config
	conn := &fakeConn{}
	c := New(Config{}, WithRunner((&recorder{}).run), WithLogf(nil),
		WithSSHDialer(func(_ context.Context, cfg sshx.Config) (Conn, error) {
			gotCfg = cfg
			return conn, nil
		}))

	got, err := c.Connect(context.Background(), Instance{ID: "i-1", PublicIP: "52.1.2.3", PrivateIP: "10.9.0.1"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got != conn {
		t.Error("Connect returned a different conn")
	}
	if gotCfg.Host != "10.9.0.1" {
		t.Errorf("dial Host = %q, want the private IP (default reach)", gotCfg.Host)
	}
	if gotCfg.User != DefaultSSHUser {
		t.Errorf("dial User = %q, want %q", gotCfg.User, DefaultSSHUser)
	}
	if gotCfg.KeyPath != DefaultKeyFile {
		t.Errorf("dial KeyPath = %q, want %q", gotCfg.KeyPath, DefaultKeyFile)
	}
}

// TestConnectFallsBackToPublicWhenPrivateEmpty covers a describe race (private
// IP not yet populated): Connect must not hard-fail, it falls back to public.
func TestConnectFallsBackToPublicWhenPrivateEmpty(t *testing.T) {
	var gotCfg sshx.Config
	conn := &fakeConn{}
	c := New(Config{}, WithRunner((&recorder{}).run), WithLogf(nil),
		WithSSHDialer(func(_ context.Context, cfg sshx.Config) (Conn, error) {
			gotCfg = cfg
			return conn, nil
		}))
	if _, err := c.Connect(context.Background(), Instance{ID: "i-1", PublicIP: "52.1.2.3"}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if gotCfg.Host != "52.1.2.3" {
		t.Errorf("dial Host = %q, want fallback to public IP", gotCfg.Host)
	}
}

// TestConnectReachPublicForcesPublicIP covers the explicit opt-out.
func TestConnectReachPublicForcesPublicIP(t *testing.T) {
	var gotCfg sshx.Config
	conn := &fakeConn{}
	c := New(Config{Reach: "public"}, WithRunner((&recorder{}).run), WithLogf(nil),
		WithSSHDialer(func(_ context.Context, cfg sshx.Config) (Conn, error) {
			gotCfg = cfg
			return conn, nil
		}))
	if _, err := c.Connect(context.Background(), Instance{ID: "i-1", PublicIP: "52.1.2.3", PrivateIP: "10.9.0.1"}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if gotCfg.Host != "52.1.2.3" {
		t.Errorf("dial Host = %q, want the public IP (reach=public)", gotCfg.Host)
	}
}

func TestConnectNoReachableIP(t *testing.T) {
	c := New(Config{}, WithRunner((&recorder{}).run), WithLogf(nil))
	if _, err := c.Connect(context.Background(), Instance{ID: "i-1"}); err == nil {
		t.Fatal("expected error connecting to instance with no reachable IP")
	}
}

// TestWaitSSHProbesEachInstance is the same reach-bug regression for WaitSSH:
// it must probe the PRIVATE IP by default, not the public one.
func TestWaitSSHProbesEachInstance(t *testing.T) {
	var mu sync.Mutex
	var probed []string
	c := New(Config{}, WithRunner((&recorder{}).run), WithLogf(nil),
		WithPortProbe(func(_ context.Context, host string, port int) error {
			mu.Lock()
			probed = append(probed, fmt.Sprintf("%s:%d", host, port))
			mu.Unlock()
			return nil
		}))
	instances := []Instance{
		{ID: "i-1", PublicIP: "1.1.1.1", PrivateIP: "10.9.0.1"},
		{ID: "i-2", PublicIP: "2.2.2.2", PrivateIP: "10.9.0.2"},
	}
	if err := c.WaitSSH(context.Background(), instances, "/key.pem"); err != nil {
		t.Fatalf("WaitSSH: %v", err)
	}
	if strings.Join(probed, ",") != "10.9.0.1:22,10.9.0.2:22" {
		t.Errorf("probed = %v, want each private IP on :22", probed)
	}
}

// TestWaitSSHReachPublicProbesPublicIP covers the explicit opt-out.
func TestWaitSSHReachPublicProbesPublicIP(t *testing.T) {
	var probed []string
	c := New(Config{Reach: "public"}, WithRunner((&recorder{}).run), WithLogf(nil),
		WithPortProbe(func(_ context.Context, host string, port int) error {
			probed = append(probed, fmt.Sprintf("%s:%d", host, port))
			return nil
		}))
	instances := []Instance{{ID: "i-1", PublicIP: "1.1.1.1", PrivateIP: "10.9.0.1"}}
	if err := c.WaitSSH(context.Background(), instances, "/key.pem"); err != nil {
		t.Fatalf("WaitSSH: %v", err)
	}
	if strings.Join(probed, ",") != "1.1.1.1:22" {
		t.Errorf("probed = %v, want the public IP (reach=public)", probed)
	}
}

func TestWaitSSHRequiresKeyFile(t *testing.T) {
	c := New(Config{}, WithRunner((&recorder{}).run), WithLogf(nil))
	if err := c.WaitSSH(context.Background(), []Instance{{PrivateIP: "10.9.0.1"}}, ""); err == nil {
		t.Fatal("expected error with empty key file")
	}
}

// TestWaitSSHFailsFastWithinTimeout is the cost-safety regression: a host that
// never becomes reachable must return an error within SSHWaitTimeout, not hang
// for the caller's (much longer) ctx deadline.
func TestWaitSSHFailsFastWithinTimeout(t *testing.T) {
	c := New(Config{SSHWaitTimeout: 50 * time.Millisecond}, WithRunner((&recorder{}).run), WithLogf(nil),
		WithPortProbe(func(_ context.Context, _ string, _ int) error {
			return fmt.Errorf("connection refused")
		}))
	// A long-lived parent ctx: if WaitSSH obeyed only ctx, it would hang here.
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	start := time.Now()
	err := c.WaitSSH(ctx, []Instance{{ID: "i-1", PrivateIP: "10.9.0.1"}}, "/key.pem")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected WaitSSH to fail for a never-reachable host")
	}
	if elapsed > 2*time.Second {
		t.Errorf("WaitSSH took %v, want it bounded by its own ~50ms SSHWaitTimeout", elapsed)
	}
}
