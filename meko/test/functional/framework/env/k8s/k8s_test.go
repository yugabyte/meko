package k8s

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/cognitomock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekostack"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/reap"
	execk8s "github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
)

type fakeStack struct {
	cfg       mekostack.Config
	deployErr error
	attachErr error
	gone      bool
	mu        sync.Mutex
	deployed  int
	attached  int
	torn      int
	closed    int
	collected int
	product   string
	database  string
}

func (f *fakeStack) Deploy(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deployed++
	return f.deployErr
}
func (f *fakeStack) Attach(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attached++
	return f.attachErr
}
func (f *fakeStack) Teardown(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.torn++
	return nil
}
func (f *fakeStack) Close() { f.mu.Lock(); f.closed++; f.mu.Unlock() }
func (f *fakeStack) Collect(context.Context, apps.ArtifactSink) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.collected++
	return nil
}
func (f *fakeStack) NamespaceGone(context.Context) (bool, error) { return f.gone, nil }
func (f *fakeStack) Namespace() string                           { return f.cfg.Namespace }
func (f *fakeStack) APIURL() string                              { return "http://api." + f.cfg.Namespace }
func (f *fakeStack) MCPURL() string                              { return "http://mcp." + f.cfg.Namespace + "/mcp" }
func (f *fakeStack) API() *mekoapi.Client                        { return mekoapi.New(f.APIURL()) }
func (f *fakeStack) LLM() *llmmock.Client {
	return llmmock.NewClient("http://llm." + f.cfg.Namespace)
}
func (f *fakeStack) Kube() *execk8s.Kubectl {
	return execk8s.New(execk8s.Config{Context: f.cfg.Context, Namespace: f.cfg.Namespace})
}
func (f *fakeStack) Workloads(context.Context) ([]mekostack.Workload, error) { return nil, nil }
func (f *fakeStack) Extensions(context.Context) ([]string, error)            { return nil, nil }
func (f *fakeStack) Upgrade(context.Context, string, string) error           { return nil }
func (f *fakeStack) ProbeAPI(context.Context) error                          { return nil }
func (f *fakeStack) GuardMark(context.Context) (mekostack.GuardMark, error) {
	return mekostack.GuardMark{}, nil
}
func (f *fakeStack) GuardCheck(context.Context, mekostack.GuardMark) ([]mekostack.Finding, error) {
	return nil, nil
}
func (f *fakeStack) DatapacksWithoutLangfuseProject(context.Context) ([]string, error) {
	return nil, nil
}
func (f *fakeStack) Versions(context.Context) (string, string) { return f.product, f.database }
func (f *fakeStack) Backing(context.Context) (mekostack.Backing, error) {
	if f.cfg.Backing != nil {
		return *f.cfg.Backing, nil
	}
	return mekostack.Backing{Mode: mekostack.ModeBundled, Replication: 1, Tservers: 1}, nil
}
func (f *fakeStack) YSQL(context.Context, string) (string, error) { return "", nil }
func (f *fakeStack) AuthMode() mekostack.AuthMode                 { return mekostack.AuthNone }
func (f *fakeStack) NewAccounts(context.Context, string, int) ([]mekostack.Account, error) {
	return nil, nil
}
func (f *fakeStack) CognitoControl(context.Context) (*cognitomock.Client, error) { return nil, nil }

type factory struct {
	mu     sync.Mutex
	stacks []*fakeStack
	deploy func(ns string) error
}

func (f *factory) build(cfg mekostack.Config) Stack {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &fakeStack{cfg: cfg, product: "0.261002.0-b75b2a658", database: "2026.1.0.0-b1-meko-2"}
	if f.deploy != nil {
		s.deployErr = f.deploy(cfg.Namespace)
	}
	f.stacks = append(f.stacks, s)
	return s
}

func (f *factory) byNS(ns string) *fakeStack {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.stacks {
		if s.cfg.Namespace == ns {
			return s
		}
	}
	return nil
}

func newProvider(t *testing.T, cfg Config) (*Provider, *factory) {
	t.Helper()
	if cfg.RunID == "" {
		cfg.RunID = "run42"
	}
	if cfg.Context == "" {
		cfg.Context = "ctx"
	}
	f := &factory{}
	p := New(cfg)
	p.logf = nil
	p.newStack = f.build
	return p, f
}

func TestNamespaceNaming(t *testing.T) {
	if got := SharedNamespace("run42"); got != "mekoqa-st-run42" {
		t.Errorf("SharedNamespace = %q", got)
	}
	if got := DedicatedNamespace("run42", 3); got != "mekoqa-ds-run42-3" {
		t.Errorf("DedicatedNamespace = %q", got)
	}
	if got := RunIDFrom("3F2504E0-4F89-11D3-9A0C-0305E82C3301"); got != "3f2504e04f" {
		t.Errorf("RunIDFrom(uuid) = %q, want ten lowercase alphanumerics", got)
	}
	a, b := RunIDFrom(""), RunIDFrom("")
	if len(a) != 8 || a == b {
		t.Errorf("random run ids = %q, %q, want two distinct 8-char ids", a, b)
	}
	for _, id := range []string{RunIDFrom("a-b-c"), RunIDFrom("UPPER")} {
		if DedicatedNamespace(id, 99999) != strings.ToLower(DedicatedNamespace(id, 99999)) || len(DedicatedNamespace(id, 99999)) > 63 {
			t.Errorf("namespace for %q is not a DNS label", id)
		}
	}
}

func TestSingleStackTestsShareOneStack(t *testing.T) {
	p, f := newProvider(t, Config{})
	ctx := context.Background()
	if err := p.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.stacks) != 1 || f.stacks[0].cfg.Namespace != "mekoqa-st-run42" || f.stacks[0].deployed != 1 {
		t.Fatalf("stacks = %+v, want one deploy into mekoqa-st-run42", f.stacks)
	}
	e1, err := p.Provision(ctx, core.EnvSpec{Shape: core.SingleStack}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e2, _ := p.Provision(ctx, core.EnvSpec{Shape: core.SingleStack}, nil)
	k1, k2 := e1.(*Env), e2.(*Env)
	if k1.Namespace() != "mekoqa-st-run42" || k2.Namespace() != k1.Namespace() || k1.Stack() != k2.Stack() {
		t.Errorf("environments do not share the stack: %q %q", k1.Namespace(), k2.Namespace())
	}
	if k1.URL() != "http://api.mekoqa-st-run42" || k1.MCPURL() != "http://mcp.mekoqa-st-run42/mcp" {
		t.Errorf("URLs = %q %q", k1.URL(), k1.MCPURL())
	}
	if k1.LLM() == nil || k1.LLM().URL() != "http://llm.mekoqa-st-run42" {
		t.Errorf("LLM() = %v, want the stack's mock client", k1.LLM())
	}
	if k1.Versions().Product != "0.261002.0-b75b2a658" || k1.Versions().YB != "2026.1.0.0-b1-meko-2" {
		t.Errorf("versions = %+v", k1.Versions())
	}
	if h := k1.Handles(); h.Namespace != "mekoqa-st-run42" || h.Cluster != "ctx" {
		t.Errorf("handles = %+v", h)
	}
	if k1.Kube() == nil || k1.Kube().Namespace() != "mekoqa-st-run42" {
		t.Error("Kube() is not bound to the stack namespace")
	}
	if err := p.Teardown(ctx, e1); err != nil {
		t.Fatal(err)
	}
	if f.stacks[0].torn != 0 {
		t.Error("a test's teardown deleted the run's stack")
	}
	if err := k1.Collect(ctx, nil); err != nil || f.stacks[0].collected != 1 {
		t.Errorf("Collect did not reach the stack: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if f.stacks[0].torn != 1 {
		t.Errorf("Close tore the stack down %d times, want 1", f.stacks[0].torn)
	}
	if plan := p.ReapPlan(); !plan.Empty() {
		t.Errorf("plan after Close = %+v, want empty", plan)
	}
}

func TestFailedPrepareTearsTheStackDown(t *testing.T) {
	p, f := newProvider(t, Config{})
	f.deploy = func(string) error { return errors.New("helm timed out") }
	if err := p.Prepare(context.Background()); err == nil || !strings.Contains(err.Error(), "helm timed out") {
		t.Fatalf("Prepare err = %v", err)
	}
	if f.stacks[0].torn != 1 {
		t.Errorf("a failed bring-up was torn down %d times, want 1", f.stacks[0].torn)
	}
	if _, err := p.Provision(context.Background(), core.EnvSpec{Shape: core.SingleStack}, nil); err == nil {
		t.Error("Provision handed out an environment with no stack")
	}
}

func TestDedicatedStacksGetTheirOwnNamespaceAndGoWithTheTest(t *testing.T) {
	p, f := newProvider(t, Config{})
	var changes int
	p.OnReapPlanChange(func() { changes++ })
	ctx := context.Background()
	e1, err := p.Provision(ctx, core.EnvSpec{Shape: core.DedicatedStack}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := p.Provision(ctx, core.EnvSpec{Shape: core.DedicatedStack}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e1.(*Env).Namespace() != "mekoqa-ds-run42-1" || e2.(*Env).Namespace() != "mekoqa-ds-run42-2" {
		t.Fatalf("namespaces = %q %q", e1.(*Env).Namespace(), e2.(*Env).Namespace())
	}
	plan := p.ReapPlan()
	if plan == nil || len(plan.Namespaces) != 2 {
		t.Fatalf("plan = %+v, want both dedicated namespaces", plan)
	}
	for _, n := range plan.Namespaces {
		if n.Kind != reap.KindMeko || n.KubeContext != "ctx" || !strings.HasPrefix(n.Namespace, "mekoqa-ds-run42-") {
			t.Errorf("plan entry = %+v", n)
		}
	}
	if err := p.Teardown(ctx, e1); err != nil {
		t.Fatal(err)
	}
	if f.byNS("mekoqa-ds-run42-1").torn != 1 || f.byNS("mekoqa-ds-run42-2").torn != 0 {
		t.Error("teardown did not hit exactly the first test's stack")
	}
	if plan := p.ReapPlan(); plan == nil || len(plan.Namespaces) != 1 || plan.Namespaces[0].Namespace != "mekoqa-ds-run42-2" {
		t.Errorf("plan after one teardown = %+v", plan)
	}
	if changes != 3 {
		t.Errorf("plan change callbacks = %d, want 3 (two builds, one teardown)", changes)
	}
}

func TestFailedDedicatedProvisionIsTornDownAndNotPlanned(t *testing.T) {
	p, f := newProvider(t, Config{})
	f.deploy = func(string) error { return errors.New("boom") }
	if _, err := p.Provision(context.Background(), core.EnvSpec{Shape: core.DedicatedStack}, nil); err == nil {
		t.Fatal("Provision succeeded")
	}
	if f.stacks[0].torn != 1 || !p.ReapPlan().Empty() {
		t.Errorf("torn=%d plan=%+v", f.stacks[0].torn, p.ReapPlan())
	}
}

func TestAttachBuildsNothingAndKeepsTheStack(t *testing.T) {
	p, f := newProvider(t, Config{Namespace: "meko-byoc-dev"})
	if !p.Attaching() || p.Validate() != nil {
		t.Fatal("an attaching provider must not need a build config")
	}
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := f.stacks[0]
	if s.cfg.Namespace != "meko-byoc-dev" || s.attached != 1 || s.deployed != 0 {
		t.Fatalf("attach=%d deploy=%d ns=%s", s.attached, s.deployed, s.cfg.Namespace)
	}
	if _, ok := p.StandingEnv(); ok {
		t.Error("a stack the provider did not build was offered for recording")
	}
	if !p.ReapPlan().Empty() {
		t.Error("an attached stack is in the reap plan")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if s.torn != 0 || s.closed != 1 {
		t.Errorf("Close: torn=%d closed=%d, want the stack left alone and its forwards closed", s.torn, s.closed)
	}
}

func TestAttachFailureIsReported(t *testing.T) {
	p, f := newProvider(t, Config{Namespace: "gone"})
	f.stacks = nil
	p.newStack = func(c mekostack.Config) Stack {
		s := &fakeStack{cfg: c, attachErr: errors.New("namespace not found")}
		f.stacks = append(f.stacks, s)
		return s
	}
	err := p.Prepare(context.Background())
	if err == nil || !strings.Contains(err.Error(), "namespace not found") {
		t.Fatalf("err = %v", err)
	}
	if f.stacks[0].torn != 0 {
		t.Error("a failed attach deleted the namespace")
	}
}

func TestStandingEnvCarriesWhatEnvRmAndRunEnvNeed(t *testing.T) {
	p, f := newProvider(t, Config{Stack: mekostack.Config{Release: "meko"}})
	if _, ok := p.StandingEnv(); ok {
		t.Fatal("a provider with no stack offered one")
	}
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	se, ok := p.StandingEnv()
	if !ok {
		t.Fatal("no standing env")
	}
	if f.stacks[0].closed != 1 {
		t.Error("the port-forwards were left open for a process that is about to exit")
	}
	if se.Provider != "k8s" || se.Shape != "single_stack" || se.Info["context"] != "ctx" || se.Info["namespace"] != "mekoqa-st-run42" {
		t.Errorf("standing env = %+v", se)
	}
	plan, err := reap.FromJSON(se.Info["reap_plan"])
	if err != nil || plan.Empty() || plan.Namespaces[0].Kind != reap.KindMeko || plan.Namespaces[0].HelmRelease != "meko" {
		t.Errorf("reap plan = %+v, %v", plan, err)
	}
}

func TestRemoteShape(t *testing.T) {
	p, _ := newProvider(t, Config{})
	_, err := p.Provision(context.Background(), core.EnvSpec{Shape: core.Remote}, nil)
	var shape *core.UnsupportedShapeError
	if !errors.As(err, &shape) {
		t.Fatalf("err = %v, want an UnsupportedShapeError so the test skips", err)
	}
	p, _ = newProvider(t, Config{RemoteURL: "https://dev.example", RemoteMCPURL: "https://mcp.example/mcp"})
	env, err := p.Provision(context.Background(), core.EnvSpec{Shape: core.Remote}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := env.(*Env)
	if e.URL() != "https://dev.example" || e.MCPURL() != "https://mcp.example/mcp" || e.Kube() != nil || e.Stack() != nil {
		t.Errorf("remote env = %+v", e)
	}
	if err := e.Collect(context.Background(), nil); err != nil {
		t.Errorf("Collect on a remote env: %v", err)
	}
	if err := p.Teardown(context.Background(), env); err != nil {
		t.Errorf("Teardown of a remote env: %v", err)
	}
	if e.MCP("pat").URL() != "https://mcp.example/mcp" {
		t.Error("MCP client does not target the endpoint")
	}
}

func TestSharedInfraGone(t *testing.T) {
	p, f := newProvider(t, Config{})
	if _, gone := p.SharedInfraGone(context.Background()); gone {
		t.Error("gone before any stack exists")
	}
	_ = p.Prepare(context.Background())
	if _, gone := p.SharedInfraGone(context.Background()); gone {
		t.Error("gone while the namespace exists")
	}
	f.stacks[0].gone = true
	where, gone := p.SharedInfraGone(context.Background())
	if !gone || where != "namespace mekoqa-st-run42" {
		t.Errorf("SharedInfraGone = %q, %v", where, gone)
	}
}

func TestAttachByReferenceIsRefused(t *testing.T) {
	p, _ := newProvider(t, Config{})
	if _, err := p.Attach(context.Background(), "x"); err == nil {
		t.Fatal("Attach succeeded, which would let `env rm` report a teardown that never happened")
	}
}

func TestVersionOrEmpty(t *testing.T) {
	for in, want := range map[string]string{
		"0.261002.0-b75b2a658": "0.261002.0-b75b2a658",
		"custom-0123456789ab":  "",
		"":                     "",
	} {
		if got := versionOrEmpty(in); got != want {
			t.Errorf("versionOrEmpty(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateRefusesABuildWithNoQuayCredential(t *testing.T) {
	p, _ := newProvider(t, Config{Stack: mekostack.Config{ImageVersion: "1"}})
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "Quay credential") {
		t.Fatalf("err = %v", err)
	}
}
