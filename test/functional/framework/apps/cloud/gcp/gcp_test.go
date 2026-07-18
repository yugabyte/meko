package gcp

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

// recorder is a scripted fake gcloud runner: it records every argv and answers
// via an optional respond hook (call index + argv → result).
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

// createJSON is a canned `instances create --format=json` output (a JSON
// array, even for a single created instance).
func createJSON(n int) string {
	return fmt.Sprintf(`[{"name":"gcp-test-%03d","status":"PROVISIONING","networkInterfaces":[{"networkIP":"10.128.0.%d","accessConfigs":[{"natIP":"34.0.0.%d"}]}]}]`, n, n, n)
}

func subcommand(argv []string) string {
	// argv = ["gcloud","compute","instances","<verb>",...]
	if len(argv) >= 4 {
		return argv[3]
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
		if subcommand(argv) == "create" {
			return execpkg.Result{ExitCode: 0, Stdout: createJSON(call)}, nil
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
	if instances[0].ID != "gcp-test-000" || instances[2].ID != "gcp-test-002" {
		t.Errorf("instance ids = %v", instances)
	}
	if instances[0].PublicIP != "34.0.0.0" || instances[0].PrivateIP != "10.128.0.0" {
		t.Errorf("instance[0] IPs = %+v", instances[0])
	}

	calls := rec.recorded()
	if len(calls) != 3 {
		t.Fatalf("got %d gcloud calls, want 3 (one instances-create per instance)", len(calls))
	}

	for i, argv := range calls {
		if argv[0] != "gcloud" || argv[1] != "compute" || argv[2] != "instances" || subcommand(argv) != "create" {
			t.Fatalf("call %d not a compute instances create: %v", i, argv)
		}
		if got := argVal(argv, "--project"); got != DefaultProject {
			t.Errorf("call %d --project = %q, want %q", i, got, DefaultProject)
		}
		if got := argVal(argv, "--zone"); got != DefaultZone {
			t.Errorf("call %d --zone = %q, want %q", i, got, DefaultZone)
		}
		if got := argVal(argv, "--network"); got != DefaultNetwork {
			t.Errorf("call %d --network = %q, want %q", i, got, DefaultNetwork)
		}
		if got := argVal(argv, "--subnet"); got != DefaultSubnetwork {
			t.Errorf("call %d --subnet = %q, want %q", i, got, DefaultSubnetwork)
		}
		if got := argVal(argv, "--machine-type"); got != DefaultMachineType {
			t.Errorf("call %d --machine-type = %q, want %q", i, got, DefaultMachineType)
		}

		// Mandatory labels all present, run tag included.
		labels := argVal(argv, "--labels")
		for _, want := range []string{
			"yb_owner=" + DefaultOwner,
			"yb_task=" + labelTask,
			"yb_dept=" + labelDept,
			"autoclean=true",
			"ampqa=1",
			"ampqa-run=abc123",
		} {
			if !strings.Contains(labels, want) {
				t.Errorf("call %d labels missing %q in %q", i, want, labels)
			}
		}
	}
}

func TestRunInstancesImageSelection(t *testing.T) {
	newClient := func(cfg Config) (*recorder, *Client) {
		rec := &recorder{respond: func(call int, argv []string) (execpkg.Result, error) {
			if subcommand(argv) == "create" {
				return execpkg.Result{ExitCode: 0, Stdout: createJSON(call)}, nil
			}
			return execpkg.Result{ExitCode: 0}, nil
		}}
		return rec, New(cfg, WithRunner(rec.run), WithLogf(nil))
	}

	// A family resolves via --image-family; a specific image via --image; and a
	// family wins when both are set (a family is always the current image).
	cases := []struct {
		name                        string
		cfg                         Config
		wantFamily, wantImage, none bool
	}{
		{"family", Config{ImageFamily: "almalinux-8", ImageProject: "almalinux-cloud"}, true, false, false},
		{"image", Config{Image: "almalinux-8-v20260706", ImageProject: "almalinux-cloud"}, false, true, false},
		{"family-wins", Config{ImageFamily: "almalinux-8", Image: "almalinux-8-v20260706"}, true, false, false},
		{"neither", Config{}, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, c := newClient(tc.cfg)
			if _, err := c.RunInstances(context.Background(), RunSpec{Count: 1, RunTag: "img"}); err != nil {
				t.Fatalf("RunInstances: %v", err)
			}
			argv := rec.recorded()[0]
			hasFamily := argHas(argv, "--image-family")
			hasImage := argHas(argv, "--image")
			if hasFamily != tc.wantFamily {
				t.Errorf("--image-family present=%v, want %v (argv %v)", hasFamily, tc.wantFamily, argv)
			}
			if hasImage != tc.wantImage {
				t.Errorf("--image present=%v, want %v (argv %v)", hasImage, tc.wantImage, argv)
			}
			if tc.none && (hasFamily || hasImage) {
				t.Errorf("expected no image flags with neither set: %v", argv)
			}
		})
	}
}

// TestNeverMintsCredentials is the core hygiene guarantee: across every
// lifecycle operation, this app never mints an SSH key (OS Login create/add),
// a fresh service-account key, or a new service account — it only ever reuses
// what's already configured.
func TestNeverMintsCredentials(t *testing.T) {
	rec := &recorder{}
	c := newRunClient(t, rec)
	if _, err := c.RunInstances(context.Background(), RunSpec{Count: 2, RunTag: "t"}); err != nil {
		t.Fatalf("RunInstances: %v", err)
	}
	_ = c.WaitRunning(context.Background(), []string{"gcp-test-000"})
	_ = c.Terminate(context.Background(), []string{"gcp-test-000"})
	_ = c.ReapByTag(context.Background(), "t")

	badSubstrings := []string{
		"os-login ssh-keys create",
		"os-login ssh-keys add",
		"compute os-login",
		"iam service-accounts keys create",
		"iam service-accounts create",
	}
	for _, argv := range rec.recorded() {
		if argv[0] != "gcloud" {
			t.Fatalf("non-gcloud command shelled: %v", argv)
		}
		joined := strings.Join(argv, " ")
		for _, bad := range badSubstrings {
			if strings.Contains(joined, bad) {
				t.Fatalf("credential-minting command issued (hygiene violation): %v", argv)
			}
		}
	}
}

// TestRunInstancesInjectsExistingPublicKey covers SSH-key reuse: when
// public_key_file is configured, RunInstances must inject its CONTENT (never
// generate a new key) into instance metadata as "<ssh_user>:<content>".
func TestRunInstancesInjectsExistingPublicKey(t *testing.T) {
	rec := &recorder{}
	rec.respond = func(call int, argv []string) (execpkg.Result, error) {
		if subcommand(argv) == "create" {
			return execpkg.Result{ExitCode: 0, Stdout: createJSON(call)}, nil
		}
		return execpkg.Result{ExitCode: 0}, nil
	}
	var readPath string
	c := New(Config{PublicKeyFile: "~/.ssh/shared-ampqa.pub"}, WithRunner(rec.run), WithLogf(nil),
		WithPubKeyReader(func(path string) (string, error) {
			readPath = path
			return "ssh-rsa AAAAB3NzaC1yc2EEXISTINGKEY comment\n", nil
		}))
	if _, err := c.RunInstances(context.Background(), RunSpec{Count: 1, RunTag: "key1"}); err != nil {
		t.Fatalf("RunInstances: %v", err)
	}
	if strings.HasPrefix(readPath, "~") {
		t.Errorf("public key path not expanded: %q", readPath)
	}
	argv := rec.recorded()[0]
	meta := argVal(argv, "--metadata")
	want := "ssh-keys=" + DefaultSSHUser + ":ssh-rsa AAAAB3NzaC1yc2EEXISTINGKEY comment"
	if meta != want {
		t.Errorf("--metadata = %q, want %q", meta, want)
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
		if subcommand(argv) == "create" {
			if call == 1 {
				return execpkg.Result{ExitCode: 1, Stderr: "boom"}, nil
			}
			return execpkg.Result{ExitCode: 0, Stdout: createJSON(call)}, nil
		}
		return execpkg.Result{ExitCode: 0}, nil
	}
	c := New(Config{}, WithRunner(rec.run), WithLogf(nil))

	if _, err := c.RunInstances(context.Background(), RunSpec{Count: 3, RunTag: "roll"}); err == nil {
		t.Fatal("expected error on second launch failure")
	}
	var terminated bool
	for _, argv := range rec.recorded() {
		if subcommand(argv) == "delete" && argHas(argv, "gcp-test-000") {
			terminated = true
		}
	}
	if !terminated {
		t.Error("first instance was not terminated on rollback — leak")
	}
}

func TestWaitRunningPollsUntilRunning(t *testing.T) {
	rec := &recorder{}
	statuses := []string{"PROVISIONING", "STAGING", "RUNNING"}
	rec.respond = func(call int, argv []string) (execpkg.Result, error) {
		st := statuses[call]
		if call >= len(statuses)-1 {
			st = "RUNNING"
		}
		return execpkg.Result{ExitCode: 0, Stdout: fmt.Sprintf(`{"name":"i-1","status":%q}`, st)}, nil
	}
	c := New(Config{}, WithRunner(rec.run), WithLogf(nil))
	if err := c.WaitRunning(context.Background(), []string{"i-1"}); err != nil {
		t.Fatalf("WaitRunning: %v", err)
	}
	calls := rec.recorded()
	if len(calls) == 0 || subcommand(calls[0]) != "describe" {
		t.Fatalf("calls = %v, want describe calls", calls)
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
	rec := &recorder{}
	c := New(Config{}, WithRunner(rec.run), WithLogf(nil))
	if err := c.Terminate(context.Background(), nil); err != nil {
		t.Fatalf("Terminate(nil): %v", err)
	}
	if len(rec.recorded()) != 0 {
		t.Errorf("empty Terminate issued a call")
	}

	rec2 := &recorder{respond: func(_ int, _ []string) (execpkg.Result, error) {
		return execpkg.Result{ExitCode: 1, Stderr: "ERROR: (gcloud.compute.instances.delete) Could not fetch resource: - The resource ... was not found"}, nil
	}}
	c2 := New(Config{}, WithRunner(rec2.run), WithLogf(nil))
	if err := c2.Terminate(context.Background(), []string{"gone"}); err != nil {
		t.Errorf("Terminate of gone instance should be idempotent, got %v", err)
	}

	rec3 := &recorder{respond: func(_ int, _ []string) (execpkg.Result, error) {
		return execpkg.Result{ExitCode: 1, Stderr: "PERMISSION_DENIED"}, nil
	}}
	c3 := New(Config{}, WithRunner(rec3.run), WithLogf(nil))
	if err := c3.Terminate(context.Background(), []string{"i-x"}); err == nil {
		t.Error("expected error on genuine terminate failure")
	}
}

func TestReapByTagFiltersAndTerminates(t *testing.T) {
	rec := &recorder{}
	rec.respond = func(_ int, argv []string) (execpkg.Result, error) {
		if subcommand(argv) == "list" {
			return execpkg.Result{ExitCode: 0, Stdout: `[{"name":"i-a","status":"RUNNING","zone":"https://www.googleapis.com/compute/v1/projects/p/zones/us-west1-a"},{"name":"i-b","status":"RUNNING","zone":"https://www.googleapis.com/compute/v1/projects/p/zones/us-west1-a"}]`}, nil
		}
		return execpkg.Result{ExitCode: 0}, nil
	}
	c := New(Config{Zone: "us-west1-b"}, WithRunner(rec.run), WithLogf(nil))
	if err := c.ReapByTag(context.Background(), "Run-42"); err != nil {
		t.Fatalf("ReapByTag: %v", err)
	}
	calls := rec.recorded()
	if subcommand(calls[0]) != "list" {
		t.Fatalf("first call = %v, want instances list", calls[0])
	}
	if got := argVal(calls[0], "--filter"); !strings.Contains(got, "labels.ampqa-run=run-42") {
		t.Errorf("list not filtered by run tag: %q", got)
	}
	// Both nodes are in us-west1-a — a DIFFERENT zone than the configured
	// us-west1-b — so the delete must target their ACTUAL zone, not the
	// configured one (the leak the region-wide reap fixes).
	last := calls[len(calls)-1]
	if subcommand(last) != "delete" || !argHas(last, "i-a") || !argHas(last, "i-b") {
		t.Errorf("delete call = %v, want i-a and i-b", last)
	}
	if got := argVal(last, "--zone"); got != "us-west1-a" {
		t.Errorf("delete --zone = %q, want the instances' actual zone us-west1-a", got)
	}
}

// TestReapByTagDeletesPerActualZone is the regression guard for the leak that
// motivated the region-wide reap: a YBA GCP-provider universe spreads its nodes
// across the region, so reap must delete EACH node in its own zone — never the
// single configured zone — or a node in another zone silently survives (a
// wrong-zone delete returns "was not found", which is swallowed as success).
func TestReapByTagDeletesPerActualZone(t *testing.T) {
	rec := &recorder{}
	rec.respond = func(_ int, argv []string) (execpkg.Result, error) {
		if subcommand(argv) == "list" {
			return execpkg.Result{ExitCode: 0, Stdout: `[{"name":"i-a","status":"RUNNING","zone":"https://www.googleapis.com/compute/v1/projects/p/zones/us-west1-a"},{"name":"i-c","status":"RUNNING","zone":"us-west1-c"}]`}, nil
		}
		return execpkg.Result{ExitCode: 0}, nil
	}
	c := New(Config{Zone: "us-west1-b"}, WithRunner(rec.run), WithLogf(nil))
	if err := c.ReapByTag(context.Background(), "run-x"); err != nil {
		t.Fatalf("ReapByTag: %v", err)
	}
	// One delete per zone, each targeting the node's own zone; none uses the
	// configured us-west1-b (no node lives there).
	deletes := map[string]string{} // instance id → --zone it was deleted in
	for _, call := range rec.recorded() {
		if subcommand(call) != "delete" {
			continue
		}
		z := argVal(call, "--zone")
		if z == "us-west1-b" {
			t.Errorf("delete used the configured zone us-west1-b, no node lives there: %v", call)
		}
		for _, id := range []string{"i-a", "i-c"} {
			if argHas(call, id) {
				deletes[id] = z
			}
		}
	}
	if deletes["i-a"] != "us-west1-a" {
		t.Errorf("i-a deleted in %q, want us-west1-a", deletes["i-a"])
	}
	if deletes["i-c"] != "us-west1-c" {
		t.Errorf("i-c deleted in %q (bare-name zone), want us-west1-c", deletes["i-c"])
	}
}

func TestReapStaleFiltersByAge(t *testing.T) {
	old := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	fresh := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	rec := &recorder{}
	rec.respond = func(_ int, argv []string) (execpkg.Result, error) {
		if subcommand(argv) == "list" {
			return execpkg.Result{ExitCode: 0, Stdout: fmt.Sprintf(
				`[{"name":"i-old","creationTimestamp":%q,"status":"RUNNING"},{"name":"i-new","creationTimestamp":%q,"status":"RUNNING"}]`,
				old, fresh)}, nil
		}
		return execpkg.Result{ExitCode: 0}, nil
	}
	c := New(Config{}, WithRunner(rec.run), WithLogf(nil))
	if err := c.ReapStale(context.Background(), time.Hour); err != nil {
		t.Fatalf("ReapStale: %v", err)
	}
	last := rec.recorded()[len(rec.recorded())-1]
	if subcommand(last) != "delete" {
		t.Fatalf("last call = %v, want delete", last)
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

// TestConnectWiring is the reach regression: with the default config (reach
// unset → "private"), Connect MUST dial the internal IP, never the external
// one.
func TestConnectWiring(t *testing.T) {
	var gotCfg sshx.Config
	conn := &fakeConn{}
	c := New(Config{}, WithRunner((&recorder{}).run), WithLogf(nil),
		WithSSHDialer(func(_ context.Context, cfg sshx.Config) (Conn, error) {
			gotCfg = cfg
			return conn, nil
		}))

	got, err := c.Connect(context.Background(), Instance{ID: "i-1", PublicIP: "34.1.2.3", PrivateIP: "10.128.0.1"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got != conn {
		t.Error("Connect returned a different conn")
	}
	if gotCfg.Host != "10.128.0.1" {
		t.Errorf("dial Host = %q, want the private IP (default reach)", gotCfg.Host)
	}
	if gotCfg.User != DefaultSSHUser {
		t.Errorf("dial User = %q, want %q", gotCfg.User, DefaultSSHUser)
	}
}

func TestConnectReachPublicForcesPublicIP(t *testing.T) {
	var gotCfg sshx.Config
	conn := &fakeConn{}
	c := New(Config{Reach: "public"}, WithRunner((&recorder{}).run), WithLogf(nil),
		WithSSHDialer(func(_ context.Context, cfg sshx.Config) (Conn, error) {
			gotCfg = cfg
			return conn, nil
		}))
	if _, err := c.Connect(context.Background(), Instance{ID: "i-1", PublicIP: "34.1.2.3", PrivateIP: "10.128.0.1"}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if gotCfg.Host != "34.1.2.3" {
		t.Errorf("dial Host = %q, want the public IP (reach=public)", gotCfg.Host)
	}
}

func TestConnectNoReachableIP(t *testing.T) {
	c := New(Config{}, WithRunner((&recorder{}).run), WithLogf(nil))
	if _, err := c.Connect(context.Background(), Instance{ID: "i-1"}); err == nil {
		t.Fatal("expected error connecting to instance with no reachable IP")
	}
}

// TestWaitSSHProbesEachInstance is the reach regression for WaitSSH: it must
// probe AND auth-handshake the internal IP by default, not the external one.
func TestWaitSSHProbesEachInstance(t *testing.T) {
	var mu sync.Mutex
	var probed, dialed []string
	c := New(Config{}, WithRunner((&recorder{}).run), WithLogf(nil),
		WithPortProbe(func(_ context.Context, host string, port int) error {
			mu.Lock()
			probed = append(probed, fmt.Sprintf("%s:%d", host, port))
			mu.Unlock()
			return nil
		}),
		WithSSHDialer(func(_ context.Context, cfg sshx.Config) (Conn, error) {
			mu.Lock()
			dialed = append(dialed, fmt.Sprintf("%s@%s:%s", cfg.User, cfg.Host, cfg.KeyPath))
			mu.Unlock()
			return &fakeConn{}, nil
		}))
	instances := []Instance{
		{ID: "i-1", PublicIP: "1.1.1.1", PrivateIP: "10.128.0.1"},
		{ID: "i-2", PublicIP: "2.2.2.2", PrivateIP: "10.128.0.2"},
	}
	if err := c.WaitSSH(context.Background(), instances, "/key.pem"); err != nil {
		t.Fatalf("WaitSSH: %v", err)
	}
	if strings.Join(probed, ",") != "10.128.0.1:22,10.128.0.2:22" {
		t.Errorf("probed = %v, want each private IP on :22", probed)
	}
	// The auth gate must dial each private IP with the configured user + key.
	want := DefaultSSHUser + "@10.128.0.1:/key.pem," + DefaultSSHUser + "@10.128.0.2:/key.pem"
	if strings.Join(dialed, ",") != want {
		t.Errorf("dialed = %v, want %v", dialed, want)
	}
}

// TestWaitSSHWaitsForAuthNotJustOpenPort is the GCE key-provisioning regression:
// an open :22 is not enough — WaitSSH must keep retrying until the auth
// handshake succeeds (the guest agent installs the metadata key after sshd
// opens), then return.
func TestWaitSSHWaitsForAuthNotJustOpenPort(t *testing.T) {
	var mu sync.Mutex
	dials := 0
	c := New(Config{}, WithRunner((&recorder{}).run), WithLogf(nil),
		WithPortProbe(func(context.Context, string, int) error { return nil }),
		WithSSHDialer(func(_ context.Context, _ sshx.Config) (Conn, error) {
			mu.Lock()
			defer mu.Unlock()
			dials++
			if dials < 3 {
				return nil, fmt.Errorf("ssh: unable to authenticate")
			}
			return &fakeConn{}, nil
		}))
	if err := c.WaitSSH(context.Background(), []Instance{{ID: "i-1", PrivateIP: "10.128.0.1"}}, "/key.pem"); err != nil {
		t.Fatalf("WaitSSH: %v", err)
	}
	if dials != 3 {
		t.Errorf("auth dials = %d, want 3 (retried past the not-yet-authable window)", dials)
	}
}

func TestWaitSSHRequiresKeyFile(t *testing.T) {
	c := New(Config{}, WithRunner((&recorder{}).run), WithLogf(nil))
	if err := c.WaitSSH(context.Background(), []Instance{{PrivateIP: "10.128.0.1"}}, ""); err == nil {
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	start := time.Now()
	err := c.WaitSSH(ctx, []Instance{{ID: "i-1", PrivateIP: "10.128.0.1"}}, "/key.pem")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected WaitSSH to fail for a never-reachable host")
	}
	if elapsed > 2*time.Second {
		t.Errorf("WaitSSH took %v, want it bounded by its own ~50ms SSHWaitTimeout", elapsed)
	}
}

func TestAuthEnvInjection(t *testing.T) {
	if env := (Config{}).authEnv(); env != nil {
		t.Errorf("no service account configured should inject nothing, got %v", env)
	}
	env := Config{ServiceAccountKeyFile: "/tmp/sa.json"}.authEnv()
	joined := strings.Join(env, " ")
	if !strings.Contains(joined, "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE=/tmp/sa.json") {
		t.Errorf("authEnv = %v, want CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE", env)
	}
}

func TestSanitizeGCENameAndLabelValue(t *testing.T) {
	if got := sanitizeGCEName("TestOwner-AmpQA_Run-1"); got != "testowner-ampqa-run-1" {
		t.Errorf("sanitizeGCEName = %q", got)
	}
	if got := sanitizeGCEName("123-bad-start"); !strings.HasPrefix(got, "i-") {
		t.Errorf("sanitizeGCEName of a digit-leading string = %q, want an i- prefix", got)
	}
	if got := sanitizeLabelValue("Run/42.test"); got != "run_42_test" {
		t.Errorf("sanitizeLabelValue = %q", got)
	}
}
