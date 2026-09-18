// Command reconcile-agent is the standalone agent that converges an environment
// to YCP's desired state. It is the sole YCP client for its environment: it
// enrolls as its first check-in, pulls desired state, and drives the generic
// reconcile engine against a backend chosen by the environment's kind,
// reporting inventory back on each status heartbeat.
//
// ENV_KIND selects the backend: "amp" (the default, so an existing
// yb-amp-controller release that sets nothing keeps working) drives the amp
// controller's REST API; "meko" drives meko's api_server tenant API. The
// enrollment carries the same kind and YCP refuses one that disagrees with how the
// environment was registered; that refusal is fatal here, where a transient
// failure falls back to the persisted identity — otherwise a flipped ENV_KIND
// would converge an environment whose enrolment was permanently rejected.
//
// Every YCP call authenticates with the environment's YBM API key (YCP_API_KEY),
// so the agent holds no earned credential: enrollment is idempotent and repeated
// at every startup as the first check-in. The state Secret keeps only the
// identity that check-in confirmed, so a restart that cannot reach the control
// plane still knows which environment it is.
//
// All traffic is outbound: agent -> k8s API, agent -> YCP, agent -> the backend's
// API (in-cluster).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"meko/ycp/internal/api"
	"meko/ycp/internal/ycpclient"
	"meko/ycp/reconcile"
	"meko/ycp/reconcile/ampbackend"
	"meko/ycp/reconcile/mekobackend"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Environment kinds this agent can serve, and so the backends it can build.
const (
	kindAmp  = "amp"
	kindMeko = "meko"
)

func main() {
	// The one subcommand: the chart's post-install telemetry preflight runs this
	// same image, so the probe ships with the agent rather than in a curl
	// container the customer's registry may not be able to pull.
	//
	// An agent tag older than this arm has no os.Args handling at all: it falls
	// through to the agent path and dies at "ENV_NAME is required", because the
	// preflight Job sets none of the agent's env. The hook still fails, but the log
	// sends the operator after agent configuration instead of the tag bump that
	// actually fixes it — worth knowing when triaging a preflight that failed for a
	// reason nothing here explains.
	if len(os.Args) > 1 {
		if os.Args[1] != "preflight" {
			log.Fatalf("unknown subcommand %q (the only one is \"preflight\"); "+
				"an older agent image has no subcommands at all, so bump agent.image.tag", os.Args[1])
		}
		if err := runPreflight(context.Background()); err != nil {
			log.Fatal(err)
		}
		log.Printf("telemetry preflight passed: this environment is shipping metrics to yugabyte-cloud")
		return
	}

	ycpURL := strings.TrimRight(env("YCP_URL", "http://ycp:8080/api/v1"), "/")
	// AMP_CONTROLLER_URL must include the API base path (the controller mounts
	// its REST API at /api/v1).
	ampURL := strings.TrimRight(env("AMP_CONTROLLER_URL", "http://amp-controller:8080/api/v1"), "/")
	// MEKO_API_URL likewise includes the base path; meko's api_server can also be
	// mounted under a --base-path prefix, which would sit before /api/v1.
	mekoURL := strings.TrimRight(env("MEKO_API_URL", "http://meko-api-server-service:80/api/v1"), "/")
	// Defaulted to amp so a yb-amp-controller release that sets nothing keeps
	// working; a meko install opts in.
	kind := env("ENV_KIND", kindAmp)
	envName := env("ENV_NAME", "")
	placement := env("ENV_PLACEMENT", "cloud")
	apiKey := os.Getenv("YCP_API_KEY")
	devAdminSecret := os.Getenv("YCP_DEV_ADMIN_SECRET")
	devAPIKeyID := os.Getenv("YCP_DEV_API_KEY_ID")
	agentVer := env("AGENT_VERSION", "dev")
	telemetry := telemetryFromEnv()
	telWatch := newTelemetryWatch(os.Getenv(envCollectorMetricsURL))
	interval := 10 * time.Second
	if d, err := time.ParseDuration(os.Getenv("RECONCILE_INTERVAL")); err == nil && d > 0 {
		interval = d
	}

	log.Printf("reconcile-agent starting: kind=%s env=%s ycp=%s", kind, envName, ycpURL)
	if envName == "" {
		log.Fatalf("ENV_NAME is required")
	}
	devMode := devAdminSecret != "" && devAPIKeyID != ""
	if apiKey == "" && !devMode {
		log.Fatalf("YCP_API_KEY is required (or both YCP_DEV_ADMIN_SECRET and YCP_DEV_API_KEY_ID for a standalone control plane)")
	}

	kc, err := newK8s()
	if err != nil {
		log.Fatalf("k8s init: %v", err)
	}
	st, err := kc.readState(stateSecret)
	if err != nil {
		// Not fatal: the state is a resume nicety, not a credential. The check-in
		// below re-establishes identity from the API key regardless.
		log.Printf("WARNING could not read agent state: %v", err)
		st = state{}
	}
	if st.EnvID != "" && (st.EnvName != envName || (st.EnvKind != "" && st.EnvKind != kind)) {
		// The operator re-pointed this agent; the persisted identity is for a
		// different environment, so it must not be resumed.
		log.Printf("environment changed (%s/%s -> %s/%s) — discarding persisted identity",
			st.EnvKind, st.EnvName, kind, envName)
		st = state{}
	}

	ycp := ycpclient.New(ycpURL, apiKey, &http.Client{Timeout: 15 * time.Second})
	if devMode {
		log.Printf("dev mode: authenticating with the admin secret + api key id headers")
		ycp.SetDevHeaders(devAdminSecret, devAPIKeyID)
	}
	var backend reconcile.Backend
	var tasks *ampbackend.TaskCollector
	switch kind {
	case kindAmp:
		b, berr := ampbackend.New(ampURL, os.Getenv("AMP_CONTROLLER_TOKEN"), nil)
		if berr != nil {
			log.Fatalf("amp backend: %v", berr)
		}
		log.Printf("driving the amp controller at %s", ampURL)
		backend = b
		// The task log is the amp controller's; a meko environment has none, so the
		// collector is built here rather than beside the agent and stays nil for
		// every other kind.
		tasks = ampbackend.NewTaskCollector(b, log.Printf)
	case kindMeko:
		// Optional, unlike amp's controller token, because an api-server running
		// without Cognito authenticates nobody: DevAuth puts every request on one
		// dev account with all scopes. That is the BYOC data-plane shape, and it
		// has no token to give. A Cognito-enabled meko needs one.
		token := os.Getenv("MEKO_API_TOKEN")
		if token == "" {
			log.Printf("MEKO_API_TOKEN is unset: calling %s unauthenticated, "+
				"which works only against an api-server running without Cognito", mekoURL)
		}
		log.Printf("driving the meko api-server at %s", mekoURL)
		backend = mekobackend.New(mekoURL, token, nil)
	default:
		// The switch is the only place the served kinds are written down, so a
		// kind added to the constants without a case here fails at startup
		// rather than leaving backend nil for the first tick to dereference.
		log.Fatalf("ENV_KIND=%q is not a kind this agent serves (%s | %s)", kind, kindAmp, kindMeko)
	}

	a := &agent{
		ycp:       ycp,
		kc:        kc,
		envName:   envName,
		kind:      kind,
		placement: placement,
		agentVer:  agentVer,
		telemetry: telemetry,
		telWatch:  telWatch,
		tasks:     tasks,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// First check-in. Enrollment is idempotent and key-authenticated, so every
	// startup enrolls. Transient failures (control plane not up yet) are retried
	// with capped backoff — falling back to the persisted identity when there is
	// one, so an outage does not hold the reconcile loop hostage. A 401 cannot be
	// fixed from here — the key was revoked or never bound — so it is logged
	// loudly on every attempt; a redeploy with a fresh key Secret recovers.
	if err := a.enrollWithRetry(ctx, st); err != nil {
		if ctx.Err() != nil {
			log.Printf("shutting down before enrolling: %v", err)
			return
		}
		// Not a shutdown: the control plane refused this enrolment for good. Exit
		// non-zero so the pod crash-loops and exit-code alerting sees it — a
		// misconfiguration that returns 0 looks like a clean stop.
		log.Fatalf("enroll: %v", err)
	}

	// Pin the controller to this YCP environment before driving it. The binding
	// is write-once on the controller: a conflict means another YCP already owns
	// this controller, and we must not fight the agent that bound it. Anything
	// else (controller not up yet on a fresh install, transient network) is
	// retried with backoff rather than crash-looping the pod.
	//
	// amp only. meko's api_server has no equivalent write-once binding, so
	// nothing stops two control planes driving one meko — tracked as a gap in
	// ARCHITECTURE.md rather than papered over with a check that cannot be made
	// here.
	if kind == kindAmp {
		if err := bindControllerWithRetry(ctx, ampURL, os.Getenv("AMP_CONTROLLER_TOKEN"), ycpURL, a.envID, log.Printf); err != nil {
			if ctx.Err() != nil {
				log.Printf("shutting down before controller bound: %v", err)
				return
			}
			log.Fatalf("bind controller: %v", err) // a binding conflict is fatal
		}
	}

	eng := reconcile.New(backend, reconcile.Options{})
	eng.Run(ctx, a, a, interval, log.Printf)
	log.Printf("reconcile-agent stopped")
}

// errBindConflict is a write-once binding rejection: the controller is already
// bound to a different YCP. It is fatal and must not be retried — retrying
// cannot change the answer, and we must not fight the agent that bound it.
var errBindConflict = errors.New("controller already bound to a different YCP")

// bindControllerWithRetry binds, retrying transient failures (controller not up
// yet, network) with capped backoff until it succeeds or ctx is cancelled. A
// binding conflict is returned immediately for the caller to treat as fatal.
func bindControllerWithRetry(ctx context.Context, ampURL, token, ycpURL, envID string, logf func(string, ...any)) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		err := bindController(ctx, ampURL, token, ycpURL, envID)
		if err == nil {
			return nil
		}
		if errors.Is(err, errBindConflict) || ctx.Err() != nil {
			return err
		}
		logf("bind controller: %v (retrying in %s)", err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// bindController pins the amp controller to this YCP environment via the
// controller's write-once binding endpoint. A 409 is returned wrapped in
// errBindConflict; any other non-2xx is a transient error the caller retries.
func bindController(ctx context.Context, ampURL, token, ycpURL, envID string) error {
	body, _ := json.Marshal(map[string]string{"ycp_url": ycpURL, "environment_id": envID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ampURL+"/ycp/binding", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", errBindConflict, strings.TrimSpace(string(b)))
	default:
		return fmt.Errorf("bind controller: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
}

// stateSecret persists the environment id the check-in confirmed, so a restart
// that cannot reach the control plane still knows which environment it is. No
// credential lives in it: the API key is operator-provided.
const stateSecret = "reconcile-agent-state"

// agent adapts the YCP client to the engine's Source + Reporter and owns the
// enrollment and last-good-desired logic.
type agent struct {
	ycp       *ycpclient.Client
	kc        *k8s
	envName   string
	kind      string
	placement string
	agentVer  string

	// telemetry is what this deployment is configured to ship, read from the env
	// the chart set from the same values the collectors render from. Reported on
	// enroll and on every tick; the control plane is what decides whether it is
	// acceptable. nil for a deployment whose chart sets none of it.
	telemetry *api.Telemetry
	// telWatch turns the collectors' own counters into whether this environment is
	// still SHIPPING, which the configuration above cannot see. nil when the chart
	// wired no collector endpoint.
	telWatch *telemetryWatch

	envID    string
	envUUID  string
	lastGood api.DesiredState
	haveGood bool

	// tasks relays the controller's task log so the console can see what is running
	// and what failed. Its own reader, not part of the reconcile engine: the engine
	// only cares about tasks it planned for a resource it is converging, while the
	// console needs every task, including ones an operator or the amp UI planned
	// directly.
	//
	// amp only, and nil for every other kind: the task log is the amp controller's,
	// and meko's verbs are synchronous, so there is no asynchronous work for a
	// console to watch.
	tasks *ampbackend.TaskCollector
}

// enrollWithRetry performs the startup check-in, falling back to the persisted
// identity when the control plane is unreachable and one exists.
func (a *agent) enrollWithRetry(ctx context.Context, st state) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		resp, err := a.ycp.Enroll(ctx, api.EnrollRequest{
			Name:      a.envName,
			Placement: a.placement,
			Kind:      a.kind,
			Telemetry: a.telemetry,
		})
		if err == nil {
			a.envID = resp.EnvironmentID
			a.envUUID = resp.EnvironmentUUID
			log.Printf("enrolled env=%s id=%s uuid=%s", a.envName, a.envID, a.envUUID)
			if werr := a.kc.writeState(stateSecret,
				state{EnvID: a.envID, EnvUUID: a.envUUID, EnvName: a.envName, EnvKind: a.kind}); werr != nil {
				// Not fatal: the agent works for this lifetime; it just cannot
				// resume through an outage that spans a restart.
				log.Printf("WARNING could not persist agent state: %v", werr)
			}
			return nil
		}
		if errors.Is(err, ycpclient.ErrKindMismatch) {
			// The environment is not this agent's kind. Resuming a persisted
			// identity here would converge an environment whose enrolment the
			// control plane has permanently refused — and the persisted identity is
			// exactly what a flipped ENV_KIND leaves behind, since a state Secret
			// written before the kind was recorded reads as a match.
			return fmt.Errorf("enroll: ENV_KIND=%s is not the registered kind of environment %q: %w",
				a.kind, a.envName, err)
		}
		if errors.Is(err, ycpclient.ErrUnauthorized) {
			log.Printf("ENROLL REJECTED (401): the API key is revoked or bound to no environment; "+
				"register a new environment and redeploy with its key (retrying in %s)", backoff)
		} else {
			log.Printf("enroll: %v (retrying in %s)", err, backoff)
		}
		// Resume from persisted identity whichever way the check-in failed, so the
		// reconcile loop can hold the environment up while the cause is fixed --
		// the same shape as a mid-run failure, which keeps reconciling the last
		// desired state it read.
		if st.EnvID != "" {
			log.Printf("resuming as persisted env id=%s; the loop retries on its own", st.EnvID)
			a.envID = st.EnvID
			a.envUUID = st.EnvUUID
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// Desired implements reconcile.Source. A successful fetch is fresh (deletes
// allowed); on error it falls back to the last-good set with fresh=false so the
// engine converges creates/updates but never garbage-collects on stale data. A
// 401 is logged loudly and not retried within the tick: the key the agent holds
// is the only credential it has, so re-asking cannot change the answer.
func (a *agent) Desired(ctx context.Context) (api.DesiredState, bool, error) {
	ds, err := a.ycp.Desired(ctx)
	if err != nil {
		if errors.Is(err, ycpclient.ErrUnauthorized) {
			log.Printf("DESIRED-STATE REJECTED (401): the API key is revoked or bound to no environment; " +
				"holding the last desired state and changing nothing")
		}
		return a.fallback(err)
	}
	a.lastGood, a.haveGood = ds, true
	return ds, true, nil
}

func (a *agent) fallback(err error) (api.DesiredState, bool, error) {
	if a.haveGood {
		return a.lastGood, false, err
	}
	return api.DesiredState{}, false, err
}

// Report implements reconcile.Reporter: post a status heartbeat with the
// reconciled inventory and applied revision. A retryable failure or a stuck
// resource makes the environment DEGRADED; a permanent verdict (drift, rejected
// or blocked) is reported per-resource but does not by itself degrade the env.
func (a *agent) Report(ctx context.Context, ds api.DesiredState, res reconcile.Result) error {
	stateStr := api.StateReady
	if len(res.Failed) > 0 || len(res.Stuck) > 0 {
		stateStr = api.StateDegraded
	}
	// Read the collectors before reporting, so the control plane sees this tick's
	// view rather than the previous one's. Two GETs to an in-cluster Service, on a
	// loop already making several.
	now := time.Now()
	a.telWatch.Observe(ctx, now)

	report := api.StatusReport{
		EnvironmentState: stateStr,
		AgentVersion:     a.agentVer,
		AppliedRevision:  ds.Revision,
		Resources:        inventory(res),
		Telemetry:        a.telWatch.Report(a.telemetry, now),
	}
	// Workload health is additive, like the task log below: the console's components
	// tab is worth having, but not at the cost of the convergence report that the
	// environment's state and every resource row ride on. A namespace we cannot read
	// reports no components rather than no status.
	//
	// Note what this cannot catch. Report is only reached when Reconcile succeeded,
	// and Reconcile starts by listing through the api-server — so a crash-looping
	// api-server posts no status at all and reads as a stale heartbeat, not as
	// `meko-api-server 0/1`. This covers the workloads the loop does not depend on.
	if a.kc != nil {
		comps, cerr := a.kc.components()
		if cerr != nil {
			log.Printf("collect components (reporting status anyway): %v", cerr)
		} else {
			report.Components = comps
		}
	}
	// Task visibility is additive: a controller that will not serve its task log
	// must not also cost us the convergence report, which is what the environment's
	// state and every resource row ride on. So this logs and carries on.
	var tasks []api.TaskStatus
	commitTasks := func() {}
	if a.tasks != nil {
		var terr error
		if tasks, commitTasks, terr = a.tasks.Collect(ctx); terr != nil {
			log.Printf("collect amp tasks (reporting inventory anyway): %v", terr)
		}
	}
	report.Tasks = tasks

	tasksAccepted, err := a.ycp.PostStatus(ctx, report)
	if errors.Is(err, ycpclient.ErrUnauthorized) {
		log.Printf("STATUS REJECTED (401): the API key is revoked or bound to no environment")
	}
	switch {
	case err != nil:
		// The report did not land, so the delta is still ours to deliver.
	case !tasksAccepted:
		// The control plane took the report but could not store the tasks. Holding the
		// delta is the whole point of the two-phase commit: committing here would
		// retire a transition nothing has recorded, and the feed cannot re-offer it.
		log.Printf("status accepted but the task delta was NOT stored; holding %d task(s) for the next tick",
			len(tasks))
	default:
		commitTasks()
	}
	return err
}

// inventory maps a tick's result to the flat resource inventory YCP renders in
// the console, folding each resource's reason-for-not-converging (Failed,
// Stuck, a permanent verdict, or an Updating task still in flight) onto its row.
// A create/delete that failed for a resource not in actual (a create that never
// landed, a pool that cannot be deleted) is emitted as its own row so the reason
// is still visible.
//
// A permanent verdict names WHICH kind it is — see reconcile.PermanentReason —
// because the console renders a different sentence for each and only one of them
// is "this cannot be changed in place".
func inventory(res reconcile.Result) []api.ResourceStatus {
	inActual := make(map[reconcile.ResourceKey]bool, len(res.Actual))
	out := make([]api.ResourceStatus, 0, len(res.Actual))
	for _, act := range res.Actual {
		inActual[act.Key] = true
		parent := ""
		if act.Parent.Kind != "" {
			parent = act.Parent.String()
		}
		rs := api.ResourceStatus{
			Kind:   act.Key.Kind,
			ID:     act.Key.String(),
			Name:   leaf(act.Key.Name),
			Parent: parent,
			State:  act.State,
			Attrs:  act.Attrs,
		}
		if err, ok := res.Failed[act.Key]; ok {
			rs.Error = err.Error()
		} else if d, ok := res.Stuck[act.Key]; ok {
			rs.Error = fmt.Sprintf("no progress: %q for %s", act.State, d.Round(time.Second))
		} else if err, ok := res.Permanent[act.Key]; ok {
			rs.State, rs.Error = reconcile.PermanentReasonOf(err).State(), err.Error()
		} else if err, ok := res.Updating[act.Key]; ok {
			// An update the controller is still working on. Not an error — the state
			// says what is happening, so "generation 3 declared, 1 applied" is legible
			// rather than something an operator has to infer from a stale attr.
			rs.State, rs.Error = "updating", err.Error()
		}
		out = append(out, rs)
	}

	// Failed/permanent keys with no actual row (a create that never landed, a
	// withdrawn pool that cannot be deleted) still need a row to carry the reason.
	type orphan struct{ state, err string }
	orphans := map[reconcile.ResourceKey]orphan{}
	for k, err := range res.Failed {
		if !inActual[k] {
			orphans[k] = orphan{state: "error", err: err.Error()}
		}
	}
	for k, err := range res.Permanent {
		if !inActual[k] {
			if _, dup := orphans[k]; !dup {
				// Same three-way verdict as a row that does exist: a create the backend
				// refused reads "rejected", not the generic "error" that says nothing
				// about whether re-declaring would help.
				orphans[k] = orphan{state: reconcile.PermanentReasonOf(err).State(), err: err.Error()}
			}
		}
	}
	keys := make([]reconcile.ResourceKey, 0, len(orphans))
	for k := range orphans {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for _, k := range keys {
		out = append(out, api.ResourceStatus{
			Kind:  k.Kind,
			ID:    k.String(),
			Name:  leaf(k.Name),
			State: orphans[k].state,
			Error: orphans[k].err,
		})
	}
	return out
}

func leaf(name string) string {
	segs := strings.Split(name, "/")
	return segs[len(segs)-1]
}
