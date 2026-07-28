package server

import (
	"net/http"
	"testing"
)

// TestHealthz verifies the liveness probe endpoint used by the Deployment.
func TestHealthz(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/healthz = %d, want 200", resp.StatusCode)
	}
}

// TestListAndGetEnvironments covers the console's read endpoints, including the
// 404 path for an unknown id.
func TestListAndGetEnvironments(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	if code, body := do(t, "GET", ts.URL+"/api/v1/environments", "", nil); code != 200 {
		t.Fatalf("list = %d, want 200 (%v)", code, body)
	}

	if code, _ := do(t, "GET", ts.URL+"/api/v1/environments/itest", "", nil); code != 200 {
		t.Fatalf("get seeded env = %d, want 200", code)
	}

	if code, _ := do(t, "GET", ts.URL+"/api/v1/environments/does-not-exist", "", nil); code != 404 {
		t.Fatalf("get unknown env = %d, want 404", code)
	}
}

// TestRegisterRequiresName rejects an environment create with no name.
func TestRegisterRequiresName(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	if code, _ := do(t, "POST", ts.URL+"/api/v1/environments", "", map[string]any{"region": "us-east-1"}); code != 400 {
		t.Fatalf("nameless register = %d, want 400", code)
	}
}

// TestEntitlements is a static read-only endpoint (plan comes from YBM Aeon).
func TestEntitlements(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	code, body := do(t, "GET", ts.URL+"/api/v1/entitlements", "", nil)
	if code != 200 {
		t.Fatalf("entitlements = %d, want 200", code)
	}
	if body["plan"] != "enterprise" {
		t.Fatalf("plan = %v, want enterprise", body["plan"])
	}
}

// TestLifecycleActions drives PATCH (suspend) and DELETE (deregister) and
// confirms the fleet state reflects each.
func TestLifecycleActions(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	if code, _ := do(t, "PATCH", ts.URL+"/api/v1/environments/itest", "", map[string]any{"action": "suspend"}); code != 202 {
		t.Fatalf("suspend = %d, want 202", code)
	}
	if _, e := do(t, "GET", ts.URL+"/api/v1/environments/itest", "", nil); e["state"] != "SUSPENDED" {
		t.Fatalf("state after suspend = %v, want SUSPENDED", e["state"])
	}

	if code, _ := do(t, "DELETE", ts.URL+"/api/v1/environments/itest", "", nil); code != 202 {
		t.Fatalf("deregister = %d, want 202", code)
	}
	if _, e := do(t, "GET", ts.URL+"/api/v1/environments/itest", "", nil); e["state"] != "DEREGISTERED" {
		t.Fatalf("state after deregister = %v, want DEREGISTERED", e["state"])
	}

	// action on an unknown env → 404
	if code, _ := do(t, "PATCH", ts.URL+"/api/v1/environments/ghost", "", map[string]any{"action": "suspend"}); code != 404 {
		t.Fatalf("suspend unknown env = %d, want 404", code)
	}
}

// TestBootstrapProgressAdvancesState is the pre-agent installer's outbound
// report driving the environment through the bootstrap state machine.
func TestBootstrapProgressAdvancesState(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	if code, _ := do(t, "POST", ts.URL+"/api/v1/agent/bootstrap-progress?envId=itest", "", map[string]any{
		"phase": "CLUSTER_PROVISIONING", "step": "create-nodepool",
	}); code != 204 {
		t.Fatalf("bootstrap-progress = %d, want 204", code)
	}
	if _, e := do(t, "GET", ts.URL+"/api/v1/environments/itest", "", nil); e["state"] != "CLUSTER_PROVISIONING" {
		t.Fatalf("state after bootstrap-progress = %v, want CLUSTER_PROVISIONING", e["state"])
	}
}

// TestAgentEndpointsRejectMissingParams covers the guard rails on the agent
// surface: desired-state needs an envId, status needs a bearer token.
func TestAgentEndpointsRejectMissingParams(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	// desired-state without envId → 400
	if code, _ := do(t, "GET", ts.URL+"/api/v1/agent/desired-state", "", nil); code != 400 {
		t.Fatalf("desired-state without envId = %d, want 400", code)
	}

	// status without a bearer → 401
	if code, _ := do(t, "POST", ts.URL+"/api/v1/agent/status?envId=itest", "", map[string]any{"environment_state": "READY"}); code != 401 {
		t.Fatalf("unauth status = %d, want 401", code)
	}

	// status with a wrong bearer → 401
	if code, _ := do(t, "POST", ts.URL+"/api/v1/agent/status?envId=itest", "not-the-token", map[string]any{"environment_state": "READY"}); code != 401 {
		t.Fatalf("bad-bearer status = %d, want 401", code)
	}
}
