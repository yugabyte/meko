package artifact_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/yugabyte/yb-amp/test/functional/framework/artifact"
	"github.com/yugabyte/yb-amp/test/functional/framework/exec"
)

// scriptExec is a scripted fake exec.RemoteExecutor + Fetch: it records every
// command/copy/fetch call and returns canned results matched by substring (in
// registration order). Mirrors apps/dockerhost's test double.
type scriptExec struct {
	mu       sync.Mutex
	commands []string
	copies   []string
	fetches  []string
	rules    []rule
	fallback exec.Result
	noFetch  bool // when true, Fetch is not exposed (type-assertion fails)
}

type rule struct {
	match string
	res   exec.Result
	err   error
}

func (s *scriptExec) on(match string, res exec.Result) *scriptExec {
	s.rules = append(s.rules, rule{match: match, res: res})
	return s
}

func (s *scriptExec) Run(_ context.Context, cmd string) (exec.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, cmd)
	for _, r := range s.rules {
		if strings.Contains(cmd, r.match) {
			return r.res, r.err
		}
	}
	return s.fallback, nil
}

func (s *scriptExec) Copy(_ context.Context, local, remote string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.copies = append(s.copies, local+" -> "+remote)
	return nil
}

func (s *scriptExec) Fetch(_ context.Context, remote, local string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetches = append(s.fetches, remote+" -> "+local)
	return os.WriteFile(local, []byte("relayed-content"), 0o644)
}

func (s *scriptExec) recordedCommands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.commands))
	copy(out, s.commands)
	return out
}

// noFetchExec wraps a scriptExec via a plain (non-embedded) field so Fetch is
// never promoted — it simulates a transport that can't relay (no Fetch
// method), unlike a real ssh.Client.
type noFetchExec struct {
	inner *scriptExec
}

func (n *noFetchExec) Run(ctx context.Context, cmd string) (exec.Result, error) {
	return n.inner.Run(ctx, cmd)
}
func (n *noFetchExec) Copy(ctx context.Context, l, r string) error { return n.inner.Copy(ctx, l, r) }

func httpSpec(t *testing.T, srvURL string) artifact.Spec {
	t.Helper()
	return artifact.Spec{
		Name:     "widget",
		Version:  "1.0",
		GlobPath: "widget.bin",
		URL:      srvURL,
		Type:     artifact.TypeHTTP,
	}
}

func TestDeployUploadPathCopiesFromCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	d := &artifact.Deployer{CacheDir: t.TempDir(), HTTPClient: srv.Client()}
	ex := &scriptExec{fallback: exec.Result{ExitCode: 1}} // stat/wc "not found" by default
	ex.on("stat -c", exec.Result{ExitCode: 1})

	spec := httpSpec(t, srv.URL)
	err := d.Deploy(context.Background(), []exec.RemoteExecutor{ex}, spec, artifact.DeployOptions{RemoteDir: "/opt/tools"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(ex.copies) != 1 {
		t.Fatalf("copies = %v, want exactly one upload", ex.copies)
	}
	if !strings.HasSuffix(ex.copies[0], "-> /opt/tools/widget.bin") {
		t.Errorf("copy destination wrong: %s", ex.copies[0])
	}
}

func TestDeploySkipsWhenRemoteSizeMatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	d := &artifact.Deployer{CacheDir: t.TempDir(), HTTPClient: srv.Client()}
	ex := &scriptExec{}
	ex.on("stat -c", exec.Result{ExitCode: 0, Stdout: "7\n"}) // "payload" is 7 bytes

	spec := httpSpec(t, srv.URL)
	err := d.Deploy(context.Background(), []exec.RemoteExecutor{ex}, spec, artifact.DeployOptions{RemoteDir: "/opt/tools"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(ex.copies) != 0 {
		t.Errorf("expected no upload when remote size already matches, got %v", ex.copies)
	}
}

func TestDeployRemoteDownloadRunsCurlOnTarget(t *testing.T) {
	d := &artifact.Deployer{}
	ex := &scriptExec{}
	ex.on("stat -c", exec.Result{ExitCode: 1})

	spec := artifact.Spec{
		Name: "widget", Version: "1.0", GlobPath: "widget.bin",
		URL: "https://example.test/widget.bin", Type: artifact.TypeHTTP, RemoteDownload: true,
	}
	err := d.Deploy(context.Background(), []exec.RemoteExecutor{ex}, spec, artifact.DeployOptions{RemoteDir: "/opt/tools"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	cmds := ex.recordedCommands()
	var sawCurl bool
	for _, c := range cmds {
		if strings.Contains(c, "curl") && strings.Contains(c, "https://example.test/widget.bin") {
			sawCurl = true
		}
	}
	if !sawCurl {
		t.Errorf("expected a curl command over commands: %v", cmds)
	}
}

func TestDeployRemoteDownloadInjectsSecretViaEnvNotArgv(t *testing.T) {
	d := &artifact.Deployer{}
	ex := &scriptExec{}
	ex.on("stat -c", exec.Result{ExitCode: 1})

	spec := artifact.Spec{
		Name: "gh-tool", Version: "1.0", GlobPath: "tool.tar.gz",
		URL: "https://example.test/tool.tar.gz", Type: artifact.TypeGithubRelease, RemoteDownload: true, Secret: "gh_token",
	}
	err := d.Deploy(context.Background(), []exec.RemoteExecutor{ex}, spec,
		artifact.DeployOptions{RemoteDir: "/opt/tools", Secret: "sekrit"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	cmds := ex.recordedCommands()
	var sawEnvPrefix bool
	for _, c := range cmds {
		if strings.HasPrefix(c, "AMPQA_TOKEN=") && strings.Contains(c, "sekrit") {
			sawEnvPrefix = true
		}
		if strings.Contains(c, "-H ") && !strings.Contains(c, "$AMPQA_TOKEN") && strings.Contains(c, "sekrit") {
			t.Errorf("secret must not be embedded directly as a -H argument: %s", c)
		}
	}
	if !sawEnvPrefix {
		t.Errorf("expected an AMPQA_TOKEN=sekrit env-assignment prefix over commands: %v", cmds)
	}
}

func TestDeployRemoteUnzipRunsTar(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	d := &artifact.Deployer{CacheDir: t.TempDir(), HTTPClient: srv.Client()}
	ex := &scriptExec{}
	ex.on("stat -c", exec.Result{ExitCode: 1})

	spec := artifact.Spec{
		Name: "widget", Version: "1.0", GlobPath: "widget.tar.gz",
		URL: srv.URL, Type: artifact.TypeHTTP, RemoteUnzip: true,
	}
	err := d.Deploy(context.Background(), []exec.RemoteExecutor{ex}, spec, artifact.DeployOptions{RemoteDir: "/opt/tools"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	cmds := ex.recordedCommands()
	var sawTar bool
	for _, c := range cmds {
		if strings.Contains(c, "tar -xzf") {
			sawTar = true
		}
	}
	if !sawTar {
		t.Errorf("expected a tar -xzf command over commands: %v", cmds)
	}
}

func TestDeployTargetLabelMismatchIsNoop(t *testing.T) {
	d := &artifact.Deployer{}
	ex := &scriptExec{}
	spec := artifact.Spec{
		Name: "widget", Version: "1.0", GlobPath: "widget.bin",
		URL: "https://example.test/widget.bin", Type: artifact.TypeHTTP, RemoteDownload: true, Target: "server",
	}
	err := d.Deploy(context.Background(), []exec.RemoteExecutor{ex}, spec, artifact.DeployOptions{RemoteDir: "/opt", Label: "client"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(ex.recordedCommands()) != 0 {
		t.Errorf("expected no commands for a non-matching target, got %v", ex.recordedCommands())
	}
}

func TestDeployInternalFanoutSeedsOnceThenRelays(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	d := &artifact.Deployer{CacheDir: t.TempDir(), HTTPClient: srv.Client()}
	seed := &scriptExec{}
	seed.on("stat -c", exec.Result{ExitCode: 1})
	target2 := &scriptExec{}
	target2.on("stat -c", exec.Result{ExitCode: 1})
	target3 := &scriptExec{}
	target3.on("stat -c", exec.Result{ExitCode: 1})

	spec := artifact.Spec{
		Name: "widget", Version: "1.0", GlobPath: "widget.bin",
		URL: srv.URL, Type: artifact.TypeHTTP, InternalDownload: true,
	}
	execs := []exec.RemoteExecutor{seed, target2, target3}
	if err := d.Deploy(context.Background(), execs, spec, artifact.DeployOptions{RemoteDir: "/opt/tools"}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if len(seed.copies) != 1 {
		t.Fatalf("seed target should be uploaded to exactly once, got %v", seed.copies)
	}
	if len(seed.fetches) != 1 {
		t.Fatalf("seed target should be fetched from exactly once (the relay), got %v", seed.fetches)
	}
	if len(target2.copies) != 1 || len(target3.copies) != 1 {
		t.Errorf("both non-seed targets should receive exactly one upload: t2=%v t3=%v", target2.copies, target3.copies)
	}
}

func TestDeployInternalFanoutFallsBackWithoutFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	d := &artifact.Deployer{CacheDir: t.TempDir(), HTTPClient: srv.Client()}
	seedInner := &scriptExec{}
	seedInner.on("stat -c", exec.Result{ExitCode: 1})
	seed := &noFetchExec{inner: seedInner}
	target2 := &scriptExec{}
	target2.on("stat -c", exec.Result{ExitCode: 1})

	spec := artifact.Spec{
		Name: "widget", Version: "1.0", GlobPath: "widget.bin",
		URL: srv.URL, Type: artifact.TypeHTTP, InternalDownload: true,
	}
	execs := []exec.RemoteExecutor{seed, target2}
	if err := d.Deploy(context.Background(), execs, spec, artifact.DeployOptions{RemoteDir: "/opt/tools"}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(seed.inner.fetches) != 0 {
		t.Errorf("seed without Fetch must not be relayed from, got %v", seed.inner.fetches)
	}
	if len(target2.copies) != 1 {
		t.Errorf("fallback should still upload to the remaining target, got %v", target2.copies)
	}
}

func TestDeployNoTargetsErrors(t *testing.T) {
	d := &artifact.Deployer{}
	spec := httpSpec(t, "https://example.test/widget.bin")
	if err := d.Deploy(context.Background(), nil, spec, artifact.DeployOptions{RemoteDir: "/opt"}); err == nil {
		t.Fatal("expected an error with zero targets")
	}
}
