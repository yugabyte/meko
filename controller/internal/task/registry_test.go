package task

import (
	"encoding/json"
	"testing"

	"github.com/yugabyte/yb-amp/controller/internal/deploy"
)

// These tests cover the Registry (registry.go): the kind->builder reconstruction
// the worker depends on (decode persisted args + bind deps -> typed step), and
// that DefaultRegistry / ProductionRegistry register the expected step catalog
// wired with their deps.

func TestRegistryBuildAndMissing(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Build("absent", nil); err == nil {
		t.Error("Build of an unregistered kind returned nil error, want not-found")
	}
	r.Register("k", func(json.RawMessage, Deps) (Step, error) { return &fakeStep{kind: "k"}, nil })
	step, err := r.Build("k", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Build of a registered kind: %v", err)
	}
	if step.Kind() != "k" {
		t.Errorf("built step Kind() = %q, want k", step.Kind())
	}
}

func TestRegistryRegisterReplaces(t *testing.T) {
	r := NewRegistry()
	r.Register("dup", func(json.RawMessage, Deps) (Step, error) { return &fakeStep{kind: "first"}, nil })
	r.Register("dup", func(json.RawMessage, Deps) (Step, error) { return &fakeStep{kind: "second"}, nil })
	step, _ := r.Build("dup", nil)
	if step.Kind() != "second" {
		t.Errorf("a later Register of the same kind did not replace the earlier builder: got %q", step.Kind())
	}
}

func TestDefaultRegistryHasNoop(t *testing.T) {
	r := DefaultRegistry(Deps{})
	step, err := r.Build(KindNoop, nil)
	if err != nil {
		t.Fatalf("DefaultRegistry is missing the noop step: %v", err)
	}
	if _, isNoop := step.(noopStep); !isNoop {
		t.Errorf("noop kind builds %T, want noopStep", step)
	}
}

func TestProductionRegistryHasFullCatalog(t *testing.T) {
	deps := Deps{
		State:    newFakeStateStore(),
		Recipes:  newFakeRecipeStore(),
		Deployer: &fakeDeployer{},
		EndpointYB: deploy.YBConn{
			HostPorts: "yb:5433", User: "yugabyte", DBName: "yb_storage",
		},
		ControllerURL: "http://sc:50051",
		Bootstrap:     true,
	}
	r := ProductionRegistry(deps)

	for _, kind := range []string{
		KindNoop,
		StepCreateProject,
		StepCreateBranch,
		StepRecordBranchRecipe,
		StepCreateEndpoint,
		StepDeleteEndpoint,
	} {
		// Empty args decode into a zero-valued step, so every kind builds.
		step, err := r.Build(kind, json.RawMessage(`{}`))
		if err != nil {
			t.Errorf("ProductionRegistry cannot build step kind %q: %v", kind, err)
			continue
		}
		if step.Kind() != kind {
			t.Errorf("kind %q builds a step whose Kind() = %q", kind, step.Kind())
		}
	}

	// The create_endpoint step is built wired with the deploy collaborators from
	// deps (bound at construction, not at Execute time).
	step, _ := r.Build(StepCreateEndpoint, json.RawMessage(`{}`))
	ce, ok := step.(*createEndpointStep)
	if !ok {
		t.Fatalf("create_endpoint builds %T, want *createEndpointStep", step)
	}
	if ce.deployer == nil || ce.recipes == nil || ce.state == nil {
		t.Error("create_endpoint step is missing wired dependencies")
	}
	if !ce.bootstrap || ce.scURL != "http://sc:50051" {
		t.Errorf("create_endpoint config = {bootstrap:%v scurl:%q}, want {true http://sc:50051}", ce.bootstrap, ce.scURL)
	}
}

func TestRegistryBuildRejectsBadArgs(t *testing.T) {
	// Undecodable args are a build error for every real step kind — the worker
	// fails the step rather than run it half-formed. (The noop step ignores args.)
	r := ProductionRegistry(Deps{})
	bad := json.RawMessage(`not json`)
	for _, kind := range []string{
		StepCreateProject,
		StepCreateBranch,
		StepRecordBranchRecipe,
		StepCreateEndpoint,
		StepDeleteEndpoint,
	} {
		if _, err := r.Build(kind, bad); err == nil {
			t.Errorf("%s: Build on bad args returned nil error, want a decode error", kind)
		}
	}
}
