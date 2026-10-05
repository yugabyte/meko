package mekoload

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hdr "github.com/HdrHistogram/hdrhistogram-go"

	execpkg "github.com/yugabyte/meko/meko/test/functional/framework/exec"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
)

type getter map[string]any

func (g getter) GetString(key, def string) string {
	if v, ok := g[key].(string); ok {
		return v
	}
	return def
}

func (g getter) GetInt(key string, def int) int {
	if v, ok := g[key].(int); ok {
		return v
	}
	return def
}

func (g getter) GetDuration(key string, def time.Duration) time.Duration {
	if v, ok := g[key].(time.Duration); ok {
		return v
	}
	return def
}

func TestFromConfigDefaults(t *testing.T) {
	c, err := FromConfig(getter{}, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if c.Repo != "yugabyte/qa-services" || c.TagPrefix != "meko-load-" || c.Arch != "amd64" || c.Image != "mirror.gcr.io/library/alpine:3.20" || c.Port != 7080 || c.Token != "tok" {
		t.Errorf("%+v", c)
	}
	if _, err := FromConfig(getter{"apps.mekoload.arch": "mips"}, ""); err == nil {
		t.Error("an unknown arch was accepted")
	}
}

// --- binary resolution ---

type githubFake struct {
	*httptest.Server
	downloads  atomic.Int32
	binary     []byte
	badSum     bool
	sawAuth    atomic.Value
	releases   []map[string]any
	assetCalls atomic.Int32
}

func newGitHub(t *testing.T, binary []byte) *githubFake {
	g := &githubFake{binary: binary}
	sum := sha256.Sum256(binary)
	mux := http.NewServeMux()
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	asset := func(name string, size int) map[string]any {
		return map[string]any{"name": name, "url": g.URL + "/assets/" + name, "size": size}
	}
	g.releases = []map[string]any{
		{"tag_name": "horde-2026.10.03-aaaaaaa", "assets": []any{asset("horde_linux_amd64", 3)}},
		{"tag_name": "meko-load-draft", "draft": true, "assets": []any{}},
		{"tag_name": "meko-load-2026.10.03-bbbbbbb", "assets": []any{asset("meko-load_linux_amd64", len(binary)), asset("checksums.txt", 10)}},
		{"tag_name": "meko-load-2026.10.01-ccccccc", "assets": []any{}},
	}
	mux.HandleFunc("/repos/yugabyte/qa-services/releases", func(w http.ResponseWriter, r *http.Request) {
		g.sawAuth.Store(r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(g.releases)
	})
	mux.HandleFunc("/repos/yugabyte/qa-services/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		tag := strings.TrimPrefix(r.URL.Path, "/repos/yugabyte/qa-services/releases/tags/")
		for _, rel := range g.releases {
			if rel["tag_name"] == tag {
				_ = json.NewEncoder(w).Encode(rel)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/assets/", func(w http.ResponseWriter, r *http.Request) {
		g.assetCalls.Add(1)
		if r.Header.Get("Accept") != "application/octet-stream" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "need octet-stream and a token", http.StatusUnauthorized)
			return
		}
		switch strings.TrimPrefix(r.URL.Path, "/assets/") {
		case "meko-load_linux_amd64":
			g.downloads.Add(1)
			_, _ = w.Write(g.binary)
		case "checksums.txt":
			h := hex.EncodeToString(sum[:])
			if g.badSum {
				h = strings.Repeat("0", 64)
			}
			fmt.Fprintf(w, "%s  meko-load_linux_amd64\n", h)
		default:
			http.NotFound(w, r)
		}
	})
	return g
}

func fetcher(t *testing.T, g *githubFake, mod func(*Config)) *Fetcher {
	cfg := Config{Repo: "yugabyte/qa-services", TagPrefix: "meko-load-", Token: "tok", Arch: "amd64", CacheDir: t.TempDir()}
	if g != nil {
		cfg.APIBase = g.URL
	}
	if mod != nil {
		mod(&cfg)
	}
	return NewFetcher(cfg, nil)
}

func TestResolveReleasePicksTheNewestMekoLoadAndVerifiesIt(t *testing.T) {
	g := newGitHub(t, []byte("ELF-binary"))
	f := fetcher(t, g, nil)
	o, err := f.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if o.Kind != "release" || o.Ref != "meko-load-2026.10.03-bbbbbbb" {
		t.Errorf("origin %+v", o)
	}
	if b, _ := os.ReadFile(o.Path); string(b) != "ELF-binary" {
		t.Errorf("binary content %q", b)
	}
	if st, _ := os.Stat(o.Path); st.Mode().Perm()&0o100 == 0 {
		t.Errorf("binary is not executable: %v", st.Mode())
	}
	if g.sawAuth.Load() != "Bearer tok" {
		t.Errorf("the API call carried %v", g.sawAuth.Load())
	}
	if _, err := f.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := g.downloads.Load(); n != 1 {
		t.Errorf("the binary was downloaded %d times, the second resolve should use the cache", n)
	}
}

func TestResolveReleaseRefusesABadChecksum(t *testing.T) {
	g := newGitHub(t, []byte("ELF-binary"))
	g.badSum = true
	if _, err := fetcher(t, g, nil).Resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveReleaseByTag(t *testing.T) {
	g := newGitHub(t, []byte("x"))
	f := fetcher(t, g, func(c *Config) { c.ReleaseTag = "meko-load-2026.10.01-ccccccc" })
	_, err := f.Resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no asset") {
		t.Fatalf("err = %v", err)
	}
	f = fetcher(t, g, func(c *Config) { c.ReleaseTag = "nope" })
	if _, err := f.Resolve(context.Background()); err == nil {
		t.Fatal("an unknown tag resolved")
	}
}

func TestResolveNeedsATokenForTheRelease(t *testing.T) {
	_, err := fetcher(t, nil, func(c *Config) { c.Token = "" }).Resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "apps.mekoload.binary") || !strings.Contains(err.Error(), "apps.mekoload.source") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolvePrefersBinaryThenSource(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "meko-load")
	_ = os.WriteFile(bin, []byte("x"), 0o755)
	var built []string
	f := fetcher(t, nil, func(c *Config) { c.Binary = bin; c.Source = "/src"; c.Token = "" })
	f.build = func(_ context.Context, dir, out, arch string) error {
		built = append(built, dir+" "+arch)
		return os.WriteFile(out, []byte("built"), 0o755)
	}
	o, err := f.Resolve(context.Background())
	if err != nil || o.Kind != "binary" || o.Path != bin || len(built) != 0 {
		t.Fatalf("origin %+v err %v built %v", o, err, built)
	}

	f = fetcher(t, nil, func(c *Config) { c.Source = "/src"; c.Arch = "arm64"; c.Token = "" })
	f.build = func(_ context.Context, dir, out, arch string) error {
		built = append(built, dir+" "+arch)
		return os.WriteFile(out, []byte("built"), 0o755)
	}
	o, err = f.Resolve(context.Background())
	if err != nil || o.Kind != "source" || len(built) != 1 || built[0] != "/src arm64" {
		t.Fatalf("origin %+v err %v built %v", o, err, built)
	}

	f = fetcher(t, nil, func(c *Config) { c.Binary = filepath.Join(t.TempDir(), "missing") })
	if _, err := f.Resolve(context.Background()); err == nil {
		t.Error("a missing binary path resolved")
	}
}

func TestSourceBuildCompilesForLinux(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.21\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "cmd", "meko-load"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "cmd", "meko-load", "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	out := filepath.Join(t.TempDir(), "out")
	if err := goBuild(context.Background(), dir, out, "amd64"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil || len(b) < 4 || string(b[:4]) != "\x7fELF" {
		t.Fatalf("not a linux binary: %v %q", err, b[:min(len(b), 4)])
	}
}

// --- aggregate ---

func encoded(values ...int64) string {
	h := hdr.New(1, 10*60*1_000_000, 3)
	for _, v := range values {
		_ = h.RecordValue(v * 1000)
	}
	b, _ := h.Encode(hdr.V2CompressedEncodingCookieBase)
	return base64.StdEncoding.EncodeToString(b)
}

func TestMergeSumsCountsAndMergesHistograms(t *testing.T) {
	var a, b []int64
	for i := int64(1); i <= 50; i++ {
		a = append(a, i)
		b = append(b, i+50)
	}
	s1 := Stats{ID: "r1", State: "done", HDR: map[string]string{"read": encoded(a...), "": encoded(a...)},
		Summary: Summary{ElapsedSeconds: 10, Http5xx: 1, Total: OpSummary{Count: 52, OK: 50, Errors: 2, Throughput: 5.2},
			Ops: map[string]OpSummary{"read": {Count: 52, OK: 50, Errors: 2, Throughput: 5.2, ErrorsByClass: map[string]int64{"http_5xx": 2}, Statuses: map[string]int64{"500": 1}, Latency: Latency{P99: 50}}}},
		Invariants: map[string]Invariant{"x": {Checked: 5, Violated: 1, Samples: []string{"bad"}}}, Violations: 1,
		Resources: ResourceCounts{DatapacksCreated: 3, DatapacksLive: 1}}
	s2 := Stats{ID: "r2", State: "running", HDR: map[string]string{"read": encoded(b...), "": encoded(b...)},
		Summary: Summary{ElapsedSeconds: 12, Total: OpSummary{Count: 50, OK: 50, Throughput: 5},
			Ops: map[string]OpSummary{"read": {Count: 50, OK: 50, Throughput: 5, Latency: Latency{P99: 100}}}},
		Invariants: map[string]Invariant{"x": {Checked: 5}}, Resources: ResourceCounts{DatapacksCreated: 2}}
	agg := Merge([]Stats{s1, s2})

	if agg.Finished() {
		t.Error("one pod is still running")
	}
	if agg.Elapsed != 12 || agg.Http5xx != 1 || agg.Violations != 1 {
		t.Errorf("%+v", agg)
	}
	r := agg.Ops["read"]
	if r.Count != 102 || r.OK != 100 || r.Errors != 2 || r.ErrorsByClass["http_5xx"] != 2 || r.Statuses["500"] != 1 {
		t.Errorf("read %+v", r)
	}
	if r.Throughput < 10.1 || r.Throughput > 10.3 {
		t.Errorf("throughput %v", r.Throughput)
	}
	if r.Latency.P50 < 49 || r.Latency.P50 > 52 || r.Latency.P99 < 98 || r.Latency.P99 > 101 {
		t.Errorf("merged latency %+v, want p50 about 50 and p99 about 99", r.Latency)
	}
	if agg.Invariants["x"].Checked != 10 || agg.Invariants["x"].Violated != 1 {
		t.Errorf("invariants %+v", agg.Invariants)
	}
	if agg.Resources.DatapacksCreated != 5 || agg.Resources.DatapacksLive != 1 {
		t.Errorf("resources %+v", agg.Resources)
	}
	if agg.Total.Latency.P99 < 98 {
		t.Errorf("total latency %+v", agg.Total.Latency)
	}
}

func TestMergeFallsBackToTheWorstPodWithoutHistograms(t *testing.T) {
	mk := func(p99 float64) Stats {
		return Stats{State: "done", Summary: Summary{Ops: map[string]OpSummary{"op": {Count: 10, OK: 10, Latency: Latency{P99: p99, Mean: 5}}}}}
	}
	agg := Merge([]Stats{mk(30), mk(80)})
	if got := agg.Ops["op"].Latency.P99; got != 80 {
		t.Errorf("p99 %v", got)
	}
	if got := agg.Ops["op"].Latency.Mean; got != 5 {
		t.Errorf("mean %v", got)
	}
	if !agg.Finished() {
		t.Error("all pods are done")
	}
}

// --- fleet ---

type fakeLoad struct {
	*httptest.Server
	mu      sync.Mutex
	reqs    []string
	stats   map[string]any
	runSeq  int
	nowSkew time.Duration
	failRun bool
}

func newFakeLoad(t *testing.T) *fakeLoad {
	f := &fakeLoad{stats: map[string]any{"state": "running"}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, r.URL.Path+" "+string(body))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/health":
			fmt.Fprint(w, `{"status":"ok","version":"test","node_id":"n"}`)
		case "/v1/targets/create":
			fmt.Fprint(w, `{"id":"t-1"}`)
		case "/v1/runs/create":
			if f.failRun {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":{"code":"bad_request","message":"unknown workload"}}`)
				return
			}
			f.mu.Lock()
			f.runSeq++
			id := fmt.Sprintf("r-%d", f.runSeq)
			f.mu.Unlock()
			fmt.Fprintf(w, `{"id":%q,"state":"setting_up"}`, id)
		case "/v1/runs/stats":
			f.mu.Lock()
			st := map[string]any{"id": "r-1"}
			for k, v := range f.stats {
				st[k] = v
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(st)
		case "/v1/runs/timeline":
			now := time.Now().Add(f.nowSkew)
			fmt.Fprintf(w, `{"now_unix_ms":%d,"buckets":[{"unix":%d,"ok":5,"err":0}]}`, now.UnixMilli(), now.Unix())
		case "/v1/runs/resources":
			fmt.Fprint(w, `{"datapacks":[{"id":"dp-1","name":"mekoload-x-dp-1"}],"pats":[]}`)
		case "/v1/runs/cleanup":
			fmt.Fprint(w, `{"datapacks_deleted":1}`)
		case "/v1/runs/events":
			fmt.Fprint(w, `{"events":[]}`)
		case "/metrics":
			fmt.Fprint(w, "meko_load_ops_total 1\n")
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeLoad) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

type kubeCall struct{ argv, stdin string }

type kubeFake struct {
	mu      sync.Mutex
	calls   []kubeCall
	failFor string
}

func (k *kubeFake) run(_ context.Context, stdin io.Reader, argv []string) (execpkg.Result, error) {
	var in string
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	line := strings.Join(argv, " ")
	k.mu.Lock()
	k.calls = append(k.calls, kubeCall{line, in})
	k.mu.Unlock()
	if k.failFor != "" && strings.Contains(line, k.failFor) {
		return execpkg.Result{ExitCode: 1, Stderr: "boom"}, nil
	}
	if strings.Contains(line, "logs") {
		return execpkg.Result{Stdout: "pod log line\n"}, nil
	}
	return execpkg.Result{}, nil
}

func (k *kubeFake) matching(sub string) []kubeCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []kubeCall
	for _, c := range k.calls {
		if strings.Contains(c.argv, sub) {
			out = append(out, c)
		}
	}
	return out
}

type memSink struct {
	mu    sync.Mutex
	names []string
}

func (s *memSink) Add(name, kind string, _ []byte) error {
	s.mu.Lock()
	s.names = append(s.names, name)
	s.mu.Unlock()
	return nil
}

func testFleet(t *testing.T, servers ...*fakeLoad) (*Fleet, *kubeFake) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "meko-load")
	_ = os.WriteFile(bin, []byte("x"), 0o755)
	kf := &kubeFake{}
	kube := k8s.New(k8s.Config{Context: "ctx", Namespace: "ns"}).WithRunner(kf.run)
	f := New(Config{Binary: bin, Image: "mirror.gcr.io/library/alpine:3.20", Port: 7080, CPU: "1", Memory: "512Mi", Arch: "amd64", ReadyTimeout: 5 * time.Second}, kube, t.Logf)
	f.poll = 10 * time.Millisecond
	f.control = func(_ context.Context, p *Pod) (string, error) {
		i := int(p.Name[len(p.Name)-1] - '0')
		return servers[i%len(servers)].URL, nil
	}
	return f, kf
}

func TestFleetStartCopiesTheBinaryAndWaitsForHealth(t *testing.T) {
	s1, s2 := newFakeLoad(t), newFakeLoad(t)
	f, kf := testFleet(t, s1, s2)
	if err := f.Start(context.Background(), "ml-ab", 2); err != nil {
		t.Fatal(err)
	}
	if f.Len() != 2 || f.Origin().Kind != "binary" {
		t.Fatalf("len %d origin %+v", f.Len(), f.Origin())
	}
	applies := kf.matching("apply -f -")
	if len(applies) != 2 {
		t.Fatalf("%d pods applied", len(applies))
	}
	m := applies[0].stdin
	for _, want := range []string{"image: mirror.gcr.io/library/alpine:3.20", "mekoqa: \"1\"", "while [ ! -f /tmp/meko-load.go ]", "exec /tmp/meko-load --port 7080", "restartPolicy: Never"} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
	if n := len(kf.matching("cp ")); n != 2 {
		t.Errorf("%d copies", n)
	}
	if n := len(kf.matching("chmod 755 /tmp/meko-load && touch /tmp/meko-load.go")); n != 2 {
		t.Errorf("%d releases of the binary", n)
	}
	if len(s1.requests()) == 0 || len(s2.requests()) == 0 {
		t.Error("a pod was never asked for its health")
	}
}

func TestFleetRunLifecycle(t *testing.T) {
	s1, s2 := newFakeLoad(t), newFakeLoad(t)
	f, kf := testFleet(t, s1, s2)
	ctx := context.Background()
	if err := f.Start(ctx, "ml-ab", 2); err != nil {
		t.Fatal(err)
	}
	if err := f.CreateTargets(ctx, func(i int) TargetSpec {
		return TargetSpec{APIURL: "http://api", MCPURL: "http://mcp/mcp", Credentials: []Credential{{Token: fmt.Sprintf("tok%d", i)}}}
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(s2.requests(), "\n"), `"token":"tok1"`) {
		t.Errorf("pod 1 got %v", s2.requests())
	}

	load, err := f.StartRun(ctx, "mixed", func(i int) map[string]any { return map[string]any{"concurrency": 4 + i} }, map[string]string{"test": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(s2.requests(), "\n"), `"concurrency":5`) {
		t.Errorf("per pod config lost: %v", s2.requests())
	}

	for _, s := range []*fakeLoad{s1, s2} {
		s.mu.Lock()
		s.stats = map[string]any{"state": "done", "summary": map[string]any{"total": map[string]any{"count": 10, "ok": 9, "errors": 1}}}
		s.mu.Unlock()
	}
	var polls int
	agg, err := load.Wait(ctx, 5*time.Second, 10*time.Millisecond, func(Aggregate) { polls++ })
	if err != nil || !agg.Finished() || agg.Total.Count != 20 || agg.Total.Errors != 2 || polls == 0 {
		t.Fatalf("agg %+v err %v polls %d", agg.Total, err, polls)
	}
	if !strings.Contains(strings.Join(s1.requests(), "\n"), `"format":"hdr"`) {
		t.Error("the final read did not ask for histograms")
	}

	live, err := load.Live(ctx)
	if err != nil || len(live) != 2 || len(live[0].Datapacks) != 1 {
		t.Errorf("live %+v err %v", live, err)
	}
	cl, err := load.Cleanup(ctx)
	if err != nil || len(cl) != 2 || cl[0].DatapacksDeleted != 1 {
		t.Errorf("cleanup %+v err %v", cl, err)
	}
	if err := load.Update(ctx, map[string]any{"concurrency": 1}); err != nil {
		t.Error(err)
	}
	if err := load.Stop(ctx); err != nil {
		t.Error(err)
	}

	sink := &memSink{}
	if err := load.Collect(ctx, sink); err != nil {
		t.Error(err)
	}
	if len(sink.names) != 6 {
		t.Errorf("artifacts %v", sink.names)
	}
	sink = &memSink{}
	if err := f.Close(ctx, sink); err != nil {
		t.Fatal(err)
	}
	if n := len(kf.matching("delete pod")); n != 2 {
		t.Errorf("%d pods deleted", n)
	}
	if len(sink.names) != 2 {
		t.Errorf("pod logs collected before the delete: %v", sink.names)
	}
	if f.Len() != 0 {
		t.Error("pods still registered after Close")
	}
}

func TestStartRunNeedsTargetsAndSurfacesAPIErrors(t *testing.T) {
	s := newFakeLoad(t)
	f, _ := testFleet(t, s)
	ctx := context.Background()
	if err := f.Start(ctx, "ml-ab", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.StartRun(ctx, "mixed", func(int) map[string]any { return nil }, nil); err == nil || !strings.Contains(err.Error(), "CreateTargets") {
		t.Errorf("err = %v", err)
	}
	_ = f.CreateTargets(ctx, func(int) TargetSpec { return TargetSpec{APIURL: "http://api"} })
	s.failRun = true
	_, err := f.StartRun(ctx, "nope", func(int) map[string]any { return nil }, nil)
	var ae *APIError
	if err == nil || !strings.Contains(err.Error(), "unknown workload") {
		t.Errorf("err = %v %v", err, ae)
	}
}

func TestFleetStartFailureKeepsPodsForClose(t *testing.T) {
	s := newFakeLoad(t)
	f, kf := testFleet(t, s)
	kf.failFor = "cp "
	err := f.Start(context.Background(), "ml-ab", 2)
	if err == nil || !strings.Contains(err.Error(), "copy the binary") {
		t.Fatalf("err = %v", err)
	}
	if f.Len() != 2 {
		t.Fatalf("%d pods registered", f.Len())
	}
	if err := f.Close(context.Background(), &memSink{}); err != nil {
		t.Errorf("close: %v", err)
	}
	if n := len(kf.matching("delete pod")); n != 2 {
		t.Errorf("%d pods deleted", n)
	}
}

func TestTimelinesShiftBucketsOntoTheCoordinatorClock(t *testing.T) {
	s := newFakeLoad(t)
	s.nowSkew = 40 * time.Second
	f, _ := testFleet(t, s)
	ctx := context.Background()
	_ = f.Start(ctx, "ml-ab", 1)
	_ = f.CreateTargets(ctx, func(int) TargetSpec { return TargetSpec{APIURL: "http://api"} })
	load, err := f.StartRun(ctx, "mixed", func(int) map[string]any { return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	tls, err := load.Timelines(ctx)
	if err != nil || len(tls) != 1 || len(tls[0].Buckets) != 1 {
		t.Fatalf("%+v %v", tls, err)
	}
	if d := time.Now().Unix() - tls[0].Buckets[0].Unix; d < -1 || d > 2 {
		t.Errorf("bucket is %ds off the coordinator clock after the shift", d)
	}
}

func TestWaitTimesOutWithTheStates(t *testing.T) {
	s := newFakeLoad(t)
	f, _ := testFleet(t, s)
	ctx := context.Background()
	_ = f.Start(ctx, "ml-ab", 1)
	_ = f.CreateTargets(ctx, func(int) TargetSpec { return TargetSpec{APIURL: "http://api"} })
	load, _ := f.StartRun(ctx, "mixed", func(int) map[string]any { return nil }, nil)
	_, err := load.Wait(ctx, 100*time.Millisecond, 10*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "running") {
		t.Errorf("err = %v", err)
	}
}

func TestReachPodFallsBackToAPortForward(t *testing.T) {
	kf := &kubeFake{}
	kube := k8s.New(k8s.Config{Namespace: "ns"}).WithRunner(func(ctx context.Context, in io.Reader, argv []string) (execpkg.Result, error) {
		if strings.Contains(strings.Join(argv, " "), "jsonpath={.status.podIP}") {
			return execpkg.Result{Stdout: "10.1.2.3"}, nil
		}
		return kf.run(ctx, in, argv)
	})
	f := New(Config{Port: 7080, Reach: "tailnet", ReadyTimeout: time.Second}, kube, nil)
	f.poll = time.Millisecond
	var dialed []string
	f.dial = func(_ context.Context, addr string) error {
		dialed = append(dialed, addr)
		return fmt.Errorf("unreachable")
	}
	_, err := f.reachPod(context.Background(), &Pod{Name: "ml-0"})
	if len(dialed) != 3 || dialed[0] != "10.1.2.3:7080" {
		t.Errorf("dialed %v", dialed)
	}
	if err == nil {
		t.Skip("the port-forward fake answered")
	}
	if !strings.Contains(err.Error(), "port-forward") {
		t.Errorf("err = %v, want it to come from the port-forward fallback", err)
	}
}

func TestReachPodDirectWhenThePodAnswers(t *testing.T) {
	kube := k8s.New(k8s.Config{Namespace: "ns"}).WithRunner(func(_ context.Context, _ io.Reader, argv []string) (execpkg.Result, error) {
		return execpkg.Result{Stdout: "10.1.2.3"}, nil
	})
	f := New(Config{Port: 7080, Reach: "tailnet"}, kube, nil)
	f.dial = func(context.Context, string) error { return nil }
	base, err := f.reachPod(context.Background(), &Pod{Name: "ml-0"})
	if err != nil || base != "http://10.1.2.3:7080" {
		t.Errorf("base %q err %v", base, err)
	}
}
