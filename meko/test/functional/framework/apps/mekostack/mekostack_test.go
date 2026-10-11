package mekostack

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/cognitomock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/minio"
	execpkg "github.com/yugabyte/meko/meko/test/functional/framework/exec"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
)

type call struct {
	argv  []string
	stdin string
}

func (c call) line() string { return strings.Join(c.argv, " ") }

// cluster is a scripted kubectl and helm. handle sees every command.
type cluster struct {
	mu     sync.Mutex
	calls  []call
	handle func(line string, c call) (execpkg.Result, bool)
}

func (f *cluster) run(_ context.Context, stdin io.Reader, argv []string) (execpkg.Result, error) {
	var in string
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	c := call{argv: argv, stdin: in}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if f.handle != nil {
		if res, ok := f.handle(c.line(), c); ok {
			return res, nil
		}
	}
	return execpkg.Result{}, nil
}

func (f *cluster) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.line()
	}
	return out
}

func (f *cluster) count(sub string) int {
	n := 0
	for _, l := range f.lines() {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func (f *cluster) first(sub string) int {
	for i, l := range f.lines() {
		if strings.Contains(l, sub) {
			return i
		}
	}
	return -1
}

func out(s string) (execpkg.Result, bool) { return execpkg.Result{Stdout: s}, true }

func notFound(what string) (execpkg.Result, bool) {
	return execpkg.Result{ExitCode: 1, Stderr: fmt.Sprintf("Error from server (NotFound): %s not found", what)}, true
}

func testConfig(t *testing.T) Config {
	t.Helper()
	chart := t.TempDir()
	if err := os.WriteFile(filepath.Join(chart, "Chart.yaml"), []byte("name: meko\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return Config{
		Context: "ctx", Namespace: "mekoqa-st-abc", RunID: "abc", Owner: "me", TTL: time.Hour,
		ChartPath: chart, Release: "meko",
		ValuesFiles:   []string{"values-gcp.yaml", "values-dev.yaml", "values-byoc.yaml"},
		ImageVersion:  "0.261002.0-b75b2a658",
		ImageTags:     map[string]string{"mcp_server": "custom-abc123"},
		DatabaseImage: "quay.io/yugabyte/meko-yugabytedb:2026.1.0.0-b1-meko-2",
		APIReplicas:   3, RagWorkers: true, LLM: LLMMock, OpenAIKey: "sk-unused",
		QuayUser: "yugabyte+robot", QuayToken: "quay-token-value",
		S3Bucket: "mekoqa", S3Region: "us-east-1",
		ProvisionTimeout: 20 * time.Minute, ReadyTimeout: time.Second, RolloutTimeout: time.Minute,
		Reach: ReachTailnet, ValuesDir: t.TempDir(),
	}
}

func TestComponentTagResolution(t *testing.T) {
	cfg := testConfig(t)
	if got := cfg.Tag("api_server"); got != "0.261002.0-b75b2a658" {
		t.Errorf("api_server tag = %q, want the shared version", got)
	}
	if got := cfg.Tag("mcp_server"); got != "custom-abc123" {
		t.Errorf("mcp_server tag = %q, want its own", got)
	}
}

func TestValidateRefusesNonQuayAndBadInput(t *testing.T) {
	cases := map[string]func(*Config){
		"a database image from ECR": func(c *Config) {
			c.DatabaseImage = "161908593886.dkr.ecr.us-east-2.amazonaws.com/yugabyte-db/meko-yugabytedb:1"
		},
		"a database image from a fork": func(c *Config) { c.DatabaseImage = "quay.io/someone/meko-yugabytedb:1" },
		"a database image with a digest": func(c *Config) {
			c.DatabaseImage = "quay.io/yugabyte/meko-yugabytedb@sha256:abc"
		},
		"a tag carrying a path":  func(c *Config) { c.ImageTags["ui"] = "x/../../y" },
		"no version at all":      func(c *Config) { c.ImageVersion = "" },
		"no quay credential":     func(c *Config) { c.QuayToken = "" },
		"no api replicas":        func(c *Config) { c.APIReplicas = 0 },
		"negative mcp replicas":  func(c *Config) { c.MCPReplicas = -1 },
		"too many partitions":    func(c *Config) { c.HashPartitions = 1001 },
		"an unknown reach":       func(c *Config) { c.Reach = "carrier-pigeon" },
		"a chart that is absent": func(c *Config) { c.ChartPath = filepath.Join(c.ChartPath, "nope") },
	}
	for name, mutate := range cases {
		cfg := testConfig(t)
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: Validate passed", name)
		}
	}
	if err := testConfig(t).Validate(); err != nil {
		t.Errorf("a good config: %v", err)
	}
}

func TestCheckQuayOnly(t *testing.T) {
	ok := []string{
		"quay.io/yugabyte/meko-api-server:1",
		"quay.io/yugabyte/meko-yugabytedb:2026.1.0.0-b1-meko-2",
		"docker.io/valkey/valkey:9.0.1",
		"nginx:1.27-alpine",
		"cgr.dev/chainguard/minio:latest",
		"quay.io/yugabyte/ycp-reconciler-agent:1",
	}
	if err := CheckQuayOnly(ok); err != nil {
		t.Errorf("CheckQuayOnly(ok) = %v", err)
	}
	bad := []string{
		"161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/rag-agent:1.0.4",
		"quay.io/personal/meko-api-server:1",
		"docker.io/library/meko-ui:1",
		"quay.io/yugabyte-itest/meko-mcp-server:1",
	}
	for _, ref := range bad {
		if err := CheckQuayOnly([]string{ref}); err == nil {
			t.Errorf("CheckQuayOnly(%q) passed", ref)
		}
	}
}

func TestIsMekoImage(t *testing.T) {
	for ref, want := range map[string]bool{
		"quay.io/yugabyte/meko-ui:1":       true,
		"meko/rag-agent:1":                 true,
		"registry:5000/meko/api-server:1":  true,
		"quay.io/yugabyte/yugabyte:1":      false,
		"docker.io/valkey/valkey:9":        false,
		"quay.io/yugabyte/mekotron-core:1": false,
	} {
		if got := IsMekoImage(ref); got != want {
			t.Errorf("IsMekoImage(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestValuesCarryTheStackSettings(t *testing.T) {
	cfg := testConfig(t)
	dp, err := NewDataPlaneSecrets(bytes.NewReader(bytes.Repeat([]byte{0xab}, 512)))
	if err != nil {
		t.Fatal(err)
	}
	m := MinIO{AccessKey: "ak", SecretKey: "sk", APIEndpoint: "http://minio.ns.svc.cluster.local:9000", WorkerEndpoint: "http://10.1.2.3:9000"}
	v, err := cfg.Values(dp, m)
	if err != nil {
		t.Fatal(err)
	}
	b, err := MarshalValues(v)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := yaml.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	get := func(path ...string) any {
		var cur any = back
		for _, p := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[p]
		}
		return cur
	}
	checks := map[string]any{
		"apiServer.staticArgs.s3AuthMode":                "minio",
		"apiServer.config.S3_BUCKET":                     "mekoqa",
		"apiServer.replicas":                             3,
		"apiServer.image.tag":                            "0.261002.0-b75b2a658",
		"mekoMcpServer.image.tag":                        "custom-abc123",
		"dataPlaneSecrets.s3AccessKey":                   "ak",
		"dataPlaneSecrets.dbPassword":                    dp.DBPassword,
		"ragAgent.config.AWS_ENDPOINT_URL_S3":            "http://10.1.2.3:9000",
		"ragAgent.config.AWS_S3_BUCKET_NAME":             "mekoqa",
		"ragAgent.gpu.enabled":                           false,
		"yugabyte.Image.repository":                      "quay.io/yugabyte/meko-yugabytedb",
		"yugabyte.Image.tag":                             "2026.1.0.0-b1-meko-2",
		"apiServer.migrate.extensionsImage":              cfg.DatabaseImage,
		"inferenceGateway.config.INFERENCE_LLM_PROVIDER": "openai",
	}
	for path, want := range checks {
		if got := get(strings.Split(path, ".")...); got != want {
			t.Errorf("%s = %v (%T), want %v", path, got, got, want)
		}
	}
	for _, worker := range []string{"text", "conversations", "pdf"} {
		w, _ := get("ragAgent", worker).(map[string]any)
		if _, present := w["nodeSelector"]; !present || w["nodeSelector"] != nil {
			t.Errorf("ragAgent.%s.nodeSelector = %v, want an explicit null", worker, w["nodeSelector"])
		}
	}
	if !strings.Contains(string(b), "nodeSelector: null") {
		t.Errorf("the rendered YAML has no explicit null nodeSelector:\n%s", b)
	}
}

func TestAllDigitSecretsStayStrings(t *testing.T) {
	dp, err := NewDataPlaneSecrets(bytes.NewReader(make([]byte, 512)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dp.LangfuseEncryptKey, "0000") || len(dp.LangfuseEncryptKey) != 64 {
		t.Fatalf("encryption key = %q, want 64 hex chars", dp.LangfuseEncryptKey)
	}
	v, _ := testConfig(t).Values(dp, MinIO{})
	b, _ := MarshalValues(v)
	var back struct {
		DataPlaneSecrets map[string]any `yaml:"dataPlaneSecrets"`
	}
	if err := yaml.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if got, ok := back.DataPlaneSecrets["langfuseEncryptionKey"].(string); !ok || got != dp.LangfuseEncryptKey {
		t.Errorf("langfuseEncryptionKey read back as %T %v, want the 64-char string", back.DataPlaneSecrets["langfuseEncryptionKey"], back.DataPlaneSecrets["langfuseEncryptionKey"])
	}
}

func TestFreshSecretsPerStack(t *testing.T) {
	a, _ := NewDataPlaneSecrets(nil)
	b, _ := NewDataPlaneSecrets(nil)
	if a.DBPassword == b.DBPassword || a.LangfuseEncryptKey == b.LangfuseEncryptKey || a.LangfuseNextauth == b.LangfuseNextauth {
		t.Error("two stacks drew the same secrets")
	}
}

func TestMaskedValuesHideSecretsOnly(t *testing.T) {
	cfg := testConfig(t)
	dp, _ := NewDataPlaneSecrets(nil)
	v, _ := cfg.Values(dp, MinIO{AccessKey: "ak", SecretKey: "sk", WorkerEndpoint: "http://10.0.0.1:9000"})
	b, _ := MarshalValues(MaskedValues(v))
	text := string(b)
	for _, secret := range []string{dp.DBPassword, dp.LangfuseEncryptKey, dp.LangfuseInitPassword, dp.LangfuseProjectKey, "sk-unused", "sk\n"} {
		if strings.Contains(text, secret) {
			t.Errorf("masked values still contain %q", secret)
		}
	}
	for _, kept := range []string{dp.LangfuseInitEmail, "0.261002.0-b75b2a658", "http://10.0.0.1:9000"} {
		if !strings.Contains(text, kept) {
			t.Errorf("masked values lost %q", kept)
		}
	}
	lits := SecretLiterals(v)
	if len(lits) < 10 {
		t.Errorf("SecretLiterals found %d, want every dataPlaneSecrets credential", len(lits))
	}
	if got := RedactArgs("pw="+dp.DBPassword, lits); strings.Contains(got, dp.DBPassword) {
		t.Errorf("RedactArgs left the password: %s", got)
	}
}

func TestRenderedImages(t *testing.T) {
	manifest := `
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      initContainers:
        - name: init
          image: quay.io/yugabyte/meko-yugabytedb:1
      containers:
        - name: a
          image: "nginx:1.27-alpine"
---
apiVersion: v1
kind: StatefulSet
spec:
  containers:
    - image: quay.io/yugabyte/meko-yugabytedb:1
`
	got, err := RenderedImages(manifest)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"nginx:1.27-alpine", "quay.io/yugabyte/meko-yugabytedb:1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("RenderedImages = %v, want %v", got, want)
	}
}

func TestRepairReleaseHandlesPendingStates(t *testing.T) {
	cases := []struct {
		status    string
		listed    bool
		wantCalls []string
		wantNone  []string
	}{
		{"pending-install", true, []string{"helm --kube-context ctx uninstall meko -n mekoqa-st-abc"}, []string{"rollback"}},
		{"pending-upgrade", true, []string{"helm --kube-context ctx rollback meko -n mekoqa-st-abc"}, []string{" uninstall "}},
		{"pending-rollback", true, []string{"helm --kube-context ctx rollback meko -n mekoqa-st-abc"}, []string{" uninstall "}},
		{"deployed", true, nil, []string{"rollback", " uninstall "}},
		{"failed", true, nil, []string{"rollback", " uninstall "}},
		{"", false, nil, []string{"rollback", " uninstall "}},
	}
	for _, tc := range cases {
		cl := &cluster{handle: func(line string, _ call) (execpkg.Result, bool) {
			if strings.Contains(line, " list --deployed ") {
				if !tc.listed {
					return out("[]")
				}
				return out(fmt.Sprintf(`[{"name":"other","status":"deployed"},{"name":"meko","namespace":"mekoqa-st-abc","status":%q}]`, tc.status))
			}
			return execpkg.Result{}, false
		}}
		a := New(testConfig(t)).WithRunner(cl.run).WithLogf(nil)
		if err := a.repairRelease(context.Background()); err != nil {
			t.Errorf("%q: %v", tc.status, err)
		}
		for _, want := range tc.wantCalls {
			if cl.count(want) != 1 {
				t.Errorf("%q: %q ran %d times in %v", tc.status, want, cl.count(want), cl.lines())
			}
		}
		for _, none := range tc.wantNone {
			if cl.count(none) != 0 {
				t.Errorf("%q: %q ran in %v", tc.status, none, cl.lines())
			}
		}
	}
}

func TestRepairReleaseSurfacesAFailedRollback(t *testing.T) {
	cl := &cluster{handle: func(line string, _ call) (execpkg.Result, bool) {
		switch {
		case strings.Contains(line, " list --deployed "):
			return out(`[{"name":"meko","status":"pending-upgrade"}]`)
		case strings.Contains(line, " rollback "):
			return execpkg.Result{ExitCode: 1, Stderr: "Error: release has no deployed revision"}, true
		}
		return execpkg.Result{}, false
	}}
	a := New(testConfig(t)).WithRunner(cl.run).WithLogf(nil)
	if err := a.repairRelease(context.Background()); err == nil || !strings.Contains(err.Error(), "no deployed revision") {
		t.Fatalf("err = %v", err)
	}
}

func TestWorkloadsAndNotReady(t *testing.T) {
	cl := &cluster{handle: func(line string, _ call) (execpkg.Result, bool) {
		switch {
		case strings.Contains(line, "get deployments"):
			return out("meko-api-server\t3\t3\nrag-agent-pdf\t1\t\n")
		case strings.Contains(line, "get statefulsets"):
			return out("yb-master\t1\t1\nyb-tserver\t1\t1\n")
		}
		return execpkg.Result{}, false
	}}
	a := New(testConfig(t)).WithRunner(cl.run).WithLogf(nil)
	ws, err := a.Workloads(context.Background())
	if err != nil || len(ws) != 4 {
		t.Fatalf("Workloads = %+v, %v", ws, err)
	}
	nr := NotReady(ws)
	if len(nr) != 1 || nr[0].Name != "rag-agent-pdf" || nr[0].Kind != "deployment" {
		t.Errorf("NotReady = %+v, want only rag-agent-pdf", nr)
	}
}

func TestExtensions(t *testing.T) {
	cl := &cluster{handle: func(line string, _ call) (execpkg.Result, bool) {
		switch {
		case strings.Contains(line, "get pods -l app=yb-tserver"):
			return out("yb-tserver-0")
		case strings.Contains(line, "exec yb-tserver-0 -c yb-tserver"):
			return out("mage\npg_dist_rag\nplpgsql\nvector\n")
		}
		return execpkg.Result{}, false
	}}
	a := New(testConfig(t)).WithRunner(cl.run).WithLogf(nil)
	exts, err := a.Extensions(context.Background())
	if err != nil || strings.Join(exts, ",") != "mage,pg_dist_rag,plpgsql,vector" {
		t.Fatalf("Extensions = %v, %v", exts, err)
	}
}

func TestInitDoneNeedsEveryInitContainerAndTheLogLine(t *testing.T) {
	state := map[string]string{
		"app=meko-api-server":     "ensure-extensions=0\nensure-extensions=0\nmigrate=0\nensure-extensions=0\nmigrate=0\nmigrate=0\n",
		"app=langfuse-web-worker": "metadata-schema-init=0\ntracing-schema-init=\n",
	}
	logs := "creating extension vector\nextensions ready\n"
	cl := &cluster{handle: func(line string, _ call) (execpkg.Result, bool) {
		for sel, o := range state {
			if strings.Contains(line, "get pods -l "+sel+" ") {
				return out(o)
			}
		}
		if strings.Contains(line, "logs deployment/meko-api-server -c ensure-extensions") {
			return out(logs)
		}
		return execpkg.Result{}, false
	}}
	a := New(testConfig(t)).WithRunner(cl.run).WithLogf(nil)

	if done, err := a.initDone(context.Background()); done || err == nil || !strings.Contains(err.Error(), "tracing-schema-init") {
		t.Fatalf("with tracing-schema-init unfinished: done=%v err=%v", done, err)
	}
	state["app=langfuse-web-worker"] = "metadata-schema-init=0\ntracing-schema-init=0\n"
	if done, err := a.initDone(context.Background()); !done || err != nil {
		t.Fatalf("all finished: done=%v err=%v", done, err)
	}
	logs = "creating extension vector\n"
	if done, err := a.initDone(context.Background()); done || err == nil || !strings.Contains(err.Error(), "extensions ready") {
		t.Fatalf("without the success line: done=%v err=%v", done, err)
	}
	state["app=meko-api-server"] = ""
	logs = "extensions ready"
	if done, err := a.initDone(context.Background()); done || err == nil {
		t.Fatalf("no pods yet: done=%v err=%v", done, err)
	}
}

func newHealthServer(t *testing.T, health, datapacks int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			w.WriteHeader(health)
			fmt.Fprint(w, `{"data":{"status":"ok","database":"ok","system_database":"ok","data_database":"ok"}}`)
		case "/api/v1/datapacks":
			w.WriteHeader(datapacks)
			fmt.Fprint(w, `{"data":[]}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func readyCluster() *cluster {
	return &cluster{handle: func(line string, _ call) (execpkg.Result, bool) {
		switch {
		case strings.Contains(line, "get pods -l app=meko-api-server -o jsonpath={range"):
			return out("ensure-extensions=0\nmigrate=0\n")
		case strings.Contains(line, "get pods -l app=langfuse-web-worker -o jsonpath={range"):
			return out("metadata-schema-init=0\ntracing-schema-init=0\n")
		case strings.Contains(line, "logs deployment/meko-api-server"):
			return out("extensions ready")
		case strings.Contains(line, "get service meko-api-server-service"), strings.Contains(line, "get service meko-mcp-server"):
			return out("10.96.0.10")
		case strings.Contains(line, "get service llmmock"):
			return out("10.96.0.20")
		case strings.Contains(line, "get pods -l app=yb-tserver"):
			return out("yb-tserver-0")
		case strings.Contains(line, "exec yb-tserver-0 -c yb-tserver"):
			return out("1\nUPDATE 1\n")
		}
		return execpkg.Result{}, false
	}}
}

func TestWaitReadyRunsTheSchemaRequestAfterHealth(t *testing.T) {
	srv := newHealthServer(t, 200, 200)
	a := New(testConfig(t)).WithRunner(readyCluster().run).WithLogf(nil).WithPollInterval(time.Millisecond)
	a.dial = func(context.Context, string) error { return nil }
	a.newAPI = func(string) *mekoapi.Client { return mekoapi.New(srv.URL) }
	if err := a.waitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := a.APIURL(), "http://10.96.0.10:80"; got != want {
		t.Errorf("APIURL = %q, want the Service address %q", got, want)
	}
	if got, want := a.MCPURL(), "http://10.96.0.10:80/mcp"; got != want {
		t.Errorf("MCPURL = %q, want %q", got, want)
	}
}

func TestWaitReadyFailsFastOnAnAuthErrorFromTheSchemaRequest(t *testing.T) {
	srv := newHealthServer(t, 200, 401)
	a := New(testConfig(t)).WithRunner(readyCluster().run).WithLogf(nil).WithPollInterval(time.Millisecond)
	a.dial = func(context.Context, string) error { return nil }
	a.newAPI = func(string) *mekoapi.Client { return mekoapi.New(srv.URL) }
	start := time.Now()
	err := a.waitReady(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want the 401", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("a 401 was retried until the timeout")
	}
}

func TestWaitReadyKeepsRetryingA5xx(t *testing.T) {
	srv := newHealthServer(t, 200, 500)
	cfg := testConfig(t)
	cfg.ReadyTimeout = 30 * time.Millisecond
	a := New(cfg).WithRunner(readyCluster().run).WithLogf(nil).WithPollInterval(5 * time.Millisecond)
	a.dial = func(context.Context, string) error { return nil }
	a.newAPI = func(string) *mekoapi.Client { return mekoapi.New(srv.URL) }
	err := a.waitReady(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want a timeout carrying the last 500", err)
	}
}

func TestReachFallsBackToAPortForward(t *testing.T) {
	cl := readyCluster()
	a := New(testConfig(t)).WithRunner(cl.run).WithLogf(nil)
	a.dial = func(context.Context, string) error { return fmt.Errorf("no route") }
	a.WithStarter(func(ctx context.Context, argv []string) (k8s.Process, error) {
		return &fakeProc{out: "Forwarding from 127.0.0.1:41234 -> 25433\n"}, nil
	})
	url, err := a.reach(context.Background(), apiService, apiServicePort, "app="+apiDeployment, apiPodPort)
	if err != nil {
		t.Fatal(err)
	}
	if url != "http://127.0.0.1:41234" {
		t.Errorf("url = %q, want the forwarded address", url)
	}
	a.Close()
	if len(a.pfs) != 0 {
		t.Errorf("Close left %d forwards", len(a.pfs))
	}
}

type fakeProc struct{ out string }

func (p *fakeProc) Stdout() io.Reader { return strings.NewReader(p.out) }
func (p *fakeProc) Kill() error       { return nil }
func (p *fakeProc) Wait() error       { select {} }

func TestFileSinkStaysInsideItsDir(t *testing.T) {
	dir := t.TempDir()
	s := fileSink{dir}
	if err := s.Add("../../escape.txt", "txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("Stack logs/pod/c.log", "log", []byte("y")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"escape.txt", "Stack logs/pod/c.log"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
	}
}

// minioFake stands in for apps/minio so Deploy can run against the scripted
// cluster.
func minioFake(_ context.Context, kube *k8s.Kubectl, _ minio.Spec) (string, error) {
	return "http://minio." + kube.Namespace() + ".svc.cluster.local:9000", nil
}

func llmFake(context.Context, *k8s.Kubectl, llmmock.Spec) (*llmmock.Server, error) {
	return &llmmock.Server{}, nil
}

const quayManifest = `
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - image: quay.io/yugabyte/meko-api-server:0.261002.0-b75b2a658
        - image: docker.io/valkey/valkey:9.0.1
`

func deployCluster(manifest string) *cluster {
	ready := readyCluster()
	return &cluster{handle: func(line string, c call) (execpkg.Result, bool) {
		switch {
		case strings.Contains(line, " template "):
			return out(manifest)
		case strings.Contains(line, "get namespace mekoqa-st-abc -o json"):
			return notFound(`namespaces "mekoqa-st-abc"`)
		case strings.Contains(line, "get service minio"):
			return out("10.96.5.5")
		case strings.Contains(line, " list --deployed "):
			return out("[]")
		case strings.Contains(line, "get deployments"):
			return out("meko-api-server\t3\t3\n")
		case strings.Contains(line, "get statefulsets"):
			return out("yb-tserver\t1\t1\n")
		}
		return ready.handle(line, c)
	}}
}

func deployApp(t *testing.T, cl *cluster) *App {
	t.Helper()
	srv := newHealthServer(t, 200, 200)
	a := New(testConfig(t)).WithRunner(cl.run).WithLogf(nil).WithPollInterval(time.Millisecond)
	a.deployMinIO = minioFake
	a.deployLLM = llmFake
	a.dial = func(context.Context, string) error { return nil }
	a.newAPI = func(string) *mekoapi.Client { return mekoapi.New(srv.URL) }
	return a
}

func TestDeploySequenceAndWhatItSends(t *testing.T) {
	cl := deployCluster(quayManifest)
	a := deployApp(t, cl)
	if err := a.Deploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := cl.lines()
	order := []string{
		" template ", "get namespace mekoqa-st-abc -o json", "apply -f -", "annotate namespace mekoqa-st-abc mekoqa-expires=",
		"get service minio", " list --deployed ", "values-langfuse-first.yaml", "get pods -l app=langfuse-web-worker", "values-one-api-server.yaml",
		"mekoqa-st-abc-values.yaml --timeout", "get statefulsets", "patch deployment meko-api-server",
		"rollout status deployment/meko-api-server", "patch deployment meko-mcp-server", "rollout status deployment/meko-mcp-server",
		"patch deployment rag-agent-text", "rollout status deployment/rag-agent-pdf",
		"UPDATE meko_system.users SET user_type = 'enterprise' WHERE cognito_sub = 'dev-local'",
	}
	prev := -1
	for _, want := range order {
		idx := -1
		for i := prev + 1; i < len(lines); i++ {
			if strings.Contains(lines[i], want) {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Fatalf("%q did not follow the previous step; calls:\n%s", want, strings.Join(lines, "\n"))
		}
		prev = idx
	}

	var upgrade call
	for _, c := range cl.calls {
		line := c.line()
		if !strings.Contains(line, " upgrade --install ") {
			continue
		}
		upgrade = c
		if strings.Contains(line, "values-one-api-server.yaml") {
			b, err := os.ReadFile(c.argv[len(c.argv)-3])
			if err != nil || !strings.Contains(string(b), "replicas: 1") {
				t.Errorf("the migration pass does not run one api-server pod: %v\n%s", err, b)
			}
		}
		if i := strings.Index(line, "values-langfuse-first.yaml"); i >= 0 {
			staged := c.argv[len(c.argv)-3]
			b, err := os.ReadFile(staged)
			if err != nil || !strings.Contains(string(b), "replicas: 0") {
				t.Errorf("the langfuse-first values %s do not hold the api-server at 0 replicas: %v\n%s", staged, err, b)
			}
		}
	}
	line := upgrade.line()
	gcp := strings.Index(line, "values-gcp.yaml")
	dev := strings.Index(line, "values-dev.yaml")
	byoc := strings.Index(line, "values-byoc.yaml")
	gen := strings.Index(line, "mekoqa-st-abc-values.yaml")
	if !(gcp > 0 && gcp < dev && dev < byoc && byoc < gen) {
		t.Errorf("preset order wrong: %s", line)
	}
	for _, want := range []string{"--kube-context ctx", "upgrade --install meko ", "-n mekoqa-st-abc", "--timeout 1200s"} {
		if !strings.Contains(line, want) {
			t.Errorf("helm upgrade lacks %q: %s", want, line)
		}
	}
	if strings.Contains(line, "--create-namespace") {
		t.Errorf("helm was told to create the namespace: %s", line)
	}
	if strings.Contains(line, "--wait") {
		t.Errorf("helm was told to wait, which fails the install on the migrations' progress deadline: %s", line)
	}

	valuesPath := upgrade.argv[len(upgrade.argv)-3]
	if !strings.HasSuffix(valuesPath, "mekoqa-st-abc-values.yaml") {
		t.Fatalf("could not find the values path in %v", upgrade.argv)
	}
	st, err := os.Stat(valuesPath)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("values file %s: %v %v, want mode 0600", valuesPath, st, err)
	}
	values, _ := os.ReadFile(valuesPath)
	var secrets []string
	for _, key := range []string{"dbPassword", "langfuseEncryptionKey", "langfuseInitUserPassword", "mcpInternalSecret"} {
		var m struct {
			D map[string]string `yaml:"dataPlaneSecrets"`
		}
		if err := yaml.Unmarshal(values, &m); err != nil || m.D[key] == "" {
			t.Fatalf("values lack %s: %v", key, err)
		}
		secrets = append(secrets, m.D[key])
	}
	secrets = append(secrets, "quay-token-value")
	for _, l := range lines {
		for _, s := range secrets {
			if strings.Contains(l, s) {
				t.Errorf("a secret reached a command line: %s", l)
			}
		}
	}

	var nsApply, pull, api, rag call
	for _, c := range cl.calls {
		switch {
		case c.stdin != "" && strings.Contains(c.stdin, "kind: Namespace"):
			nsApply = c
		case strings.Contains(c.stdin, "kubernetes.io/dockerconfigjson"):
			pull = c
		case strings.Contains(c.line(), "patch deployment meko-api-server") && strings.Contains(c.line(), "S3_ENDPOINT"):
			api = c
		case strings.Contains(c.stdin, "name: rag-agent-secret"):
			rag = c
		}
	}
	for _, want := range []string{`mekoqa-managed-by: "mekoqa"`, `mekoqa-run: "abc"`, `mekoqa-owner: "me"`, `mekoqa-test: "true"`} {
		if !strings.Contains(nsApply.stdin, want) {
			t.Errorf("namespace manifest lacks %s:\n%s", want, nsApply.stdin)
		}
	}
	if !strings.Contains(pull.stdin, "name: quay-registry-creds") {
		t.Errorf("pull secret manifest: %s", pull.stdin)
	}
	if !strings.Contains(api.line(), "S3_ENDPOINT") || !strings.Contains(api.line(), "http://minio.mekoqa-st-abc.svc.cluster.local:9000") {
		t.Errorf("api-server patch = %s", api.line())
	}
	if !strings.Contains(rag.stdin, `AWS_ACCESS_KEY_ID: "mekoqa-minio"`) {
		t.Errorf("rag secret = %s", rag.stdin)
	}
	if !strings.Contains(string(values), "http://10.96.5.5:9000") {
		t.Errorf("the rag workers were not pointed at the MinIO ClusterIP:\n%s", values)
	}
}

func TestDeployRefusesANonQuayImageBeforeCreatingAnything(t *testing.T) {
	cl := deployCluster(quayManifest + `
---
kind: Deployment
spec:
  containers:
    - image: 161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/rag-agent:1.0.4
`)
	a := deployApp(t, cl)
	err := a.Deploy(context.Background())
	if err == nil || !strings.Contains(err.Error(), "meko/rag-agent") {
		t.Fatalf("err = %v, want the ECR image named", err)
	}
	for _, l := range cl.lines() {
		if strings.Contains(l, " apply ") || strings.Contains(l, " create ") || strings.Contains(l, " upgrade ") {
			t.Errorf("something was created before the refusal: %s", l)
		}
	}
	if a.nsCreated {
		t.Error("nsCreated is set after a refusal")
	}
}

func TestDeployRefusesAnUnlabelledNamespace(t *testing.T) {
	cl := deployCluster(quayManifest)
	inner := cl.handle
	cl.handle = func(line string, c call) (execpkg.Result, bool) {
		if strings.Contains(line, "get namespace mekoqa-st-abc -o json") {
			return out(`{"metadata":{"name":"mekoqa-st-abc","labels":{"team":"someone-else"}}}`)
		}
		return inner(line, c)
	}
	err := deployApp(t, cl).Deploy(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not a mekoqa namespace") {
		t.Fatalf("err = %v", err)
	}
	if cl.count(" upgrade ") != 0 {
		t.Error("helm ran against a namespace that is not ours")
	}
}

func TestFailedDeployRecordsDiagnosticsAndDeletesNothing(t *testing.T) {
	cl := deployCluster(quayManifest)
	inner := cl.handle
	cl.handle = func(line string, c call) (execpkg.Result, bool) {
		switch {
		case strings.Contains(line, " upgrade --install "):
			return execpkg.Result{ExitCode: 1, Stderr: "Error: context deadline exceeded"}, true
		case strings.Contains(line, "get pods -o jsonpath"):
			return out("meko-api-server-0\tPending\tFalse\tuid1\t0,\tUnschedulable|0/3 nodes are available: Insufficient cpu\n")
		case strings.Contains(line, "logs deployment/langfuse-web-worker -c metadata-schema-init"):
			return out("waiting for ysql at yb-tservers:5433 ...\n")
		}
		return inner(line, c)
	}
	cfg := testConfig(t)
	cfg.DiagDir = t.TempDir()
	a := New(cfg).WithRunner(cl.run).WithLogf(nil).WithPollInterval(time.Millisecond)
	a.deployMinIO = minioFake
	a.deployLLM = llmFake
	err := a.Deploy(context.Background())
	if err == nil {
		t.Fatal("Deploy succeeded")
	}
	for _, want := range []string{"context deadline exceeded", "meko-api-server-0", "Insufficient cpu", "metadata-schema-init", "waiting for ysql"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	for _, l := range cl.lines() {
		if strings.Contains(l, " delete ") || strings.Contains(l, " uninstall ") {
			t.Errorf("a failed Deploy deleted something: %s", l)
		}
	}
	if _, statErr := os.Stat(filepath.Join(cfg.DiagDir, "mekoqa-st-abc", "Stack logs", "init", "langfuse-web-worker", "metadata-schema-init.log")); statErr != nil {
		t.Errorf("the init container log was not written: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(cfg.DiagDir, "mekoqa-st-abc", "Stack values.yaml")); statErr != nil {
		t.Errorf("the masked values were not written: %v", statErr)
	}
}

func TestTeardownCollectsFirstAndOnlyTouchesLabelledNamespaces(t *testing.T) {
	ours := `{"metadata":{"labels":{"mekoqa-managed-by":"mekoqa","mekoqa-run":"abc"}}}`
	theirs := `{"metadata":{"labels":{"app":"dev"}}}`
	for name, body := range map[string]string{"ours": ours, "theirs": theirs} {
		cl := &cluster{handle: func(line string, _ call) (execpkg.Result, bool) {
			if strings.Contains(line, "get namespace mekoqa-st-abc -o json") {
				return out(body)
			}
			if strings.Contains(line, "get events") {
				return out("LAST SEEN   TYPE   REASON\n")
			}
			return execpkg.Result{}, false
		}}
		cfg := testConfig(t)
		cfg.DiagDir = t.TempDir()
		a := New(cfg).WithRunner(cl.run).WithLogf(nil)
		err := a.Teardown(context.Background())
		deleted := cl.count("delete namespace mekoqa-st-abc")
		switch name {
		case "ours":
			if err != nil || deleted != 1 || cl.count(" uninstall meko ") != 1 {
				t.Errorf("ours: err=%v deleted=%d uninstall=%d", err, deleted, cl.count(" uninstall meko "))
			}
			if first, del := cl.first("get events"), cl.first("delete namespace"); first < 0 || first > del {
				t.Errorf("ours: logs were collected after the delete (events at %d, delete at %d)", first, del)
			}
		case "theirs":
			if err == nil || deleted != 0 || cl.count(" uninstall ") != 0 {
				t.Errorf("theirs: err=%v deleted=%d uninstall=%d, want a refusal and no delete", err, deleted, cl.count(" uninstall "))
			}
		}
	}
}

func TestTeardownOfAGoneNamespaceIsFine(t *testing.T) {
	cl := &cluster{handle: func(line string, _ call) (execpkg.Result, bool) {
		if strings.Contains(line, "get namespace") {
			return notFound(`namespaces "mekoqa-st-abc"`)
		}
		return execpkg.Result{}, false
	}}
	a := New(testConfig(t)).WithRunner(cl.run).WithLogf(nil)
	if err := a.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cl.count("delete") != 0 {
		t.Error("deleted something that is not there")
	}
}

func TestReapChecksTheLabelFirst(t *testing.T) {
	// Reap builds its own runner from a kubeconfig, so the label read is driven
	// through the App it wraps.
	cl := &cluster{handle: func(line string, _ call) (execpkg.Result, bool) {
		if strings.Contains(line, "get namespace") {
			return out(`{"metadata":{"labels":{"app":"dev"}}}`)
		}
		return execpkg.Result{}, false
	}}
	a := New(Config{Context: "c", Namespace: "dev-ns", Release: "meko"}).WithRunner(cl.run).WithLogf(nil)
	if owned, err := a.owns(context.Background()); owned || err == nil {
		t.Errorf("owns = %v, %v for an unlabelled namespace", owned, err)
	}
}

func TestRenderedSupport(t *testing.T) {
	newer := `kind: Deployment
metadata: {name: meko-api-server}
spec:
  template:
    spec:
      containers:
        - name: meko-api-server
          env:
            - {name: S3_ENDPOINT, value: x}
---
kind: Secret
metadata: {name: rag-agent-secret}
stringData: {YUGABYTEDB_CONNECTION_STRING: x, AWS_ACCESS_KEY_ID: k, AWS_SECRET_ACCESS_KEY: s}
`
	older := `kind: Deployment
metadata: {name: meko-api-server}
spec:
  template:
    spec:
      containers:
        - name: meko-api-server
          env:
            - {name: S3_BUCKET, value: x}
---
kind: Secret
metadata: {name: inference-gateway-aws}
stringData: {AWS_ACCESS_KEY_ID: k}
---
kind: Secret
metadata: {name: rag-agent-secret}
stringData: {YUGABYTEDB_CONNECTION_STRING: x}
`
	for _, tc := range []struct {
		name     string
		manifest string
		want     ChartSupport
	}{
		{"newer chart", newer, ChartSupport{S3EndpointEnv: true, RagAWSKeys: true}},
		{"older chart", older, ChartSupport{}},
	} {
		got, err := RenderedSupport(tc.manifest)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// A migrator that refuses a dirty version never recovers, so the wait stops at
// once instead of running out the provision timeout.
func TestWaitWorkloadsStopsOnWedgedMigrations(t *testing.T) {
	cl := &cluster{handle: func(line string, _ call) (execpkg.Result, bool) {
		switch {
		case strings.Contains(line, "get deployments"):
			return out("meko-api-server\t3\t0\n")
		case strings.Contains(line, "get statefulsets"):
			return out("yb-tserver\t1\t1\n")
		case strings.Contains(line, "logs deployment/meko-api-server -c migrate"):
			return out(`level=ERROR msg="migration failed" purpose=data error="Dirty database version 1. Fix and force version."` + "\n")
		}
		return execpkg.Result{}, false
	}}
	a := New(testConfig(t)).WithRunner(cl.run).WithLogf(nil).WithPollInterval(time.Millisecond)
	err := a.waitWorkloads(context.Background())
	if err == nil || !strings.Contains(err.Error(), "api-server migrations wedged on a dirty version") {
		t.Fatalf("waitWorkloads = %v, want the wedged-migrations error", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("the wait ran out its timeout instead of stopping: %v", err)
	}
}

func TestClusterURLs(t *testing.T) {
	api, mcp := ClusterURLs("mekoqa-ds-ab12-1")
	if api != "http://meko-api-server-service.mekoqa-ds-ab12-1.svc:80" || mcp != "http://meko-mcp-server.mekoqa-ds-ab12-1.svc:80/mcp" {
		t.Errorf("api %q mcp %q", api, mcp)
	}
}

func TestUniverseUsage(t *testing.T) {
	c := &cluster{handle: func(line string, _ call) (execpkg.Result, bool) {
		switch {
		case strings.Contains(line, "get pods"):
			return execpkg.Result{Stdout: "yb-tserver-0"}, true
		case strings.Contains(line, "num_datapacks_created"):
			return execpkg.Result{Stdout: "412|1000|409\n"}, true
		}
		return execpkg.Result{}, false
	}}
	a := New(Config{Namespace: "ns"}).WithRunner(c.run)
	u, err := a.UniverseUsage(context.Background())
	if err != nil || u != (UniverseUsage{Counted: 412, Max: 1000, Datapacks: 409}) {
		t.Errorf("%+v %v", u, err)
	}
}

func TestLLMMockIsDeployedBeforeTheChartAndReached(t *testing.T) {
	cl := deployCluster(quayManifest)
	a := deployApp(t, cl)
	var deployedAt = -1
	a.deployLLM = func(ctx context.Context, kube *k8s.Kubectl, spec llmmock.Spec) (*llmmock.Server, error) {
		deployedAt = len(cl.lines())
		return &llmmock.Server{}, nil
	}
	if err := a.Deploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if first := cl.first(" upgrade --install "); deployedAt < 0 || deployedAt > first {
		t.Errorf("the mock was deployed after %d commands, helm first ran at %d", deployedAt, first)
	}
	if got, want := a.LLMURL(), "http://10.96.0.20:8080"; got != want {
		t.Errorf("LLMURL = %q, want %q", got, want)
	}
	if a.LLM() == nil {
		t.Errorf("LLM() is nil on a stack with a mock")
	}
}

func TestLLMEnvIsPatchedOnlyWhenTheChartHasNoExtraEnv(t *testing.T) {
	for name, tc := range map[string]struct {
		manifest string
		patched  bool
	}{
		"a chart without extraEnv": {quayManifest, true},
		"a chart with extraEnv":    {quayManifest + chartWithLLMEnv, false},
	} {
		t.Run(name, func(t *testing.T) {
			cl := deployCluster(tc.manifest)
			a := deployApp(t, cl)
			if err := a.Deploy(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := cl.count("patch deployment meko-mcp-server") == 1 && cl.count("patch deployment meko-api-server") >= 1
			if tc.patched != got {
				t.Fatalf("patched = %v, want %v; calls:\n%s", got, tc.patched, strings.Join(cl.lines(), "\n"))
			}
			if !tc.patched {
				return
			}
			for _, c := range cl.calls {
				line := c.line()
				if strings.Contains(line, "patch deployment meko-mcp-server") {
					for _, want := range []string{"OPENAI_BASE_URL", "http://llmmock.mekoqa-st-abc.svc.cluster.local:8080/v1", "AWS_ENDPOINT_URL_BEDROCK_RUNTIME", "AWS_ACCESS_KEY_ID", `"name":"meko-mcp"`} {
						if !strings.Contains(line, want) {
							t.Errorf("mcp-server patch lacks %s: %s", want, line)
						}
					}
				}
			}
		})
	}
}

const chartWithLLMEnv = `
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: meko-api-server
spec:
  template:
    spec:
      containers:
        - env:
            - {name: OPENAI_BASE_URL, value: x}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: meko-mcp-server
spec:
  template:
    spec:
      containers:
        - env:
            - {name: OPENAI_BASE_URL, value: x}
`

func TestLLMOffDeploysNothingAndSetsNoEnv(t *testing.T) {
	cl := deployCluster(quayManifest)
	a := deployApp(t, cl)
	a.cfg.LLM = LLMOff
	a.deployLLM = func(context.Context, *k8s.Kubectl, llmmock.Spec) (*llmmock.Server, error) {
		t.Errorf("the mock was deployed with llm off")
		return &llmmock.Server{}, nil
	}
	if err := a.Deploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := cl.count("llmmock"); n != 0 || cl.count("patch deployment meko-mcp-server") != 0 {
		t.Errorf("%d commands mention the mock, the mcp-server was patched: %v", n, cl.count("patch deployment meko-mcp-server"))
	}
	if a.LLM() != nil {
		t.Errorf("LLM() is not nil with llm off")
	}
}

func TestAttachToAStackWithoutAMockLeavesLLMNil(t *testing.T) {
	cl := readyCluster()
	inner := cl.handle
	cl.handle = func(line string, c call) (execpkg.Result, bool) {
		if strings.Contains(line, "get service llmmock") {
			return notFound(`services "llmmock"`)
		}
		return inner(line, c)
	}
	srv := newHealthServer(t, 200, 200)
	a := New(testConfig(t)).WithRunner(cl.run).WithLogf(nil).WithPollInterval(time.Millisecond)
	a.dial = func(context.Context, string) error { return nil }
	a.newAPI = func(string) *mekoapi.Client { return mekoapi.New(srv.URL) }
	if err := a.waitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.LLM() != nil {
		t.Errorf("LLM() = %v, want nil for a stack built without a mock", a.LLMURL())
	}
}

func TestValuesPointEveryLLMCallerAtTheMock(t *testing.T) {
	cfg := testConfig(t)
	dp, _ := NewDataPlaneSecrets(nil)
	m := MinIO{AccessKey: "ak", SecretKey: "sk", APIEndpoint: "http://minio:9000", WorkerEndpoint: "http://10.0.0.1:9000"}
	base := "http://llmmock.mekoqa-st-abc.svc.cluster.local:8080"

	v, err := cfg.Values(dp, m)
	if err != nil {
		t.Fatal(err)
	}
	cfgOf := func(comp string) map[string]any {
		return v[comp].(map[string]any)["config"].(map[string]any)
	}
	for _, comp := range []string{"inferenceGateway", "ragAgent"} {
		c := cfgOf(comp)
		if c["OPENAI_BASE_URL"] != base+"/v1" || c["AWS_ENDPOINT_URL_BEDROCK_RUNTIME"] != base {
			t.Errorf("%s config = %v", comp, c)
		}
	}
	if v["ragAgent"].(map[string]any)["openai"].(map[string]any)["enabled"] != true {
		t.Errorf("the rag workers get no OPENAI_API_KEY: ragAgent.openai.enabled is not true")
	}
	names := func(env any) map[string]string {
		out := map[string]string{}
		for _, e := range env.([]any) {
			m := e.(map[string]any)
			out[m["name"].(string)] = m["value"].(string)
		}
		return out
	}
	api := names(v["apiServer"].(map[string]any)["extraEnv"])
	if api["OPENAI_BASE_URL"] != base+"/v1" || len(api) != 1 {
		t.Errorf("api-server extraEnv = %v", api)
	}
	mcp := names(v["mekoMcpServer"].(map[string]any)["extraEnv"])
	for _, k := range []string{"OPENAI_BASE_URL", "AWS_ENDPOINT_URL_BEDROCK_RUNTIME", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_REGION"} {
		if mcp[k] == "" {
			t.Errorf("mcp-server extraEnv lacks %s: %v", k, mcp)
		}
	}

	cfg.LLM = LLMOff
	v, err = cfg.Values(dp, m)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := MarshalValues(v)
	for _, bad := range []string{"OPENAI_BASE_URL", "AWS_ENDPOINT_URL_BEDROCK_RUNTIME", "extraEnv"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("with llm off the values still carry %s", bad)
		}
	}
}

func TestValidateRefusesAnUnknownLLMMode(t *testing.T) {
	cfg := testConfig(t)
	cfg.LLM = "real"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "env.mekostack.llm") {
		t.Errorf("Validate = %v, want a complaint about env.mekostack.llm", err)
	}
}

func TestHealthOKReadsTheEnvelope(t *testing.T) {
	for body, want := range map[string]bool{
		`{"data":{"status":"ok","database":"ok","system_database":"ok","data_database":"ok"}}`: true,
		`{"data":{"status":"ok","database":"ok"}}`:                                             true,
		`{"data":{"status":"ok","data_database":"error"}}`:                                     false,
		`{"status":"ok","database":"ok"}`:                                                      false,
		`<html>502</html>`:                                                                     false,
	} {
		if got := healthOK(body); got != want {
			t.Errorf("healthOK(%s) = %v, want %v", body, got, want)
		}
	}
}

func TestCognitoOverlayKeepsTheLLMEnvAndFlags(t *testing.T) {
	base := map[string]any{"apiServer": map[string]any{
		"extraEnv": []any{map[string]any{"name": "OPENAI_BASE_URL", "value": "http://llmmock/v1"}, map[string]any{"name": "MEKO_ENABLE_PROJECTS", "value": "true"}},
		"replicas": 3,
	}}
	v := overlay(base, apiServerCognito(cognitomock.Spec{Region: "us-east-1"}.WithDefaults(), cognitoMockHost{ClusterIP: "10.0.0.2"}, true))
	api := v["apiServer"].(map[string]any)
	got := map[string]bool{}
	for _, e := range api["extraEnv"].([]any) {
		got[e.(map[string]any)["name"].(string)] = true
	}
	for _, k := range []string{"OPENAI_BASE_URL", "MEKO_ENABLE_PROJECTS", "SSL_CERT_FILE"} {
		if !got[k] {
			t.Errorf("api-server extraEnv lost %s after the Cognito overlay: %v", k, api["extraEnv"])
		}
	}
	if api["replicas"] != 3 || api["cognito"] == nil {
		t.Errorf("overlay dropped a key: %v", api)
	}
}

func TestLangfuseUploadsGoToTheStackMinIO(t *testing.T) {
	m := MinIO{AccessKey: "ak", SecretKey: "sk", APIEndpoint: "http://minio.ns.svc.cluster.local:9000"}
	cfg := Config{S3Bucket: "mekoqa", S3Region: "us-east-1"}
	lf := langfuseS3(cfg, m)
	for _, kind := range []string{"EVENT_UPLOAD", "MEDIA_UPLOAD", "BATCH_EXPORT"} {
		p := "LANGFUSE_S3_" + kind + "_"
		if lf[p+"BUCKET"] != "mekoqa" || lf[p+"ENDPOINT"] != m.APIEndpoint || lf[p+"FORCE_PATH_STYLE"] != "true" {
			t.Errorf("%s config %v", kind, lf)
		}
	}
}
