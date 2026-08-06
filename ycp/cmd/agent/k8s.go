// Kubernetes API calls the stack reconcilers need on top of the minimal client in
// main.go: server-side apply, reading a workload's real readiness back, listing by
// label, and the one Secret the agent creates for itself.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// applySSA applies obj with server-side apply.
//
// The alternative — POST and treat 409 as success — is what the datapack path did,
// and it is create-once: change an image in desired state and the agent reports it
// applied while the old pod keeps running. A blind PUT is not the fix either, since
// it fails on immutable fields (a Service's clusterIP is assigned by the server, and
// PUTting it back as "" is rejected). Apply merges field-by-field and is idempotent,
// which is exactly the semantics a reconcile loop wants.
//
// force=true takes ownership of fields another manager set. Without it, an object
// someone once edited with kubectl comes back as a conflict on every tick forever.
func (k *k8s) applySSA(path string, obj any) error {
	body, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	url := path + "?fieldManager=" + fieldManager + "&force=true"
	req, err := http.NewRequest(http.MethodPatch, k.host+url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Accept", "application/json")
	// Apply patches are declared as YAML; JSON is valid YAML, so the JSON body above
	// is accepted. There is no application/apply-patch+json.
	req.Header.Set("Content-Type", "application/apply-patch+yaml")
	resp, err := k.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return nil
	}
	return fmt.Errorf("apply %s -> %d: %s", path, resp.StatusCode, truncate(b))
}

// deletePath deletes one object. 404 is success: the goal is absence.
func (k *k8s) deletePath(path string) error {
	code, b, err := k.do(http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusOK, http.StatusAccepted, http.StatusNotFound:
		return nil
	}
	return fmt.Errorf("delete %s -> %d: %s", path, code, truncate(b))
}

// listLabeled lists objects at a collection path matching a label selector.
func (k *k8s) listLabeled(path, selector string) ([]nsItem, error) {
	var out nameList
	if err := k.getJSON(path+"?labelSelector="+url.QueryEscape(selector), &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// workloadReadiness reports (ready, desired) replicas for a Deployment or
// StatefulSet. Both spell it the same way in status, and both leave readyReplicas
// absent rather than 0 before the first pod reports in.
func (k *k8s) workloadReadiness(ns, kind, name string) (ready, total int, err error) {
	var path string
	switch kind {
	case "StatefulSet":
		path = stsPath(ns, name)
	case "Deployment":
		path = depPath(ns, name)
	default:
		return 0, 0, fmt.Errorf("no readiness for kind %q", kind)
	}
	var w struct {
		Spec struct {
			Replicas *int `json:"replicas"`
		} `json:"spec"`
		Status struct {
			ReadyReplicas int `json:"readyReplicas"`
		} `json:"status"`
	}
	if err := k.getJSON(path, &w); err != nil {
		return 0, 0, err
	}
	total = 1
	if w.Spec.Replicas != nil {
		total = *w.Spec.Replicas
	}
	return w.Status.ReadyReplicas, total, nil
}

// secretValue reads one key out of a Secret.
func (k *k8s) secretValue(ns, name, key string) (string, error) {
	code, body, err := k.do(http.MethodGet, secretPath(ns, name), nil)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("get secret %s/%s -> %d: %s", ns, name, code, truncate(body))
	}
	var secret struct {
		Data map[string]string `json:"data"`
	}
	if uerr := json.Unmarshal(body, &secret); uerr != nil {
		return "", uerr
	}
	raw, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("secret %s/%s has no key %q", ns, name, key)
	}
	dec, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("decode %s/%s key %q: %w", ns, name, key, err)
	}
	return strings.TrimSpace(string(dec)), nil
}

// ensurePassword returns the password in a Secret, creating the Secret with a
// generated one if it is absent.
//
// Read-then-create, and NOT apply: applying a freshly generated password every tick
// would rotate the credential under a database that has already initialised with
// the old one — the database keeps the first password, the workloads get the newest,
// and nothing can connect. Whoever wins the create race wins for good; the loser
// reads back what is stored.
func (k *k8s) ensurePassword(ns, name, key string) (string, error) {
	pw, err := k.secretValue(ns, name, key)
	if err == nil && pw != "" {
		return pw, nil
	}
	gen, gerr := generatePassword()
	if gerr != nil {
		return "", gerr
	}
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": name, "namespace": ns,
			"labels": map[string]string{"app.kubernetes.io/managed-by": fieldManager},
		},
		"stringData": map[string]string{key: gen},
	})
	code, b, derr := k.do(http.MethodPost, "/api/v1/namespaces/"+ns+"/secrets", body)
	if derr != nil {
		return "", derr
	}
	switch code {
	case http.StatusCreated:
		return gen, nil
	case http.StatusConflict:
		// Someone created it between the read and the write — or it existed but was
		// missing the key. Read it back rather than assume the generated value.
		return k.secretValue(ns, name, key)
	}
	return "", fmt.Errorf("create secret %s/%s -> %d: %s", ns, name, code, truncate(b))
}
