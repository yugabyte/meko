package task

import (
	"testing"

	"github.com/yugabyte/yb-amp/controller/internal/deploy"
)

// These tests cover the Registry (registry.go): the kind->handler lookup the
// worker depends on, and that DefaultRegistry / ProductionRegistry register the
// expected step catalog.

func TestRegistryRegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Lookup("absent"); ok {
		t.Error("Lookup of an unregistered kind returned ok, want not-found")
	}
	r.Register(&fakeHandler{kind: "k"})
	h, ok := r.Lookup("k")
	if !ok {
		t.Fatal("Lookup of a registered kind returned not-found")
	}
	if h.Kind() != "k" {
		t.Errorf("looked-up handler Kind() = %q, want k", h.Kind())
	}
}

func TestRegistryRegisterReplaces(t *testing.T) {
	r := NewRegistry()
	first := &fakeHandler{kind: "dup"}
	second := &fakeHandler{kind: "dup"}
	r.Register(first)
	r.Register(second)
	h, _ := r.Lookup("dup")
	if h != second {
		t.Error("a later Register of the same kind did not replace the earlier handler")
	}
}

func TestDefaultRegistryHasNoop(t *testing.T) {
	r := DefaultRegistry()
	h, ok := r.Lookup(KindNoop)
	if !ok {
		t.Fatal("DefaultRegistry is missing the noop step")
	}
	if _, isNoop := h.(NoopHandler); !isNoop {
		t.Errorf("noop kind maps to %T, want NoopHandler", h)
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
		SCEndpointURL: "http://sc:50051",
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
		h, ok := r.Lookup(kind)
		if !ok {
			t.Errorf("ProductionRegistry is missing step kind %q", kind)
			continue
		}
		if h.Kind() != kind {
			t.Errorf("kind %q maps to a handler whose Kind() = %q", kind, h.Kind())
		}
	}

	// The create_endpoint handler must carry the deploy collaborators it needs.
	h, _ := r.Lookup(StepCreateEndpoint)
	ce, ok := h.(CreateEndpointHandler)
	if !ok {
		t.Fatalf("create_endpoint handler is %T, want CreateEndpointHandler", h)
	}
	if ce.Deployer == nil || ce.Recipes == nil || ce.State == nil {
		t.Error("create_endpoint handler is missing wired dependencies")
	}
	if !ce.Bootstrap || ce.SCURL != "http://sc:50051" {
		t.Errorf("create_endpoint handler config = {bootstrap:%v scurl:%q}, want {true http://sc:50051}", ce.Bootstrap, ce.SCURL)
	}
}
