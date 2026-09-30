package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"meko/ycp/reconcile/ampbackend"
)

// jobUpgrader runs the controller release's `helm upgrade` as a Kubernetes Job
// (ampbackend.Upgrader). The Job is not a member of the release, so the upgrade
// replacing this agent's pod does not kill the helm client: helm sees the
// release's hooks through and records the revision, and the Job's status is the
// record the replacement pod reads.
//
// The pod is this agent's own, minus its process: the same image (which carries
// helm and sets its cache dirs), ServiceAccount, pull secrets, placement and
// volume mounts — the chart registry login among them — and the env helm reads
// (see helmEnv). The values go in a Secret named like the Job and owned by it, so
// they leave no trace in the Job spec and expire with it.
type jobUpgrader struct {
	kc      *k8s
	release string
	// pod names this agent's own pod, whose spec the Job copies.
	pod string
}

const (
	// upgradeRunLabel carries the release a Job upgrades; Latest selects on it.
	upgradeRunLabel = "amp.yugabyte.com/helm-upgrade-of"
	// helmUpgradeTimeout bounds helm's wait on the release's hooks. The telemetry
	// preflight allows itself five minutes and more.
	helmUpgradeTimeout = "15m"
	// The Job's own deadline is past helm's, so helm times out and says why before
	// the Job controller kills it silently.
	upgradeJobDeadlineSeconds = 20 * 60
	upgradeJobTTLSeconds      = 60 * 60
	upgradeValuesKey          = "values.yaml"
	upgradeValuesMount        = "/values"
	// upgradeValuesVolume is named so it cannot collide with a volume copied from
	// the agent's pod.
	upgradeValuesVolume = "helm-upgrade-values"
)

func newJobUpgrader(kc *k8s, release string) *jobUpgrader {
	pod, _ := os.Hostname()
	return &jobUpgrader{kc: kc, release: release, pod: env("POD_NAME", pod)}
}

func (j *jobUpgrader) jobsPath() string {
	return fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs", j.kc.ns)
}

func (j *jobUpgrader) Start(ctx context.Context, name string, args []string, values []byte) error {
	self, err := j.ownPod(ctx)
	if err != nil {
		return err
	}
	// A run of this name that already finished ran these exact inputs and did not
	// stick: Start is only called when the release does not carry them, and moving
	// between two versions and back revisits the same values. It is removed and run
	// again rather than taken as done. One still running is the upgrade itself.
	prior, found, err := j.jobStatus(ctx, name)
	if err != nil {
		return err
	}
	if found && prior.state() == ampbackend.UpgradeRunning {
		return nil
	}
	if found {
		if err := j.Remove(ctx, name); err != nil {
			return fmt.Errorf("remove the finished run %s: %w", name, err)
		}
	}
	// Secret first: the Job's pod cannot start without it, and one left behind by a
	// crash before the Job existed is the same bytes the retry writes (the name is
	// a hash of them).
	secret := map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata":   map[string]any{"name": name, "labels": map[string]string{upgradeRunLabel: j.release}},
		"type":       "Opaque",
		"stringData": map[string]string{upgradeValuesKey: string(values)},
	}
	if _, err := j.create(ctx, j.kc.secretPath(""), secret); err != nil {
		return fmt.Errorf("create values secret %s: %w", name, err)
	}
	existed, err := j.create(ctx, j.jobsPath(), j.jobSpec(name, args, self))
	if err != nil {
		return fmt.Errorf("create job %s: %w", name, err)
	}
	if existed && found {
		// The removed run is still being deleted; taking it as this run would be the
		// very mistake the removal is for.
		return fmt.Errorf("the finished run %s is still being deleted; the upgrade starts on a later tick", name)
	}
	return j.ownSecret(ctx, name)
}

// jobStatus reads one run by name.
func (j *jobUpgrader) jobStatus(ctx context.Context, name string) (jobStatus, bool, error) {
	code, raw, err := j.kc.doCtx(ctx, http.MethodGet, j.jobsPath()+"/"+name, "", nil)
	if err != nil {
		return jobStatus{}, false, err
	}
	switch code {
	case http.StatusOK:
	case http.StatusNotFound:
		return jobStatus{}, false, nil
	default:
		return jobStatus{}, false, fmt.Errorf("read job %s: HTTP %d: %s", name, code, strings.TrimSpace(string(raw)))
	}
	var js jobStatus
	if err := json.Unmarshal(raw, &js); err != nil {
		return jobStatus{}, false, fmt.Errorf("decode job %s: %w", name, err)
	}
	return js, true, nil
}

// jobSpec is the Job that runs `helm <args>` against the mounted values.
func (j *jobUpgrader) jobSpec(name string, args []string, self podTemplate) map[string]any {
	argv := append(append([]string{}, args...),
		"-f", upgradeValuesMount+"/"+upgradeValuesKey, "--timeout", helmUpgradeTimeout)
	container := map[string]any{
		"name":    "helm",
		"image":   self.Image,
		"command": []string{defaultHelmBin},
		"args":    argv,
		// helm's stderr becomes the termination message on failure, which is how
		// Latest reports helm's own words without reading logs.
		"terminationMessagePolicy": "FallbackToLogsOnError",
		"volumeMounts": append(append([]json.RawMessage{}, self.VolumeMounts...),
			mustRaw(map[string]any{"name": upgradeValuesVolume, "mountPath": upgradeValuesMount, "readOnly": true})),
	}
	if len(self.Env) > 0 {
		container["env"] = self.Env
	}
	if self.ImagePullPolicy != "" {
		container["imagePullPolicy"] = self.ImagePullPolicy
	}
	if self.ContainerSecurityContext != nil {
		container["securityContext"] = self.ContainerSecurityContext
	}
	pod := map[string]any{
		"restartPolicy":      "Never",
		"serviceAccountName": self.ServiceAccountName,
		"containers":         []any{container},
		"volumes": append(append([]json.RawMessage{}, self.Volumes...),
			mustRaw(map[string]any{"name": upgradeValuesVolume, "secret": map[string]any{"secretName": name}})),
	}
	for k, v := range map[string]any{
		"imagePullSecrets": self.ImagePullSecrets, "nodeSelector": self.NodeSelector,
		"tolerations": self.Tolerations, "securityContext": self.SecurityContext,
		"affinity": self.Affinity,
	} {
		if v != nil {
			pod[k] = v
		}
	}
	labels := map[string]string{upgradeRunLabel: j.release}
	return map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "labels": labels},
		"spec": map[string]any{
			// Never retried by the Job controller: a second helm against a release the
			// first left pending-upgrade is refused anyway, and the agent decides.
			"backoffLimit":            0,
			"activeDeadlineSeconds":   upgradeJobDeadlineSeconds,
			"ttlSecondsAfterFinished": upgradeJobTTLSeconds,
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec":     pod,
			},
		},
	}
}

// ownSecret makes the Job the owner of its values Secret, so the Job's TTL — or
// Remove — collects both.
func (j *jobUpgrader) ownSecret(ctx context.Context, name string) error {
	code, raw, err := j.kc.doCtx(ctx, http.MethodGet, j.jobsPath()+"/"+name, "", nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("read job %s: HTTP %d: %s", name, code, strings.TrimSpace(string(raw)))
	}
	var job struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		return fmt.Errorf("decode job %s: %w", name, err)
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"ownerReferences": []map[string]any{{
		"apiVersion": "batch/v1", "kind": "Job", "name": name, "uid": job.Metadata.UID,
	}}}})
	code, raw, err = j.kc.doCtx(ctx, http.MethodPatch, j.kc.secretPath(name), "application/merge-patch+json", patch)
	if err != nil {
		return err
	}
	if code >= 300 {
		return fmt.Errorf("set the owner of secret %s: HTTP %d: %s", name, code, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (j *jobUpgrader) Latest(ctx context.Context) (ampbackend.UpgradeRun, error) {
	q := url.Values{"labelSelector": {upgradeRunLabel + "=" + j.release}}
	code, raw, err := j.kc.doCtx(ctx, http.MethodGet, j.jobsPath()+"?"+q.Encode(), "", nil)
	if err != nil {
		return ampbackend.UpgradeRun{}, err
	}
	if code != http.StatusOK {
		return ampbackend.UpgradeRun{}, fmt.Errorf("list upgrade jobs: HTTP %d: %s", code, strings.TrimSpace(string(raw)))
	}
	var list struct {
		Items []jobStatus `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return ampbackend.UpgradeRun{}, fmt.Errorf("decode upgrade jobs: %w", err)
	}
	if len(list.Items) == 0 {
		return ampbackend.UpgradeRun{}, nil
	}
	sort.Slice(list.Items, func(a, b int) bool {
		return list.Items[a].Metadata.CreationTimestamp > list.Items[b].Metadata.CreationTimestamp
	})
	newest := list.Items[0]
	run := ampbackend.UpgradeRun{Name: newest.Metadata.Name, State: newest.state()}
	if run.State == ampbackend.UpgradeFailed {
		run.Message = j.failure(ctx, newest)
	}
	return run, nil
}

// jobStatus is the part of a Job Latest reads. creationTimestamp is RFC 3339 in
// UTC, so it orders as a string.
type jobStatus struct {
	Metadata struct {
		Name              string `json:"name"`
		CreationTimestamp string `json:"creationTimestamp"`
	} `json:"metadata"`
	Status struct {
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

func (s jobStatus) state() string {
	for _, c := range s.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case "Complete":
			return ampbackend.UpgradeSucceeded
		case "Failed":
			return ampbackend.UpgradeFailed
		}
	}
	return ampbackend.UpgradeRunning
}

// failure is helm's own error from the Job's pod, falling back to the Job's
// condition (a deadline, a pod that never started).
func (j *jobUpgrader) failure(ctx context.Context, job jobStatus) string {
	q := url.Values{"labelSelector": {"job-name=" + job.Metadata.Name}}
	code, raw, err := j.kc.doCtx(ctx, http.MethodGet, fmt.Sprintf("/api/v1/namespaces/%s/pods?%s", j.kc.ns, q.Encode()), "", nil)
	if err == nil && code == http.StatusOK {
		var pods struct {
			Items []struct {
				Status struct {
					ContainerStatuses []struct {
						State struct {
							Terminated *struct {
								Message string `json:"message"`
							} `json:"terminated"`
						} `json:"state"`
					} `json:"containerStatuses"`
				} `json:"status"`
			} `json:"items"`
		}
		if json.Unmarshal(raw, &pods) == nil {
			for _, p := range pods.Items {
				for _, cs := range p.Status.ContainerStatuses {
					if t := cs.State.Terminated; t != nil && strings.TrimSpace(t.Message) != "" {
						return strings.TrimSpace(t.Message)
					}
				}
			}
		}
	}
	for _, c := range job.Status.Conditions {
		if c.Type == "Failed" && c.Message != "" {
			return c.Message
		}
	}
	return "the upgrade job failed without a message"
}

func (j *jobUpgrader) Remove(ctx context.Context, name string) error {
	body, _ := json.Marshal(map[string]string{"propagationPolicy": "Background"})
	code, raw, err := j.kc.doCtx(ctx, http.MethodDelete, j.jobsPath()+"/"+name, "application/json", body)
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusNotFound {
		return fmt.Errorf("delete job %s: HTTP %d: %s", name, code, strings.TrimSpace(string(raw)))
	}
	// Owned by the Job, so collected with it; deleted here too for a Secret whose
	// owner was never set.
	code, raw, err = j.kc.doCtx(ctx, http.MethodDelete, j.kc.secretPath(name), "", nil)
	if err != nil {
		return err
	}
	if code >= 300 && code != http.StatusNotFound {
		return fmt.Errorf("delete secret %s: HTTP %d: %s", name, code, strings.TrimSpace(string(raw)))
	}
	return nil
}

// create POSTs an object. "Already exists" is not an error; existed says so.
func (j *jobUpgrader) create(ctx context.Context, path string, obj any) (existed bool, err error) {
	body, err := json.Marshal(obj)
	if err != nil {
		return false, err
	}
	code, raw, err := j.kc.doCtx(ctx, http.MethodPost, strings.TrimSuffix(path, "/"), "application/json", body)
	if err != nil {
		return false, err
	}
	if code == http.StatusConflict {
		return true, nil
	}
	if code >= 300 {
		return false, fmt.Errorf("HTTP %d: %s", code, strings.TrimSpace(string(raw)))
	}
	return false, nil
}

// podTemplate is what the Job copies from this agent's pod.
type podTemplate struct {
	Image                    string
	ImagePullPolicy          string
	ServiceAccountName       string
	ImagePullSecrets         any
	NodeSelector             any
	Tolerations              any
	Affinity                 any
	SecurityContext          any
	ContainerSecurityContext any
	// Env is helmEnv's selection from the agent container's env; VolumeMounts are
	// its mounts bar the ServiceAccount token, and Volumes the pod volumes they use.
	Env          []json.RawMessage
	VolumeMounts []json.RawMessage
	Volumes      []json.RawMessage
}

func (j *jobUpgrader) ownPod(ctx context.Context) (podTemplate, error) {
	if j.pod == "" {
		return podTemplate{}, fmt.Errorf("this agent does not know its own pod name")
	}
	code, raw, err := j.kc.doCtx(ctx, http.MethodGet, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", j.kc.ns, j.pod), "", nil)
	if err != nil {
		return podTemplate{}, err
	}
	if code != http.StatusOK {
		return podTemplate{}, fmt.Errorf("read own pod %s: HTTP %d: %s", j.pod, code, strings.TrimSpace(string(raw)))
	}
	return parseOwnPod(raw)
}

// agentContainer is the container in the agent's pod that runs this binary.
const agentContainer = "reconcile-agent"

func parseOwnPod(raw []byte) (podTemplate, error) {
	var p struct {
		Spec struct {
			ServiceAccountName string            `json:"serviceAccountName"`
			ImagePullSecrets   any               `json:"imagePullSecrets"`
			NodeSelector       any               `json:"nodeSelector"`
			Tolerations        any               `json:"tolerations"`
			Affinity           any               `json:"affinity"`
			SecurityContext    any               `json:"securityContext"`
			Volumes            []json.RawMessage `json:"volumes"`
			Containers         []struct {
				Name            string            `json:"name"`
				Image           string            `json:"image"`
				ImagePullPolicy string            `json:"imagePullPolicy"`
				SecurityContext any               `json:"securityContext"`
				Env             []json.RawMessage `json:"env"`
				VolumeMounts    []json.RawMessage `json:"volumeMounts"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return podTemplate{}, fmt.Errorf("decode own pod: %w", err)
	}
	if len(p.Spec.Containers) == 0 {
		return podTemplate{}, fmt.Errorf("own pod has no containers")
	}
	c := p.Spec.Containers[0]
	for _, cc := range p.Spec.Containers {
		if cc.Name == agentContainer {
			c = cc
		}
	}
	t := podTemplate{
		Image: c.Image, ImagePullPolicy: c.ImagePullPolicy, ContainerSecurityContext: c.SecurityContext,
		ServiceAccountName: p.Spec.ServiceAccountName, ImagePullSecrets: p.Spec.ImagePullSecrets,
		NodeSelector: p.Spec.NodeSelector, Tolerations: p.Spec.Tolerations, Affinity: p.Spec.Affinity,
		SecurityContext: p.Spec.SecurityContext, Env: helmEnv(c.Env),
	}
	// Every mount but the ServiceAccount token, which the Job's pod is given its own
	// of: the chart registry login lives in one (HELM_REGISTRY_CONFIG), and a
	// local-directory CONTROLLER_CHART in another.
	used := map[string]bool{}
	for _, m := range c.VolumeMounts {
		var mount struct {
			Name      string `json:"name"`
			MountPath string `json:"mountPath"`
		}
		if json.Unmarshal(m, &mount) != nil || strings.HasPrefix(mount.MountPath, serviceAccountDir) {
			continue
		}
		used[mount.Name] = true
		t.VolumeMounts = append(t.VolumeMounts, m)
	}
	for _, v := range p.Spec.Volumes {
		var vol struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(v, &vol) == nil && used[vol.Name] {
			t.Volumes = append(t.Volumes, v)
		}
	}
	return t, nil
}

// serviceAccountDir is where Kubernetes mounts a pod's ServiceAccount token.
const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// helmEnv is the part of the agent container's env helm itself reads: HELM_*
// (the registry login, the cache dirs), custom CA bundles, and the egress proxy
// the install may need to reach the chart registry (agent.extraEnv). Not a proxy
// on loopback: that is a sidecar in the agent's pod — the tailnet route to YCP —
// which the Job does not have, and pointing helm at it would cut the chart pull
// off entirely. Nothing else: the agent's own YCP credentials have no business in
// a helm pod.
func helmEnv(env []json.RawMessage) []json.RawMessage {
	var out []json.RawMessage
	for _, e := range env {
		var v struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if json.Unmarshal(e, &v) != nil {
			continue
		}
		upper := strings.ToUpper(v.Name)
		switch {
		case strings.HasPrefix(upper, "HELM_"), upper == "SSL_CERT_FILE", upper == "SSL_CERT_DIR", upper == "NO_PROXY":
		case upper == "HTTP_PROXY" || upper == "HTTPS_PROXY" || upper == "ALL_PROXY":
			if loopbackURL(v.Value) {
				continue
			}
		default:
			continue
		}
		out = append(out, e)
	}
	return out
}

// loopbackURL reports whether a proxy URL points at this pod itself.
func loopbackURL(raw string) bool {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
