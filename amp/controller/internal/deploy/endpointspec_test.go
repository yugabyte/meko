package deploy

import (
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
)

// TestEndpointSpecFromRendersEveryField pins every field of the assembled spec.
// Omitting one is silent — the deploy succeeds and the endpoint comes back
// missing a mount or reverted to the chart's default size — so completeness is
// asserted here, once, rather than in each calling step. The reflect sweep is the
// guard for a field added to EndpointSpec that the assembler forgets.
func TestEndpointSpecFromRendersEveryField(t *testing.T) {
	epID, branchID := uuid.New(), uuid.New()
	yb := YBConn{HostPorts: "yb:5433", User: "yugabyte", DBName: "yb_storage"}
	mounts := []Extension{{Name: "pgvector", Image: "quay.io/yugabyte/amp-ext-pgvector:0.8.2"}}

	spec := EndpointSpecFrom(core.EndpointDeploy{
		ID: epID, BranchID: branchID, PgVersion: 18,
		CPU: "8", Memory: "16Gi",
	}, yb, "http://sc:50051", mounts)

	if spec.Name != EndpointName(epID) {
		t.Errorf("Name = %q, want %q", spec.Name, EndpointName(epID))
	}
	if spec.EndpointID != epID.String() {
		t.Errorf("EndpointID = %q, want %q", spec.EndpointID, epID)
	}
	if spec.BranchID != branchID {
		t.Errorf("BranchID = %v, want %v (the storage branch id verbatim)", spec.BranchID, branchID)
	}
	if spec.PgVersion != 18 {
		t.Errorf("PgVersion = %d, want 18", spec.PgVersion)
	}
	if spec.YB != yb {
		t.Errorf("YB = %+v, want %+v", spec.YB, yb)
	}
	if spec.ControllerURL != "http://sc:50051" {
		t.Errorf("ControllerURL = %q, want http://sc:50051", spec.ControllerURL)
	}
	if spec.CPU != "8" || spec.Memory != "16Gi" {
		t.Errorf("CPU/Memory = %q/%q, want 8/16Gi", spec.CPU, spec.Memory)
	}
	if !reflect.DeepEqual(spec.Extensions, mounts) {
		t.Errorf("Extensions = %+v, want %+v", spec.Extensions, mounts)
	}

	v := reflect.ValueOf(spec)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Errorf("EndpointSpec.%s was not rendered (zero value) — every field must come from the deploy record or the explicit inputs",
				v.Type().Field(i).Name)
		}
	}
}

// An endpoint that mounts nothing renders nil, not an empty slice: the recreate
// config the callers derive from the spec omits the key entirely rather than
// recording an empty set.
func TestEndpointSpecFromWithoutMounts(t *testing.T) {
	spec := EndpointSpecFrom(core.EndpointDeploy{ID: uuid.New(), BranchID: uuid.New()}, YBConn{}, "", nil)
	if spec.Extensions != nil {
		t.Errorf("Extensions = %+v, want nil", spec.Extensions)
	}
}
