// Command agent is a minimal meko-agent. It reads real cluster facts from the
// in-cluster Kubernetes API (no client-go — just the service-account token),
// completes the enrollment-token handshake with YCP, then on each tick:
//   - pulls desired state (bearer-authed),
//   - reconciles the desired Meko stack into its own namespace,
//   - reports status (real cluster facts + observed component health).
//
// All traffic is outbound: agent -> k8s API, agent -> YCP.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"meko/ycp/internal/api"
)

const (
	tokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	caPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	nsPath    = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	// managedLabel marks namespaces this agent provisions for datapacks.
	managedLabel = "meko.dev/datapack"

	// stateSecret holds the env-scoped status token between restarts, and
	// statusTokenKey is its key. Separate from the operator-supplied enrollment
	// Secret: that one is a short-lived bootstrap credential, this one is the
	// long-lived credential the agent earns by using it. Without persisting it the
	// agent must re-enroll on every restart, and the enrollment token has usually
	// expired by then — an unrecoverable pod restart.
	stateSecret    = "meko-agent-state"
	statusTokenKey = "status-token"
	// envIDKey stores the environment id the control plane assigned. It is NOT
	// derivable from ENV_NAME: the server slugs the name (lowercase, spaces and
	// underscores to hyphens), so "Acme Prod" becomes "acme-prod". Guessing it
	// client-side would duplicate server logic and send the wrong ?envId= on every
	// call after a restart.
	envIDKey = "environment-id"
	// envNameKey records the ENV_NAME the state was enrolled under, so a changed
	// ENV_NAME is detectable — otherwise the persisted id silently wins and
	// re-pointing an agent at another environment does nothing.
	envNameKey = "environment-name"
	// stackKey stores the meko-stack spec the agent enrolled with, so a restart can
	// bring the data plane back without asking the control plane anything.
	stackKey = "stack"
	// desiredKey stores the last desired state the agent read, so an outage that spans a
	// restart still reconciles toward what it was last told rather than what it was
	// registered with.
	desiredKey = "desired"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---- Kubernetes REST client (in-cluster) ----

type k8s struct {
	host  string
	token string
	http  *http.Client
}

func newK8s() (*k8s, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := env("KUBERNETES_SERVICE_PORT", "443")
	if host == "" {
		return nil, fmt.Errorf("not running in-cluster (KUBERNETES_SERVICE_HOST unset)")
	}
	tok, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read sa token: %w", err)
	}
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read sa ca: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return &k8s{
		host:  fmt.Sprintf("https://%s:%s", host, port),
		token: strings.TrimSpace(string(tok)),
		http:  &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}},
	}, nil
}

func (k *k8s) do(method, path string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, k.host+path, rdr)
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

func (k *k8s) getJSON(path string, out any) error {
	code, b, err := k.do("GET", path, nil)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("%s -> %d: %s", path, code, truncate(b))
	}
	return json.Unmarshal(b, out)
}

func truncate(b []byte) string {
	if len(b) > 300 {
		return string(b[:300])
	}
	return string(b)
}

type nsItem struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	// Status.Phase is "Terminating" for a namespace already being deleted. Only
	// namespaces carry it; it stays empty for everything else nsItem decodes.
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}
type nameList struct {
	Items []nsItem `json:"items"`
}

func (k *k8s) facts() (api.ClusterFacts, error) {
	f := api.ClusterFacts{Provider: env("ENV_PROVIDER", "gke"), Region: env("ENV_REGION", "us-west1")}
	var ver struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := k.getJSON("/version", &ver); err != nil {
		return f, err
	}
	f.K8sVersion = ver.GitVersion
	var nodes nameList
	if err := k.getJSON("/api/v1/nodes", &nodes); err != nil {
		return f, err
	}
	f.NodeCount = len(nodes.Items)
	return f, nil
}

func (k *k8s) namespaces() ([]nsItem, error) {
	var ns nameList
	if err := k.getJSON("/api/v1/namespaces", &ns); err != nil {
		return nil, err
	}
	return ns.Items, nil
}

// ensureNamespace creates a labeled namespace if it does not already exist.
func (k *k8s) ensureNamespace(name, datapack string) (created bool, err error) {
	code, _, err := k.do("GET", "/api/v1/namespaces/"+name, nil)
	if err != nil {
		return false, err
	}
	if code == 200 {
		return false, nil
	}
	body, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"name": name,
			"labels": map[string]string{
				"app.kubernetes.io/managed-by":       "meko-agent",
				managedLabel:                         datapack,
				"pod-security.kubernetes.io/enforce": "baseline",
			},
		},
	})
	code, b, err := k.do("POST", "/api/v1/namespaces", body)
	if err != nil {
		return false, err
	}
	if code == 201 {
		return true, nil
	}
	if code == 409 {
		return false, nil
	}
	return false, fmt.Errorf("create ns %s -> %d: %s", name, code, truncate(b))
}

// deleteNamespace removes a namespace (GC). 404 is treated as success (already gone).
// Deleting the namespace cascades to the datapack's Deployment/Service/Secret.
func (k *k8s) deleteNamespace(name string) error {
	code, b, err := k.do("DELETE", "/api/v1/namespaces/"+name, nil)
	if err != nil {
		return err
	}
	if code == 200 || code == 202 || code == 404 {
		return nil
	}
	return fmt.Errorf("delete ns %s -> %d: %s", name, code, truncate(b))
}

// ensure POSTs an object, treating 201 (created) and 409 (already exists) as success.
func (k *k8s) ensure(path string, obj any) error {
	body, _ := json.Marshal(obj)
	code, b, err := k.do("POST", path, body)
	if err != nil {
		return err
	}
	if code == 201 || code == 409 {
		return nil
	}
	return fmt.Errorf("POST %s -> %d: %s", path, code, truncate(b))
}

// ---- persisted agent state ----

// ownNamespace reads the namespace the agent is running in from its projected
// service-account volume. The state Secret lives here, alongside the agent.
func ownNamespace() string {
	b, err := os.ReadFile(nsPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// agentState is what the agent remembers across restarts.
type agentState struct {
	// envName is the ENV_NAME this state was enrolled under. Kept so a changed
	// ENV_NAME can be detected: without it, re-pointing an agent at a different
	// environment silently does nothing, because the persisted id wins.
	envName string
	token   string
	envID   string
	// stack is the meko-stack spec this agent enrolled with. Persisted alongside the
	// token for the same reason the token is: an agent that had to re-enrol to remember
	// what to run would depend on a 30-minute bootstrap credential forever. With it
	// here, a restart brings the whole data plane back with no control-plane call in the
	// path at all — the reconcile loop can begin before the first successful request.
	stack json.RawMessage
	// desired is the last desired state the agent successfully read, as the JSON array
	// of resources. It is what the agent reconciles toward when the control plane is
	// unreachable, and it is persisted so that a RESTART during an outage does not lose
	// it and fall back to the far staler enrolled stack.
	//
	// That is not theoretical. An agent ran for two and a half days after its control
	// plane's namespace was deleted, holding six components up on the in-memory copy; a
	// single eviction in that window would have silently reverted the environment to
	// whatever it was registered with, weeks of edits ago.
	desired json.RawMessage
}

// loadState reads the persisted state. The three cases are deliberately distinct:
//
//	(nil, nil)   no state yet — a fresh install, so enroll
//	(nil, err)   the state could not be read — do NOT enroll
//	(state, nil) usable state
//
// Collapsing the middle case into "no state" is what makes a transient API error
// look like a fresh install: the agent would re-enroll on a credential it may no
// longer be able to use, having thrown away a perfectly good one it just could not
// read. A 404 is the only "no state" answer.
func (k *k8s) loadState(ns string) (*agentState, error) {
	if ns == "" {
		return nil, fmt.Errorf("own namespace unknown (no %s)", nsPath)
	}
	code, body, err := k.do("GET", "/api/v1/namespaces/"+ns+"/secrets/"+stateSecret, nil)
	if err != nil {
		return nil, fmt.Errorf("read state secret: %w", err)
	}
	if code == 404 {
		return nil, nil // genuinely absent
	}
	if code != 200 {
		return nil, fmt.Errorf("read state secret -> %d: %s", code, truncate(body))
	}
	var s struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("decode state secret: %w", err)
	}
	// Secret values come back base64-encoded.
	dec := func(key string) string {
		raw, derr := base64.StdEncoding.DecodeString(s.Data[key])
		if derr != nil {
			return ""
		}
		return strings.TrimSpace(string(raw))
	}
	st := &agentState{envName: dec(envNameKey), token: dec(statusTokenKey), envID: dec(envIDKey)}
	if raw := dec(desiredKey); raw != "" && json.Valid([]byte(raw)) {
		st.desired = json.RawMessage(raw)
	}
	if raw := dec(stackKey); raw != "" {
		// A stack that will not parse is dropped rather than carried: reconciling a
		// half-decoded spec would apply a stack with fields silently missing. Without it
		// the agent falls back to whatever desired state offers, and says so.
		if json.Valid([]byte(raw)) {
			st.stack = json.RawMessage(raw)
		} else {
			log.Printf("persisted stack in %s/%s is not valid JSON — ignoring it", ns, stateSecret)
		}
	}
	// A token without the id it is scoped to is unusable — every agent call carries
	// ?envId=. Treat a partial write as absent rather than half-trusting it.
	if st.token == "" || st.envID == "" {
		log.Printf("state secret %s/%s is incomplete — ignoring it and enrolling", ns, stateSecret)
		return nil, nil
	}
	return st, nil
}

// saveState writes the status token and environment id to the state Secret,
// creating it or replacing its contents. Failure is logged, not fatal — the agent
// still works for this lifetime; it just cannot survive a restart, which is the
// behaviour before any of this existed.
func (k *k8s) saveState(ns string, st agentState) error {
	if ns == "" {
		return fmt.Errorf("own namespace unknown (no %s)", nsPath)
	}
	data := map[string]string{
		envNameKey: st.envName, statusTokenKey: st.token, envIDKey: st.envID,
	}
	if len(st.stack) > 0 {
		data[stackKey] = string(st.stack)
	}
	if len(st.desired) > 0 {
		data[desiredKey] = string(st.desired)
	}
	obj := map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": stateSecret, "namespace": ns,
			"labels": map[string]string{"app.kubernetes.io/managed-by": "meko-agent"},
		},
		"stringData": data,
	}
	body, _ := json.Marshal(obj)
	code, b, err := k.do("POST", "/api/v1/namespaces/"+ns+"/secrets", body)
	if err != nil {
		return err
	}
	if code == 201 {
		return nil
	}
	// Already there — PUT over it. A PUT rather than delete-then-create so there is
	// no window where the token is absent.
	if code == 409 {
		code, b, err = k.do("PUT", "/api/v1/namespaces/"+ns+"/secrets/"+stateSecret, body)
		if err != nil {
			return err
		}
		if code == 200 {
			return nil
		}
	}
	return fmt.Errorf("save state secret -> %d: %s", code, truncate(b))
}

// ---- YCP client ----

type ycp struct {
	base  string
	http  *http.Client
	token string // env-scoped status token (set after enroll)
}

func (m *ycp) req(method, path string, body, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, m.base+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if m.token != "" {
		req.Header.Set("Authorization", "Bearer "+m.token)
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		if err := decodeBody(resp.Body, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// decodeBody unwraps the two response shapes the agent has to speak to.
//
// api_server wraps payloads as {"data":…,"error":…}, but "error" is omitempty:
// on success it is dropped, so the body is {"data":…} with no "error" key.
// Decoding that straight into the payload struct silently yields zero values —
// which meant an empty environment_id and status_token, so every following
// status report was rejected with 400 while enrollment had already succeeded
// server-side. So an envelope is identified by the "data" key ALONE (requiring
// "error" too missed every success response). A flat payload — what the retired
// reference control plane returned — has no "data" key and is decoded as-is.
func decodeBody(r io.Reader, out any) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		hasEnvelope bool
	}
	// The api_server envelope always carries "data" and omits "error" on success,
	// so the "data" key alone identifies it. A flat payload (retired reference
	// control plane) has its fields at the top level and no "data" key.
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) == nil {
		if _, ok := probe["data"]; ok {
			env.hasEnvelope = true
		}
	}
	if !env.hasEnvelope {
		return json.Unmarshal(raw, out)
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	if env.Error != nil && env.Error.Message != "" {
		return fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

// enroll performs the token handshake and records the returned status token. It
// returns the environment id and the stack the control plane says this environment
// runs — everything needed to bring Meko up, in one response.
func (m *ycp) enroll(name, placement, enrollToken string, f api.ClusterFacts) (string, json.RawMessage, error) {
	var er api.EnrollResponse
	code, err := m.req("POST", "/api/v1/agent/enroll", api.EnrollRequest{
		Name: name, Placement: placement, EnrollmentToken: enrollToken, ClusterFacts: f,
	}, &er)
	if err != nil {
		return "", nil, err
	}
	if code != 200 {
		return "", nil, fmt.Errorf("enroll -> %d", code)
	}
	m.token = er.StatusToken
	return er.EnvironmentID, er.Stack, nil
}

func main() {
	// These defaults are demo residue and none of them are right for a real
	// deployment; the chart sets every one. ENV_NAME especially: unset, an agent
	// enrolls as some other environment's name rather than failing.
	// YCP_URL may carry a subpath (https://devcloud.yugabyte.com/ycp) now that
	// the control plane can be mounted under one. Every request appends
	// "/api/v1/…", so a trailing slash here would ask for "//api/v1/…" — a path
	// no route matches.
	ycpURL := strings.TrimRight(env("YCP_URL", "http://ycp:8080"), "/")
	envName := env("ENV_NAME", "itest-mcs")
	placement := env("ENV_PLACEMENT", "cloud")
	enrollToken := os.Getenv("ENROLLMENT_TOKEN")
	agentVer := env("AGENT_VERSION", "v1.4.2")
	interval := 10 * time.Second

	log.Printf("meko-agent starting: env=%s ycp=%s", envName, ycpURL)
	if enrollToken == "" {
		log.Printf("WARNING: ENROLLMENT_TOKEN empty — enrollment will be rejected")
	}

	kc, err := newK8s()
	if err != nil {
		log.Fatalf("k8s init: %v", err)
	}

	// One connection cache for the reconcilers that need a database.
	dpPool := &dbPool{}
	defer dpPool.close()

	m := &ycp{base: ycpURL, http: &http.Client{Timeout: 15 * time.Second}}

	facts, err := kc.facts()
	if err != nil {
		log.Fatalf("read cluster facts: %v", err)
	}
	log.Printf("cluster facts: provider=%s region=%s k8s=%s nodes=%d", facts.Provider, facts.Region, facts.K8sVersion, facts.NodeCount)

	// Enrollment is first contact only. The status token it returns is long-lived
	// and env-scoped, so once we have one we persist it and never touch the
	// enrollment token again — it is a bootstrap credential with a short TTL, and
	// on any restart after that TTL it would be useless.
	ns := ownNamespace()
	st, err := kc.loadState(ns)
	if err != nil {
		// Refuse to guess. Enrolling here would spend a bootstrap credential to
		// replace one that may well be sitting in the Secret we simply failed to
		// read, and on a restart into a throttled API server that trades a working
		// agent for a locked-out one. Crash-looping surfaces the real problem.
		log.Fatalf("cannot read agent state: %v", err)
	}

	// envID and stack are what the agent needs to do its job: who it is, and what it
	// runs. Both come from enrollment and both are persisted, so neither is fetched
	// again on a restart.
	var envID string
	var stack json.RawMessage
	// resumedDesired is the persisted fallback, and only for the identity we resumed.
	// Re-pointing an agent at a different environment must not leave it reconciling the
	// previous environment's resources when the control plane is unreachable.
	var resumedDesired json.RawMessage
	switch {
	case st != nil && st.envName != envName:
		// ENV_NAME changed under us: the operator is re-pointing this agent. The
		// persisted identity is for a different environment, so discard it rather
		// than silently continuing to serve the old one.
		log.Printf("ENV_NAME changed (%q -> %q) — discarding persisted state and re-enrolling", st.envName, envName)
	case st != nil:
		m.token, envID, stack, resumedDesired = st.token, st.envID, st.stack, st.desired
		log.Printf("resuming as env=%s with the persisted status token; not enrolling", envID)
	}

	if envID == "" {
		for {
			id, s, eerr := m.enroll(envName, placement, enrollToken, facts)
			if eerr == nil {
				envID, stack = id, s
				break
			}
			log.Printf("enroll failed (%v), retrying in 3s", eerr)
			time.Sleep(3 * time.Second)
		}
		log.Printf("enrolled: environment_id=%s (status token acquired)", envID)
		// Fatal on purpose. An agent that cannot persist works until its first
		// restart and then silently stops reporting — the exact failure this whole
		// change exists to remove. Crash-looping makes a missing RBAC grant obvious
		// while the enrollment token is still valid and reusable.
		if serr := kc.saveState(ns, agentState{envName: envName, token: m.token, envID: envID, stack: stack}); serr != nil {
			log.Fatalf("enrolled but could not persist agent state: %v "+
				"(the ServiceAccount needs get/create/update on secrets in %s)", serr, ns)
		}
	}
	if len(stack) > 0 {
		log.Printf("enrolled with a Meko stack to run; deploying it on every tick")
	} else {
		log.Printf("no stack came with enrollment — this environment runs only what desired state declares")
	}

	// wasReady distinguishes "still coming up" from "was up and broke", which is the
	// difference between PROVISIONING and DEGRADED. The state used to be a fixed
	// three-tick walk to READY regardless of what was running; with real components
	// to report there is no reason to narrate a lifecycle instead of observing one.
	wasReady := false
	// lastGood is the newest desired state the agent has actually read. See
	// resourcesToApply: it is what the agent falls back to during a control-plane
	// outage, so that an outage cannot rewind the data plane to what the environment
	// was registered with. Seeded from the state Secret, so an outage that spans a
	// restart keeps it.
	var lastGood []api.DesiredResource
	// persistedDesired is what is currently in the Secret, so the write below happens
	// when desired state CHANGES rather than on every tick. A Secret write every ten
	// seconds, forever, is a lot of API traffic and audit noise for a value that changes
	// when an operator does something.
	var persistedDesired json.RawMessage
	lastGood, persistedDesired = resumeDesired(resumedDesired)
	if lastGood != nil {
		log.Printf("resuming with the last desired state read before restart (%d resources)", len(lastGood))
	}
	for {
		// 1) Desired state, which is where the stack was written at registration and where
		//    an operator's later changes go. When it can be read it is authoritative —
		//    including when it is empty, which is how an environment is told to run
		//    nothing. When it cannot, the stack the agent enrolled with stands: that is
		//    what carrying it in the enroll response buys, a data plane the agent can
		//    deploy from something it already holds, on its first tick and after every
		//    restart, with no successful request in the path.
		var ds api.DesiredState
		dcode, derr := m.req("GET", "/api/v1/agent/desired-state?envId="+envID, nil, &ds)
		desiredRead := derr == nil && dcode >= 200 && dcode <= 299
		switch {
		case dcode == 401 || dcode == 404:
			// The status token was rejected — revoked, rotated, or the environment
			// was re-registered. Re-enrolling needs an enrollment token that is still
			// valid; past its TTL an operator has to issue a new one.
			//
			// Not a `continue`: the stack below is reconciled anyway. An environment
			// whose token went stale should not have its data plane left to rot while
			// somebody mints a new one.
			log.Printf("desired-state %d — re-enrolling", dcode)
			if id, s, err := m.enroll(envName, placement, enrollToken, facts); err == nil {
				envID = id
				if len(s) > 0 {
					stack = s
				}
				if err := kc.saveState(ns, agentState{
					envName: envName, token: m.token, envID: envID, stack: stack, desired: persistedDesired,
				}); err != nil {
					log.Printf("WARNING could not persist the new agent state: %v", err)
				}
			}
		case !desiredRead:
			log.Printf("desired-state unavailable (code=%d err=%v) — holding what we last read and changing nothing else", dcode, derr)
		}
		if desiredRead {
			// Non-nil even when empty: "" and "nothing" are different answers, and an
			// empty slice is the one that means the operator withdrew everything.
			lastGood = ds.Resources
			if lastGood == nil {
				lastGood = []api.DesiredResource{}
			}
			// Persist it, but only when it changed — see persistedDesired.
			if raw, merr := json.Marshal(lastGood); merr == nil && string(raw) != string(persistedDesired) {
				if serr := kc.saveState(ns, agentState{
					envName: envName, token: m.token, envID: envID, stack: stack, desired: raw,
				}); serr != nil {
					// Not fatal: the agent works for this lifetime, it just loses the
					// fallback if it restarts mid-outage. That is where it was before.
					log.Printf("WARNING could not persist desired state: %v", serr)
				} else {
					persistedDesired = raw
				}
			}
		}
		resources := resourcesToApply(desiredRead, ds.Resources, lastGood, stack)

		// 2) Reconcile. Each resource goes to the reconciler for its Kind: meko-stack
		//    (the whole data plane, in mekostack.go) or one of the individual kinds in
		//    stack.go. A kind with no reconciler is logged and skipped — an older agent
		//    must ignore what a newer control plane sends, not die on it.
		var stackRes []api.DesiredResource
		for _, res := range resources {
			if stackKinds[res.Kind] {
				stackRes = append(stackRes, res)
				continue
			}
			log.Printf("no reconciler for kind %q — skipping %q", res.Kind, res.Name)
		}

		sk := newStack(kc, ns, dpPool)
		sk.reconcile(stackRes)
		// GC only when desired state was actually read. It deletes what is no longer
		// declared, and on a tick where the control plane could not be reached the agent
		// does not know what is no longer declared — it knows only what it enrolled with.
		// Collecting against that would delete every resource an operator had added.
		if desiredRead {
			sk.gc()
		}

		// Components are read back from the API server, never assumed. A fleet view
		// that shows what was requested rather than what is running is worse than one
		// that shows nothing.
		comps := append([]api.Component{
			{Name: "meko-agent", Healthy: 1, Total: 1, Version: agentVer},
		}, sk.report()...)
		state := stateFor(comps, &wasReady)

		scode, err := m.req("POST", "/api/v1/agent/status?envId="+envID, api.StatusReport{
			EnvironmentState: state, AgentVersion: agentVer, AppliedRevision: ds.Revision,
			ClusterFacts: facts, Components: comps,
		}, nil)
		switch {
		case err != nil:
			log.Printf("status error: %v", err)
		case scode == 401 || scode == 404:
			log.Printf("status %d — re-enrolling", scode)
			if id, s, e := m.enroll(envName, placement, enrollToken, facts); e == nil {
				envID = id
				if len(s) > 0 {
					stack = s
				}
				// Persist here too. Without it the rotated token lives only in
				// memory and the next restart resumes with the dead one — the agent
				// then 401s from the very first call.
				if e := kc.saveState(ns, agentState{
					envName: envName, token: m.token, envID: envID, stack: stack, desired: persistedDesired,
				}); e != nil {
					log.Printf("WARNING could not persist the rotated agent state: %v", e)
				}
			}
		default:
			log.Printf("status: state=%s components=[%s] declared=%d rev=%d -> %d",
				state, summarize(comps), len(stackRes), ds.Revision, scode)
		}
		time.Sleep(interval)
	}
}

// ---- data-plane database ----
//
// What remains of the database code after datapacks were retired: creating the
// database a declared workload runs against. The per-datapack machinery that used to
// live here — starter schemas, dropping databases, listing them for GC — went with
// the datapack layer, since an environment is now the unit and Meko manages its own
// tenants inside it.

// baseName collapses a name into a safe SQL identifier.
var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func baseName(name string) string { return nonAlnum.ReplaceAllString(strings.ToLower(name), "") }

type dpdb struct {
	pool    *pgxpool.Pool
	baseURL string // DSN to the admin database; per-database DSNs swap the path
}

func newDPDB(dsn string) (*dpdb, error) {
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &dpdb{pool: pool, baseURL: dsn}, nil
}

// urlFor returns the DSN pointing at a specific database.
func (d *dpdb) urlFor(dbName string) string {
	u, err := url.Parse(d.baseURL)
	if err != nil {
		return d.baseURL
	}
	u.Path = "/" + dbName
	return u.String()
}

func (d *dpdb) close() {
	if d != nil {
		d.pool.Close()
	}
}

// ensureDatabase creates a database if it is not already there. CREATE DATABASE
// cannot run from a connection to the database being created, hence the admin DSN.
func (d *dpdb) ensureDatabase(name string) error {
	ctx := context.Background()
	var one int
	err := d.pool.QueryRow(ctx, `SELECT 1 FROM pg_database WHERE datname=$1`, name).Scan(&one)
	if err == nil {
		return nil // already exists
	}
	if err != pgx.ErrNoRows {
		return err
	}
	// name is a sanitized identifier (baseName), safe to interpolate.
	_, err = d.pool.Exec(ctx, `CREATE DATABASE "`+name+`"`)
	return err
}

// createExtension installs an extension into the database this connection is for.
//
// CASCADE because the extensions Meko needs have their own dependencies — pg_dist_rag
// pulls in `vector` — and installing them in the wrong order otherwise fails with a
// missing-dependency error that reads like the extension itself is unavailable.
//
// IF NOT EXISTS because this runs on every tick. It is not free the first time (mage
// and pg_dist_rag each create a schema), but on a database that already has them it is
// a catalogue lookup.
//
// name is validated by validExtensionName before it reaches here; it cannot be a bind
// parameter, since CREATE EXTENSION takes an identifier rather than a value.
func (d *dpdb) createExtension(name string) error {
	_, err := d.pool.Exec(context.Background(), `CREATE EXTENSION IF NOT EXISTS "`+name+`" CASCADE`)
	return err
}

// dbPool caches one connection per DSN, for the reconcilers that need one.
//
// Keyed rather than a single slot: nothing here is known at startup — a `database`
// resource may be declared on any tick, and its Service takes a while to answer — so
// a failure to connect is a logged skip and a retry next tick, never fatal.
type dbPool struct {
	conns map[string]*dpdb
	// warned suppresses the repeated log for a DSN that is still unreachable, not the
	// retry itself. A database that is starting fails every tick for the first half
	// minute, and that should not bury everything else in the log.
	warned map[string]bool
	// said is the same idea for the reconcilers' own messages, keyed by caller. It lives
	// on the pool because the pool is the one thing that survives a tick — the stack is
	// rebuilt from scratch each time round the loop.
	said map[string]string
}

// logOnce logs msg only when it differs from the last message logged under key.
//
// A YugabyteDB takes about ninety seconds to come up and the loop ticks every ten, so
// each reconciler waiting on it would otherwise print the same line nine times, three
// reconcilers deep. What an operator watching a bring-up needs to see is the transitions,
// and those are exactly what repetition hides.
func (p *dbPool) logOnce(key, msg string) {
	if p.said[key] == msg {
		return
	}
	if p.said == nil {
		p.said = map[string]string{}
	}
	p.said[key] = msg
	log.Print(msg)
}

func (p *dbPool) use(dsn string) *dpdb {
	if dsn == "" {
		return nil
	}
	if d, ok := p.conns[dsn]; ok {
		return d
	}
	// Retry on every tick while unconnected: a database declared this tick is not
	// accepting connections yet, so the first attempt failing is the normal case.
	d, err := newDPDB(dsn)
	if err != nil {
		if !p.warned[dsn] {
			log.Printf("data-plane db not reachable yet (%v) — retrying each tick", err)
			if p.warned == nil {
				p.warned = map[string]bool{}
			}
			p.warned[dsn] = true // suppress the repeat, not the retry
		}
		return nil
	}
	if p.conns == nil {
		p.conns = map[string]*dpdb{}
	}
	p.conns[dsn] = d
	delete(p.warned, dsn)
	log.Printf("data-plane db connected (%s)", redactDSN(dsn))
	return d
}

func (p *dbPool) close() {
	for dsn, d := range p.conns {
		d.close()
		delete(p.conns, dsn)
	}
}

// redactDSN strips the password before a DSN reaches the log.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "(unparseable dsn)"
	}
	if _, hasPW := u.User.Password(); hasPW {
		u.User = url.UserPassword(u.User.Username(), "xxxxx")
	}
	return u.Redacted()
}
