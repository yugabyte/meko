package features

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
)

// rejectingCtl answers every create with the scripted status, recording what
// was asked. status 0 accepts.
type rejectingCtl struct {
	status    int
	endpoints []controller.CreateEndpointRequest
	clusters  []controller.CreateClusterRequest
}

func (r *rejectingCtl) reply() error {
	if r.status == 0 {
		return nil
	}
	return &controller.APIError{StatusCode: r.status, Code: "invalid_request"}
}

func (r *rejectingCtl) CreateCluster(_ context.Context, _ string, req controller.CreateClusterRequest) (controller.Task, string, error) {
	r.clusters = append(r.clusters, req)
	return controller.Task{}, "", r.reply()
}

func (r *rejectingCtl) CreateEndpoint(_ context.Context, _, _, _ string, req controller.CreateEndpointRequest) (controller.Task, string, error) {
	r.endpoints = append(r.endpoints, req)
	return controller.Task{}, "", r.reply()
}

func TestConnectivityValidationDemandsFour400s(t *testing.T) {
	proj := ampCluster{poolID: "p", projID: "c", branchID: "b"}
	ctl := &rejectingCtl{status: http.StatusBadRequest}
	if err := connectivityValidationSteps(context.Background(), ctl, proj, "conn-x", 16); err != nil {
		t.Fatalf("all-400 controller: %v", err)
	}
	if len(ctl.endpoints) != 3 || len(ctl.clusters) != 1 {
		t.Fatalf("sent %d endpoint and %d cluster creates, want 3 and 1", len(ctl.endpoints), len(ctl.clusters))
	}
	if ctl.clusters[0].WithWriter || ctl.clusters[0].Connectivity == nil {
		t.Errorf("the cluster case must carry connectivity WITHOUT with_writer: %+v", ctl.clusters[0])
	}
	for _, req := range ctl.endpoints {
		if req.Connectivity == nil {
			t.Errorf("an endpoint case sent no connectivity block: %+v", req)
		}
	}

	accepted := &rejectingCtl{}
	err := connectivityValidationSteps(context.Background(), accepted, proj, "conn-x", 16)
	if err == nil || !strings.Contains(err.Error(), "accepted, want 400") {
		t.Errorf("an accepting controller must fail the step, got %v", err)
	}

	server := &rejectingCtl{status: http.StatusInternalServerError}
	err = connectivityValidationSteps(context.Background(), server, proj, "conn-x", 16)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500, want 400") {
		t.Errorf("a 500 must be reported as the wrong status, got %v", err)
	}
}

func TestLBServiceVerdict(t *testing.T) {
	gke := parseLBService("LoadBalancer|55433|map[networking.gke.io/load-balancer-type:Internal]")
	if err := lbServiceVerdict(gke, "gcp", "55433"); err != nil {
		t.Errorf("gke internal: %v", err)
	}
	aws := parseLBService("LoadBalancer|55433|map[service.beta.kubernetes.io/aws-load-balancer-scheme:internal service.beta.kubernetes.io/aws-load-balancer-type:nlb]")
	if err := lbServiceVerdict(aws, "aws", "55433"); err != nil {
		t.Errorf("aws internal: %v", err)
	}
	cases := map[string]lbService{
		"ClusterIP":          parseLBService("ClusterIP|55433|map[networking.gke.io/load-balancer-type:Internal]"),
		"supervisor exposed": parseLBService("LoadBalancer|55433 9899|map[networking.gke.io/load-balancer-type:Internal]"),
		"internet-facing":    parseLBService("LoadBalancer|55433|map[]"),
	}
	for name, svc := range cases {
		if err := lbServiceVerdict(svc, "gcp", "55433"); err == nil {
			t.Errorf("%s: want a verdict error", name)
		}
	}
	if err := lbServiceVerdict(gke, "kind", "55433"); err == nil {
		t.Error("an unknown cloud has no internal annotation to check and must not pass")
	}
}

func TestLBAddressVerdict(t *testing.T) {
	if err := lbAddressVerdict("abc.elb.amazonaws.com:55433", "abc.elb.amazonaws.com"); err != nil {
		t.Errorf("hostname match: %v", err)
	}
	if err := lbAddressVerdict("10.1.2.3:55433", "10.1.2.3"); err != nil {
		t.Errorf("ip match: %v", err)
	}
	err := lbAddressVerdict("10.244.0.9:55433", "10.1.2.3")
	if err == nil || !strings.Contains(err.Error(), "fell back") {
		t.Errorf("a pod IP must be reported as the fallback, got %v", err)
	}
}

func TestCloudFromProviderID(t *testing.T) {
	for id, want := range map[string]string{
		"gce://yugabyte/us-west1-b/gke-node-1": "gcp",
		"aws://us-west-2a/i-0123":              "aws",
		"azure:///subscriptions/x/vm":          "azure",
		"kind://docker/kind/kind-worker":       "",
		"":                                     "",
	} {
		if got := cloudFromProviderID(id); got != want {
			t.Errorf("%q: got %q, want %q", id, got, want)
		}
	}
}
