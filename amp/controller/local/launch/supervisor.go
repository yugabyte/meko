package launch

import (
	"bufio"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/controller/local/cfg"
	"github.com/yugabyte/amp/controller/local/config"
	"github.com/yugabyte/amp/controller/local/proc"
)

// An endpoint's three processes — safekeeper, pageserver, compute_ctl — are run
// by `endpoint-supervisor`, the same binary that is PID 1 of a deployed endpoint
// pod. The launcher spawns one per endpoint and talks to it over the endpoint's
// control socket.
//
// The launcher is fire-and-forget: it starts things and exits, so it cannot be
// the thing that notices a daemon died. The supervisor is resident, so the local
// stack fails and recovers the way the deployed one does.
//
// The launcher brings the two daemon tomls and lets the supervisor render the
// compute spec (PRERENDERED_CONFIG=daemons). Their content comes from an
// environment the supervisor cannot see — per-endpoint data directories and ports
// allocated by a free-port search — while the compute's settings are its own to
// render, from the invariants, this endpoint's amp_registry rows, and the
// local-only values supervisorEnv passes below.

// supervisorName is what proc calls the supervisor's pidfile and log inside the
// endpoint directory.
const supervisorName = "endpoint_supervisor"

// supervisorBin is the binary, alongside the daemons it starts.
func supervisorBin(env *config.LocalEnv) string {
	return filepath.Join(env.YbAmpDistribDir, "endpoint-supervisor")
}

// sunPathMax is the longest a Unix socket path can be: `sockaddr_un.sun_path`
// is 108 bytes on Linux and one is the NUL.
const sunPathMax = 107

// supervisorSocket is the control socket the supervisor binds for an endpoint.
//
// Beside the endpoint's other run-time state, unless the repo directory is deep
// enough that the path cannot be a socket — a real case, since the repo lives
// wherever the tree was checked out. The fallback is derived from the run
// directory so both ends agree without passing the name around.
//
// This has to stay in lockstep with `socket_path` in
// compute/endpoint_supervisor/src/control.rs: the two compute it independently,
// and a disagreement is a launcher that cannot find a running supervisor.
func supervisorSocket(env *config.LocalEnv, endpointID string) string {
	dir := endpointDir(env, endpointID)
	if beside := filepath.Join(dir, "supervisor.sock"); len(beside) <= sunPathMax {
		return beside
	}
	return fmt.Sprintf("/tmp/yb-amp-%d-%x.sock", os.Getuid(), md5.Sum([]byte(dir)))
}

// supervisorRunning reports whether the endpoint's supervisor is live.
func supervisorRunning(env *config.LocalEnv, endpointID string) bool {
	return proc.Running(supervisorName, endpointDir(env, endpointID))
}

// startEndpointSupervisor spawns the supervisor for one endpoint, unless it is
// already running. It returns once the supervisor answers its control socket,
// which it binds only after every service is up and the pageserver/safekeeper
// handshake has run.
//
// Idempotent, because the launcher has several doors into the same endpoint
// (`endpoint start`, `start`, a per-daemon restart) and each of them needs the
// supervisor up without caring which one got there first.
func startEndpointSupervisor(env *config.LocalEnv, le launcherEndpoint, timeout time.Duration) error {
	endpointID := le.conf.EndpointID
	if supervisorRunning(env, endpointID) {
		return nil
	}
	dir := endpointDir(env, endpointID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create endpoint dir %q: %w", dir, err)
	}
	childEnv, err := supervisorEnv(env, le)
	if err != nil {
		return err
	}
	// The socket answering is the readiness signal: the supervisor binds it last,
	// once all three services have started and the handshake has run.
	statusCheck := func() error {
		_, err := supervisorRequest(env, endpointID, "status", 2*time.Second)
		return err
	}
	return proc.StartProcess(
		supervisorName,
		dir,
		supervisorBin(env),
		// On the command line as well as in RUN_DIR, so the endpoint's path is in
		// the supervisor's argv: that is what `ps` shows, and what a teardown
		// reaping by path anchor (clean.sh's `pkill -f $YB_AMP_REPO_DIR`) matches.
		// A supervisor it misses restarts the daemons it just killed.
		[]string{"--run-dir", dir},
		childEnv,
		// The supervisor claims its own pidfile, like the daemons it starts. A
		// descriptor locked here and passed down would be inherited by every child
		// it spawns, and the lock would outlive it — leaving a dead supervisor
		// indistinguishable from a live one. See
		// compute/endpoint_supervisor/src/pidfile.rs.
		proc.PidExpect,
		statusCheck,
		timeout,
	)
}

// stopEndpointSupervisor stops an endpoint's supervisor, which stops its
// children in reverse order on the way out.
func stopEndpointSupervisor(env *config.LocalEnv, endpointID string, immediate bool) error {
	mode := proc.StopFast
	if immediate {
		mode = proc.StopImmediate
	}
	if err := proc.StopProcess(supervisorName, endpointDir(env, endpointID), mode); err != nil {
		return err
	}
	// The supervisor may have been SIGKILLed and removed nothing, and a socket
	// file with nothing behind it makes every later request fail with "connection
	// refused" rather than the clearer "no supervisor running".
	_ = os.Remove(supervisorSocket(env, endpointID))
	return nil
}

// supervisorEnv is the environment contract of compute/endpoint_supervisor: the
// ports, paths and identities it would otherwise read off a pod's ConfigMap.
//
// Every port here also appears in a config file the launcher already wrote. The
// supervisor uses its copy for health probes and for the handshake, so the two
// have to agree — which is why they are derived from the same conf structs
// rather than defaulted.
func supervisorEnv(env *config.LocalEnv, le launcherEndpoint) (map[string]string, error) {
	conf := le.conf
	psConf := env.PageserverConfForBranch(conf.BranchID)
	if psConf == nil {
		return nil, fmt.Errorf("no pageserver serves branch %s", conf.BranchID)
	}
	psHTTP, err := portOf(psConf.ListenHTTPAddr)
	if err != nil {
		return nil, err
	}
	psPg, err := portOf(psConf.ListenPgAddr)
	if err != nil {
		return nil, err
	}
	var skConf *config.SafekeeperConf
	for i := range env.Safekeepers {
		if env.Safekeepers[i].EndpointID == conf.EndpointID {
			skConf = &env.Safekeepers[i]
			break
		}
	}
	if skConf == nil {
		return nil, fmt.Errorf("no safekeeper for endpoint %s", conf.EndpointID)
	}

	// The launcher brings the two daemon tomls, whose content comes from an
	// environment the supervisor cannot see, and lets it render the compute spec.
	// Rendering our own copy of that meant a second list of the same Postgres
	// settings, which drifted (https://github.com/yugabyte/meko/issues/1050); what is genuinely local goes through
	// COMPUTE_EXTRA_SETTINGS_JSON below instead.
	extra, err := json.Marshal(cfg.LocalComputeSettings(localComputeParams(env, le)))
	if err != nil {
		return nil, fmt.Errorf("marshal local compute settings: %w", err)
	}
	// compute_ctl gates its external HTTP on this key set, and the launcher probes
	// /status with a token signed by the matching private key. The supervisor's
	// default is empty, which is the pod's and rejects everything — so without
	// this the launcher times out waiting for an endpoint that is actually up.
	jwks, err := json.Marshal(conf.ComputeCtlConfig.Jwks)
	if err != nil {
		return nil, fmt.Errorf("marshal compute_ctl jwks: %w", err)
	}

	out := map[string]string{
		"PRERENDERED_CONFIG": "daemons",
		// The registry row's id, which is what the endpoint's pg_settings are
		// keyed by — not conf.EndpointID, which is the human handle ("main") and
		// is what COMPUTE_ID carries. The supervisor requires a UUID here and
		// only warns when it does not get one, so a name would leave the compute
		// silently running on derived defaults with none of the operator's
		// settings.
		"ENDPOINT_ID":                 le.ID.String(),
		"COMPUTE_EXTRA_SETTINGS_JSON": string(extra),
		"COMPUTE_CTL_JWKS_JSON":       string(jwks),
		"RUN_DIR":                     endpointDir(env, conf.EndpointID),
		"BIN_DIR":                     env.YbAmpDistribDir,
		"PG_DISTRIB_DIR":              env.PgDistribDirRaw(),
		"BRANCH_ID":                   conf.BranchID.String(),
		"ENDPOINT_PG_VERSION":         strconv.Itoa(conf.PgVersion),

		"COMPUTE_ID":                     conf.EndpointID,
		"COMPUTE_PORT":                   strconv.Itoa(int(conf.PgPort)),
		"COMPUTE_CTL_HTTP_PORT":          strconv.Itoa(int(conf.ExternalHTTPPort)),
		"COMPUTE_CTL_INTERNAL_HTTP_PORT": strconv.Itoa(int(conf.InternalHTTPPort)),

		"PAGESERVER_PG_PORT":   strconv.Itoa(int(psPg)),
		"PAGESERVER_HTTP_PORT": strconv.Itoa(int(psHTTP)),
		"SAFEKEEPER_PG_PORT":   strconv.Itoa(int(skConf.PgPort)),
		// What walproposer connects to, which is the tenant-only port when one is
		// configured. Passed separately so the spec names it once: emitting
		// yb_amp.safekeepers a second time from the launcher would leave Postgres
		// taking the last occurrence and compute_ctl's ParsedSpec taking the
		// first (GenericOptionsSearch::find), which is how the two end up talking
		// to different safekeepers.
		"SAFEKEEPER_COMPUTE_PORT": strconv.Itoa(int(skConf.GetComputePort())),
		"SAFEKEEPER_HTTP_PORT":    strconv.Itoa(int(skConf.HTTPPort)),

		// A local run is a place where daemons are killed on purpose — the
		// integration suite SIGKILLs the safekeeper several times per scenario —
		// and the pod's budget would read that as an endpoint that cannot stay up
		// and take the whole thing down mid-test.
		"SUPERVISOR_MAX_RESTARTS":            "1000",
		"SUPERVISOR_RESTART_BACKOFF_SECONDS": "0",
	}
	if psConf.ListenGRPCAddr != nil {
		grpcPort, err := portOf(*psConf.ListenGRPCAddr)
		if err != nil {
			return nil, err
		}
		out["PAGESERVER_GRPC_PORT"] = strconv.Itoa(int(grpcPort))
	}
	// An endpoint created before this port existed has none, and serves no
	// settings API rather than binding port 0.
	if conf.SettingsHTTPPort != 0 {
		out["SUPERVISOR_HTTP_PORT"] = strconv.Itoa(int(conf.SettingsHTTPPort))
	}
	if conf.PrivilegedRoleName != nil {
		out["PRIVILEGED_ROLE_NAME"] = *conf.PrivilegedRoleName
	}
	// The whole connection, not just enough for the readiness wait: since the
	// supervisor renders the compute spec it also reads this endpoint's
	// pg_settings out of amp_registry itself, and `fetch_overrides` downgrades a
	// failed read to a warning — so a connection missing its sslmode or its
	// password produces a compute that boots looking healthy with none of the
	// operator's settings, rather than an error.
	if yb := env.Controller.Yb; yb != nil {
		out["YB_HOST_PORTS"] = yb.HostPorts
		out["YB_USER"] = yb.EffectiveUser()
		out["YB_DBNAME"] = yb.EffectiveDbname()
		if yb.Sslmode != nil {
			out["YB_SSLMODE"] = *yb.Sslmode
		}
		if yb.Sslrootcert != nil {
			out["YB_SSLROOTCERT"] = *yb.Sslrootcert
		}
		if yb.PasswordEnv != nil {
			// The name, and the value it names: proc scrubs the environment, so
			// the variable the supervisor is told to read has to travel with it.
			out["YB_PASSWORD_ENV"] = *yb.PasswordEnv
			if v, ok := os.LookupEnv(*yb.PasswordEnv); ok {
				out[*yb.PasswordEnv] = v
			}
		}
	}
	for k, v := range pgLibEnv(env) {
		out[k] = v
	}
	// The fencing escape hatch must be reachable here too: with a fencing bug,
	// this is where an operator turns it off, and proc's env scrub would
	// otherwise drop it (it is neither YB_-prefixed nor allowlisted).
	if v, ok := os.LookupEnv("REQUIRE_WRITE_TOKEN"); ok {
		out["REQUIRE_WRITE_TOKEN"] = v
	}
	// proc's allowlist covers what the daemons read (RUST_LOG, FAILPOINTS, YB_*)
	// but not these two, and compute_ctl shells out to initdb and postgres, which
	// expect a PATH and a HOME.
	for _, name := range []string{"PATH", "HOME"} {
		if v, ok := os.LookupEnv(name); ok {
			out[name] = v
		}
	}
	// An externalized extension mounted from an image can need two things from
	// the environment, which the Helm chart supplies in a pod and which the scrub
	// above would otherwise drop here:
	//
	//   EXTENSION_LIBRARY_PATH  dirs of system libraries the extension bundles.
	//                           The supervisor appends this to its own
	//                           LD_LIBRARY_PATH — note it must NOT be merged into
	//                           LD_LIBRARY_PATH here, which pgLibEnv owns and
	//                           overwrites.
	//   EXTENSION_ENV           comma-separated names of runtime data-dir
	//                           variables to forward (PROJ_DATA, GDAL_DATA, …).
	//                           The names are the extension's, not ours, so they
	//                           are named rather than pattern-matched.
	//
	// Both are unset in ordinary local use; the extension test driver sets them
	// (test/integration/compute/extensions.sh --registered).
	if v, ok := os.LookupEnv("EXTENSION_LIBRARY_PATH"); ok {
		out["EXTENSION_LIBRARY_PATH"] = v
	}
	if names, ok := os.LookupEnv("EXTENSION_ENV"); ok {
		for _, name := range strings.Split(names, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if v, ok := os.LookupEnv(name); ok {
				out[name] = v
			}
		}
	}
	return out, nil
}

// supervisorRequest sends one control request and returns the reply.
//
// One request per connection, matching the server: the protocol is a line in and
// a line out, so there is no state to keep and nothing to reconnect.
func supervisorRequest(env *config.LocalEnv, endpointID, request string, timeout time.Duration) (string, error) {
	path := supervisorSocket(env, endpointID)
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return "", fmt.Errorf("no supervisor for endpoint %s (control socket %q): %w", endpointID, path, err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return "", err
	}
	if _, err := fmt.Fprintf(conn, "%s\n", request); err != nil {
		return "", fmt.Errorf("send %q to the supervisor: %w", request, err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read the supervisor's reply to %q: %w", request, err)
	}
	reply = strings.TrimSpace(reply)
	if strings.HasPrefix(reply, "err:") {
		return "", fmt.Errorf("supervisor refused %q: %s", request, strings.TrimSpace(strings.TrimPrefix(reply, "err:")))
	}
	return reply, nil
}

// supervisorStop asks the supervisor to stop one of its children.
//
// `immediate` is SIGQUIT rather than SIGTERM — stop now, no shutdown checkpoint
// — which is what the launcher's `--mode immediate` means everywhere else.
func supervisorStop(env *config.LocalEnv, endpointID, verb, service string, immediate bool) error {
	request := verb + " " + service
	if immediate {
		request += " immediate"
	}
	_, err := supervisorRequest(env, endpointID, request, daemonStopTimeout)
	return err
}

// supervisorStart asks the supervisor to start one of its children and gate on
// its health probe.
//
// The budget goes in the request so both ends give up at the same moment. With
// only a deadline on this side, a start that is merely slow reads as a failure
// here while the supervisor carries on and succeeds — and the error is a socket
// timeout, which says nothing about the service. The socket gets a margin on top
// so the supervisor is always the one to time out, and its reason is what the
// caller sees.
func supervisorStart(env *config.LocalEnv, endpointID, service string, budget time.Duration) error {
	request := fmt.Sprintf("start %s %d", service, int(budget.Seconds()))
	_, err := supervisorRequest(env, endpointID, request, budget+supervisorReplyMargin)
	return err
}

// supervisorReplyMargin is how much longer than the caller's budget this waits
// for a reply, covering the supervisor's own spawn and bookkeeping.
const supervisorReplyMargin = 15 * time.Second
