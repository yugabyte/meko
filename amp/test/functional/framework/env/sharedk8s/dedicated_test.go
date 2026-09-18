package sharedk8s

import (
	"context"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybbacking"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/reap"
)

// fakeBacking stands in for a ybdeploy.Provisioner: it records the config it
// was built from and serves a co-located store so the storage create runs
// through the recorded kubectl rather than a dial.
type fakeBacking struct {
	cfg        *core.Config
	mode       string
	ybaNS      string
	provisions int
	closed     int
	tornDown   int
}

func (f *fakeBacking) Provision(context.Context, string) (ybbacking.BackingStore, func(context.Context) error, *reap.Plan, error) {
	f.provisions++
	store := ybbacking.Colocated{
		Static: ybbacking.Static{
			Conn:  pg.ConnInfo{Host: "10.1.1.1", Port: 5433, User: "yugabyte", Database: "yugabyte"},
			Addrs: []string{"10.1.1.1:5433", "10.2.1.1:5433", "10.3.1.1:5433"},
		},
		ControllerAddrs: []string{"10.1.1.1:5433", "10.2.1.1:5433", "10.3.1.1:5433"},
		KubeNamespace:   "yb-dev-ampqa-mr",
		KubePod:         "yb-tserver-0",
	}
	td := func(context.Context) error { f.tornDown++; return nil }
	return store, td, &reap.Plan{Cloud: &reap.CloudReap{Cloud: "gcp", RunTag: "mr"}}, nil
}

func (f *fakeBacking) Plan() *reap.Plan {
	if f.ybaNS == "" {
		return nil
	}
	return &reap.Plan{Namespaces: []reap.NSReap{{Kind: reap.KindYBAStack, KubeContext: "itest", Namespace: f.ybaNS}}}
}

func (f *fakeBacking) Close(context.Context) error { f.closed++; return nil }

// A test declaring its own BackingConfig gets a backing built from the overlaid
// config on a provisioner of its own, which its stack closes after the backing.
func TestDedicatedStackBackingConfigGetsItsOwnProvisioner(t *testing.T) {
	f := newFakeCtl()
	defer f.close()
	rec := &k8sRecorder{}
	p := testDeployProvider(f, rec)
	cfg, err := core.LoadConfig(nil, nil, []string{"env.yb_deploy=yb_k8s"})
	if err != nil {
		t.Fatal(err)
	}
	p.WithYBDeployConfig(cfg)
	var own *fakeBacking
	shared := 0
	p.newBackingProvisioner = func(c *core.Config, mode string) (backingProvisioner, error) {
		own = &fakeBacking{cfg: c, mode: mode, ybaNS: "ampqa-yba-own"}
		return own, nil
	}
	p.backing = &fakeBacking{} // the run's shared provisioner must stay unused
	p.backing.(*fakeBacking).provisions = shared

	spec := core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack,
		BackingConfig: func(c *core.Config) map[string]string {
			if c.GetString("env.yb_deploy", "") != "yb_k8s" {
				t.Errorf("overlay fn got env.yb_deploy=%q, want the run config", c.GetString("env.yb_deploy", ""))
			}
			return map[string]string{"env.yb_deploy": "yba_provisioned", "apps.yba.provider_type": "gcp", "apps.yba.gcp.num_nodes": "int:3"}
		}}
	env, err := p.Provision(context.Background(), spec, core.NewRunSeed(9, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if own == nil || own.provisions != 1 {
		t.Fatalf("own provisioner = %+v, want built and used once", own)
	}
	if own.mode != "yba_provisioned" || own.cfg.GetString("apps.yba.provider_type", "") != "gcp" || own.cfg.GetInt("apps.yba.gcp.num_nodes", 0) != 3 {
		t.Errorf("own provisioner built from mode %q cfg provider_type=%q num_nodes=%d, want the overlay applied",
			own.mode, own.cfg.GetString("apps.yba.provider_type", ""), own.cfg.GetInt("apps.yba.gcp.num_nodes", 0))
	}
	if cfg.GetString("env.yb_deploy", "") != "yb_k8s" {
		t.Errorf("the run config was mutated by the overlay")
	}
	if p.backing.(*fakeBacking).provisions != shared {
		t.Errorf("the shared provisioner served a test that declared its own backing")
	}
	e := env.(*Env)
	if e.own.ampCfg.Backing.Mode != "external" || !strings.Contains(e.own.ampCfg.Backing.HostPorts, "10.2.1.1:5433") {
		t.Errorf("stack backing = %+v, want the own store", e.own.ampCfg.Backing)
	}
	plan := p.ReapPlan()
	if plan == nil || plan.Cloud == nil || plan.Cloud.RunTag != "mr" || !hasNS(plan, "ampqa-yba-own") {
		t.Errorf("reap plan must name the own universe and its YBA: %+v", plan)
	}

	if err := p.Teardown(context.Background(), env); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if own.tornDown != 1 || own.closed != 1 {
		t.Errorf("own backing torn down %d, YBA closed %d; want 1 and 1", own.tornDown, own.closed)
	}
	if plan := p.ReapPlan(); plan != nil && hasNS(plan, "ampqa-yba-own") {
		t.Errorf("reap plan still names the closed YBA")
	}
}

// Without a BackingConfig a dedicated stack keeps using the run's shared backing.
func TestDedicatedStackWithoutBackingConfigUsesTheSharedProvisioner(t *testing.T) {
	f := newFakeCtl()
	defer f.close()
	rec := &k8sRecorder{}
	p := testDeployProvider(f, rec)
	cfg, err := core.LoadConfig(nil, nil, []string{"env.yb_deploy=yba_provisioned"})
	if err != nil {
		t.Fatal(err)
	}
	p.WithYBDeployConfig(cfg)
	p.newBackingProvisioner = func(*core.Config, string) (backingProvisioner, error) {
		t.Fatal("an own provisioner was built for a test with no BackingConfig")
		return nil, nil
	}
	shared := &fakeBacking{}
	p.backing = shared
	env, err := p.Provision(context.Background(), core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack}, core.NewRunSeed(9, true))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if shared.provisions != 1 {
		t.Errorf("shared provisioner provisions = %d, want 1", shared.provisions)
	}
	if err := p.Teardown(context.Background(), env); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if shared.closed != 0 {
		t.Errorf("a stack must not close the run's shared YBA")
	}
}
