package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps/cloud"
	"github.com/yugabyte/yb-amp/test/functional/framework/core"
	"github.com/yugabyte/yb-amp/test/functional/framework/env/registry"
	execpkg "github.com/yugabyte/yb-amp/test/functional/framework/exec"
)

// fakeConn is a fake SSH connection to an instance.
type fakeConn struct {
	mu     sync.Mutex
	closed bool
}

func (f *fakeConn) Run(context.Context, string) (execpkg.Result, error) {
	return execpkg.Result{ExitCode: 0}, nil
}
func (f *fakeConn) Copy(context.Context, string, string) error { return nil }
func (f *fakeConn) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// fakeVM is a scripted fake cloud.VM.
type fakeVM struct {
	meta      cloud.Meta
	launched  []cloud.Instance
	described []cloud.Instance
	conns     []*fakeConn
	connIdx   int

	terminated [][]string
	reaped     []string

	runErr    error
	termErr   error
	reapErr   error
	connErrAt int // return an error connecting node #connErrAt; -1 → never
}

func (f *fakeVM) RunInstances(_ context.Context, spec cloud.RunSpec) ([]cloud.Instance, error) {
	if f.runErr != nil {
		return nil, f.runErr
	}
	return f.launched, nil
}
func (f *fakeVM) WaitRunning(context.Context, []string) error { return nil }
func (f *fakeVM) WaitSSH(context.Context, []cloud.Instance, string) error {
	return nil
}
func (f *fakeVM) Describe(context.Context, map[string]string) ([]cloud.Instance, error) {
	return f.described, nil
}
func (f *fakeVM) Terminate(_ context.Context, ids []string) error {
	f.terminated = append(f.terminated, append([]string(nil), ids...))
	return f.termErr
}
func (f *fakeVM) ReapByTag(_ context.Context, runTag string) error {
	f.reaped = append(f.reaped, runTag)
	return f.reapErr
}
func (f *fakeVM) ReapStale(context.Context, time.Duration) error { return nil }
func (f *fakeVM) Connect(_ context.Context, inst cloud.Instance) (cloud.Conn, error) {
	if f.connErrAt >= 0 && f.connIdx == f.connErrAt {
		return nil, errors.New("connect failed")
	}
	c := f.conns[f.connIdx]
	f.connIdx++
	return c, nil
}
func (f *fakeVM) Meta() cloud.Meta { return f.meta }

func threeInstances() []cloud.Instance {
	return []cloud.Instance{
		{ID: "i-0", PublicIP: "52.0.0.0", PrivateIP: "10.0.0.0", State: "running"},
		{ID: "i-1", PublicIP: "52.0.0.1", PrivateIP: "10.0.0.1", State: "running"},
		{ID: "i-2", PublicIP: "52.0.0.2", PrivateIP: "10.0.0.2", State: "running"},
	}
}

func newFake(insts []cloud.Instance) *fakeVM {
	conns := make([]*fakeConn, len(insts))
	for i := range conns {
		conns[i] = &fakeConn{}
	}
	return &fakeVM{
		meta:      cloud.Meta{Region: "us-west-2", SSHUser: "ec2-user", KeyFile: "/key.pem"},
		launched:  insts,
		described: insts,
		conns:     conns,
		connErrAt: -1,
	}
}

func provider(t *testing.T, fake *fakeVM, opts ...Option) *Provider {
	t.Helper()
	base := []Option{WithLogf(nil)}
	p := New(Config{NodeCount: 3}, fake, append(base, opts...)...)
	p.newTag = func() string { return "tag01" }
	return p
}

func TestProvisionWiresNodes(t *testing.T) {
	fake := newFake(threeInstances())
	p := provider(t, fake)

	env, err := p.Provision(context.Background(), core.EnvSpec{}, nil)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	e := env.(*Env)
	if got := len(e.Nodes()); got != 3 {
		t.Fatalf("Nodes() = %d, want 3", got)
	}
	if addrs := e.NodeAddrs(); len(addrs) != 3 || addrs[0] != "10.0.0.0" || addrs[2] != "10.0.0.2" {
		t.Errorf("NodeAddrs = %v", addrs)
	}
	if e.RunTag() != "tag01" {
		t.Errorf("RunTag = %q, want tag01", e.RunTag())
	}
	if e.Exec() == nil {
		t.Error("Exec() is nil")
	}
	if e.ControllerURL() != "" {
		t.Error("ControllerURL should be empty for a bare host env")
	}
}

func TestTeardownTerminatesLaunchedIDs(t *testing.T) {
	fake := newFake(threeInstances())
	p := provider(t, fake)
	env, err := p.Provision(context.Background(), core.EnvSpec{}, nil)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := p.Teardown(context.Background(), env); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(fake.terminated) != 1 {
		t.Fatalf("terminate called %d times, want 1", len(fake.terminated))
	}
	got := fake.terminated[0]
	if len(got) != 3 || got[0] != "i-0" || got[1] != "i-1" || got[2] != "i-2" {
		t.Errorf("terminated ids = %v, want [i-0 i-1 i-2]", got)
	}
	// SSH connections closed.
	for i, c := range fake.conns {
		if !c.closed {
			t.Errorf("conn %d not closed on teardown", i)
		}
	}
	// Clean teardown → no reap needed.
	if len(fake.reaped) != 0 {
		t.Errorf("reap ran on a clean teardown: %v", fake.reaped)
	}
}

func TestTeardownReapsAndSurfacesOnTerminateFailure(t *testing.T) {
	fake := newFake(threeInstances())
	fake.termErr = errors.New("terminate boom")
	p := provider(t, fake)
	env, err := p.Provision(context.Background(), core.EnvSpec{}, nil)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	err = p.Teardown(context.Background(), env)
	if err == nil {
		t.Fatal("expected teardown error to be surfaced")
	}
	// Safety-net reap ran with the run tag.
	if len(fake.reaped) != 1 || fake.reaped[0] != "tag01" {
		t.Errorf("reap = %v, want [tag01]", fake.reaped)
	}
}

func TestProvisionCleansUpOnConnectFailure(t *testing.T) {
	fake := newFake(threeInstances())
	fake.connErrAt = 1 // second Connect fails
	p := provider(t, fake)
	if _, err := p.Provision(context.Background(), core.EnvSpec{}, nil); err == nil {
		t.Fatal("expected provision error on connect failure")
	}
	// The launched instances must have been terminated (no leak).
	if len(fake.terminated) == 0 {
		t.Fatal("no terminate on provision failure — leak")
	}
	last := fake.terminated[len(fake.terminated)-1]
	if len(last) != 3 {
		t.Errorf("cleanup terminated %v, want all three launched ids", last)
	}
}

func TestRegistryEntryRecordedAndRemoved(t *testing.T) {
	fake := newFake(threeInstances())
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	p := provider(t, fake, WithRegistry(reg))

	env, err := p.Provision(context.Background(), core.EnvSpec{}, nil)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	entries, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("registry has %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Provider != "vm" {
		t.Errorf("entry provider = %q, want vm", e.Provider)
	}
	if e.Data["cloud"] != "aws" {
		t.Errorf("entry cloud = %q, want aws", e.Data["cloud"])
	}
	if e.Data["run_tag"] != "tag01" {
		t.Errorf("entry run_tag = %q, want tag01", e.Data["run_tag"])
	}
	if e.Data["instance_ids"] != "i-0,i-1,i-2" {
		t.Errorf("entry instance_ids = %q", e.Data["instance_ids"])
	}

	if err := p.Teardown(context.Background(), env); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	entries, _ = reg.List()
	if len(entries) != 0 {
		t.Errorf("registry still has %d entries after teardown, want 0", len(entries))
	}
}

func TestTeardownKeepsRegistryEntryWhenReclaimFails(t *testing.T) {
	fake := newFake(threeInstances())
	fake.termErr = errors.New("terminate boom")
	fake.reapErr = errors.New("reap boom")
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	p := provider(t, fake, WithRegistry(reg))
	env, err := p.Provision(context.Background(), core.EnvSpec{}, nil)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := p.Teardown(context.Background(), env); err == nil {
		t.Fatal("expected teardown to surface the terminate error")
	}
	// Both terminate and the reap net failed: the instances may still be alive,
	// so the standing record MUST survive as the only handle a later reap has.
	entries, _ := reg.List()
	if len(entries) != 1 {
		t.Fatalf("registry has %d entries, want 1 kept for reclaim", len(entries))
	}
}

func TestProvisionClosesConnsWhenRegistryAddFails(t *testing.T) {
	fake := newFake(threeInstances())
	// A registry path whose parent is a regular FILE makes MkdirAll — and thus
	// Add — fail, exercising Provision's registry-record failure branch.
	base := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(base, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := registry.New(filepath.Join(base, "envs.json"))
	p := provider(t, fake, WithRegistry(reg))
	if _, err := p.Provision(context.Background(), core.EnvSpec{}, nil); err == nil {
		t.Fatal("expected provision to fail when the registry record cannot be written")
	}
	// The SSH connections opened during provisioning must be closed (no fd leak).
	for i, c := range fake.conns {
		if !c.closed {
			t.Errorf("conn %d left open after failed registry record", i)
		}
	}
	// And the launched instances must have been torn down.
	if len(fake.terminated) == 0 {
		t.Fatal("instances not terminated after failed registry record")
	}
}

func TestFromConfigDefaults(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c := FromConfig(cfg)
	if c.Cloud != DefaultCloud {
		t.Errorf("Cloud = %q, want %q", c.Cloud, DefaultCloud)
	}
	if c.NodeCount != DefaultNodeCount {
		t.Errorf("NodeCount = %d, want %d", c.NodeCount, DefaultNodeCount)
	}
	if c.InstanceType != "" {
		t.Errorf("InstanceType = %q, want empty (inherit driver default)", c.InstanceType)
	}
	if !c.Configured() {
		t.Error("Configured() = false, want true")
	}
}

// TestFromConfigLegacyAwsvmKeys confirms the env.awsvm.* keys still drive the
// env when env.vm.* is not set (back-compat alias).
func TestFromConfigLegacyAwsvmKeys(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, []string{
		"env.awsvm.node_count=int:5",
		"env.awsvm.instance_type=m7g.2xlarge",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c := FromConfig(cfg)
	if c.NodeCount != 5 {
		t.Errorf("NodeCount = %d, want 5 (legacy env.awsvm.node_count)", c.NodeCount)
	}
	if c.InstanceType != "m7g.2xlarge" {
		t.Errorf("InstanceType = %q, want m7g.2xlarge (legacy env.awsvm.instance_type)", c.InstanceType)
	}
}
