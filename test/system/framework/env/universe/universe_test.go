package universe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/yba"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/ybastack"
	"github.com/yugabyte/yb-amp/test/system/framework/core"
	"github.com/yugabyte/yb-amp/test/system/framework/exec"
)

// --- attach_yba ---

func fakeYBAServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/customers/c1/universes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]any{
			map[string]any{
				"universeUUID": "u-1",
				"name":         "prod-uni",
				"universeDetails": map[string]any{
					"clusters": []any{map[string]any{"userIntent": map[string]any{"ybSoftwareVersion": "2025.1.0.0-b9"}}},
					"nodeDetailsSet": []any{
						map[string]any{"nodeName": "n1", "isTserver": true, "state": "Live", "ysqlServerRpcPort": 5433, "cloudInfo": map[string]any{"private_ip": "10.1.0.1"}},
						map[string]any{"nodeName": "n2", "isTserver": true, "state": "Live", "ysqlServerRpcPort": 5433, "cloudInfo": map[string]any{"private_ip": "10.1.0.2"}},
					},
				},
			},
		})
	})
	return httptest.NewServer(mux)
}

// providerWithYBA wires the provider to hit srv with the test http client.
func providerWithYBA(cfg Config, srv *httptest.Server) *Provider {
	p := New(cfg)
	p.newYBA = func(c Config) *yba.Client {
		return yba.New(srv.URL, c.YBACustomer, c.YBAToken,
			yba.WithHTTPClient(srv.Client()),
			yba.WithPoll(time.Millisecond, 2*time.Millisecond))
	}
	return p
}

func TestAttachYBAExposesConnInfo(t *testing.T) {
	srv := fakeYBAServer(t)
	defer srv.Close()
	cfg := Config{Mode: ModeAttachYBA, YBAURL: srv.URL, YBACustomer: "c1", YBAToken: "tok", UniverseRef: "prod-uni"}
	p := providerWithYBA(cfg, srv)

	env, err := p.Provision(context.Background(), core.EnvSpec{Shape: core.Universe}, nil)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	acc, ok := env.(interface{ PGEndpoint() pg.ConnInfo })
	if !ok {
		t.Fatalf("env does not expose PGEndpoint")
	}
	ci := acc.PGEndpoint()
	if ci.Host != "10.1.0.1" || ci.Port != 5433 {
		t.Errorf("conn = %s:%d, want 10.1.0.1:5433", ci.Host, ci.Port)
	}
	if env.Versions().YB != "2025.1.0.0-b9" {
		t.Errorf("YB version = %q", env.Versions().YB)
	}
	if env.ControllerURL() != "" {
		t.Errorf("ControllerURL = %q, want empty", env.ControllerURL())
	}
	if e := env.(*Env); len(e.NodeAddrs()) != 2 {
		t.Errorf("node addrs = %v, want 2", e.NodeAddrs())
	}
}

func TestAttachYBATeardownNoOpUnlessOwned(t *testing.T) {
	srv := fakeYBAServer(t)
	defer srv.Close()

	// Not owned → teardown must not call DELETE.
	var deleted bool
	// Wrap: add a delete handler that flips the flag; if hit, the no-op contract broke.
	mux := srv.Config.Handler.(*http.ServeMux)
	mux.HandleFunc("/api/v1/customers/c1/universes/u-1", func(w http.ResponseWriter, r *http.Request) {
		deleted = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"taskUUID": "t1"})
	})

	cfg := Config{Mode: ModeAttachYBA, YBAURL: srv.URL, YBACustomer: "c1", YBAToken: "tok", UniverseRef: "prod-uni", Owned: false}
	p := providerWithYBA(cfg, srv)
	env, err := p.Provision(context.Background(), core.EnvSpec{}, nil)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := p.Teardown(context.Background(), env); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if deleted {
		t.Errorf("unowned universe must not be deleted on teardown")
	}
}

func TestAttachYBATeardownDeletesWhenOwned(t *testing.T) {
	srv := fakeYBAServer(t)
	defer srv.Close()
	var deleted bool
	mux := srv.Config.Handler.(*http.ServeMux)
	mux.HandleFunc("/api/v1/customers/c1/universes/u-1", func(w http.ResponseWriter, r *http.Request) {
		deleted = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"taskUUID": "t1"})
	})
	mux.HandleFunc("/api/v1/customers/c1/tasks/t1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": yba.TaskSuccess})
	})

	cfg := Config{Mode: ModeAttachYBA, YBAURL: srv.URL, YBACustomer: "c1", YBAToken: "tok", UniverseRef: "prod-uni", Owned: true}
	p := providerWithYBA(cfg, srv)
	env, err := p.Provision(context.Background(), core.EnvSpec{}, nil)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := p.Teardown(context.Background(), env); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !deleted {
		t.Errorf("owned universe must be deleted on teardown")
	}
}

// --- attach_address ---

func TestAttachAddressStaticTarget(t *testing.T) {
	cfg := Config{Mode: ModeAttachAddress, Addresses: []string{"db1.example:5433", "db2.example"}}
	p := New(cfg)
	env, err := p.Provision(context.Background(), core.EnvSpec{}, nil)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	e := env.(*Env)
	ci := e.PGEndpoint()
	if ci.Host != "db1.example" || ci.Port != 5433 {
		t.Errorf("conn = %s:%d", ci.Host, ci.Port)
	}
	// Bare host gets the default port.
	if got := e.NodeAddrs()[1]; got != "db2.example:5433" {
		t.Errorf("addr[1] = %q, want db2.example:5433", got)
	}
	// Teardown is always a no-op (must not error).
	if err := p.Teardown(context.Background(), env); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if env.Exec() != nil {
		t.Errorf("attach_address must not expose an executor")
	}
}

// --- provision_yugabyted ---

// scriptExec is a scripted fake RemoteExecutor that also satisfies the
// transport interface (Close).
type scriptExec struct {
	mu       sync.Mutex
	host     string
	commands []string
	status   string
	closed   bool
}

func (s *scriptExec) Run(_ context.Context, cmd string) (exec.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, cmd)
	if strings.Contains(cmd, "status") {
		return exec.Result{ExitCode: 0, Stdout: s.status}, nil
	}
	return exec.Result{ExitCode: 0}, nil
}

func (s *scriptExec) Copy(context.Context, string, string) error { return nil }
func (s *scriptExec) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *scriptExec) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.commands))
	copy(out, s.commands)
	return out
}

// findContains returns the first recorded command containing sub, or "".
func findContains(cmds []string, sub string) string {
	for _, c := range cmds {
		if strings.Contains(c, sub) {
			return c
		}
	}
	return ""
}

func TestProvisionYugabytedStartAndDestroy(t *testing.T) {
	execs := map[string]*scriptExec{}
	cfg := Config{
		Mode:       ModeProvisionYugabyted,
		Hosts:      []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
		InstallDir: "/opt/yb",
		RF:         3,
	}
	p := New(cfg)
	p.dial = func(_ context.Context, _ Config, host string) (transport, error) {
		s := &scriptExec{host: host, status: "Status: Running"}
		execs[host] = s
		return s, nil
	}
	p.waitPG = func(context.Context, pg.ConnInfo, time.Duration) error { return nil }
	p.detectVer = func(context.Context, pg.ConnInfo) string { return "2.99.0.0-test" }

	env, err := p.Provision(context.Background(), core.EnvSpec{Shape: core.Universe}, nil)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	// Bootstrap node 0 starts without --join; nodes 1,2 join it. (Start may
	// run a post_install probe/command ahead of the actual `yugabyted start`,
	// so search rather than assume it's cmds[0].)
	c0 := execs["10.0.0.1"].recorded()
	startCmd := findContains(c0, "start")
	if startCmd == "" || strings.Contains(startCmd, "--join") {
		t.Errorf("node0 start command = %v", c0)
	}
	for _, h := range []string{"10.0.0.2", "10.0.0.3"} {
		c := execs[h].recorded()
		if findContains(c, "--join='10.0.0.1'") == "" {
			t.Errorf("node %s commands = %v, want a --join='10.0.0.1' command", h, c)
		}
	}
	if e := env.(*Env); e.Exec() == nil {
		t.Errorf("provisioned env must expose an executor")
	}

	if err := p.Teardown(context.Background(), env); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	// Destroy stops+wipes and transports close.
	for h, s := range execs {
		if !s.closed {
			t.Errorf("transport for %s not closed on teardown", h)
		}
		var sawStop, sawWipe bool
		for _, c := range s.recorded() {
			if strings.Contains(c, "stop") {
				sawStop = true
			}
			if strings.Contains(c, "rm -rf") {
				sawWipe = true
			}
		}
		if !sawStop || !sawWipe {
			t.Errorf("node %s: stop=%v wipe=%v", h, sawStop, sawWipe)
		}
	}
}

// --- provision_yba ---

// ybaEvents records the ordered call log the fake stack + client share, so a
// test can assert LIFO teardown (universe delete BEFORE yba teardown).
type ybaEvents struct {
	mu  sync.Mutex
	log []string
}

func (e *ybaEvents) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, s)
}

func (e *ybaEvents) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.log))
	copy(out, e.log)
	return out
}

// fakeStack is a fake ybaStack: Deploy returns a canned Info (or an error) and
// records deploy/teardown into the shared event log.
type fakeStack struct {
	ev        *ybaEvents
	info      ybastack.Info
	deployErr error
	teardowns int
}

func (f *fakeStack) Deploy(context.Context) (ybastack.Info, error) {
	f.ev.add("deploy")
	if f.deployErr != nil {
		return ybastack.Info{}, f.deployErr
	}
	return f.info, nil
}

func (f *fakeStack) Teardown(context.Context) error {
	f.ev.add("stack.teardown")
	f.teardowns++
	return nil
}

// fakeProvisioner is a fake ybaProvisioner recording the create/delete calls and
// injecting failures at chosen steps.
type fakeProvisioner struct {
	ev             *ybaEvents
	universe       yba.Universe
	universeID     string
	ensureErr      error
	regionErr      error
	instTypeErr    error
	accessKeyErr   error
	releaseErr     error
	createErr      error
	waitErr        error
	readyErr       error
	forceDeleteErr error
	getErr         error
	reapErr        error
	deletes        int
	instTypes      int
	releases       int
	reaps          int
	nsReaps        int
	gceReaps       int
	k8sEnsures     int
	gcpEnsures     int
	readyChecks    int
	ybcSets        int
	ybcEnabled     bool
}

func (f *fakeProvisioner) EnsureAWSProvider(context.Context, yba.ProviderSpec) (string, error) {
	f.ev.add("ensure_provider")
	if f.ensureErr != nil {
		return "", f.ensureErr
	}
	return "prov-1", nil
}

func (f *fakeProvisioner) EnsureK8sProvider(context.Context, yba.K8sProviderSpec) (string, error) {
	f.ev.add("ensure_k8s_provider")
	f.k8sEnsures++
	if f.ensureErr != nil {
		return "", f.ensureErr
	}
	return "prov-k8s", nil
}

func (f *fakeProvisioner) EnsureGCPProvider(context.Context, yba.GCPProviderSpec) (string, error) {
	f.ev.add("ensure_gcp_provider")
	f.gcpEnsures++
	if f.ensureErr != nil {
		return "", f.ensureErr
	}
	return "prov-gcp", nil
}

func (f *fakeProvisioner) SetYbcEnabledForProvider(_ context.Context, _ string, enabled bool) error {
	f.ev.add("set_ybc")
	f.ybcEnabled = enabled
	f.ybcSets++
	return nil
}

func (f *fakeProvisioner) EnsureRelease(context.Context, string, string, string) error {
	f.ev.add("ensure_release")
	f.releases++
	return f.releaseErr
}

func (f *fakeProvisioner) AddInstanceType(context.Context, string, yba.InstanceTypeSpec) error {
	f.ev.add("add_instance_type")
	f.instTypes++
	return f.instTypeErr
}

func (f *fakeProvisioner) ProviderAccessKeyCode(context.Context, string) (string, error) {
	if f.accessKeyErr != nil {
		return "", f.accessKeyErr
	}
	return "yb-dev-aws-2-key", nil
}

func (f *fakeProvisioner) RegionUUIDsForCodes(context.Context, string, ...string) ([]string, error) {
	if f.regionErr != nil {
		return nil, f.regionErr
	}
	return []string{"region-1"}, nil
}

func (f *fakeProvisioner) CreateUniverseFromSpec(context.Context, yba.UniverseSpec) (yba.TaskRef, error) {
	f.ev.add("create_universe")
	if f.createErr != nil {
		return yba.TaskRef{}, f.createErr
	}
	return yba.TaskRef{ResourceUUID: f.universeID, TaskUUID: "t-create"}, nil
}

func (f *fakeProvisioner) WaitTask(context.Context, string) (yba.TaskStatus, error) {
	if f.waitErr != nil {
		return yba.TaskStatus{}, f.waitErr
	}
	return yba.TaskStatus{}, nil
}

func (f *fakeProvisioner) WaitUniverseReady(_ context.Context, _ string, _ time.Duration) error {
	f.ev.add("wait_ready")
	f.readyChecks++
	return f.readyErr
}

func (f *fakeProvisioner) GetUniverse(context.Context, string) (yba.Universe, error) {
	if f.getErr != nil {
		return yba.Universe{}, f.getErr
	}
	return f.universe, nil
}

func (f *fakeProvisioner) ForceDeleteUniverseAndWait(context.Context, string) (yba.TaskRef, error) {
	f.ev.add("delete_universe")
	f.deletes++
	if f.forceDeleteErr != nil {
		return yba.TaskRef{}, f.forceDeleteErr
	}
	return yba.TaskRef{TaskUUID: "t-delete"}, nil
}

// provisionYBAProvider wires a provider for provision_yba with the two fakes and
// trivial spec builders (no disk, no config), so the flow runs fully in-process.
func provisionYBAProvider(st *fakeStack, pr *fakeProvisioner) *Provider {
	p := New(Config{Mode: ModeProvisionYBA, InsecureTLS: true})
	p.logf = nil
	p.newStack = func(string) (ybaStack, error) { return st, nil }
	p.newProvisioner = func(ybastack.Info) ybaProvisioner { return pr }
	p.buildProviderSpec = func() (yba.ProviderSpec, error) { return yba.ProviderSpec{Cloud: "aws", Name: "ampqa-aws"}, nil }
	p.buildUniverseSpec = func() (yba.UniverseSpec, error) {
		return yba.UniverseSpec{Region: "us-west-2", NumNodes: 3}, nil
	}
	// Fake the AWS tag sweep (no real AWS): record it on the shared provisioner.
	p.reapNodes = func(_ context.Context, _, _, _, _ string) error {
		pr.ev.add("reap")
		pr.reaps++
		return pr.reapErr
	}
	// Fake the k8s namespace sweep (no real kubectl): record it too, so a test
	// that flips providerType to kubernetes exercises the k8s backstop.
	p.reapNamespaces = func(_ context.Context, _, _, _ string) error {
		pr.ev.add("reap_ns")
		pr.nsReaps++
		return pr.reapErr
	}
	// Fake the GCE instance sweep (no real gcloud): record it too, so a test that
	// flips providerType to gcp exercises the GCE backstop.
	p.reapGCENodes = func(_ context.Context, _, _, _, _ string) error {
		pr.ev.add("reap_gce")
		pr.gceReaps++
		return pr.reapErr
	}
	return p
}

// provisionGCPYBAProvider wires a provider for the GCP provision_yba path:
// providerType gcp, trivial gcp spec builders, and the two fakes. The GCE
// instance sweep replaces the EC2 tag sweep.
func provisionGCPYBAProvider(st *fakeStack, pr *fakeProvisioner) *Provider {
	p := provisionYBAProvider(st, pr)
	p.providerType = "gcp"
	p.buildGCPProviderSpec = func() (yba.GCPProviderSpec, error) {
		return yba.GCPProviderSpec{
			Name: "ampqa-gcp", Project: "yugabyte", Zone: "us-west1-b",
			CredentialsJSON: `{"type":"service_account"}`, CredentialsFile: "/tmp/sa.json",
			KeyPairName: "yb-dev-gcp", KeyPairContent: "KEY",
			Regions: []yba.RegionSpec{{Code: "us-west1", Zones: []yba.ZoneSpec{{Name: "us-west1-b", Subnet: "subnet-us-west1"}}}},
		}, nil
	}
	p.buildGCPUniverseSpec = func() (yba.UniverseSpec, error) {
		return yba.UniverseSpec{ProviderCode: "gcp", Region: "us-west1", NumNodes: 1, ReplicationFactor: 1, InstanceType: "n2-standard-4", StorageType: "Persistent"}, nil
	}
	return p
}

// TestModeDispatchSelectsGCPProvider: providerType gcp drives the gcp provider
// ensure (not AWS/k8s), imports the DB release (a VM cloud), skips the AWS-only
// instance-type registration (GCP auto-populates its machine-type catalog),
// resolves the SSH access key (a keypair is configured), and confirms the
// universe healthy.
func TestModeDispatchSelectsGCPProvider(t *testing.T) {
	ev := &ybaEvents{}
	st := &fakeStack{ev: ev, info: ybastack.Info{URL: "http://yba", CustomerUUID: "c1", APIToken: "tok", YBImageRegistry: "quay/itest"}}
	pr := &fakeProvisioner{ev: ev, universe: liveUniverse(), universeID: "u-1"}
	p := provisionGCPYBAProvider(st, pr)

	env, err := p.Provision(context.Background(), core.EnvSpec{Shape: core.Universe}, core.NewRunSeed(7, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	e := env.(*Env)
	if e.mode != ModeProvisionYBA || !e.owned {
		t.Errorf("mode/owned = %q/%v", e.mode, e.owned)
	}
	if pr.gcpEnsures != 1 {
		t.Errorf("gcp provider ensures = %d, want 1", pr.gcpEnsures)
	}
	if pr.k8sEnsures != 0 {
		t.Errorf("k8s provider ensures = %d, want 0", pr.k8sEnsures)
	}
	if pr.instTypes != 0 {
		t.Errorf("instance-type registrations = %d, want 0 (GCP auto-populates its machine types)", pr.instTypes)
	}
	if pr.releases != 1 {
		t.Errorf("release imports = %d, want 1 (GCP is a VM cloud)", pr.releases)
	}
	if pr.readyChecks != 1 {
		t.Errorf("universe-ready checks = %d, want 1", pr.readyChecks)
	}
	got := ev.list()
	if indexOf(got, "ensure_gcp_provider") < 0 || indexOf(got, "ensure_provider") >= 0 {
		t.Errorf("order = %v, want ensure_gcp_provider and NOT the AWS ensure_provider", got)
	}
	ri, ei := indexOf(got, "ensure_release"), indexOf(got, "ensure_gcp_provider")
	ci := indexOf(got, "create_universe")
	if !(ri >= 0 && ei >= 0 && ci >= 0 && ri < ci && ei < ci) {
		t.Errorf("order = %v, want ensure_release + ensure_gcp_provider before create_universe", got)
	}
}

// TestProvisionGCPTeardownUsesGCEReap: the gcp teardown force-deletes the
// universe then runs the GCE instance sweep (not the EC2 tag sweep, not the
// namespace sweep) before the YBA is torn down.
func TestProvisionGCPTeardownUsesGCEReap(t *testing.T) {
	ev := &ybaEvents{}
	st := &fakeStack{ev: ev, info: ybastack.Info{URL: "http://yba", CustomerUUID: "c1", APIToken: "tok"}}
	pr := &fakeProvisioner{ev: ev, universe: liveUniverse(), universeID: "u-1"}
	p := provisionGCPYBAProvider(st, pr)

	env, err := p.Provision(context.Background(), core.EnvSpec{Shape: core.Universe}, core.NewRunSeed(7, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := p.Teardown(context.Background(), env); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if pr.deletes != 1 {
		t.Errorf("universe force-deletes = %d, want 1", pr.deletes)
	}
	if pr.gceReaps < 1 {
		t.Errorf("GCE reaps = %d, want >= 1 (the backstop must run)", pr.gceReaps)
	}
	if pr.reaps != 0 || pr.nsReaps != 0 {
		t.Errorf("EC2/namespace sweeps ran (reaps=%d nsReaps=%d), want only the GCE sweep", pr.reaps, pr.nsReaps)
	}
	if st.teardowns != 1 {
		t.Errorf("yba teardowns = %d, want 1", st.teardowns)
	}
	got := ev.list()
	di, gi, ti := indexOf(got, "delete_universe"), indexOf(got, "reap_gce"), indexOf(got, "stack.teardown")
	if !(di >= 0 && gi >= 0 && ti >= 0 && di < gi && gi < ti) {
		t.Errorf("teardown order = %v, want delete_universe → reap_gce → stack.teardown", got)
	}
}

// TestProvisionGCPReapFailureKeepsYBAUp: if the GCE sweep FAILS the YBA is kept
// up (stranding running GCE nodes with no manager is the worst outcome).
func TestProvisionGCPReapFailureKeepsYBAUp(t *testing.T) {
	ev := &ybaEvents{}
	st := &fakeStack{ev: ev, info: ybastack.Info{URL: "http://yba", CustomerUUID: "c1", APIToken: "tok"}}
	pr := &fakeProvisioner{ev: ev, universe: liveUniverse(), universeID: "u-1",
		reapErr: fmt.Errorf("gcloud instances delete failed")}
	p := provisionGCPYBAProvider(st, pr)

	env, err := p.Provision(context.Background(), core.EnvSpec{Shape: core.Universe}, core.NewRunSeed(7, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := p.Teardown(context.Background(), env); err == nil {
		t.Fatalf("expected teardown error when the GCE sweep fails")
	}
	if st.teardowns != 0 {
		t.Errorf("yba teardowns = %d, want 0 (YBA must stay up when the GCE sweep failed)", st.teardowns)
	}
}

// provisionK8sYBAProvider wires a provider for the KUBERNETES provision_yba path:
// providerType kubernetes, trivial k8s spec builders, and the two fakes. The
// namespace backstop replaces the EC2 tag sweep.
func provisionK8sYBAProvider(st *fakeStack, pr *fakeProvisioner) *Provider {
	p := provisionYBAProvider(st, pr)
	p.providerType = "kubernetes"
	p.buildK8sProviderSpec = func() (yba.K8sProviderSpec, error) {
		return yba.K8sProviderSpec{Name: "ampqa-k8s", ImageRegistry: "quay/itest", Regions: []yba.K8sRegionSpec{{Code: "us-west1", Zones: []yba.K8sZoneSpec{{Code: "us-west1-b"}}}}}, nil
	}
	p.buildK8sUniverseSpec = func() (yba.UniverseSpec, error) {
		return yba.UniverseSpec{ProviderCode: "kubernetes", Region: "us-west1", NumNodes: 1, ReplicationFactor: 1, K8sStorageClass: "standard"}, nil
	}
	// Fake the pod-IP resolution (no real kubectl): return tailnet-style pod IPs
	// distinct from the universe's in-cluster private_ip, so a test can assert the
	// k8s backing endpoint is re-targeted at them.
	p.resolveK8sYSQLAddrs = func(_ context.Context, _, _, _ string, port int) ([]string, error) {
		return []string{fmt.Sprintf("100.64.0.1:%d", port), fmt.Sprintf("100.64.0.2:%d", port)}, nil
	}
	return p
}

// TestModeDispatchSelectsK8sProvider: providerType kubernetes drives the k8s
// provider ensure (not AWS), skips the AWS-only instance-type registration and
// DB-release import, and still confirms the universe healthy.
func TestModeDispatchSelectsK8sProvider(t *testing.T) {
	ev := &ybaEvents{}
	st := &fakeStack{ev: ev, info: ybastack.Info{URL: "http://yba", CustomerUUID: "c1", APIToken: "tok", YBImageRegistry: "quay/itest"}}
	pr := &fakeProvisioner{ev: ev, universe: liveUniverse(), universeID: "u-1"}
	p := provisionK8sYBAProvider(st, pr)

	env, err := p.Provision(context.Background(), core.EnvSpec{Shape: core.Universe}, core.NewRunSeed(7, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	e := env.(*Env)
	if e.mode != ModeProvisionYBA || !e.owned {
		t.Errorf("mode/owned = %q/%v", e.mode, e.owned)
	}
	if pr.k8sEnsures != 1 {
		t.Errorf("k8s provider ensures = %d, want 1", pr.k8sEnsures)
	}
	// k8s uses NO instance-type catalog and NO downloaded tarball release.
	if pr.instTypes != 0 {
		t.Errorf("instance-type registrations = %d, want 0 (k8s has no instance types)", pr.instTypes)
	}
	if pr.releases != 0 {
		t.Errorf("release imports = %d, want 0 (k8s pulls its image from the registry)", pr.releases)
	}
	if pr.readyChecks != 1 {
		t.Errorf("universe-ready checks = %d, want 1", pr.readyChecks)
	}
	got := ev.list()
	if indexOf(got, "ensure_k8s_provider") < 0 || indexOf(got, "ensure_provider") >= 0 {
		t.Errorf("order = %v, want ensure_k8s_provider and NOT the AWS ensure_provider", got)
	}
	ei, ci := indexOf(got, "ensure_k8s_provider"), indexOf(got, "create_universe")
	if !(ei >= 0 && ci >= 0 && ei < ci) {
		t.Errorf("order = %v, want ensure_k8s_provider before create_universe", got)
	}
}

// TestProvisionK8sBackingUsesPodIPs: for a kubernetes universe the exposed
// backing endpoint must be the tserver POD IPs (tailnet-routable from the
// operator), NOT the in-cluster private_ip YBA reports in the node details —
// otherwise the framework's operator-side layer-1 workload can't reach it.
func TestProvisionK8sBackingUsesPodIPs(t *testing.T) {
	ev := &ybaEvents{}
	st := &fakeStack{ev: ev, info: ybastack.Info{URL: "http://yba", CustomerUUID: "c1", APIToken: "tok", YBImageRegistry: "quay/itest"}}
	pr := &fakeProvisioner{ev: ev, universe: liveUniverse(), universeID: "u-1"}
	p := provisionK8sYBAProvider(st, pr)

	env, err := p.Provision(context.Background(), core.EnvSpec{Shape: core.Universe}, core.NewRunSeed(7, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	e := env.(*Env)
	// liveUniverse reports in-cluster private_ip 10.2.0.x; the k8s backing must be
	// re-targeted at the faked pod IPs (100.64.0.x) on the universe's YSQL port.
	if e.pg.Host != "100.64.0.1" || e.pg.Port != 5433 {
		t.Errorf("k8s backing endpoint = %s:%d, want a tserver pod IP 100.64.0.1:5433", e.pg.Host, e.pg.Port)
	}
	if strings.HasPrefix(e.pg.Host, "10.2.0.") {
		t.Errorf("k8s backing endpoint %s is the in-cluster private_ip (unreachable over the tailnet)", e.pg.Host)
	}
	want := []string{"100.64.0.1:5433", "100.64.0.2:5433"}
	if strings.Join(e.nodeAddrs, ",") != strings.Join(want, ",") {
		t.Errorf("k8s nodeAddrs = %v, want the pod IPs %v", e.nodeAddrs, want)
	}
}

// TestResolveK8sYSQLAddrs exercises the real pod-IP resolver against a scripted
// kubectl: only namespaces whose name embeds the universe name are searched,
// tserver pods are matched BY NAME (YBA does not stamp app=yb-tserver), master
// pods are excluded, and every tserver pod IP is returned on the YSQL port.
func TestResolveK8sYSQLAddrs(t *testing.T) {
	runner := func(_ context.Context, _ io.Reader, argv []string) (exec.Result, error) {
		joined := strings.Join(argv, " ")
		switch {
		case strings.Contains(joined, "get namespaces"):
			// Only yb-dev-ampqa-uni contains the universe name "ampqa-uni".
			return exec.Result{Stdout: "default yb-dev-ampqa-uni kube-system"}, nil
		case strings.Contains(joined, "get pods") && strings.Contains(joined, "yb-dev-ampqa-uni"):
			return exec.Result{Stdout: "ybampqa-uni-yb-tserver-0 100.64.0.1\n" +
				"ybampqa-uni-yb-tserver-1 100.64.0.2\n" +
				"ybampqa-uni-yb-master-0 100.64.0.9\n"}, nil
		default:
			return exec.Result{}, fmt.Errorf("unexpected kubectl argv: %v", argv)
		}
	}
	addrs, err := k8sTServerAddrs(context.Background(), "ctx", runner, "ampqa-uni", 5433)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The master pod's IP (100.64.0.9) must NOT appear.
	want := []string{"100.64.0.1:5433", "100.64.0.2:5433"}
	if strings.Join(addrs, ",") != strings.Join(want, ",") {
		t.Fatalf("addrs = %v, want %v (master pod IP must be excluded)", addrs, want)
	}
}

// TestResolveK8sYSQLAddrsClearErrors asserts the two failure modes give a clear,
// actionable error rather than a bare "not found": no namespace matched vs a
// matched namespace with no tserver pod.
func TestResolveK8sYSQLAddrsClearErrors(t *testing.T) {
	// No namespace contains the universe name.
	noNS := func(_ context.Context, _ io.Reader, argv []string) (exec.Result, error) {
		if strings.Contains(strings.Join(argv, " "), "get namespaces") {
			return exec.Result{Stdout: "default kube-system"}, nil
		}
		return exec.Result{}, fmt.Errorf("unexpected argv: %v", argv)
	}
	if _, err := k8sTServerAddrs(context.Background(), "ctx", noNS, "ampqa-uni", 5433); err == nil ||
		!strings.Contains(err.Error(), "no k8s namespace") {
		t.Fatalf("err = %v, want a clear no-namespace error", err)
	}

	// Namespace matched but only a master pod is present.
	onlyMaster := func(_ context.Context, _ io.Reader, argv []string) (exec.Result, error) {
		joined := strings.Join(argv, " ")
		switch {
		case strings.Contains(joined, "get namespaces"):
			return exec.Result{Stdout: "yb-dev-ampqa-uni"}, nil
		case strings.Contains(joined, "get pods"):
			return exec.Result{Stdout: "ybampqa-uni-yb-master-0 100.64.0.9\n"}, nil
		default:
			return exec.Result{}, fmt.Errorf("unexpected argv: %v", argv)
		}
	}
	_, err := k8sTServerAddrs(context.Background(), "ctx", onlyMaster, "ampqa-uni", 5433)
	if err == nil || !strings.Contains(err.Error(), "yb-dev-ampqa-uni") || !strings.Contains(err.Error(), "pods seen") {
		t.Fatalf("err = %v, want a clear error naming the namespace and pods seen", err)
	}
}

// TestProvisionK8sTeardownUsesNamespaceBackstop: the k8s teardown force-deletes
// the universe then runs the NAMESPACE sweep (not the EC2 tag sweep) before the
// YBA is torn down.
func TestProvisionK8sTeardownUsesNamespaceBackstop(t *testing.T) {
	ev := &ybaEvents{}
	st := &fakeStack{ev: ev, info: ybastack.Info{URL: "http://yba", CustomerUUID: "c1", APIToken: "tok"}}
	pr := &fakeProvisioner{ev: ev, universe: liveUniverse(), universeID: "u-1"}
	p := provisionK8sYBAProvider(st, pr)

	env, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(7, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := p.Teardown(context.Background(), env); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if pr.deletes != 1 {
		t.Errorf("universe deletes = %d, want 1", pr.deletes)
	}
	if pr.nsReaps != 1 {
		t.Errorf("namespace sweeps = %d, want 1 (k8s backstop)", pr.nsReaps)
	}
	if pr.reaps != 0 {
		t.Errorf("EC2 tag sweeps = %d, want 0 (kubernetes must not touch AWS)", pr.reaps)
	}
	if st.teardowns != 1 {
		t.Errorf("yba teardowns = %d, want 1", st.teardowns)
	}
	got := ev.list()
	di, ni, ti := indexOf(got, "delete_universe"), indexOf(got, "reap_ns"), indexOf(got, "stack.teardown")
	if !(di >= 0 && ni >= 0 && ti >= 0 && di < ni && ni < ti) {
		t.Errorf("teardown order = %v, want delete_universe < reap_ns < stack.teardown", got)
	}
}

// TestProvisionK8sReapFailureKeepsYBAUp: when the namespace sweep FAILS, the YBA
// must be kept up (its teardown must not run) — the airtight-cleanup invariant,
// same as the AWS tag-sweep-failure case.
func TestProvisionK8sReapFailureKeepsYBAUp(t *testing.T) {
	ev := &ybaEvents{}
	st := &fakeStack{ev: ev, info: ybastack.Info{URL: "http://yba", CustomerUUID: "c1", APIToken: "tok"}}
	pr := &fakeProvisioner{ev: ev, universeID: "u-1",
		waitErr: fmt.Errorf("create task failed after creating pods"),
		reapErr: fmt.Errorf("kubectl delete namespace failed"),
	}
	p := provisionK8sYBAProvider(st, pr)

	if _, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(7, true)); err == nil {
		t.Fatalf("expected provision error")
	}
	if pr.nsReaps != 1 {
		t.Errorf("namespace sweeps = %d, want 1", pr.nsReaps)
	}
	if st.teardowns != 0 {
		t.Errorf("yba teardowns = %d, want 0 (YBA must stay up when the namespace sweep failed)", st.teardowns)
	}
}

func liveUniverse() yba.Universe {
	return yba.Universe{
		Name:         "ampqa-uni",
		UniverseUUID: "u-1",
		UniverseDetails: yba.UniverseDetails{
			Clusters: []yba.Cluster{{UserIntent: yba.UserIntent{YBSoftwareVersion: "2025.1.0.0-b9"}}},
			NodeDetailsSet: []yba.NodeDetails{
				{IsTserver: true, State: "Live", YSQLServerRpcPort: 5433, CloudInfo: yba.CloudInfo{PrivateIP: "10.2.0.1"}},
				{IsTserver: true, State: "Live", YSQLServerRpcPort: 5433, CloudInfo: yba.CloudInfo{PrivateIP: "10.2.0.2"}},
			},
		},
	}
}

func TestModeDispatchSelectsProvisionYBA(t *testing.T) {
	ev := &ybaEvents{}
	st := &fakeStack{ev: ev, info: ybastack.Info{URL: "http://yba", CustomerUUID: "c1", APIToken: "tok", YBImageRegistry: "quay/itest"}}
	pr := &fakeProvisioner{ev: ev, universe: liveUniverse(), universeID: "u-1"}
	p := provisionYBAProvider(st, pr)

	env, err := p.Provision(context.Background(), core.EnvSpec{Shape: core.Universe}, core.NewRunSeed(7, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	e := env.(*Env)
	if e.mode != ModeProvisionYBA {
		t.Errorf("mode = %q, want provision_yba", e.mode)
	}
	if !e.owned {
		t.Errorf("provision_yba env must be owned")
	}
	if ci := e.PGEndpoint(); ci.Host != "10.2.0.1" || ci.Port != 5433 {
		t.Errorf("endpoint = %s:%d, want 10.2.0.1:5433", ci.Host, ci.Port)
	}
	if len(e.NodeAddrs()) != 2 {
		t.Errorf("node addrs = %v, want 2", e.NodeAddrs())
	}
	if e.ybImageRegistry != "quay/itest" {
		t.Errorf("ybImageRegistry = %q, want quay/itest (carried from Info)", e.ybImageRegistry)
	}
	if got := ev.list(); len(got) < 2 || got[0] != "deploy" {
		t.Errorf("provision order = %v, want deploy first", got)
	}
	// The instance type must be registered on the provider before the universe
	// is created, or YBA NPEs on a null InstanceType.
	if pr.instTypes != 1 {
		t.Errorf("instance-type registrations = %d, want 1", pr.instTypes)
	}
	// The DB release must be imported before universe-create, or YBA 500s with
	// "no artifact found for architecture <arch>".
	if pr.releases != 1 {
		t.Errorf("release imports = %d, want 1", pr.releases)
	}
	// YBC is disabled (default) on the provider before create, so YBA skips the
	// WaitForYbcServer subtask.
	if pr.ybcSets != 1 || pr.ybcEnabled {
		t.Errorf("ybc sets = %d, enabled = %v; want 1 set to false", pr.ybcSets, pr.ybcEnabled)
	}
	// provision only passes after the universe is confirmed healthy.
	if pr.readyChecks != 1 {
		t.Errorf("universe-ready checks = %d, want 1", pr.readyChecks)
	}
	got := ev.list()
	ri := indexOf(got, "ensure_release")
	ei, si, ai, ci, wi := indexOf(got, "ensure_provider"), indexOf(got, "set_ybc"), indexOf(got, "add_instance_type"), indexOf(got, "create_universe"), indexOf(got, "wait_ready")
	if ri < 0 || ei < 0 || si < 0 || ai < 0 || ci < 0 || wi < 0 || !(ri < ci && ei < si && si < ci && ai < ci && ci < wi) {
		t.Errorf("order = %v, want ensure_release + ensure_provider < set_ybc < create_universe < wait_ready", got)
	}
}

func TestProvisionYBATeardownDeletesUniverseThenYBA(t *testing.T) {
	ev := &ybaEvents{}
	st := &fakeStack{ev: ev, info: ybastack.Info{URL: "http://yba", CustomerUUID: "c1", APIToken: "tok"}}
	pr := &fakeProvisioner{ev: ev, universe: liveUniverse(), universeID: "u-1"}
	p := provisionYBAProvider(st, pr)

	env, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(7, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := p.Teardown(context.Background(), env); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if pr.deletes != 1 {
		t.Errorf("universe deletes = %d, want 1", pr.deletes)
	}
	if st.teardowns != 1 {
		t.Errorf("yba teardowns = %d, want 1", st.teardowns)
	}
	// LIFO: the universe delete (last created) must precede the yba teardown.
	got := ev.list()
	di, ti := indexOf(got, "delete_universe"), indexOf(got, "stack.teardown")
	if di < 0 || ti < 0 || di > ti {
		t.Errorf("teardown order = %v, want delete_universe before stack.teardown", got)
	}
}

func TestProvisionYBAPartialFailureTearsYBADown(t *testing.T) {
	// Deploy succeeds, but universe-create fails: the YBA must still be torn down
	// (partial-provision cleanup), and no universe delete is attempted.
	ev := &ybaEvents{}
	st := &fakeStack{ev: ev, info: ybastack.Info{URL: "http://yba", CustomerUUID: "c1", APIToken: "tok"}}
	pr := &fakeProvisioner{ev: ev, universeID: "u-1", createErr: fmt.Errorf("boom: universe create failed")}
	p := provisionYBAProvider(st, pr)

	env, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(7, true))
	if err == nil {
		t.Fatalf("expected provision error")
	}
	if env != nil {
		t.Errorf("failed provision must return a nil env, got %T", env)
	}
	if st.teardowns != 1 {
		t.Errorf("yba teardowns = %d, want 1 (partial provision must tear the yba down)", st.teardowns)
	}
	if pr.deletes != 0 {
		t.Errorf("universe deletes = %d, want 0 (no universe was created)", pr.deletes)
	}
}

func TestProvisionYBADeployFailureTearsStackDown(t *testing.T) {
	// Deploy fails partway: it creates the namespace/pull-secret/CRBs as its
	// first steps and leaves them in place on a later-step failure, so the stack
	// teardown MUST still run (label-guarded, best-effort) to reap them. No
	// universe was created, so no universe delete is attempted.
	ev := &ybaEvents{}
	st := &fakeStack{ev: ev, deployErr: fmt.Errorf("helm install failed")}
	pr := &fakeProvisioner{ev: ev, universeID: "u-1"}
	p := provisionYBAProvider(st, pr)

	if _, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(7, true)); err == nil {
		t.Fatalf("expected provision error")
	}
	if st.teardowns != 1 {
		t.Errorf("yba teardowns = %d, want 1 (deploy leaves partial resources; cleanup must reap them)", st.teardowns)
	}
	if pr.deletes != 0 {
		t.Errorf("universe deletes = %d, want 0", pr.deletes)
	}
}

func TestProvisionYBACreateTaskFailureReapsNodes(t *testing.T) {
	// The create task launches the cloud nodes, THEN fails (e.g. the yb-controller
	// ping timeout). The universe UUID is already known (set before the wait), so
	// the launched nodes MUST be force-deleted before the YBA is torn down — never
	// orphaned. This is the exact bug that stranded 3 EC2 nodes.
	ev := &ybaEvents{}
	st := &fakeStack{ev: ev, info: ybastack.Info{URL: "http://yba", CustomerUUID: "c1", APIToken: "tok"}}
	pr := &fakeProvisioner{ev: ev, universeID: "u-1", waitErr: fmt.Errorf("pinging yb-controller: exceeded max retries")}
	p := provisionYBAProvider(st, pr)

	env, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(7, true))
	if err == nil {
		t.Fatalf("expected provision error")
	}
	if env != nil {
		t.Errorf("failed provision must return a nil env, got %T", env)
	}
	if pr.deletes != 1 {
		t.Errorf("force deletes = %d, want 1 (the launched nodes must be reaped)", pr.deletes)
	}
	if st.teardowns != 1 {
		t.Errorf("yba teardowns = %d, want 1", st.teardowns)
	}
	got := ev.list()
	di, ti := indexOf(got, "delete_universe"), indexOf(got, "stack.teardown")
	if di < 0 || ti < 0 || di > ti {
		t.Errorf("teardown order = %v, want delete_universe before stack.teardown", got)
	}
}

func TestProvisionYBADeleteFailureStillReapsThenTearsDown(t *testing.T) {
	// The create task launched nodes then failed, AND the YBA force-delete fails.
	// The tag sweep (reap) is the backstop: it terminates the nodes directly, so
	// the YBA IS then torn down (no orphan). Force-delete failure is not fatal.
	ev := &ybaEvents{}
	st := &fakeStack{ev: ev, info: ybastack.Info{URL: "http://yba", CustomerUUID: "c1", APIToken: "tok"}}
	pr := &fakeProvisioner{ev: ev, universeID: "u-1",
		waitErr:        fmt.Errorf("create task failed after launching nodes"),
		forceDeleteErr: fmt.Errorf("force delete failed"),
	}
	p := provisionYBAProvider(st, pr)

	if _, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(7, true)); err == nil {
		t.Fatalf("expected provision error")
	}
	if pr.reaps != 1 {
		t.Errorf("tag sweeps = %d, want 1 (must always run as the backstop)", pr.reaps)
	}
	if st.teardowns != 1 {
		t.Errorf("yba teardowns = %d, want 1 (nodes reaped by tag → YBA torn down)", st.teardowns)
	}
	got := ev.list()
	ri, ti := indexOf(got, "reap"), indexOf(got, "stack.teardown")
	if ri < 0 || ti < 0 || ri > ti {
		t.Errorf("order = %v, want reap before stack.teardown", got)
	}
}

func TestProvisionYBAReapFailureKeepsYBAUp(t *testing.T) {
	// The tag sweep itself FAILS (an AWS error — this run's nodes may still be
	// running). The YBA must be KEPT UP (its teardown must NOT run) so the nodes
	// keep a manager and are not orphaned — the airtight cleanup invariant.
	ev := &ybaEvents{}
	st := &fakeStack{ev: ev, info: ybastack.Info{URL: "http://yba", CustomerUUID: "c1", APIToken: "tok"}}
	pr := &fakeProvisioner{ev: ev, universeID: "u-1",
		waitErr: fmt.Errorf("create task failed after launching nodes"),
		reapErr: fmt.Errorf("aws terminate-instances failed"),
	}
	p := provisionYBAProvider(st, pr)

	if _, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(7, true)); err == nil {
		t.Fatalf("expected provision error")
	}
	if pr.reaps != 1 {
		t.Errorf("tag sweeps = %d, want 1", pr.reaps)
	}
	if st.teardowns != 0 {
		t.Errorf("yba teardowns = %d, want 0 (YBA must stay up when the tag sweep failed)", st.teardowns)
	}
}

func TestProvisionYBAEmptyUUIDResolvesByNameAndReaps(t *testing.T) {
	// The create task returns NO universe UUID (empty ResourceUUID) but the task
	// then FAILS after launching nodes. The teardown must resolve the universe by
	// its ampqa-<runTag> name AND run the tag sweep — never orphan.
	ev := &ybaEvents{}
	st := &fakeStack{ev: ev, info: ybastack.Info{URL: "http://yba", CustomerUUID: "c1", APIToken: "tok"}}
	pr := &fakeProvisioner{ev: ev, universe: liveUniverse(), universeID: "", // create ref carries no UUID
		waitErr: fmt.Errorf("create task failed after launching nodes")}
	p := provisionYBAProvider(st, pr)

	if _, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(7, true)); err == nil {
		t.Fatalf("expected provision error")
	}
	// Resolved by name (liveUniverse UUID) → force-deleted; and the tag sweep ran.
	if pr.deletes != 1 {
		t.Errorf("force deletes = %d, want 1 (resolved by name)", pr.deletes)
	}
	if pr.reaps != 1 {
		t.Errorf("tag sweeps = %d, want 1", pr.reaps)
	}
	if st.teardowns != 1 {
		t.Errorf("yba teardowns = %d, want 1", st.teardowns)
	}
}

func TestProvisionYBAGetUniverseFailureCleansUpBoth(t *testing.T) {
	// Universe created, but resolving it fails: cleanup must delete the universe
	// AND tear the YBA down, LIFO.
	ev := &ybaEvents{}
	st := &fakeStack{ev: ev, info: ybastack.Info{URL: "http://yba", CustomerUUID: "c1", APIToken: "tok"}}
	pr := &fakeProvisioner{ev: ev, universeID: "u-1", getErr: fmt.Errorf("universe details unavailable")}
	p := provisionYBAProvider(st, pr)

	if _, err := p.Provision(context.Background(), core.EnvSpec{}, core.NewRunSeed(7, true)); err == nil {
		t.Fatalf("expected provision error")
	}
	if pr.deletes != 1 {
		t.Errorf("universe deletes = %d, want 1 (created universe must be reaped)", pr.deletes)
	}
	if st.teardowns != 1 {
		t.Errorf("yba teardowns = %d, want 1", st.teardowns)
	}
	got := ev.list()
	di, ti := indexOf(got, "delete_universe"), indexOf(got, "stack.teardown")
	if di < 0 || ti < 0 || di > ti {
		t.Errorf("cleanup order = %v, want delete_universe before stack.teardown", got)
	}
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}

func TestUnknownModeErrors(t *testing.T) {
	p := New(Config{Mode: "bogus"})
	if _, err := p.Provision(context.Background(), core.EnvSpec{}, nil); err == nil {
		t.Fatalf("expected error for unknown mode")
	}
}

func TestConfiguredGate(t *testing.T) {
	if (Config{}).Configured() {
		t.Errorf("empty config must not be Configured")
	}
	if !(Config{Mode: ModeAttachAddress}).Configured() {
		t.Errorf("mode set must be Configured")
	}
}

// TestDetectYBVersionFallsBackOnConnectFailure exercises the REAL
// detectYBVersion (not the injected seam) against a closed local port: no
// listener answers, so pg.Connect fails immediately (connection refused, no
// live infra involved) and detection must fall back to "" rather than
// blocking or panicking.
func TestDetectYBVersionFallsBackOnConnectFailure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close() // nothing listens on this port anymore

	got := detectYBVersion(context.Background(), pg.ConnInfo{Host: "127.0.0.1", Port: port})
	if got != "" {
		t.Errorf("detectYBVersion against a closed port = %q, want empty (fall back cleanly)", got)
	}
}
