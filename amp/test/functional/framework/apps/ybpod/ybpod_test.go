package ybpod

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/artifact"
	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// --- pure helpers ---

func TestTarballURLAndFilenameByArch(t *testing.T) {
	if got := tarballFilename("2025.2.1.0-b141", "x86_64"); got != "yugabyte-2025.2.1.0-b141-centos-x86_64.tar.gz" {
		t.Errorf("x86_64 filename = %q", got)
	}
	if got := tarballFilename("2025.2.1.0-b141", "aarch64"); got != "yugabyte-2025.2.1.0-b141-almalinux8-aarch64.tar.gz" {
		t.Errorf("aarch64 filename = %q", got)
	}
	if got := tarballS3URL("2025.2.1.0-b141", "x86_64"); got != "s3://releases.yugabyte.com/2025.2.1.0-b141/yugabyte-2025.2.1.0-b141-centos-x86_64.tar.gz" {
		t.Errorf("s3 url = %q", got)
	}
	// An unknown arch falls back to the x86_64 variant (never emits an invalid URL).
	if got := osArch("mips"); got != "centos-x86_64" {
		t.Errorf("osArch fallback = %q", got)
	}
}

func TestExtractedDirNameDropsBuildAndArch(t *testing.T) {
	if got := extractedDirName("2025.2.1.0-b141"); got != "yugabyte-2025.2.1.0" {
		t.Errorf("extractedDirName = %q, want yugabyte-2025.2.1.0", got)
	}
}

func TestTarballSpecIsCoordinatorFetchedS3(t *testing.T) {
	spec, err := tarballSpec("2025.2.1.0-b141", "x86_64", "", "")
	if err != nil {
		t.Fatalf("tarballSpec: %v", err)
	}
	if spec.Type != artifact.TypeS3 {
		t.Errorf("spec type = %q, want s3", spec.Type)
	}
	// Coordinator-fetched (aws s3 cp locally) then kubectl cp into the pod: the
	// pod needs no AWS creds, so RemoteDownload must stay false; the pod only
	// untars (RemoteUnzip).
	if spec.RemoteDownload {
		t.Errorf("RemoteDownload = true; the pod has no AWS creds — must fetch on the coordinator")
	}
	if !spec.RemoteUnzip {
		t.Errorf("RemoteUnzip = false; the pod must untar the delivered tarball")
	}
	if !strings.HasPrefix(spec.URL, "s3://releases.yugabyte.com/") {
		t.Errorf("URL = %q, want an S3 release-bucket URL", spec.URL)
	}
	if spec.GlobPath != "yugabyte-2025.2.1.0-b141-centos-x86_64.tar.gz" {
		t.Errorf("GlobPath = %q, want the S3 tarball filename", spec.GlobPath)
	}
}

// TestTarballSpecURLOverride: a configured tarball_url template substitutes the
// {yugabytedb}/{yugabytedb_base}/{osarch} placeholders, an http source flips the
// type to http, the glob follows the URL basename, and the secret name is carried.
func TestTarballSpecURLOverride(t *testing.T) {
	tmpl := "https://mirror.example/{yugabytedb_base}/yugabyte-{yugabytedb}-{osarch}.tar.gz"
	spec, err := tarballSpec("2025.2.1.0-b141", "aarch64", tmpl, "tarball_token")
	if err != nil {
		t.Fatalf("tarballSpec: %v", err)
	}
	want := "https://mirror.example/2025.2.1.0/yugabyte-2025.2.1.0-b141-almalinux8-aarch64.tar.gz"
	if spec.URL != want {
		t.Errorf("URL = %q, want %q", spec.URL, want)
	}
	if spec.Type != artifact.TypeHTTP {
		t.Errorf("Type = %q, want http for an https source", spec.Type)
	}
	if spec.GlobPath != "yugabyte-2025.2.1.0-b141-almalinux8-aarch64.tar.gz" {
		t.Errorf("GlobPath = %q, want the URL basename", spec.GlobPath)
	}
	if spec.Secret != "tarball_token" {
		t.Errorf("Secret = %q, want tarball_token", spec.Secret)
	}
	// An unknown placeholder is a hard error (no silent leftover in the URL).
	if _, err := tarballSpec("2025.2.1.0-b141", "x86_64", "https://x/{bogus}.tgz", ""); err == nil {
		t.Error("tarballSpec with an unknown placeholder = nil error, want an error")
	}
}

func TestNamespaceAndPodNaming(t *testing.T) {
	if got := namespaceFor("ampqa-ybpod", "run7"); got != "ampqa-ybpod-run7" {
		t.Errorf("namespaceFor = %q", got)
	}
	if got := podNameFor("run7"); got != "ybpod-run7" {
		t.Errorf("podNameFor = %q", got)
	}
}

// --- Deploy happy path (fakes only) ---

func TestDeployStartsPodDeploysTarballStartsYugabyted(t *testing.T) {
	rr := &recordRunner{podIP: "10.1.2.3", nsLabel: ManagedByValue}
	dep := &recordDeployer{}
	var waitConn string

	cfg := FromConfig(fakeGet{m: map[string]any{}})
	cfg.Version = "2025.2.1.0-b141"

	info, err := New(cfg, "run7").
		WithKube(k8s.New(k8s.Config{Namespace: "ampqa-ybpod-run7"}).WithRunner(rr.run)).
		WithArtifactDeployer(dep).
		WithWaitPG(func(_ context.Context, ci pg.ConnInfo, _ time.Duration) error { waitConn = ci.Host; return nil }).
		WithLogf(nil).
		Deploy(context.Background())
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// (a) the bare pod was applied from the base image, kept alive by sleep infinity.
	if !rr.appliedContains("image: almalinux:8") || !rr.appliedContains("- \"sleep\"") {
		t.Errorf("base pod manifest missing image/sleep-infinity; applied: %v", rr.applied)
	}
	// (b) the el8 tarball was delivered via the artifact deployer, to the install dir.
	if dep.spec.Type != artifact.TypeS3 || !strings.Contains(dep.spec.URL, "centos-x86_64") {
		t.Errorf("deployed artifact = %+v, want the x86_64 S3 tarball", dep.spec)
	}
	if dep.opts.RemoteDir != DefaultInstallDir {
		t.Errorf("deploy RemoteDir = %q, want %q", dep.opts.RemoteDir, DefaultInstallDir)
	}
	// (c) `yugabyted start` was exec'd into the pod, advertising the pod IP, from
	// the install-dir-qualified binary.
	start := rr.execCmdContaining("yugabyted", "start")
	if start == "" {
		t.Fatalf("no `yugabyted start` exec into the pod; exec cmds: %v", rr.execCmds())
	}
	for _, want := range []string{"--advertise_address='10.1.2.3'", "/root/yb-software/yugabyte-2025.2.1.0/bin/yugabyted"} {
		if !strings.Contains(start, want) {
			t.Errorf("yugabyted start missing %q in: %s", want, start)
		}
	}
	// (d) YSQL readiness probed the pod-IP tailnet target.
	if waitConn != "10.1.2.3" {
		t.Errorf("waitPG host = %q, want the pod IP 10.1.2.3", waitConn)
	}
	// (e) a ClusterIP Service exposes YSQL on 5433, selecting the pod.
	if !rr.appliedContains("kind: Service") || !rr.appliedContains("port: 5433") {
		t.Errorf("YSQL Service not applied; applied: %v", rr.applied)
	}

	// (f) the two connection paths + backing handle.
	if info.InClusterHostPorts != "ybpod-ysql.ampqa-ybpod-run7.svc.cluster.local:5433" {
		t.Errorf("InClusterHostPorts = %q", info.InClusterHostPorts)
	}
	if len(info.TailnetAddrs) != 1 || info.TailnetAddrs[0] != "10.1.2.3:5433" {
		t.Errorf("TailnetAddrs = %v, want [10.1.2.3:5433]", info.TailnetAddrs)
	}
	if info.Namespace != "ampqa-ybpod-run7" || info.PodName != "ybpod-run7" || info.Version != "2025.2.1.0-b141" {
		t.Errorf("identity = ns %q pod %q ver %q", info.Namespace, info.PodName, info.Version)
	}
	col := info.Colocated()
	if col.ConnInfo().Host != "10.1.2.3" || col.ControllerHostPortsCSV() != info.InClusterHostPorts {
		t.Errorf("Colocated paths = tailnet %q / controller %q", col.ConnInfo().Host, col.ControllerHostPortsCSV())
	}
}

func TestDeployAarch64UsesAlmalinuxTarball(t *testing.T) {
	rr := &recordRunner{podIP: "10.9.9.9", nsLabel: ManagedByValue}
	dep := &recordDeployer{}

	cfg := FromConfig(fakeGet{m: map[string]any{"apps.ybpod.arch": "aarch64"}})
	cfg.Version = "2025.2.1.0-b141"

	if _, err := New(cfg, "runA").
		WithKube(k8s.New(k8s.Config{Namespace: "ampqa-ybpod-runA"}).WithRunner(rr.run)).
		WithArtifactDeployer(dep).
		WithWaitPG(func(context.Context, pg.ConnInfo, time.Duration) error { return nil }).
		WithLogf(nil).
		Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if !strings.Contains(dep.spec.URL, "almalinux8-aarch64") {
		t.Errorf("aarch64 deploy used %q, want the almalinux8-aarch64 tarball", dep.spec.URL)
	}
}

func TestDeployRequiresVersion(t *testing.T) {
	rr := &recordRunner{podIP: "10.1.2.3", nsLabel: ManagedByValue}
	_, err := New(Config{}, "run7").
		WithKube(k8s.New(k8s.Config{}).WithRunner(rr.run)).
		WithLogf(nil).
		Deploy(context.Background())
	if err == nil {
		t.Fatalf("expected error without a version")
	}
}

func TestDeployPrivateBaseImageSetsPullSecret(t *testing.T) {
	rr := &recordRunner{podIP: "10.1.2.3", nsLabel: ManagedByValue}
	dep := &recordDeployer{}

	cfg := FromConfig(fakeGet{m: map[string]any{
		"apps.ybpod.base_image":             "quay.io/some/private-el8:1",
		"apps.ybpod.base_image_pull_secret": "quay-pull",
	}})
	cfg.Version = "2025.2.1.0-b141"

	if _, err := New(cfg, "run7").
		WithKube(k8s.New(k8s.Config{Namespace: "ampqa-ybpod-run7"}).WithRunner(rr.run)).
		WithArtifactDeployer(dep).
		WithWaitPG(func(context.Context, pg.ConnInfo, time.Duration) error { return nil }).
		WithLogf(nil).
		Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if !rr.appliedContains("imagePullSecrets:") || !rr.appliedContains("- name: quay-pull") {
		t.Errorf("private base image did not reference the pull secret; applied: %v", rr.applied)
	}
}

// --- Teardown / Reap ---

func TestTeardownDeletesManagedNamespace(t *testing.T) {
	rr := &recordRunner{nsLabel: ManagedByValue}
	d := New(Config{}, "run7").
		WithKube(k8s.New(k8s.Config{Namespace: "ampqa-ybpod-run7"}).WithRunner(rr.run)).
		WithLogf(nil)
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !rr.saw("delete", "namespace", "ampqa-ybpod-run7") {
		t.Errorf("teardown did not delete the namespace; calls: %v", rr.calls)
	}
}

func TestTeardownRefusesForeignNamespace(t *testing.T) {
	rr := &recordRunner{nsLabel: "someone-else"}
	d := New(Config{}, "run7").
		WithKube(k8s.New(k8s.Config{Namespace: "ampqa-ybpod-run7"}).WithRunner(rr.run)).
		WithLogf(nil)
	if err := d.Teardown(context.Background()); err == nil {
		t.Fatalf("expected refusal to touch a foreign-labelled namespace")
	}
	if rr.saw("delete", "namespace", "ampqa-ybpod-run7") {
		t.Errorf("teardown deleted a foreign-labelled namespace; calls: %v", rr.calls)
	}
}

// --- test doubles ---

// --- fakeGet ---

type fakeGet struct{ m map[string]any }

func (g fakeGet) GetString(key, def string) string {
	if v, ok := g.m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}
func (g fakeGet) GetInt(key string, def int) int {
	if v, ok := g.m[key]; ok {
		if n, ok := v.(int); ok {
			return n
		}
	}
	return def
}
func (g fakeGet) GetDuration(key string, def time.Duration) time.Duration {
	if v, ok := g.m[key]; ok {
		if d, ok := v.(time.Duration); ok {
			return d
		}
	}
	return def
}

// recordDeployer records the single artifact.Spec + DeployOptions delivered.
type recordDeployer struct {
	spec artifact.Spec
	opts artifact.DeployOptions
}

func (d *recordDeployer) Deploy(_ context.Context, execs []execpkg.RemoteExecutor, spec artifact.Spec, opts artifact.DeployOptions) error {
	d.spec = spec
	d.opts = opts
	return nil
}

// recordRunner is a scripted k8s.Runner: it records every argv (and apply stdin),
// answers the one read Deploy makes (the pod IP) and the namespace managed-by
// label read Teardown makes; everything else succeeds with empty output.
type recordRunner struct {
	mu      sync.Mutex
	calls   [][]string
	applied []string // stdin body of every `apply -f -`
	podIP   string
	nsLabel string
}

func (r *recordRunner) run(_ context.Context, stdin io.Reader, argv []string) (execpkg.Result, error) {
	body := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		body = string(b)
	}
	j := strings.Join(argv, " ")
	r.mu.Lock()
	r.calls = append(r.calls, argv)
	if strings.Contains(j, "apply -f -") {
		r.applied = append(r.applied, body)
	}
	r.mu.Unlock()
	switch {
	case strings.Contains(j, " get pod ") && strings.Contains(j, "status.podIP"):
		return execpkg.Result{Stdout: r.podIP, ExitCode: 0}, nil
	case strings.Contains(j, " get namespace "):
		return execpkg.Result{Stdout: r.nsLabel, ExitCode: 0}, nil
	default:
		return execpkg.Result{ExitCode: 0}, nil
	}
}

func (r *recordRunner) find(sub ...string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if containsAll(c, sub) {
			return c
		}
	}
	return nil
}

func (r *recordRunner) saw(sub ...string) bool { return r.find(sub...) != nil }

func (r *recordRunner) appliedContains(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, body := range r.applied {
		if strings.Contains(body, sub) {
			return true
		}
	}
	return false
}

// execCmds returns the `sh -c` payload of every `kubectl exec` call.
func (r *recordRunner) execCmds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		if cmd := shDashCPayload(c); cmd != "" {
			out = append(out, cmd)
		}
	}
	return out
}

// execCmdContaining returns the first exec payload containing every substring.
func (r *recordRunner) execCmdContaining(subs ...string) string {
	for _, cmd := range r.execCmds() {
		ok := true
		for _, s := range subs {
			if !strings.Contains(cmd, s) {
				ok = false
				break
			}
		}
		if ok {
			return cmd
		}
	}
	return ""
}

// shDashCPayload returns the argument after `sh -c` in an argv, or "".
func shDashCPayload(argv []string) string {
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] == "sh" && argv[i+1] == "-c" {
			return argv[i+2]
		}
	}
	return ""
}

func containsAll(argv, subs []string) bool {
	for _, s := range subs {
		found := false
		for _, a := range argv {
			if a == s {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
