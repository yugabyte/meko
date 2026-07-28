package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"meko/mccp/internal/store"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st := store.NewMemory()
	if err := st.SeedEnrollment("itest", "cloud", "mko_enr_seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return httptest.NewServer(Handler(st))
}

func do(t *testing.T, method, url, bearer string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, url, rdr)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestEnrollmentHandshake(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	base := ts.URL

	// enroll with a bad token → 401
	if code, _ := do(t, "POST", base+"/api/v1/agent/enroll", "", map[string]any{"name": "itest", "enrollment_token": "wrong"}); code != 401 {
		t.Fatalf("bad-token enroll = %d, want 401", code)
	}

	// enroll with the seeded token → 200 + status token
	code, body := do(t, "POST", base+"/api/v1/agent/enroll", "", map[string]any{
		"name": "itest", "enrollment_token": "mko_enr_seed",
		"cluster_facts": map[string]any{"node_count": 3, "region": "us-west1"},
	})
	if code != 200 {
		t.Fatalf("enroll = %d, want 200", code)
	}
	status, _ := body["status_token"].(string)
	if status == "" {
		t.Fatal("no status_token returned")
	}

	// status without a bearer → 401
	if code, _ := do(t, "POST", base+"/api/v1/agent/status?envId=itest", "", map[string]any{"environment_state": "READY"}); code != 401 {
		t.Fatalf("unauth status = %d, want 401", code)
	}

	// status with the bearer → 204
	code, _ = do(t, "POST", base+"/api/v1/agent/status?envId=itest", status, map[string]any{
		"environment_state": "READY", "cluster_facts": map[string]any{"node_count": 3},
	})
	if code != 204 {
		t.Fatalf("authed status = %d, want 204", code)
	}

	// reusable within TTL: re-enrolling with the same token → 200 (agent restart case)
	if code, _ := do(t, "POST", base+"/api/v1/agent/enroll", "", map[string]any{"name": "itest", "enrollment_token": "mko_enr_seed"}); code != 200 {
		t.Fatalf("reused-token enroll = %d, want 200 (reusable within TTL)", code)
	}
}

// TestControlPlaneLifecycle exercises the full pull loop through the HTTP API:
// register -> enroll -> set desired -> authed desired-state -> agent status report
// -> the fleet + datapacks endpoints reflect the reported provisioned state.
func TestControlPlaneLifecycle(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	base := ts.URL

	// admin registers an environment (mints an enrollment token)
	_, reg := do(t, "POST", base+"/api/v1/environments", "", map[string]any{"name": "acme", "region": "us-east-1"})
	tok, _ := reg["enrollment_token"].(string)
	if tok == "" {
		t.Fatal("no enrollment token")
	}

	// agent enrolls -> status token
	_, en := do(t, "POST", base+"/api/v1/agent/enroll", "", map[string]any{
		"name": "acme", "enrollment_token": tok, "cluster_facts": map[string]any{"provider": "gke", "node_count": 3},
	})
	status, _ := en["status_token"].(string)
	if status == "" {
		t.Fatal("no status token")
	}

	// admin declares a desired datapack
	if code, _ := do(t, "POST", base+"/api/v1/environments/acme/desired", "", map[string]any{
		"resources": []any{map[string]any{"kind": "datapack", "name": "orders"}},
	}); code != 202 {
		t.Fatalf("set desired = %d", code)
	}

	// agent pulls desired-state (authed) and sees it
	_, ds := do(t, "GET", base+"/api/v1/agent/desired-state?envId=acme", status, nil)
	if res, _ := ds["resources"].([]any); len(res) != 1 {
		t.Fatalf("desired-state resources = %d, want 1", len(res))
	}

	// agent reports it provisioned (real db + endpoint), READY
	if code, _ := do(t, "POST", base+"/api/v1/agent/status?envId=acme", status, map[string]any{
		"environment_state": "READY",
		"datapack_phases": []any{map[string]any{
			"id": "mccp-dp-orders", "name": "orders", "phase": "Active",
			"db": "dp_orders", "endpoint": "http://mcp.mccp-dp-orders.svc:3000",
		}},
	}); code != 204 {
		t.Fatalf("status = %d", code)
	}

	// fleet + datapacks reflect the reported state
	_, env := do(t, "GET", base+"/api/v1/environments/acme", "", nil)
	if env["state"] != "READY" || env["datapacks"].(float64) != 1 {
		t.Fatalf("env after status = state:%v datapacks:%v", env["state"], env["datapacks"])
	}
	_, _ = env, ds
	code, _ := do(t, "GET", base+"/api/v1/datapacks?envId=acme", "", nil)
	if code != 200 {
		t.Fatalf("datapacks = %d", code)
	}
	// verify the datapack carries the reported db + endpoint
	resp, err := http.Get(base + "/api/v1/datapacks?envId=acme")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var dps []map[string]any
	json.NewDecoder(resp.Body).Decode(&dps)
	if len(dps) != 1 || dps[0]["db"] != "dp_orders" || dps[0]["endpoint"] != "http://mcp.mccp-dp-orders.svc:3000" {
		t.Fatalf("datapacks = %v", dps)
	}
}

func TestRegisterMintsTokenAndDesiredDatapacks(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	base := ts.URL

	// register an environment → mints an enrollment token
	code, body := do(t, "POST", base+"/api/v1/environments", "", map[string]any{"name": "acme", "region": "us-east-1"})
	if code != 201 {
		t.Fatalf("register = %d, want 201", code)
	}
	tok, _ := body["enrollment_token"].(string)
	if tok == "" {
		t.Fatal("register did not mint an enrollment_token")
	}

	// enroll the new env with the minted token
	_, ebody := do(t, "POST", base+"/api/v1/agent/enroll", "", map[string]any{"name": "acme", "enrollment_token": tok})
	status, _ := ebody["status_token"].(string)
	if status == "" {
		t.Fatal("enroll with minted token failed")
	}

	// set desired datapacks
	if code, _ := do(t, "POST", base+"/api/v1/environments/acme/datapacks", "", map[string]any{"names": []string{"alpha", "beta"}}); code != 202 {
		t.Fatalf("set desired = %d, want 202", code)
	}

	// desired-state (bearer-authed) returns them as generic kind="datapack" resources
	code, ds := do(t, "GET", base+"/api/v1/agent/desired-state?envId=acme", status, nil)
	if code != 200 {
		t.Fatalf("desired-state = %d, want 200", code)
	}
	res, _ := ds["resources"].([]any)
	if len(res) != 2 {
		t.Fatalf("desired-state resources = %d, want 2", len(res))
	}
	first, _ := res[0].(map[string]any)
	if first["kind"] != "datapack" {
		t.Fatalf("resource kind = %v, want datapack", first["kind"])
	}
}
