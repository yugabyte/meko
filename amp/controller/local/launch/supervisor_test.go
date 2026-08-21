package launch

import (
	"strings"
	"testing"

	"github.com/yugabyte/amp/controller/local/config"
)

// envWithProvisionedEndpoint is a launcher env for one endpoint whose pair has
// already been provisioned: the ports below are what the supervisor has to be
// told about, because they are also in the config files it will not re-render.
func envWithProvisionedEndpoint() (*config.LocalEnv, launcherEndpoint) {
	branch := testBranchID
	env := envWithTemplates()
	env.Pageservers = []config.PageServerConf{{
		ListenPgAddr:   "127.0.0.1:64010",
		ListenHTTPAddr: "127.0.0.1:9908",
		ListenGRPCAddr: ptrStr("127.0.0.1:51061"),
		DataDir:        "/tmp/repo/endpoints/main/pageserver",
		BranchID:       &branch,
	}}
	env.Safekeepers = []config.SafekeeperConf{{
		PgPort:     5464,
		HTTPPort:   7686,
		EndpointID: "main",
		BranchID:   &branch,
	}}
	env.Controller.Yb = &config.YbConnConfig{HostPorts: "127.0.0.1:5433"}

	le := launcherEndpoint{conf: endpointConf{
		EndpointID:       "main",
		BranchID:         branch,
		PgPort:           55433,
		ExternalHTTPPort: 3080,
		InternalHTTPPort: 47119,
		SettingsHTTPPort: 47120,
		PgVersion:        18,
	}}
	return env, le
}

// The supervisor probes these ports and does the pageserver/safekeeper handshake
// over them, while the daemons bind what the launcher wrote into their config.
// A default here would produce an endpoint whose supervisor waits forever on a
// port nothing is listening on.
func TestSupervisorEnvCarriesTheAllocatedPorts(t *testing.T) {
	env, le := envWithProvisionedEndpoint()
	got, err := supervisorEnv(env, le)
	if err != nil {
		t.Fatalf("supervisorEnv: %v", err)
	}
	want := map[string]string{
		"PRERENDERED_CONFIG":             "true",
		"RUN_DIR":                        "/tmp/repo/endpoints/main",
		"BIN_DIR":                        "/yb_amp",
		"BRANCH_ID":                      "9a054903-242f-46bc-a0c9-cc6b91a37e4c",
		"COMPUTE_ID":                     "main",
		"COMPUTE_PORT":                   "55433",
		"COMPUTE_CTL_HTTP_PORT":          "3080",
		"COMPUTE_CTL_INTERNAL_HTTP_PORT": "47119",
		"PAGESERVER_PG_PORT":             "64010",
		"PAGESERVER_HTTP_PORT":           "9908",
		"PAGESERVER_GRPC_PORT":           "51061",
		"SAFEKEEPER_PG_PORT":             "5464",
		"SAFEKEEPER_HTTP_PORT":           "7686",
		"ENDPOINT_PG_VERSION":            "18",
		"SUPERVISOR_HTTP_PORT":           "47120",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// The compute's two HTTP ports come from a free-port search, so they are not
// contiguous. The supervisor defaults the internal one to external+1, which is
// right in a pod and would collide here.
func TestSupervisorEnvDoesNotLetTheInternalPortBeDerived(t *testing.T) {
	env, le := envWithProvisionedEndpoint()
	got, err := supervisorEnv(env, le)
	if err != nil {
		t.Fatalf("supervisorEnv: %v", err)
	}
	if got["COMPUTE_CTL_INTERNAL_HTTP_PORT"] == "" {
		t.Fatal("COMPUTE_CTL_INTERNAL_HTTP_PORT must be passed, not derived")
	}
}

// Without a port the supervisor serves no settings API, and a settings change
// applies only on the next endpoint restart. An endpoint created before this
// port existed has a zero here, which must not become a request to bind port 0.
func TestSupervisorEnvOmitsTheSettingsPortWhenUnallocated(t *testing.T) {
	env, le := envWithProvisionedEndpoint()
	le.conf.SettingsHTTPPort = 0
	got, err := supervisorEnv(env, le)
	if err != nil {
		t.Fatalf("supervisorEnv: %v", err)
	}
	if v, ok := got["SUPERVISOR_HTTP_PORT"]; ok {
		t.Errorf("SUPERVISOR_HTTP_PORT = %q, want it absent", v)
	}
}

// A local run kills daemons on purpose — the integration suite SIGKILLs the
// safekeeper several times in one scenario — and the pod's budget would read
// that as an endpoint that cannot stay up and exit mid-test.
func TestSupervisorEnvRaisesTheRestartBudget(t *testing.T) {
	env, le := envWithProvisionedEndpoint()
	got, err := supervisorEnv(env, le)
	if err != nil {
		t.Fatalf("supervisorEnv: %v", err)
	}
	if got["SUPERVISOR_MAX_RESTARTS"] == "" {
		t.Error("SUPERVISOR_MAX_RESTARTS must be raised above the pod default")
	}
}

// The socket path is computed independently on both sides, and a disagreement
// is a launcher that cannot find a supervisor that is running fine. This pins
// the Go half of the rule in compute/endpoint_supervisor/src/control.rs:
// beside the run dir when it fits, a short derived path when it does not.
func TestSupervisorSocketStaysBindable(t *testing.T) {
	short := &config.LocalEnv{BaseDataDir: "/data"}
	if got, want := supervisorSocket(short, "main"), "/data/endpoints/main/supervisor.sock"; got != want {
		t.Errorf("socket = %q, want %q", got, want)
	}

	// A repo checked out somewhere deep: the path is over the limit, and before
	// the fallback the endpoint came all the way up and then failed to bind.
	deep := &config.LocalEnv{BaseDataDir: "/" + strings.Repeat("x", 120)}
	got := supervisorSocket(deep, "main")
	if len(got) > sunPathMax {
		t.Errorf("socket %q is %d bytes, over the %d-byte limit", got, len(got), sunPathMax)
	}
	if got == supervisorSocket(&config.LocalEnv{BaseDataDir: "/" + strings.Repeat("y", 120)}, "main") {
		t.Error("two different run directories must not share a socket")
	}
	if got != supervisorSocket(deep, "main") {
		t.Error("the same run directory must always yield the same socket")
	}
}

// An endpoint whose pair was never provisioned has no ports to hand over, and
// saying so beats starting a supervisor that probes the defaults.
func TestSupervisorEnvRefusesAnUnprovisionedEndpoint(t *testing.T) {
	env, le := envWithProvisionedEndpoint()
	env.Safekeepers = nil
	if _, err := supervisorEnv(env, le); err == nil {
		t.Fatal("want an error when no safekeeper serves the endpoint")
	}

	env, le = envWithProvisionedEndpoint()
	env.Pageservers = nil
	if _, err := supervisorEnv(env, le); err == nil {
		t.Fatal("want an error when no pageserver serves the branch")
	}
}
