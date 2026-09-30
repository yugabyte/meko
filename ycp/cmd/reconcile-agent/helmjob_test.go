package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"meko/ycp/reconcile/ampbackend"
)

// fakeJobAPI is the slice of the Kubernetes API jobUpgrader uses, in memory:
// this agent's own pod, Secrets, Jobs and their pods.
type fakeJobAPI struct {
	mu      sync.Mutex
	ownPod  string
	secrets map[string]map[string]any
	jobs    map[string]map[string]any
	// jobPods holds each Job's pod status, as the Job controller would report it.
	jobPods  map[string]map[string]any
	deleted  []string
	patched  map[string][]byte
	creates  int
	lastPath string
}

func newFakeJobAPI(t *testing.T) (*fakeJobAPI, *k8s) {
	t.Helper()
	f := &fakeJobAPI{
		ownPod: `{"spec":{"serviceAccountName":"amp-reconcile-agent",
			"imagePullSecrets":[{"name":"quay-pull-secret"}],
			"nodeSelector":{"pool":"system"},
			"containers":[{"name":"sidecar","image":"tailscale:1"},
			              {"name":"reconcile-agent","image":"quay.io/yugabyte/ycp-reconciler-agent:v1","imagePullPolicy":"IfNotPresent",
			               "env":[{"name":"HTTPS_PROXY","value":"http://localhost:1055"}]}]}}`,
		secrets: map[string]map[string]any{}, jobs: map[string]map[string]any{},
		jobPods: map[string]map[string]any{}, patched: map[string][]byte{},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, &k8s{host: srv.URL, tok: "sa-token", ns: "amp", http: srv.Client()}
}

func (f *fakeJobAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	p := r.URL.Path
	f.lastPath = p
	name := p[strings.LastIndex(p, "/")+1:]
	switch {
	case r.Method == http.MethodGet && p == "/api/v1/namespaces/amp/pods/agent-pod-0":
		io.WriteString(w, f.ownPod)
	case r.Method == http.MethodPost && p == "/api/v1/namespaces/amp/secrets":
		f.post(w, body, f.secrets)
	case r.Method == http.MethodPost && p == "/apis/batch/v1/namespaces/amp/jobs":
		f.post(w, body, f.jobs)
	case r.Method == http.MethodGet && p == "/apis/batch/v1/namespaces/amp/jobs":
		want := r.URL.Query().Get("labelSelector")
		var items []map[string]any
		for _, j := range f.jobs {
			labels, _ := j["metadata"].(map[string]any)["labels"].(map[string]any)
			if want == upgradeRunLabel+"="+labels[upgradeRunLabel].(string) {
				items = append(items, j)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/apis/batch/v1/namespaces/amp/jobs/"):
		j, ok := f.jobs[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(j)
	case r.Method == http.MethodPatch && strings.HasPrefix(p, "/api/v1/namespaces/amp/secrets/"):
		f.patched[name] = body
	case r.Method == http.MethodGet && p == "/api/v1/namespaces/amp/pods":
		job := strings.TrimPrefix(r.URL.Query().Get("labelSelector"), "job-name=")
		var items []map[string]any
		if pod, ok := f.jobPods[job]; ok {
			items = append(items, pod)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.Method == http.MethodDelete:
		f.deleted = append(f.deleted, p)
		delete(f.jobs, name)
		delete(f.secrets, name)
	default:
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"unexpected `+r.Method+` `+p+`"}`)
	}
}

func (f *fakeJobAPI) post(w http.ResponseWriter, body []byte, into map[string]map[string]any) {
	var obj map[string]any
	_ = json.Unmarshal(body, &obj)
	meta := obj["metadata"].(map[string]any)
	name := meta["name"].(string)
	if _, exists := into[name]; exists {
		w.WriteHeader(http.StatusConflict)
		return
	}
	f.creates++
	meta["uid"] = "uid-" + name
	meta["creationTimestamp"] = "2026-09-29T20:00:0" + string(rune('0'+len(into))) + "Z"
	into[name] = obj
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(obj)
}

func (f *fakeJobAPI) setCondition(job, typ, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[job]["status"] = map[string]any{"conditions": []map[string]any{{"type": typ, "status": "True", "message": msg}}}
}

var upgradeArgs = []string{"upgrade", "yb-amp-controller", "https://charts/yb-amp-controller-1.tgz", "--namespace", "amp"}

// The Job runs helm from this agent's image, as its ServiceAccount, placed like it
// — and carries none of the values, which are in a Secret it owns.
func TestJobUpgraderStartsAJobThatOutlivesTheAgent(t *testing.T) {
	f, kc := newFakeJobAPI(t)
	up := &jobUpgrader{kc: kc, release: "yb-amp-controller", pod: "agent-pod-0"}
	values := []byte(`{"controller":{"image":{"tag":"v2"}},"secret":"hunter2"}`)

	if err := up.Start(t.Context(), "yb-amp-controller-upgrade-abc", upgradeArgs, values); err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := f.jobs["yb-amp-controller-upgrade-abc"]
	raw, _ := json.Marshal(job)
	if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), `"v2"`) {
		t.Errorf("the Job spec carries the values: %s", raw)
	}
	var spec struct {
		Spec struct {
			BackoffLimit *int `json:"backoffLimit"`
			Template     struct {
				Spec struct {
					ServiceAccountName string                  `json:"serviceAccountName"`
					RestartPolicy      string                  `json:"restartPolicy"`
					ImagePullSecrets   []struct{ Name string } `json:"imagePullSecrets"`
					NodeSelector       map[string]string       `json:"nodeSelector"`
					Containers         []struct {
						Image   string            `json:"image"`
						Command []string          `json:"command"`
						Args    []string          `json:"args"`
						Env     []json.RawMessage `json:"env"`
						Policy  string            `json:"terminationMessagePolicy"`
					} `json:"containers"`
					Volumes []struct {
						Secret struct {
							SecretName string `json:"secretName"`
						} `json:"secret"`
					} `json:"volumes"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	pod := spec.Spec.Template.Spec
	c := pod.Containers[0]
	if c.Image != "quay.io/yugabyte/ycp-reconciler-agent:v1" || strings.Join(c.Command, " ") != defaultHelmBin {
		t.Errorf("container = %s %v, want the agent's image running helm", c.Image, c.Command)
	}
	if got, want := strings.Join(c.Args, " "), strings.Join(upgradeArgs, " ")+" -f /values/values.yaml --timeout "+helmUpgradeTimeout; got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
	if len(c.Env) != 0 {
		t.Errorf("the Job copies the agent's env (%d vars); its proxy points at a sidecar the Job does not have", len(c.Env))
	}
	if c.Policy != "FallbackToLogsOnError" {
		t.Errorf("terminationMessagePolicy = %q, want helm's stderr kept", c.Policy)
	}
	if pod.ServiceAccountName != "amp-reconcile-agent" || pod.RestartPolicy != "Never" ||
		len(pod.ImagePullSecrets) != 1 || pod.NodeSelector["pool"] != "system" {
		t.Errorf("pod spec = %+v, want the agent's identity and placement", pod)
	}
	if spec.Spec.BackoffLimit == nil || *spec.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want 0", spec.Spec.BackoffLimit)
	}
	if len(pod.Volumes) != 1 || pod.Volumes[0].Secret.SecretName != "yb-amp-controller-upgrade-abc" {
		t.Errorf("volumes = %+v, want the values Secret", pod.Volumes)
	}

	sec := f.secrets["yb-amp-controller-upgrade-abc"]
	if sec["stringData"].(map[string]any)[upgradeValuesKey] != string(values) {
		t.Errorf("secret values = %v, want the document", sec["stringData"])
	}
	if !strings.Contains(string(f.patched["yb-amp-controller-upgrade-abc"]), `"uid":"uid-yb-amp-controller-upgrade-abc"`) {
		t.Errorf("secret owner patch = %s, want the Job as owner", f.patched["yb-amp-controller-upgrade-abc"])
	}

	// A retry after a crash is safe: both objects exist, nothing new is made.
	before := f.creates
	if err := up.Start(t.Context(), "yb-amp-controller-upgrade-abc", upgradeArgs, values); err != nil {
		t.Fatalf("Start again: %v", err)
	}
	if f.creates != before {
		t.Errorf("a second Start created %d objects, want none", f.creates-before)
	}
}

func TestJobUpgraderReportsTheLatestRun(t *testing.T) {
	f, kc := newFakeJobAPI(t)
	up := &jobUpgrader{kc: kc, release: "yb-amp-controller", pod: "agent-pod-0"}
	ctx := t.Context()

	if run, err := up.Latest(ctx); err != nil || run != (ampbackend.UpgradeRun{}) {
		t.Fatalf("Latest with no runs = %+v, %v, want the zero run", run, err)
	}
	if err := up.Start(ctx, "yb-amp-controller-upgrade-old", upgradeArgs, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	f.setCondition("yb-amp-controller-upgrade-old", "Complete", "")
	if err := up.Start(ctx, "yb-amp-controller-upgrade-new", upgradeArgs, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if run, _ := up.Latest(ctx); run.Name != "yb-amp-controller-upgrade-new" || run.State != ampbackend.UpgradeRunning {
		t.Errorf("Latest = %+v, want the newer run, running", run)
	}

	// A failed run carries helm's own words, from the pod's termination message.
	f.setCondition("yb-amp-controller-upgrade-new", "Failed", "BackoffLimitExceeded")
	f.mu.Lock()
	f.jobPods["yb-amp-controller-upgrade-new"] = map[string]any{"status": map[string]any{"containerStatuses": []any{
		map[string]any{"state": map[string]any{"terminated": map[string]any{"message": "Error: UPGRADE FAILED: forbidden\n"}}}}}}
	f.mu.Unlock()
	run, err := up.Latest(ctx)
	if err != nil || run.State != ampbackend.UpgradeFailed || run.Message != "Error: UPGRADE FAILED: forbidden" {
		t.Errorf("Latest = %+v, %v, want failed with helm's error", run, err)
	}

	if err := up.Remove(ctx, run.Name); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := f.jobs[run.Name]; ok {
		t.Error("the Job is still there after Remove")
	}
	if _, ok := f.secrets[run.Name]; ok {
		t.Error("the values Secret is still there after Remove")
	}
}

// V1 -> V2 -> V1 -> V2 sends the same inputs the second time as the first, and so
// the same name. A finished run of it is the first V2 upgrade, which the rollback
// since undid: taking it as done left the second upgrade never run. It is replaced.
func TestJobUpgraderReplacesAFinishedRunOfTheSameName(t *testing.T) {
	f, kc := newFakeJobAPI(t)
	up := &jobUpgrader{kc: kc, release: "yb-amp-controller", pod: "agent-pod-0"}
	ctx := t.Context()
	const name = "yb-amp-controller-upgrade-abc"

	if err := up.Start(ctx, name, upgradeArgs, []byte(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	f.setCondition(name, "Complete", "")
	before := f.creates
	if err := up.Start(ctx, name, upgradeArgs, []byte(`{"v":2}`)); err != nil {
		t.Fatalf("Start over a finished run: %v", err)
	}
	if f.creates != before+2 {
		t.Errorf("created %d objects, want a new Secret and Job", f.creates-before)
	}
	if run, _ := up.Latest(ctx); run.State != ampbackend.UpgradeRunning {
		t.Errorf("Latest after the restart = %+v, want the new run, running", run)
	}
}

// The Job pulls the chart the way the agent does: the registry login (the pull
// secret mounted as HELM_REGISTRY_CONFIG), an egress proxy, the node affinity. It
// does not get the agent's YCP credentials, its ServiceAccount token mount (the
// Job's pod has its own), or a proxy on loopback, which is a sidecar the Job lacks.
func TestJobUpgraderCarriesWhatHelmNeedsToPullTheChart(t *testing.T) {
	self, err := parseOwnPod([]byte(`{"spec":{
		"serviceAccountName":"amp-reconcile-agent",
		"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{}}},
		"volumes":[
			{"name":"chart-pull-secret","secret":{"secretName":"quay-pull-secret"}},
			{"name":"kube-api-access-x","projected":{"sources":[]}},
			{"name":"unused","emptyDir":{}}],
		"containers":[{"name":"reconcile-agent","image":"agent:v1",
			"env":[
				{"name":"HELM_REGISTRY_CONFIG","value":"/etc/helm-registry/config.json"},
				{"name":"HTTPS_PROXY","value":"http://proxy.corp:3128"},
				{"name":"http_proxy","value":"http://127.0.0.1:1055"},
				{"name":"NO_PROXY","value":".svc"},
				{"name":"YCP_API_KEY","valueFrom":{"secretKeyRef":{"name":"amp-agent-api-key","key":"api-key"}}}],
			"volumeMounts":[
				{"name":"chart-pull-secret","mountPath":"/etc/helm-registry","readOnly":true},
				{"name":"kube-api-access-x","mountPath":"/var/run/secrets/kubernetes.io/serviceaccount"}]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	up := &jobUpgrader{release: "yb-amp-controller"}
	raw, _ := json.Marshal(up.jobSpec("yb-amp-controller-upgrade-abc", upgradeArgs, self))
	var job struct {
		Spec struct {
			Template struct {
				Spec struct {
					Affinity   any `json:"affinity"`
					Containers []struct {
						Env []struct {
							Name  string `json:"name"`
							Value string `json:"value"`
						} `json:"env"`
						VolumeMounts []struct {
							Name      string `json:"name"`
							MountPath string `json:"mountPath"`
						} `json:"volumeMounts"`
					} `json:"containers"`
					Volumes []struct {
						Name string `json:"name"`
					} `json:"volumes"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template.Spec
	var env, mounts, vols []string
	for _, e := range pod.Containers[0].Env {
		env = append(env, e.Name+"="+e.Value)
	}
	for _, m := range pod.Containers[0].VolumeMounts {
		mounts = append(mounts, m.Name+":"+m.MountPath)
	}
	for _, v := range pod.Volumes {
		vols = append(vols, v.Name)
	}
	if got, want := strings.Join(env, " "), "HELM_REGISTRY_CONFIG=/etc/helm-registry/config.json HTTPS_PROXY=http://proxy.corp:3128 NO_PROXY=.svc"; got != want {
		t.Errorf("env = %q, want %q", got, want)
	}
	if got, want := strings.Join(mounts, " "), "chart-pull-secret:/etc/helm-registry helm-upgrade-values:/values"; got != want {
		t.Errorf("mounts = %q, want %q", got, want)
	}
	if got, want := strings.Join(vols, " "), "chart-pull-secret helm-upgrade-values"; got != want {
		t.Errorf("volumes = %q, want %q", got, want)
	}
	if pod.Affinity == nil {
		t.Error("affinity was not carried")
	}
}

// Without a pod name there is no template to copy, and no Job is made from a guess.
func TestJobUpgraderNeedsItsOwnPod(t *testing.T) {
	f, kc := newFakeJobAPI(t)
	up := &jobUpgrader{kc: kc, release: "yb-amp-controller", pod: ""}
	if err := up.Start(t.Context(), "x", upgradeArgs, []byte(`{}`)); err == nil {
		t.Fatal("Start with no pod name = nil, want an error")
	}
	if f.creates != 0 {
		t.Errorf("created %d objects without a template", f.creates)
	}
}
