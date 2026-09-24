package deploy

import (
	"context"
	"reflect"
	"strings"
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
		CPU: "8", Memory: "16Gi", Version: "ep-v7.7.7",
		Connectivity: ConnectivityNLBInternal, AllowedCIDRs: []string{"10.0.0.0/16"},
		IngressShard: "pgin-0", IngressPort: 20007, IngressDNSName: "ep-abc.pg.example",
		IngressShardHost: "lb-0.example", IngressRecordType: "CNAME",
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
	if spec.Ingress.Shard != "pgin-0" || spec.Ingress.Port != 20007 ||
		spec.Ingress.DNSName != "ep-abc.pg.example" || spec.Ingress.ShardHost != "lb-0.example" ||
		spec.Ingress.RecordType != "CNAME" {
		t.Errorf("Ingress = %+v, want the slot from the deploy record: dropping any of it "+
			"unpublishes the endpoint's route or its name on the next redeploy", spec.Ingress)
	}
	if spec.PgVersion != 18 {
		t.Errorf("PgVersion = %d, want 18", spec.PgVersion)
	}
	if spec.Version != "ep-v7.7.7" {
		t.Errorf("Version = %q, want ep-v7.7.7 (the endpoint's own, not the deployer default)", spec.Version)
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
	// Carried, not defaulted: a redeploy that renders cluster_ip here prunes the
	// -lb Service and silently drops an nlb_* endpoint's load balancer.
	if spec.Connectivity != ConnectivityNLBInternal {
		t.Errorf("Connectivity = %q, want %q", spec.Connectivity, ConnectivityNLBInternal)
	}
	if !reflect.DeepEqual(spec.AllowedCIDRs, []string{"10.0.0.0/16"}) {
		t.Errorf("AllowedCIDRs = %+v, want [10.0.0.0/16]", spec.AllowedCIDRs)
	}
	// PasswordVerifier is the one field this assembler must NOT fill, so it is exempt
	// from the sweep below and asserted empty instead. Nothing stores the
	// verifier — see EndpointSpec.PasswordVerifier — so a redeploy rendering one
	// could only render a WRONG one, resetting a password the caller already
	// holds. The create step sets it directly.
	if spec.PasswordVerifier != "" {
		t.Errorf("PasswordVerifier = %q, want empty: a redeploy must not re-password the compute user", spec.PasswordVerifier)
	}

	v := reflect.ValueOf(spec)
	for i := range v.NumField() {
		if v.Type().Field(i).Name == "PasswordVerifier" {
			continue
		}
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

// A spec rendered from a record that lost its slot must not deploy. Reporting
// ":0" would send SetEndpointAddress over the address clients are using, which
// unpublishes a live endpoint with no error anywhere — the failure mode the
// projection of the slot onto EndpointDeploy exists to prevent, caught here too
// because a deploy path is the last place it can still be stopped.
func TestCreateEndpointRefusesAnIngressSpecWithNoSlot(t *testing.T) {
	k := NewKubernetesDeployer(testKubernetesConfig(), nil)
	spec := testEndpointSpec()
	spec.Connectivity = ConnectivityIngressInternal
	spec.Ingress = IngressSlot{} // what a redeploy from a slotless record renders

	_, err := k.CreateEndpoint(context.Background(), spec)
	if err == nil {
		t.Fatal("deploying an ingress endpoint with no slot must fail, not publish \":0\"")
	}
	if !strings.Contains(err.Error(), "no ingress slot") {
		t.Errorf("err = %v, want it to name the missing slot", err)
	}
}
