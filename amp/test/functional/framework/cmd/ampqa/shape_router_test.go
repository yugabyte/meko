package main

import (
	"context"
	"errors"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/sharedk8s"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

type routerStubEnv struct{ name string }

func (e *routerStubEnv) ControllerURL() string                            { return "" }
func (e *routerStubEnv) Versions() core.DetectedVersions                  { return core.DetectedVersions{} }
func (e *routerStubEnv) Handles() core.EnvHandles                         { return core.EnvHandles{} }
func (e *routerStubEnv) Exec() exec.RemoteExecutor                        { return nil }
func (e *routerStubEnv) Collect(context.Context, core.ArtifactSink) error { return nil }

type routerStubProvider struct {
	name        string
	provisioned []core.EnvSpec
	tornDown    []core.Environment
	prepared    bool
	closed      bool
	shares      bool
}

func (p *routerStubProvider) Provision(_ context.Context, spec core.EnvSpec, _ *core.Seed) (core.Environment, error) {
	p.provisioned = append(p.provisioned, spec)
	return &routerStubEnv{name: p.name}, nil
}

func (p *routerStubProvider) Teardown(_ context.Context, env core.Environment) error {
	p.tornDown = append(p.tornDown, env)
	return nil
}

func (p *routerStubProvider) Attach(context.Context, string) (core.Environment, error) {
	return nil, errors.New("no attach")
}

func (p *routerStubProvider) Prepare(context.Context) error { p.prepared = true; return nil }
func (p *routerStubProvider) Close() error                  { p.closed = true; return nil }
func (p *routerStubProvider) SharesInfraAcrossTests() bool  { return p.shares }

func TestShapeRouterRoutesDedicatedStack(t *testing.T) {
	primary := &routerStubProvider{name: "primary", shares: true}
	dedicated := &routerStubProvider{name: "dedicated"}
	r := newShapeRouter(primary, func() (core.EnvProvider, string) { return dedicated, "" })

	seed := core.NewRunSeed(1, true)
	sharedEnv, err := r.Provision(context.Background(), core.EnvSpec{Shape: core.SharedControlPlane}, seed)
	if err != nil {
		t.Fatalf("shared provision: %v", err)
	}
	dedEnv, err := r.Provision(context.Background(), core.EnvSpec{Shape: core.DedicatedStack}, seed)
	if err != nil {
		t.Fatalf("dedicated provision: %v", err)
	}
	if len(primary.provisioned) != 1 || len(dedicated.provisioned) != 1 {
		t.Fatalf("provision routing: primary=%d dedicated=%d, want 1 and 1", len(primary.provisioned), len(dedicated.provisioned))
	}

	if err := r.Teardown(context.Background(), dedEnv); err != nil {
		t.Fatalf("dedicated teardown: %v", err)
	}
	if err := r.Teardown(context.Background(), sharedEnv); err != nil {
		t.Fatalf("shared teardown: %v", err)
	}
	if len(dedicated.tornDown) != 1 || dedicated.tornDown[0] != dedEnv {
		t.Errorf("dedicated teardown got %v", dedicated.tornDown)
	}
	if len(primary.tornDown) != 1 || primary.tornDown[0] != sharedEnv {
		t.Errorf("primary teardown got %v", primary.tornDown)
	}

	if !r.SharesInfraAcrossTests() {
		t.Error("SharesInfraAcrossTests = false, want the primary's true")
	}
	if err := r.Prepare(context.Background()); err != nil || !primary.prepared {
		t.Errorf("Prepare not forwarded to primary (err=%v prepared=%v)", err, primary.prepared)
	}
	if err := r.Close(); err != nil || !primary.closed || !dedicated.closed {
		t.Errorf("Close: err=%v primary=%v dedicated=%v, want both closed", err, primary.closed, dedicated.closed)
	}
}

func TestShapeRouterDedicatedUnavailableIsShapeSkip(t *testing.T) {
	primary := &routerStubProvider{name: "primary"}
	r := newShapeRouter(primary, func() (core.EnvProvider, string) { return nil, "no kube context" })

	_, err := r.Provision(context.Background(), core.EnvSpec{Shape: core.DedicatedStack}, core.NewRunSeed(1, true))
	var shape *core.UnsupportedShapeError
	if !errors.As(err, &shape) {
		t.Fatalf("err = %v, want UnsupportedShapeError", err)
	}
	if len(primary.provisioned) != 0 {
		t.Error("a dedicated_stack test must never fall back to the primary provider")
	}
}

func TestSharedK8sRefusesDedicatedStack(t *testing.T) {
	p := sharedk8s.New(sharedk8s.Config{ControllerURL: "http://127.0.0.1:1"})
	_, err := p.Provision(context.Background(), core.EnvSpec{Shape: core.DedicatedStack}, core.NewRunSeed(1, true))
	var shape *core.UnsupportedShapeError
	if !errors.As(err, &shape) {
		t.Fatalf("sharedk8s served a dedicated_stack spec (err=%v), want UnsupportedShapeError", err)
	}
}

func TestDeriveDedicatedConfigFillsFromRunSurface(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, []string{
		"env.sharedk8s.context=ctx-from-shared",
		"env.ampstack.controller_image=quay.io/yugabyte/amp-controller:test",
		"env.ampstack.quay_user=robot",
		"env.ampstack.quay_token=tok",
	})
	if err != nil {
		t.Fatal(err)
	}
	xcfg := deriveDedicatedConfig(cfg)
	if xcfg.Context != "ctx-from-shared" {
		t.Errorf("Context = %q, want the sharedk8s context", xcfg.Context)
	}
	if xcfg.ControllerImage != "quay.io/yugabyte/amp-controller:test" {
		t.Errorf("ControllerImage = %q, want the run's ampstack image", xcfg.ControllerImage)
	}
	if xcfg.HelmChart == "" {
		t.Error("HelmChart empty, want the ampstack controller chart")
	}
	if xcfg.QuayUser != "robot" || xcfg.QuayToken != "tok" {
		t.Errorf("quay creds = %q/%q, want the run's", xcfg.QuayUser, xcfg.QuayToken)
	}

	cfg2, err := core.LoadConfig(nil, nil, []string{
		"env.sharedk8s.context=ctx-from-shared",
		"env.exclusivek8s.context=ctx-explicit",
		"env.exclusivek8s.controller_image=custom:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	xcfg2 := deriveDedicatedConfig(cfg2)
	if xcfg2.Context != "ctx-explicit" || xcfg2.ControllerImage != "custom:1" {
		t.Errorf("explicit keys lost: context=%q image=%q", xcfg2.Context, xcfg2.ControllerImage)
	}
}
