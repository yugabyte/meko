package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
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
	tok  string
	ns   string
	http *http.Client
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

func (k *k8s) do(method, path, contentType string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, k.host+path, rdr)
	req.Header.Set("Authorization", "Bearer "+k.tok)
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
