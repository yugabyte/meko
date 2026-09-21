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
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	tokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	caPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	nsPath    = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// k8s is a tiny in-cluster REST client — just the ServiceAccount token + CA, no
// client-go — used to persist the agent's identity in a Secret so a restart that
// cannot reach the control plane still knows which environment it is.
type k8s struct {
	host string
	ns   string
	http *http.Client

	// compsForbidden latches when the namespace Role lacks list on apps workloads,
	// so the components collector reports that once instead of every tick.
	compsForbidden bool

	mu  sync.RWMutex
	tok string // last token read off disk; see bearer
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
	ns, err := os.ReadFile(nsPath)
	if err != nil {
		return nil, fmt.Errorf("read sa namespace: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return &k8s{
		host: fmt.Sprintf("https://%s:%s", host, port),
		tok:  strings.TrimSpace(string(tok)),
		ns:   strings.TrimSpace(string(ns)),
		http: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}},
	}, nil
}

// bearer re-reads the ServiceAccount token per request rather than trusting the
// copy taken at startup. The kubelet rotates projected tokens on disk and expires
// the old one, which never bit while the only k8s calls happened seconds after
// boot — the controller kind polls a Deployment on every tick, for as long as the
// pod lives. A failed read keeps the last good token: a token that still has life
// left beats no Authorization header at all.
func (k *k8s) bearer() string {
	if b, err := os.ReadFile(tokenPath); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			k.mu.Lock()
			k.tok = t
			k.mu.Unlock()
			return t
		}
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.tok
}

func (k *k8s) do(method, path, contentType string, body []byte) (int, []byte, error) {
	return k.doCtx(context.Background(), method, path, contentType, body)
}

func (k *k8s) doCtx(ctx context.Context, method, path, contentType string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, _ := http.NewRequestWithContext(ctx, method, k.host+path, rdr)
	req.Header.Set("Authorization", "Bearer "+k.bearer())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

// state is the agent's persisted identity, mirrored to a k8s Secret. No
// credential lives here: the API key is operator-provided, so the only thing
// worth remembering is which environment the key was bound to when the control
// plane last answered.
type state struct {
	EnvID   string `json:"environment-id"`
	EnvUUID string `json:"environment-uuid"`
	EnvName string `json:"environment-name"`
	// EnvKind is what this identity was confirmed as, and is compared on resume: an
	// operator re-pointing this agent at an environment of the other kind must not
	// have the persisted identity silently accepted. An empty value is a state
	// written before this field existed, which is treated as a match.
	EnvKind string `json:"environment-kind,omitempty"`
}

func (k *k8s) secretPath(name string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/secrets/%s", k.ns, name)
}

// readState loads persisted identity from the Secret. A missing Secret returns
// a zero state and no error (first run).
func (k *k8s) readState(secret string) (state, error) {
	code, raw, err := k.do(http.MethodGet, k.secretPath(secret), "", nil)
	if err != nil {
		return state{}, err
	}
	if code == http.StatusNotFound {
		return state{}, nil
	}
	if code >= 300 {
		return state{}, fmt.Errorf("get secret %s: HTTP %d: %s", secret, code, raw)
	}
	var s struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return state{}, err
	}
	dec := func(k string) string {
		b, _ := base64.StdEncoding.DecodeString(s.Data[k])
		return string(b)
	}
	return state{
		EnvID:   dec("environment-id"),
		EnvUUID: dec("environment-uuid"),
		EnvName: dec("environment-name"),
		EnvKind: dec("environment-kind"),
	}, nil
}

// writeState upserts the identity Secret (create on first run, merge-patch
// after) using stringData so values are written in plaintext and base64-encoded
// by the API server. The merge-patch also nulls the data key `status-token` an
// older agent may have left behind, so no dead credential lingers in the Secret.
func (k *k8s) writeState(secret string, s state) error {
	sd := map[string]string{
		"environment-id":   s.EnvID,
		"environment-uuid": s.EnvUUID,
		"environment-name": s.EnvName,
		"environment-kind": s.EnvKind,
	}
	code, _, err := k.do(http.MethodGet, k.secretPath(secret), "", nil)
	if err != nil {
		return err
	}
	if code == http.StatusNotFound {
		body, _ := json.Marshal(map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]any{"name": secret, "namespace": k.ns},
			"stringData": sd,
		})
		c, raw, err := k.do(http.MethodPost, fmt.Sprintf("/api/v1/namespaces/%s/secrets", k.ns), "application/json", body)
		if err != nil {
			return err
		}
		if c >= 300 {
			return fmt.Errorf("create secret %s: HTTP %d: %s", secret, c, raw)
		}
		return nil
	}
	body, _ := json.Marshal(map[string]any{
		"stringData": sd,
		"data":       map[string]any{"status-token": nil},
	})
	c, raw, err := k.do(http.MethodPatch, k.secretPath(secret), "application/merge-patch+json", body)
	if err != nil {
		return err
	}
	if c >= 300 {
		return fmt.Errorf("patch secret %s: HTTP %d: %s", secret, c, raw)
	}
	return nil
}

// deploymentPath addresses one Deployment, or the namespace's collection when
// name is empty.
func (k *k8s) deploymentPath(name string) string {
	base := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments", k.ns)
	if name == "" {
		return base
	}
	return base + "/" + name
}

// releaseName discovers the helm release this agent is a member of, by reading
// the annotation helm stamps on every object it manages off any Deployment in the
// namespace.
//
// Discovered rather than configured, because the chart does not fix the release
// name and a wrong guess fails silently in the only place it matters: `helm
// upgrade <wrong-name>` answers "release: not found", once per tick, for as long
// as the environment declares a controller. Every member of a release carries the
// same annotation, so any Deployment answers — including the agent's own.
//
// An empty answer is not an error. The caller falls back to its configured
// default, which is what an install whose objects were not applied by helm needs.
func (k *k8s) releaseName(ctx context.Context) string {
	code, raw, err := k.doCtx(ctx, http.MethodGet, k.deploymentPath(""), "", nil)
	if err != nil || code != http.StatusOK {
		return ""
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return ""
	}
	for _, it := range list.Items {
		if name := it.Metadata.Annotations["meta.helm.sh/release-name"]; name != "" {
			return name
		}
	}
	return ""
}

// rolledOut reports whether a Deployment is fully on its current pod template —
// the same question `kubectl rollout status` answers, and the one that says a
// controller upgrade has landed rather than merely been submitted.
//
// observedGeneration is what makes it an answer about THIS template: without it a
// read landing between the helm upgrade and the Deployment controller noticing
// reports the old, settled ReplicaSet as complete.
// It also returns the image tag the pod template carries. That, not the version
// the controller reports about itself, is what a declared version is compared
// against: the spec names an image tag and `/version` answers with the binary's
// own stamp (`0.260714.0-199ec1a` against `custom-28386c402c60`), so comparing
// the two would never converge.
func (k *k8s) rolledOut(ctx context.Context, name string) (bool, string, error) {
	code, raw, err := k.doCtx(ctx, http.MethodGet, k.deploymentPath(name), "", nil)
	if err != nil {
		return false, "", err
	}
	if code >= 300 {
		return false, "", fmt.Errorf("get deployment %s: HTTP %d: %s", name, code, raw)
	}
	var d struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
		Spec struct {
			Replicas *int32 `json:"replicas"`
			Template struct {
				Spec struct {
					Containers []struct {
						Name  string `json:"name"`
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
		Status struct {
			ObservedGeneration int64 `json:"observedGeneration"`
			Replicas           int32 `json:"replicas"`
			UpdatedReplicas    int32 `json:"updatedReplicas"`
			AvailableReplicas  int32 `json:"availableReplicas"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return false, "", err
	}
	want := int32(1) // the API server defaults an omitted spec.replicas to 1
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	rolled := d.Status.ObservedGeneration >= d.Metadata.Generation &&
		d.Status.UpdatedReplicas == want &&
		d.Status.Replicas == d.Status.UpdatedReplicas &&
		d.Status.AvailableReplicas >= d.Status.UpdatedReplicas
	tag := ""
	if cs := d.Spec.Template.Spec.Containers; len(cs) > 0 {
		tag = imageTag(cs[0].Image)
	}
	return rolled, tag, nil
}
