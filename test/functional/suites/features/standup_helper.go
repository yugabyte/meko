package features

import (
	"context"
	"fmt"
	"log"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/yb-amp/test/functional/framework/core"
	"github.com/yugabyte/yb-amp/test/functional/framework/env/reap"
)

// Standing-environment shapes StandUp can produce, reusing the same
// provisioners the foundation run tests use. These are the values the presets
// suite passes and that a StandingEnv records as its Shape.
const (
	ShapeYugabyted = "yugabyted" // yugabyted cluster on cloud VMs (cloud = env.vm.cloud)
	ShapeRaw       = "raw"       // raw yb-master + yb-tserver cluster on cloud VMs
	ShapeYBA       = "yba"       // an owned YBA + universe (apps.yba.provider_type)
	ShapeYBK8s     = "yb_k8s"    // a YugabyteDB cluster deployed into k8s (co-located)
)

// shapeProvisioner maps a standing-env shape to its backing provisioner and the
// gate that decides whether it may run — the exact pair the foundation run
// tests wire, so setup and run stand the backing up through identical code.
func (cfg Config) shapeProvisioner(shape string) (backingProvisioner, func(*core.Config) (string, bool), error) {
	switch shape {
	case ShapeYugabyted:
		return cfg.provisionYugabytedOnAWS, cfg.gateAWS, nil
	case ShapeRaw:
		return cfg.provisionRawYugabyteOnAWS, cfg.gateAWS, nil
	case ShapeYBA:
		return cfg.provisionYBAProvisioned, cfg.gateYBAProvisioned, nil
	case ShapeYBK8s:
		return cfg.provisionYBK8s, cfg.gateYBK8s, nil
	default:
		return nil, nil, fmt.Errorf("unknown standing-env shape %q", shape)
	}
}

// StandUp is the reusable provision-and-prepare phase behind `ampqa setup` and
// the presets suite: it stands up a backing YugabyteDB cluster of the named
// shape (reusing the foundation provisioners, no duplication), deploys the amp
// control plane on it, and returns a StandingEnv describing the running
// environment plus the teardown that reaps everything. `ampqa setup` leaves the
// environment up (discarding the teardown); a normal `run` would invoke it. It
// self-skips (via t.Skip) when the shape's gate is not satisfied.
func StandUp(t *core.T, shape string) (*core.SetupResult, error) {
	cfg := FromConfig(t.Options)
	prov, gate, err := cfg.shapeProvisioner(shape)
	if err != nil {
		return nil, err
	}
	// Same double-provision guard the foundation run bodies use: StandUp
	// self-provisions its OWN backing, so if it is ever invoked with an env layer
	// that already stood up real infra (a provisioning universe.Env), running it
	// would double-provision. Under `ampqa setup` t.Env is nil (no provider
	// provisioning) and this is a no-op; it fires only if a preset is driven
	// through the run lifecycle on a real-infra env.
	if reason, ok := gateNoDoubleProvision(t.Env); !ok {
		t.Skip(reason)
		return nil, nil
	}
	if reason, ok := gate(t.Options); !ok {
		t.Skip(reason)
		return nil, nil
	}

	f := &flow{t: t, cfg: cfg, prov: prov, td: &teardownStack{}, logf: runFlowLogf(t.Masker(), log.Printf)}
	teardown := func(ctx context.Context) error { return f.td.run(ctx, f.logf) }

	if err := f.provisionBacking(t.Context); err != nil {
		_ = teardown(context.Background())
		return nil, fmt.Errorf("provision %s backing: %w", shape, err)
	}
	if err := f.waitBackingReady(t.Context); err != nil {
		_ = teardown(context.Background())
		return nil, fmt.Errorf("%s backing not ready for amp: %w", shape, err)
	}
	if err := f.deployAmp(t.Context); err != nil {
		_ = teardown(context.Background())
		return nil, fmt.Errorf("deploy amp on %s backing: %w", shape, err)
	}

	info := map[string]string{
		"shape":          shape,
		"ysql_dsn":       f.store.ConnInfo().DSN(),
		"ysql_addr":      f.store.ConnInfo().Addr(),
		"ysql_hostports": f.store.HostPortsCSV(),
		"amp_namespace":  f.namespace(),
		"cloud":          cfg.vmConfig(t.Options).Cloud,
	}
	if cl, cerr := f.controllerClient(t.Context); cerr == nil {
		info["amp_controller_url"] = cl.BaseURL()
	}
	// deployAmp stood the amp control plane up in its OWN namespace on the
	// ampstack context, separate from the backing the provisioner recorded in
	// f.reap. A cross-process `ampqa env rm` reaps only what the plan names, so
	// the amp namespace MUST be appended explicitly or it leaks — the backing
	// reap (YB namespace / cloud tag) never touches it.
	f.reap = withAmpNamespaceReap(f.reap, ampstack.FromConfig(t.Options).Context, f.namespace())
	// Record the serializable reap plan so a separate-process `ampqa env rm`
	// reaps this shape's REAL infrastructure (k8s namespaces / cloud instances)
	// without the in-memory teardown closure — the no-leak guarantee for standing
	// preset environments.
	planJSON, err := f.reap.JSON()
	if err != nil {
		_ = teardown(context.Background())
		return nil, fmt.Errorf("serialize reap plan for %s: %w", shape, err)
	}
	if planJSON != "" {
		info["reap_plan"] = planJSON
	}

	env := &core.StandingEnv{
		Name:     "preset-" + shape + "-" + f.suffix(),
		Shape:    shape,
		Provider: "functional",
		Ref:      f.suffix(),
		Info:     info,
	}
	return &core.SetupResult{Env: env, Teardown: teardown}, nil
}

// withAmpNamespaceReap appends the amp control-plane namespace (deployed by
// deployAmp on the ampstack context, separate from the backing) to the backing
// provisioner's reap plan, so a cross-process `ampqa env rm` reaps amp too. It
// tolerates a nil plan (VM shapes hand back a cloud-only plan; a defensive nil
// still yields a valid amp-only plan).
func withAmpNamespaceReap(plan *reap.Plan, kubeContext, namespace string) *reap.Plan {
	if plan == nil {
		plan = &reap.Plan{}
	}
	plan.Namespaces = append(plan.Namespaces, reap.NSReap{
		Kind:        reap.KindAmp,
		KubeContext: kubeContext,
		Namespace:   namespace,
	})
	return plan
}
