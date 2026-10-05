package k8s

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekostack"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/reap"
	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
)

type fakeUniverse struct {
	spec    universe.Spec
	prov    universe.Provider
	ns      string
	mu      *sync.Mutex
	torn    *[]string
	tearErr error
}

func (f fakeUniverse) Name() string                { return f.spec.Name }
func (f fakeUniverse) Spec() universe.Spec         { return f.spec }
func (f fakeUniverse) Provider() universe.Provider { return f.prov }
func (f fakeUniverse) Namespace() string           { return f.ns }
func (f fakeUniverse) Conn() universe.ConnInfo     { return universe.ConnInfo{} }
func (f fakeUniverse) ClusterAddr() (string, int) {
	return "db-ysql." + f.ns + ".svc.cluster.local.", 5433
}
func (f fakeUniverse) Nodes(context.Context) ([]universe.Node, error)              { return nil, nil }
func (f fakeUniverse) SQL(context.Context, string, string, string) (string, error) { return "", nil }
func (f fakeUniverse) Teardown(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.torn = append(*f.torn, f.ns)
	return f.tearErr
}

type creators struct {
	mu      sync.Mutex
	calls   []universe.Spec
	tags    []string
	plans   []*reap.Plan
	torn    []string
	err     error
	tearErr error
	p       *Provider
}

type fakeCreator struct {
	c    *creators
	prov universe.Provider
	tag  string
}

func (f fakeCreator) nsOf(name string) string { return "mekoqa-yu-" + f.tag + "-" + name }

func (f fakeCreator) Create(_ context.Context, spec universe.Spec) (universe.Universe, error) {
	f.c.mu.Lock()
	f.c.calls = append(f.c.calls, spec)
	f.c.tags = append(f.c.tags, f.tag)
	f.c.plans = append(f.c.plans, f.c.p.ReapPlan())
	f.c.mu.Unlock()
	u := fakeUniverse{spec: spec, prov: f.prov, ns: f.nsOf(spec.Name), mu: &f.c.mu, torn: &f.c.torn, tearErr: f.c.tearErr}
	return u, f.c.err
}

func (f fakeCreator) Reaps(name string) []reap.NSReap {
	return []reap.NSReap{{Kind: reap.KindYugabytedK8s, KubeContext: "ctx", Namespace: f.nsOf(name)}}
}

func dbProvider(t *testing.T, overrides ...string) (*Provider, *factory, *creators) {
	t.Helper()
	p, f := newProvider(t, Config{
		core:  load(t, overrides...),
		Stack: mekostack.Config{Release: "meko", DatabaseImage: "quay.io/yugabyte/meko-yugabytedb:2026.1.0.0-b1-meko-2"},
	})
	c := &creators{p: p}
	p.newCreator = func(prov universe.Provider, tag string) (creator, error) {
		return fakeCreator{c: c, prov: prov, tag: tag}, nil
	}
	return p, f, c
}

func names(plan *reap.Plan) []string {
	var out []string
	if plan == nil {
		return nil
	}
	for _, n := range plan.Namespaces {
		out = append(out, n.Kind+":"+n.Namespace)
	}
	return out
}

func TestTheStackDatabaseIsYugabytedRF3ByDefaultAndMekoReady(t *testing.T) {
	p, f, c := dbProvider(t)
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.calls) != 1 {
		t.Fatalf("calls = %d", len(c.calls))
	}
	s := c.calls[0]
	pr := s.Primary()
	if s.Provider != universe.Yugabyted || s.Name != "db" || pr.RF != 3 || pr.Tservers() != 3 || pr.Node.MemoryGiB != 6 || s.Image != "quay.io/yugabyte/meko-yugabytedb:2026.1.0.0-b1-meko-2" {
		t.Errorf("spec = %+v", s)
	}
	for _, flags := range []map[string]string{s.MasterGflags, s.TServerGflags} {
		if flags["ysql_yb_enable_mage"] != "true" || flags["allowed_preview_flags_csv"] != "ysql_yb_enable_mage" {
			t.Errorf("flags = %v, want mage on and allowed as a preview flag", flags)
		}
	}
	if s.MasterGflags["tablet_replicas_per_gib_limit"] != "0" {
		t.Errorf("master flags = %v", s.MasterGflags)
	}
	if got := names(c.plans[0]); len(got) != 1 || got[0] != "yugabytedk8s:mekoqa-yu-run42-db" {
		t.Errorf("when the deploy started the plan was %v, want the database already in it", got)
	}
	sb := f.stacks[0].cfg.Backing
	if sb == nil || sb.Host != "db-ysql.mekoqa-yu-run42-db.svc.cluster.local." || sb.Replication != 3 || sb.Tservers != 3 || sb.Placement["zone-a"] != 3 || sb.Mode != "yugabyted" {
		t.Errorf("stack backing = %+v", sb)
	}
	if got := names(p.ReapPlan()); strings.Join(got, ",") != "meko:mekoqa-st-run42,yugabytedk8s:mekoqa-yu-run42-db" {
		t.Errorf("plan = %v", got)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if len(c.torn) != 1 || f.stacks[0].torn != 1 || !p.ReapPlan().Empty() {
		t.Errorf("torn %v stack %d plan %v", c.torn, f.stacks[0].torn, names(p.ReapPlan()))
	}
}

func TestDBKeysShapeTheStackDatabase(t *testing.T) {
	p, _, c := dbProvider(t, "db.rf=5", "db.nodes=7", "db.cpu=2", "db.memory_gib=8", "db.disk_gib=50", "db.storage_class=premium-rwo", "db.master_gflags.foo=bar")
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	pr := c.calls[0].Primary()
	if pr.RF != 5 || pr.Tservers() != 7 || pr.Node.CPU != 2 || pr.Node.MemoryGiB != 8 || pr.Node.DiskGiB != 50 || pr.Node.StorageClass != "premium-rwo" || c.calls[0].MasterGflags["foo"] != "bar" {
		t.Errorf("spec = %+v", c.calls[0])
	}
}

func TestTheChartProviderLeavesTheBundledDatabase(t *testing.T) {
	p, f, c := dbProvider(t, "db.provider=chart")
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.calls) != 0 || f.stacks[0].cfg.Backing != nil {
		t.Errorf("a database was made for the chart provider: %+v", c.calls)
	}
}

func TestADedicatedStackTakesItsTestsDatabaseSpec(t *testing.T) {
	p, f, c := dbProvider(t)
	ctx := context.Background()
	spec := universe.Spec{Clusters: []universe.ClusterSpec{{RF: 1, Placement: map[string]int{"z": 1}}}}
	e, err := p.Provision(ctx, core.EnvSpec{Shape: core.DedicatedStack, Database: &spec}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.calls[0].Primary().RF != 1 || c.tags[0] != "run42-1" || f.stacks[0].cfg.Backing.Replication != 1 {
		t.Errorf("calls %+v tags %v", c.calls, c.tags)
	}
	if err := p.Teardown(ctx, e); err != nil {
		t.Fatal(err)
	}
	if len(c.torn) != 1 || !p.ReapPlan().Empty() {
		t.Errorf("torn %v plan %v", c.torn, names(p.ReapPlan()))
	}
}

func TestASharedStackIgnoresATestsDatabaseSpec(t *testing.T) {
	p, _, c := dbProvider(t)
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.calls[0].Primary().RF != 3 {
		t.Error("the shared stack took a test's spec")
	}
}

func TestADatabaseThatFailsLeavesNoStackAndNoPlan(t *testing.T) {
	p, f, c := dbProvider(t)
	c.err = errors.New("pods never came up")
	err := p.Prepare(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pods never came up") {
		t.Fatalf("err = %v", err)
	}
	if len(f.stacks) != 0 || len(c.torn) != 1 || !p.ReapPlan().Empty() {
		t.Errorf("stacks %d torn %v plan %v", len(f.stacks), c.torn, names(p.ReapPlan()))
	}
}

func TestADatabaseThatWillNotTearDownStaysInThePlan(t *testing.T) {
	p, _, c := dbProvider(t)
	c.tearErr = errors.New("pvc stuck")
	if err := p.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err == nil || !strings.Contains(err.Error(), "pvc stuck") {
		t.Fatalf("Close = %v", err)
	}
	if got := names(p.ReapPlan()); len(got) != 1 || !strings.HasPrefix(got[0], "yugabytedk8s:") {
		t.Errorf("plan = %v, want the database kept for a janitor", got)
	}
}

func TestAFailedStackDeployTearsTheDatabaseDownToo(t *testing.T) {
	p, f, c := dbProvider(t)
	f.deploy = func(string) error { return errors.New("migrations wedged") }
	if err := p.Prepare(context.Background()); err == nil {
		t.Fatal("Prepare succeeded")
	}
	if len(c.torn) != 1 || f.stacks[0].torn != 1 || !p.ReapPlan().Empty() {
		t.Errorf("torn %v plan %v", c.torn, names(p.ReapPlan()))
	}
}

func TestATestCreatesUniversesOfItsOwnAndTheyGoWithIt(t *testing.T) {
	p, _, c := dbProvider(t, "db.rf=1", "db.nodes=1")
	ctx := context.Background()
	if err := p.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	env, err := p.Provision(ctx, core.EnvSpec{Shape: core.SingleStack}, nil)
	if err != nil {
		t.Fatal(err)
	}
	set := env.(*Env).Universes("ab12")
	src, err := set.Create(ctx, universe.Spec{Name: "src"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Create(ctx, universe.Spec{Name: "dst", Provider: universe.YBA, Clusters: []universe.ClusterSpec{{RF: 3, Placement: map[string]int{"a": 3}}}}); err != nil {
		t.Fatal(err)
	}
	if src.Namespace() != "mekoqa-yu-run42-ab12-src" || c.tags[1] != "run42-ab12" {
		t.Errorf("ns %s tags %v", src.Namespace(), c.tags)
	}
	last := c.calls[len(c.calls)-1]
	if last.Provider != universe.YBA || last.Primary().RF != 3 || c.calls[1].Provider != universe.Yugabyted || c.calls[1].Primary().RF != 1 {
		t.Errorf("providers/specs = %+v", c.calls)
	}
	if got := names(p.ReapPlan()); len(got) != 4 {
		t.Errorf("plan = %v, want the stack, its database and the two universes", got)
	}
	if err := set.Teardown(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.torn) != 2 || !strings.HasSuffix(c.torn[0], "-dst") {
		t.Errorf("torn %v, want the last created first", c.torn)
	}
	if got := names(p.ReapPlan()); len(got) != 2 {
		t.Errorf("plan = %v, want the stack and its database", got)
	}
}

func TestUniversesRefuseTheChartProviderAndABadSpec(t *testing.T) {
	p, _, _ := dbProvider(t)
	_ = p.Prepare(context.Background())
	env, _ := p.Provision(context.Background(), core.EnvSpec{Shape: core.SingleStack}, nil)
	set := env.(*Env).Universes("x")
	if _, err := set.Create(context.Background(), universe.Spec{Name: "u", Provider: universe.Chart}); err == nil {
		t.Error("the chart provider made a universe")
	}
	bad := universe.Spec{Name: "u", Clusters: []universe.ClusterSpec{{RF: 5, Placement: map[string]int{"z": 3}}}}
	if _, err := set.Create(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "needs at least 5") {
		t.Errorf("err = %v", err)
	}
}

func TestWithMageKeepsFlagsAlreadyAllowed(t *testing.T) {
	got := withMage(map[string]string{"allowed_preview_flags_csv": "other", "x": "y"})
	if got["allowed_preview_flags_csv"] != "other,ysql_yb_enable_mage" || got["x"] != "y" {
		t.Errorf("%v", got)
	}
	if again := withMage(got); again["allowed_preview_flags_csv"] != "other,ysql_yb_enable_mage" {
		t.Errorf("duplicated: %v", again)
	}
}

func TestValidateRefusesAnUnknownProvider(t *testing.T) {
	p, _, _ := dbProvider(t, "db.provider=oracle")
	if err := p.validateDatabase(); err == nil || !strings.Contains(err.Error(), "oracle") {
		t.Errorf("err = %v", err)
	}
}
