// Command agent is a minimal meko-agent. It reads real cluster facts from the
// in-cluster Kubernetes API (no client-go — just the service-account token),
// completes the enrollment-token handshake with YCP, then on each tick:
//   - pulls desired state (bearer-authed),
//   - reconciles desired datapacks into labeled namespaces,
//   - reports status (real facts + discovered + managed datapacks).
//
// All traffic is outbound: agent -> k8s API, agent -> YCP.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"meko/ycp/internal/api"
)

const (
	tokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	caPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	// managedLabel marks namespaces this agent provisions for datapacks.
	managedLabel = "meko.dev/datapack"
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

// applyDatapackWorkload provisions the per-datapack service: a Secret with the
// datapack DB URI and a Deployment + Service running the real MCP server (dpmcp)
// over that database. (In production this is the meko-mcp-server image.)
func (k *k8s) applyDatapackWorkload(ns, dbURI, image string) error {
	const port = 3000
	labels := map[string]string{"app": "mcp", "app.kubernetes.io/managed-by": "meko-agent"}

	if err := k.ensure("/api/v1/namespaces/"+ns+"/secrets", map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata":   map[string]any{"name": "datapack-db", "namespace": ns},
		"stringData": map[string]string{"uri": dbURI},
	}); err != nil {
		return err
	}

	if err := k.ensure("/apis/apps/v1/namespaces/"+ns+"/deployments", map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "mcp", "namespace": ns, "labels": labels},
		"spec": map[string]any{
			"replicas": 1,
			"selector": map[string]any{"matchLabels": map[string]string{"app": "mcp"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"containers": []any{map[string]any{
						"name":  "mcp",
						"image": image,
						"ports": []any{map[string]any{"containerPort": port}},
						"env": []any{
							map[string]any{"name": "DP_DB_URL", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "datapack-db", "key": "uri"}}},
						},
						"readinessProbe": map[string]any{
							"httpGet":       map[string]any{"path": "/healthz", "port": port},
							"periodSeconds": 5,
						},
						"resources": map[string]any{"requests": map[string]string{"cpu": "25m", "memory": "32Mi"}, "limits": map[string]string{"cpu": "250m", "memory": "128Mi"}},
					}},
				},
			},
		},
	}); err != nil {
		return err
	}

	return k.ensure("/api/v1/namespaces/"+ns+"/services", map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "mcp", "namespace": ns, "labels": labels},
		"spec": map[string]any{
			"selector": map[string]string{"app": "mcp"},
			"ports":    []any{map[string]any{"name": "http", "port": port, "targetPort": port}},
		},
	})
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
// cmd/ycp returns the payload flat; api_server wraps it as
// {"data":…,"error":…}. Decoding the wrapped form straight into the payload
// struct silently yields zero values — which meant an empty environment_id and
// status_token, so every following status report was rejected with 400 while
// enrollment had already succeeded server-side. Detect the envelope and decode
// its "data" instead.
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
	// Only treat it as an envelope when "data" is actually present: a flat payload
	// that happens to lack every field would otherwise look like one.
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) == nil {
		if _, ok := probe["data"]; ok {
			if _, ok := probe["error"]; ok {
				env.hasEnvelope = true
			}
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

// enroll performs the token handshake and records the returned status token.
func (m *ycp) enroll(name, placement, enrollToken string, f api.ClusterFacts) (string, error) {
	var er api.EnrollResponse
	code, err := m.req("POST", "/api/v1/agent/enroll", api.EnrollRequest{
		Name: name, Placement: placement, EnrollmentToken: enrollToken, ClusterFacts: f,
	}, &er)
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", fmt.Errorf("enroll -> %d", code)
	}
	m.token = er.StatusToken
	return er.EnvironmentID, nil
}

func main() {
	ycpURL := env("YCP_URL", "http://ycp:8080")
	envName := env("ENV_NAME", "itest-mcs")
	placement := env("ENV_PLACEMENT", "cloud")
	enrollToken := os.Getenv("ENROLLMENT_TOKEN")
	managedPrefix := env("MANAGED_NS_PREFIX", "ycp-dp-")
	dpDBURL := os.Getenv("DP_DB_URL")
	mcpImage := env("DPMCP_IMAGE", "postgrest/postgrest:v12.2.3")
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

	// Data-plane database: where per-datapack databases are provisioned (the customer's
	// YugabyteDB in production). Without it, datapacks reconcile to a namespace only.
	var dp *dpdb
	if dpDBURL != "" {
		d, err := newDPDB(dpDBURL)
		if err != nil {
			log.Fatalf("data-plane db: %v", err)
		}
		dp = d
		defer dp.close()
		log.Printf("data-plane db connected")
	} else {
		log.Printf("DP_DB_URL empty — per-datapack databases will not be provisioned")
	}

	m := &ycp{base: ycpURL, http: &http.Client{Timeout: 15 * time.Second}}

	facts, err := kc.facts()
	if err != nil {
		log.Fatalf("read cluster facts: %v", err)
	}
	log.Printf("cluster facts: provider=%s region=%s k8s=%s nodes=%d", facts.Provider, facts.Region, facts.K8sVersion, facts.NodeCount)

	var envID string
	for {
		id, err := m.enroll(envName, placement, enrollToken, facts)
		if err == nil {
			envID = id
			break
		}
		log.Printf("enroll failed (%v), retrying in 3s", err)
		time.Sleep(3 * time.Second)
	}
	log.Printf("enrolled: environment_id=%s (status token acquired)", envID)

	states := []api.EnvState{api.StateProvisioning, api.StateValidating, api.StateReady}
	i := 0
	for {
		// 1) pull desired state (bearer-authed)
		var ds api.DesiredState
		dcode, _ := m.req("GET", "/api/v1/agent/desired-state?envId="+envID, nil, &ds)
		if dcode == 401 || dcode == 404 {
			log.Printf("desired-state %d — re-enrolling", dcode)
			if id, err := m.enroll(envName, placement, enrollToken, facts); err == nil {
				envID = id
			}
			time.Sleep(interval)
			continue
		}

		// 2) reconcile datapacks to match desired state (create missing, GC removed),
		//    provisioning a real per-datapack namespace + database. dps is the ACTUAL
		//    provisioned state reported back — never invented from unrelated resources.
		var desired []string
		for _, res := range ds.Resources {
			if res.Kind == "datapack" {
				desired = append(desired, res.Name)
			} else {
				log.Printf("no reconciler for kind %q — skipping %q", res.Kind, res.Name)
			}
		}
		dps := reconcileDatapacks(kc, dp, managedPrefix, mcpImage, desired)

		state := api.StateReady
		if i < len(states) {
			state = states[i]
		}
		// Only report components this agent actually runs. (mcp-server / rag-worker /
		// yugabytedb / langfuse are not yet deployed by this demo agent, so we don't
		// fabricate them.)
		comps := []api.Component{
			{Name: "meko-agent", Healthy: 1, Total: 1, Version: agentVer},
		}

		scode, err := m.req("POST", "/api/v1/agent/status?envId="+envID, api.StatusReport{
			EnvironmentState: state, AgentVersion: agentVer, AppliedRevision: ds.Revision,
			ClusterFacts: facts, Components: comps, DatapackPhases: dps,
		}, nil)
		switch {
		case err != nil:
			log.Printf("status error: %v", err)
		case scode == 401 || scode == 404:
			log.Printf("status %d — re-enrolling", scode)
			if id, e := m.enroll(envName, placement, enrollToken, facts); e == nil {
				envID = id
			}
		default:
			log.Printf("status: state=%s datapacks=%d desired=%d rev=%d -> %d", state, len(dps), len(ds.Resources), ds.Revision, scode)
		}
		if i < len(states) {
			i++
		}
		time.Sleep(interval)
	}
}

// nonAlnum collapses a datapack name into a safe identifier base used for both the
// namespace suffix and the database name.
var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func baseName(name string) string { return nonAlnum.ReplaceAllString(strings.ToLower(name), "") }

// reconcileDatapacks converges the cluster to exactly the desired datapack set:
// each desired datapack gets a namespace + a real database; anything managed but no
// longer desired is garbage-collected. Returns the ACTUAL provisioned state.
func reconcileDatapacks(kc *k8s, dp *dpdb, managedPrefix, mcpImage string, desired []string) []api.DatapackPhase {
	want := map[string]string{} // base -> display name
	var order []string
	for _, n := range desired {
		b := baseName(n)
		if b == "" {
			continue
		}
		if _, ok := want[b]; !ok {
			order = append(order, b)
		}
		want[b] = n
	}

	var reported []api.DatapackPhase
	for _, b := range order {
		ns, dbName := managedPrefix+b, "dp_"+b
		phase, errStr, dbField, endpoint := "Active", "", "", ""
		fail := func(err error) { phase, errStr = "Error", err.Error() }

		if _, err := kc.ensureNamespace(ns, want[b]); err != nil {
			fail(err)
		}
		if phase != "Error" && dp != nil {
			if err := dp.ensureDatabase(dbName); err != nil {
				fail(err)
			} else if err := dp.ensureSchema(dbName); err != nil {
				fail(err)
			} else {
				dbField = dbName
				// provision the per-datapack service over its database
				if err := kc.applyDatapackWorkload(ns, dp.urlFor(dbName), mcpImage); err != nil {
					fail(err)
				} else {
					endpoint = fmt.Sprintf("http://mcp.%s.svc.cluster.local:3000", ns)
				}
			}
		}
		reported = append(reported, api.DatapackPhase{ID: ns, Name: want[b], Phase: phase, DB: dbField, Endpoint: endpoint, Error: errStr})
	}

	// GC namespaces no longer desired
	if items, err := kc.namespaces(); err == nil {
		for _, n := range items {
			if strings.HasPrefix(n.Metadata.Name, managedPrefix) {
				if _, ok := want[strings.TrimPrefix(n.Metadata.Name, managedPrefix)]; !ok {
					if err := kc.deleteNamespace(n.Metadata.Name); err != nil {
						log.Printf("gc namespace %s: %v", n.Metadata.Name, err)
					} else {
						log.Printf("GC: deleted namespace %s", n.Metadata.Name)
					}
				}
			}
		}
	}
	// GC databases no longer desired
	if dp != nil {
		if dbs, err := dp.listDatabases("dp_"); err == nil {
			for _, dbn := range dbs {
				if _, ok := want[strings.TrimPrefix(dbn, "dp_")]; !ok {
					if err := dp.dropDatabase(dbn); err != nil {
						log.Printf("gc database %s: %v", dbn, err)
					} else {
						log.Printf("GC: dropped database %s", dbn)
					}
				}
			}
		}
	}
	sort.Slice(reported, func(i, j int) bool { return reported[i].ID < reported[j].ID })
	return reported
}

// ---- data-plane database (per-datapack DB provisioning) ----

type dpdb struct {
	pool    *pgxpool.Pool
	baseURL string // DSN to the admin ("postgres") database; per-datapack DSNs swap the path
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

// urlFor returns the DSN pointing at a specific datapack database.
func (d *dpdb) urlFor(dbName string) string {
	u, err := url.Parse(d.baseURL)
	if err != nil {
		return d.baseURL
	}
	u.Path = "/" + dbName
	return u.String()
}

// ensureSchema connects to a datapack database and creates the starter schema
// (a memories table) so its per-datapack service has something real to serve.
func (d *dpdb) ensureSchema(dbName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, d.urlFor(dbName))
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS memories (
		id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		text       text NOT NULL,
		created_at timestamptz NOT NULL DEFAULT now()
	)`)
	return err
}

func (d *dpdb) close() {
	if d != nil {
		d.pool.Close()
	}
}

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
	// db name is a sanitized identifier (baseName), safe to interpolate.
	_, err = d.pool.Exec(ctx, `CREATE DATABASE "`+name+`"`)
	return err
}

func (d *dpdb) dropDatabase(name string) error {
	_, err := d.pool.Exec(context.Background(), `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
	return err
}

func (d *dpdb) listDatabases(prefix string) ([]string, error) {
	rows, err := d.pool.Query(context.Background(), `SELECT datname FROM pg_database WHERE datname LIKE $1`, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
