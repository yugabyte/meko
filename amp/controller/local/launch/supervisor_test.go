package launch

import (
	"crypto/ed25519"
	"encoding/json"

	"github.com/google/uuid"
	"strings"
	"testing"

	"github.com/yugabyte/amp/controller/local/cfg"
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

	le := launcherEndpoint{ID: uuid.MustParse("3f2b1c4e-0000-4000-8000-000000000001"), conf: endpointConf{
		EndpointID:       "main",
		BranchID:         branch,
		PgPort:           55433,
		ExternalHTTPPort: 3080,
		InternalHTTPPort: 47119,
		SettingsHTTPPort: 47120,
		LFCStatePort:     47121,
		PgVersion:        18,
		// A real endpoint gets this at create (see `endpoint create`), and the
		// supervisor needs it to render a spec whose compute_ctl will accept the
		// launcher's own probes.
		ComputeCtlConfig: cfg.ComputeCtlConfig{
			Jwks: cfg.BuildJWKS(make(ed25519.PublicKey, ed25519.PublicKeySize)),
		},
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
		"PRERENDERED_CONFIG":             "daemons",
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
		// Endpoints share the host, so the pod's fixed LFC state port would
		// collide; each gets its own.
		"LFC_STATE_PORT": "47121",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// The launcher brings the daemon tomls and lets the supervisor render the compute
// spec, so the three inputs that decision depends on all have to be there. Each
// has a failure that is quiet rather than loud: without ENDPOINT_ID the compute
// silently comes up on derived defaults instead of the operator's pg_settings;
// without the extra settings it loses the local-only wiring; and without the
// JWKS, compute_ctl rejects the launcher's own /status probe, which looks like an
// endpoint that never started.
func TestSupervisorEnvCarriesWhatTheSupervisorRendersTheSpecFrom(t *testing.T) {
	env, le := envWithProvisionedEndpoint()
	got, err := supervisorEnv(env, le)
	if err != nil {
		t.Fatalf("supervisorEnv: %v", err)
	}
	if got["PRERENDERED_CONFIG"] != "daemons" {
		t.Errorf("PRERENDERED_CONFIG = %q, want \"daemons\" — the supervisor must render the compute spec", got["PRERENDERED_CONFIG"])
	}
	// A UUID, not the handle: the supervisor only warns when it cannot parse this,
	// so the compute comes up quietly missing every operator setting.
	if _, err := uuid.Parse(got["ENDPOINT_ID"]); err != nil {
		t.Errorf("ENDPOINT_ID = %q, want the registry row's UUID: %v", got["ENDPOINT_ID"], err)
	}
	if got["ENDPOINT_ID"] == got["COMPUTE_ID"] {
		t.Error("ENDPOINT_ID must be the registry id, not the endpoint handle COMPUTE_ID carries")
	}

	var extra map[string]string
	if err := json.Unmarshal([]byte(got["COMPUTE_EXTRA_SETTINGS_JSON"]), &extra); err != nil {
		t.Fatalf("COMPUTE_EXTRA_SETTINGS_JSON is not a flat JSON object: %v", err)
	}
	// Loopback rather than the pod's 0.0.0.0 is the one that matters most here:
	// a local compute must not be published on every interface of a shared box.
	if extra["listen_addresses"] != "127.0.0.1" {
		t.Errorf("listen_addresses = %q, want 127.0.0.1", extra["listen_addresses"])
	}
	for _, name := range []string{"shared_buffers", "max_replication_write_lag", "hot_standby"} {
		if extra[name] == "" {
			t.Errorf("%s missing from COMPUTE_EXTRA_SETTINGS_JSON", name)
		}
	}
	// The invariants are the supervisor's to render; a copy here would be the
	// second list that drifted before (https://github.com/yugabyte/meko/issues/1050).
	for _, name := range []string{"fsync", "full_page_writes", "wal_log_hints", "wal_level", "max_replication_flush_lag"} {
		if _, ok := extra[name]; ok {
			t.Errorf("%s must come from the supervisor's invariants, not from the launcher", name)
		}
	}

	var jwks struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal([]byte(got["COMPUTE_CTL_JWKS_JSON"]), &jwks); err != nil {
		t.Fatalf("COMPUTE_CTL_JWKS_JSON is not a JWKS object: %v", err)
	}
	if len(jwks.Keys) == 0 {
		t.Error("COMPUTE_CTL_JWKS_JSON has no keys — compute_ctl would reject the launcher's own probes")
	}
}

// walproposer refuses to start against more than one safekeeper ("yb_amp.
// safekeepers must name exactly one"), so a second endpoint on a fork wedged the
// whole cluster the moment the environment had two — the compute crash-looped its
// WAL proposer while the supervisor reported every service healthy. One endpoint
// hid it, because the only safekeeper was also the right one.
func TestSupervisorEnvNamesOnlyThisEndpointsSafekeeper(t *testing.T) {
	env, le := envWithProvisionedEndpoint()
	// A second endpoint's safekeeper, as `branch_ancestor` creates.
	other := testBranchID
	env.Safekeepers = append(env.Safekeepers, config.SafekeeperConf{
		PgPort:     5474,
		HTTPPort:   7696,
		EndpointID: "ep_anc_b1",
		BranchID:   &other,
	})

	got, err := supervisorEnv(env, le)
	if err != nil {
		t.Fatalf("supervisorEnv: %v", err)
	}
	if want := "5464"; got["SAFEKEEPER_COMPUTE_PORT"] != want {
		t.Errorf("SAFEKEEPER_COMPUTE_PORT = %q, want %q — this endpoint's own safekeeper",
			got["SAFEKEEPER_COMPUTE_PORT"], want)
	}
	// And nothing names it a second time: Postgres would take the last
	// occurrence while compute_ctl's ParsedSpec takes the first, so the two
	// would target different safekeepers.
	var extra map[string]string
	if err := json.Unmarshal([]byte(got["COMPUTE_EXTRA_SETTINGS_JSON"]), &extra); err != nil {
		t.Fatalf("COMPUTE_EXTRA_SETTINGS_JSON: %v", err)
	}
	if v, ok := extra["yb_amp.safekeepers"]; ok {
		t.Errorf("yb_amp.safekeepers = %q in the extra settings; the supervisor renders it once from SAFEKEEPER_COMPUTE_PORT", v)
	}
}

// The supervisor reads amp_registry itself now, and `fetch_overrides` downgrades
// a failed read to a warning — so a connection that is missing its sslmode or
// its password does not fail loudly, it produces a compute running none of the
// operator's settings. Every field of the configured connection has to travel.
func TestSupervisorEnvCarriesTheWholeYbConnection(t *testing.T) {
	env, le := envWithProvisionedEndpoint()
	mode, ca, pwEnv := "verify-full", "/etc/ssl/yb.pem", "YB_TEST_PASSWORD"
	clientCert, clientKey := "/etc/ssl/client.crt", "/etc/ssl/client.key"
	env.Controller.Yb.Sslmode = &mode
	env.Controller.Yb.Sslrootcert = &ca
	env.Controller.Yb.Sslcert = &clientCert
	env.Controller.Yb.Sslkey = &clientKey
	env.Controller.Yb.PasswordEnv = &pwEnv
	t.Setenv(pwEnv, "s3cret")

	got, err := supervisorEnv(env, le)
	if err != nil {
		t.Fatalf("supervisorEnv: %v", err)
	}
	for k, want := range map[string]string{
		"YB_SSLMODE":     mode,
		"YB_SSLROOTCERT": ca,
		// Without these the supervisor's own connection — the write fence and
		// the amp_registry pg_settings read — has no client identity, which a
		// `cert` HBA rule refuses while the daemon tomls it writes connect fine.
		"YB_SSLCERT":      clientCert,
		"YB_SSLKEY":       clientKey,
		"YB_PASSWORD_ENV": pwEnv,
		// The named variable itself, or the supervisor reads an empty password:
		// proc scrubs the environment it does not pass explicitly.
		pwEnv: "s3cret",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
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

// An externalized extension mounted from an image needs two things from the
// environment that the Helm chart supplies in a pod. supervisorEnv builds an
// explicit environment rather than inheriting one, so without forwarding these
// the compute cannot resolve the libraries the image bundles — and the failure
// is a dlopen error naming a library that is present on disk.
func TestSupervisorEnvForwardsAnExtensionsEnvironment(t *testing.T) {
	t.Setenv("EXTENSION_LIBRARY_PATH", "/extensions/postgis/system")
	t.Setenv("EXTENSION_ENV", "PROJ_DATA, GDAL_DATA ,")
	t.Setenv("PROJ_DATA", "/extensions/postgis/share/proj")
	t.Setenv("GDAL_DATA", "/extensions/postgis/share/gdal")

	env, le := envWithProvisionedEndpoint()
	got, err := supervisorEnv(env, le)
	if err != nil {
		t.Fatalf("supervisorEnv: %v", err)
	}
	for k, want := range map[string]string{
		"EXTENSION_LIBRARY_PATH": "/extensions/postgis/system",
		"PROJ_DATA":              "/extensions/postgis/share/proj",
		"GDAL_DATA":              "/extensions/postgis/share/gdal",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	// LD_LIBRARY_PATH stays the Postgres lib dir: pgLibEnv owns it, and the
	// supervisor is what appends EXTENSION_LIBRARY_PATH to it later. Merging
	// here would overwrite the value the daemons need.
	if strings.Contains(got["LD_LIBRARY_PATH"], "/extensions/") {
		t.Errorf("LD_LIBRARY_PATH carries an extension dir: %q", got["LD_LIBRARY_PATH"])
	}
}

// Unset in ordinary local use, and then nothing extra is forwarded.
func TestSupervisorEnvForwardsNoExtensionEnvByDefault(t *testing.T) {
	env, le := envWithProvisionedEndpoint()
	got, err := supervisorEnv(env, le)
	if err != nil {
		t.Fatalf("supervisorEnv: %v", err)
	}
	for _, k := range []string{"EXTENSION_LIBRARY_PATH", "EXTENSION_ENV", "PROJ_DATA"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s forwarded when unset", k)
		}
	}
}

// A name in EXTENSION_ENV that is not itself set must not appear as an empty
// value — an empty PROJ_DATA is worse than none, since libproj then looks in a
// path that does not exist instead of falling back.
func TestSupervisorEnvSkipsAnUnsetExtensionVariable(t *testing.T) {
	t.Setenv("EXTENSION_ENV", "PROJ_DATA,NOT_SET_ANYWHERE")
	t.Setenv("PROJ_DATA", "/extensions/postgis/share/proj")
	env, le := envWithProvisionedEndpoint()
	got, err := supervisorEnv(env, le)
	if err != nil {
		t.Fatalf("supervisorEnv: %v", err)
	}
	if _, ok := got["NOT_SET_ANYWHERE"]; ok {
		t.Error("an unset name in EXTENSION_ENV was forwarded")
	}
	if got["PROJ_DATA"] == "" {
		t.Error("PROJ_DATA was not forwarded")
	}
}
