// Command reconcile-agent is the standalone agent that converges an amp
// environment to YCP's desired state. It is the sole YCP client for its
// environment: it enrolls (kind=amp), pulls desired state, and drives the
// generic reconcile engine against the amp controller's REST API (the amp
// backend), reporting inventory back on each status heartbeat.
//
// The name is deliberately backend-neutral: a later phase adds a meko backend
// selected by the environment's kind, so one binary/image serves both and the
// standalone meko-agent retires. All traffic is outbound: agent -> k8s API,
// agent -> YCP, agent -> amp controller (in-cluster).
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
)

// errNoEnrollToken is returned when a (re-)enroll is needed but no enrollment
// token is configured — the operator must re-issue one for the bound env.
var errNoEnrollToken = errors.New("no enrollment token configured (set ENROLLMENT_TOKEN)")

// stateSecret holds the env-scoped status token + identity between restarts.
// Separate from the operator-supplied enrollment Secret: that is a short-lived
// bootstrap credential, this is the long-lived one the agent earns by using it.
const stateSecret = "reconcile-agent-state"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	ycpURL := strings.TrimRight(env("YCP_URL", "http://ycp:8080"), "/")
	// AMP_CONTROLLER_URL must include the API base path (the controller mounts
	// its REST API at /api/v1).
	ampURL := strings.TrimRight(env("AMP_CONTROLLER_URL", "http://amp-controller:8080/api/v1"), "/")
	envName := env("ENV_NAME", "")
	placement := env("ENV_PLACEMENT", "cloud")
	enrollToken := os.Getenv("ENROLLMENT_TOKEN")
	agentVer := env("AGENT_VERSION", "dev")
	interval := 10 * time.Second
	if d, err := time.ParseDuration(os.Getenv("RECONCILE_INTERVAL")); err == nil && d > 0 {
		interval = d
	}

	log.Printf("reconcile-agent starting: env=%s ycp=%s amp=%s", envName, ycpURL, ampURL)
	if envName == "" {
		log.Fatalf("ENV_NAME is required")
	}

	kc, err := newK8s()
	if err != nil {
		log.Fatalf("k8s init: %v", err)
	}
	st, err := kc.readState(stateSecret)
	if err != nil {
		log.Fatalf("read state: %v", err)
	}

	ycp := ycpclient.New(ycpURL, &http.Client{Timeout: 15 * time.Second})
	backend, err := ampbackend.New(ampURL, os.Getenv("AMP_CONTROLLER_TOKEN"), nil)
	if err != nil {
		log.Fatalf("amp backend: %v", err)
	}

	a := &agent{
		ycp:         ycp,
		kc:          kc,
		envName:     envName,
		placement:   placement,
		enrollToken: enrollToken,
		agentVer:    agentVer,
		st:          st,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Resume with a persisted token, or enroll for a fresh one.
	if st.StatusToken != "" && st.EnvID != "" {
		ycp.SetToken(st.StatusToken)
		log.Printf("resumed env=%s id=%s from state", st.EnvName, st.EnvID)
	} else {
		if err := a.enroll(ctx); err != nil {
			log.Fatalf("enroll: %v", err)
		}
	}

	// Pin the controller to this YCP environment before driving it. The binding
	// is write-once on the controller: a conflict means another YCP already owns
	// this controller, and we must not fight the agent that bound it. Anything
	// else (controller not up yet on a fresh install, transient network) is
	// retried with backoff rather than crash-looping the pod.
	if err := bindControllerWithRetry(ctx, ampURL, os.Getenv("AMP_CONTROLLER_TOKEN"), ycpURL, a.st.EnvID, log.Printf); err != nil {
		if ctx.Err() != nil {
			log.Printf("shutting down before controller bound: %v", err)
			return
		}
		log.Fatalf("bind controller: %v", err) // a binding conflict is fatal
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

// agent adapts the YCP client to the engine's Source + Reporter and owns the
// enroll/re-enroll and last-good-desired logic.
type agent struct {
	ycp         *ycpclient.Client
	kc          *k8s
	envName     string
	placement   string
	enrollToken string
	agentVer    string

	st       state
	lastGood api.DesiredState
	haveGood bool
}

func (a *agent) enroll(ctx context.Context) error {
	if a.enrollToken == "" {
		return errNoEnrollToken
	}
	resp, err := a.ycp.Enroll(ctx, api.EnrollRequest{
		Name:            a.envName,
		EnrollmentToken: a.enrollToken,
		Placement:       a.placement,
		Kind:            "amp",
	})
	if err != nil {
		return err
	}
	a.st = state{EnvID: resp.EnvironmentID, EnvName: a.envName, StatusToken: resp.StatusToken}
	if err := a.kc.writeState(stateSecret, a.st); err != nil {
		return err
	}
	log.Printf("enrolled env=%s id=%s", a.envName, a.st.EnvID)
	return nil
}

// Desired implements reconcile.Source. A successful fetch is fresh (deletes
// allowed); on error it falls back to the last-good set with fresh=false so the
// engine converges creates/updates but never garbage-collects on stale data. A
// 401 triggers a re-enroll and a retry.
func (a *agent) Desired(ctx context.Context) (api.DesiredState, bool, error) {
	ds, err := a.ycp.DesiredFor(ctx, a.st.EnvID)
	if err == ycpclient.ErrUnauthorized {
		if rerr := a.enroll(ctx); rerr != nil {
			return a.fallback(err)
		}
		ds, err = a.ycp.DesiredFor(ctx, a.st.EnvID)
	}
	if err != nil {
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
// resource makes the environment DEGRADED; permanent drift (an unsupported
// verb) is reported per-resource but does not by itself degrade the env.
func (a *agent) Report(ctx context.Context, ds api.DesiredState, res reconcile.Result) error {
	stateStr := api.StateReady
	if len(res.Failed) > 0 || len(res.Stuck) > 0 {
		stateStr = api.StateDegraded
	}
	report := api.StatusReport{
		EnvironmentState: stateStr,
		AgentVersion:     a.agentVer,
		AppliedRevision:  ds.Revision,
		Resources:        inventory(res),
	}
	err := a.ycp.PostStatusFor(ctx, a.st.EnvID, report)
	if err == ycpclient.ErrUnauthorized {
		if rerr := a.enroll(ctx); rerr != nil {
			return rerr
		}
		return a.ycp.PostStatusFor(ctx, a.st.EnvID, report)
	}
	return err
}

// inventory maps a tick's result to the flat resource inventory YCP renders in
// the console, folding each resource's reason-for-not-converging (Failed,
// Stuck, or Permanent drift) onto its row. A create/delete that failed for a
// resource not in actual (a create that never landed, a pool that cannot be
// deleted) is emitted as its own row so the reason is still visible.
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
		}
		if err, ok := res.Failed[act.Key]; ok {
			rs.Error = err.Error()
		} else if d, ok := res.Stuck[act.Key]; ok {
			rs.Error = fmt.Sprintf("no progress: %q for %s", act.State, d.Round(time.Second))
		} else if err, ok := res.Permanent[act.Key]; ok {
			rs.State, rs.Error = "drift", err.Error()
		}
		out = append(out, rs)
	}

	// Failed/permanent keys with no actual row (a create that never landed, a
	// withdrawn pool that cannot be deleted) still need a row to carry the reason.
	orphans := map[reconcile.ResourceKey]string{}
	for k, err := range res.Failed {
		if !inActual[k] {
			orphans[k] = err.Error()
		}
	}
	for k, err := range res.Permanent {
		if !inActual[k] {
			if _, dup := orphans[k]; !dup {
				orphans[k] = err.Error()
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
			State: "error",
			Error: orphans[k],
		})
	}
	return out
}

func leaf(name string) string {
	segs := strings.Split(name, "/")
	return segs[len(segs)-1]
}
