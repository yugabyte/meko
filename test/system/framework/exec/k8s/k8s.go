// Package k8s is the Kubernetes transport. It shells out to the ambient-authed
// `kubectl` binary — the same design choice as the docker transport shelling
// `docker` over SSH — rather than importing client-go (a very large dependency
// tree) into this standalone module. kubectl is itself a CLI client using the
// standard kubeconfig resolution, so ambient-auth "just works" and the local
// os/exec here is coordination only (issuing CLI calls to a remote cluster), not
// running any workload on the operator's machine.
//
// Kubectl provides cluster operations (Apply/Delete/Get/namespaces/wait/logs/
// port-forward). A Kubectl bound to a pod+container also implements
// exec.RemoteExecutor via `kubectl exec`, so it drops into the same env contract
// the SSH and docker transports satisfy.
package k8s

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	execpkg "github.com/yugabyte/yb-amp/test/system/framework/exec"
)

// MaxCapture bounds the stdout/stderr a single command buffers in memory.
const MaxCapture = 10 << 20 // 10 MiB

// Runner runs one kubectl/helm invocation to completion. stdin may be nil.
// Injected in tests to record argv and return canned output; the default shells
// out via os/exec.
type Runner func(ctx context.Context, stdin io.Reader, argv []string) (execpkg.Result, error)

// Process is a running long-lived command (port-forward). Injected in tests.
type Process interface {
	Stdout() io.Reader
	Kill() error
	Wait() error
}

// Starter launches a long-lived command and returns a Process. The default
// shells out via os/exec.
type Starter func(ctx context.Context, argv []string) (Process, error)

// Config describes how to reach a cluster (and optionally bind exec to a pod).
type Config struct {
	Context   string // kube-context; "" → current context
	Namespace string // "" → no -n flag
	Bin       string // "" → "kubectl"
	Pod       string // for exec: target pod ("" → Run/Copy/Fetch unusable)
	Container string // for exec: target container ("" → default container)
}

// Kubectl drives kubectl for one context/namespace (and optionally one pod).
type Kubectl struct {
	cfg     Config
	run     Runner
	startPF Starter
}

// New builds a Kubectl using the real kubectl binary.
func New(cfg Config) *Kubectl {
	if cfg.Bin == "" {
		cfg.Bin = "kubectl"
	}
	return &Kubectl{cfg: cfg, run: OSRun, startPF: osStart}
}

// WithRunner overrides the one-shot runner (tests inject a scripted recorder).
func (k *Kubectl) WithRunner(r Runner) *Kubectl { k.run = r; return k }

// WithStarter overrides the long-lived starter (tests inject a fake).
func (k *Kubectl) WithStarter(s Starter) *Kubectl { k.startPF = s; return k }

// ForPod returns a shallow copy bound to pod/container so it satisfies
// exec.RemoteExecutor (`kubectl exec`). The original is unchanged.
func (k *Kubectl) ForPod(pod, container string) *Kubectl {
	cp := *k
	cp.cfg.Pod = pod
	cp.cfg.Container = container
	return &cp
}

// Namespace is the configured namespace.
func (k *Kubectl) Namespace() string { return k.cfg.Namespace }

// Runner returns the one-shot runner (so a derived Kubectl can inherit an
// injected test runner).
func (k *Kubectl) Runner() Runner { return k.run }

// StarterFn returns the long-lived starter (so a derived Kubectl can inherit an
// injected test starter).
func (k *Kubectl) StarterFn() Starter { return k.startPF }

// base returns the kubectl argv prefix (binary + --context + -n).
func (k *Kubectl) base() []string {
	args := []string{k.cfg.Bin}
	if k.cfg.Context != "" {
		args = append(args, "--context", k.cfg.Context)
	}
	if k.cfg.Namespace != "" {
		args = append(args, "-n", k.cfg.Namespace)
	}
	return args
}

// exec runs a kubectl command (base + extra) with optional stdin and returns
// its result, erroring on a non-zero exit.
func (k *Kubectl) exec(ctx context.Context, stdin io.Reader, extra ...string) (execpkg.Result, error) {
	argv := append(k.base(), extra...)
	res, err := k.run(ctx, stdin, argv)
	if err != nil {
		return res, fmt.Errorf("kubectl %s: %w", strings.Join(extra, " "), err)
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("kubectl %s exited %d: %s", strings.Join(extra, " "), res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res, nil
}

// --- exec.RemoteExecutor (pod-bound) ---

// Run executes cmd inside the bound pod/container via `kubectl exec -- sh -c`.
func (k *Kubectl) Run(ctx context.Context, cmd string) (execpkg.Result, error) {
	if k.cfg.Pod == "" {
		return execpkg.Result{}, fmt.Errorf("k8s Run: no pod bound (use ForPod)")
	}
	extra := []string{"exec", k.cfg.Pod}
	if k.cfg.Container != "" {
		extra = append(extra, "-c", k.cfg.Container)
	}
	extra = append(extra, "--", "sh", "-c", cmd)
	argv := append(k.base(), extra...)
	res, err := k.run(ctx, nil, argv)
	if err != nil {
		return res, fmt.Errorf("kubectl exec %s: %w", k.cfg.Pod, err)
	}
	return res, nil
}

// Copy uploads localPath into the bound pod via `kubectl cp`.
func (k *Kubectl) Copy(ctx context.Context, localPath, remotePath string) error {
	if k.cfg.Pod == "" {
		return fmt.Errorf("k8s Copy: no pod bound (use ForPod)")
	}
	dst := k.podPath(remotePath)
	extra := []string{"cp", localPath, dst}
	if k.cfg.Container != "" {
		extra = append(extra, "-c", k.cfg.Container)
	}
	_, err := k.exec(ctx, nil, extra...)
	return err
}

// Fetch downloads remotePath from the bound pod to localPath via `kubectl cp`.
func (k *Kubectl) Fetch(ctx context.Context, remotePath, localPath string) error {
	if k.cfg.Pod == "" {
		return fmt.Errorf("k8s Fetch: no pod bound (use ForPod)")
	}
	src := k.podPath(remotePath)
	extra := []string{"cp", src, localPath}
	if k.cfg.Container != "" {
		extra = append(extra, "-c", k.cfg.Container)
	}
	_, err := k.exec(ctx, nil, extra...)
	return err
}

// podPath renders the "[ns/]pod:path" argument kubectl cp expects.
func (k *Kubectl) podPath(path string) string {
	if k.cfg.Namespace != "" {
		return k.cfg.Namespace + "/" + k.cfg.Pod + ":" + path
	}
	return k.cfg.Pod + ":" + path
}

// --- cluster operations ---

// Apply applies a manifest passed on stdin (`kubectl apply -f -`).
func (k *Kubectl) Apply(ctx context.Context, manifest string) error {
	_, err := k.exec(ctx, strings.NewReader(manifest), "apply", "-f", "-")
	return err
}

// Delete deletes a resource, tolerating absence.
func (k *Kubectl) Delete(ctx context.Context, kind, name string) error {
	_, err := k.exec(ctx, nil, "delete", kind, name, "--ignore-not-found")
	return err
}

// Get reads a field of a resource via jsonpath and returns the trimmed value.
func (k *Kubectl) Get(ctx context.Context, kind, name, jsonpath string) (string, error) {
	res, err := k.exec(ctx, nil, "get", kind, name, "-o", "jsonpath="+jsonpath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// Patch applies a patch to a resource (`kubectl patch <kind> <name> --type
// <patchType> -p <patch>`). patchType is one of strategic|merge|json; patch is
// the patch document (YAML or JSON, whichever kubectl accepts for the type).
func (k *Kubectl) Patch(ctx context.Context, kind, name, patchType, patch string) error {
	_, err := k.exec(ctx, nil, "patch", kind, name, "--type", patchType, "-p", patch)
	return err
}

// GetSelector reads a field of the first resource matching a label selector
// via jsonpath (`kubectl get <kind> -l <selector> -o jsonpath=<jsonpath>`),
// e.g. kind="pod", selector="app=amp-controller",
// jsonpath="{.items[0].status.podIP}".
func (k *Kubectl) GetSelector(ctx context.Context, kind, selector, jsonpath string) (string, error) {
	res, err := k.exec(ctx, nil, "get", kind, "-l", selector, "-o", "jsonpath="+jsonpath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// CreateNamespace applies a namespace with the given labels (idempotent apply).
func (k *Kubectl) CreateNamespace(ctx context.Context, name string, labels map[string]string) error {
	// Namespace apply targets the cluster, not a namespace, so drop any -n.
	nk := *k
	nk.cfg.Namespace = ""
	return nk.Apply(ctx, namespaceManifest(name, labels))
}

// DeleteNamespace deletes a namespace, tolerating absence.
func (k *Kubectl) DeleteNamespace(ctx context.Context, name string) error {
	nk := *k
	nk.cfg.Namespace = ""
	_, err := nk.exec(ctx, nil, "delete", "namespace", name, "--ignore-not-found")
	return err
}

// WaitFor blocks until a condition holds on a target (`kubectl wait`).
// target is "kind/name" (e.g. "statefulset/yugabyted"); condition is a
// --for value (e.g. "condition=Ready" or "jsonpath={.status.readyReplicas}=1").
func (k *Kubectl) WaitFor(ctx context.Context, target, condition string, timeout time.Duration) error {
	_, err := k.exec(ctx, nil, "wait", target, "--for="+condition, "--timeout="+durationArg(timeout))
	return err
}

// Logs returns up to tail lines of a pod's (or workload's) logs (0 → all).
func (k *Kubectl) Logs(ctx context.Context, target, container string, tail int) (string, error) {
	extra := []string{"logs", target}
	if container != "" {
		extra = append(extra, "-c", container)
	}
	if tail > 0 {
		extra = append(extra, "--tail", strconv.Itoa(tail))
	}
	res, err := k.exec(ctx, nil, extra...)
	if err != nil {
		return "", err
	}
	return res.Stdout + res.Stderr, nil
}

// LogsPrevious returns up to tail lines of a pod's PREVIOUS container instance
// (`kubectl logs --previous`) — the logs from before a crash/restart, which the
// live logs no longer show. It errors when the pod never restarted (no previous
// instance); callers treat that as a best-effort skip.
func (k *Kubectl) LogsPrevious(ctx context.Context, target, container string, tail int) (string, error) {
	extra := []string{"logs", target, "--previous"}
	if container != "" {
		extra = append(extra, "-c", container)
	}
	if tail > 0 {
		extra = append(extra, "--tail", strconv.Itoa(tail))
	}
	res, err := k.exec(ctx, nil, extra...)
	if err != nil {
		return "", err
	}
	return res.Stdout + res.Stderr, nil
}

// Describe returns `kubectl describe <kind> <name>` — the events and status a
// non-Running/Ready pod carries (ImagePullBackOff, CrashLoopBackOff, scheduling
// failures) that raw logs don't show.
func (k *Kubectl) Describe(ctx context.Context, kind, name string) (string, error) {
	res, err := k.exec(ctx, nil, "describe", kind, name)
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}

// Events returns the namespace's events sorted by last timestamp
// (`kubectl get events --sort-by=.lastTimestamp`).
func (k *Kubectl) Events(ctx context.Context) (string, error) {
	res, err := k.exec(ctx, nil, "get", "events", "--sort-by=.lastTimestamp")
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}

// PodState is a pod's coarse lifecycle state, enough to decide whether it is
// worth describing (a pod that is not Running-and-Ready is the interesting one).
type PodState struct {
	Name  string
	Phase string
	Ready bool
}

// PodStates returns every pod's name, phase, and Ready-condition status in one
// call, for diagnostics triage.
func (k *Kubectl) PodStates(ctx context.Context) ([]PodState, error) {
	const jsonpath = `{range .items[*]}{.metadata.name}{"\t"}{.status.phase}{"\t"}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}`
	res, err := k.exec(ctx, nil, "get", "pods", "-o", "jsonpath="+jsonpath)
	if err != nil {
		return nil, err
	}
	var states []PodState
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		st := PodState{Name: strings.TrimSpace(fields[0])}
		if len(fields) > 1 {
			st.Phase = strings.TrimSpace(fields[1])
		}
		if len(fields) > 2 {
			st.Ready = strings.EqualFold(strings.TrimSpace(fields[2]), "True")
		}
		if st.Name != "" {
			states = append(states, st)
		}
	}
	return states, nil
}

// ListPods returns the pod names in the namespace (`get pods -o name`, stripped
// of the "pod/" prefix).
func (k *Kubectl) ListPods(ctx context.Context) ([]string, error) {
	res, err := k.exec(ctx, nil, "get", "pods", "-o", "name")
	if err != nil {
		return nil, err
	}
	var pods []string
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pods = append(pods, strings.TrimPrefix(line, "pod/"))
	}
	return pods, nil
}

// PortForward starts `kubectl port-forward target <local>:remote` and returns
// once kubectl reports it is forwarding (or errors on early exit / ctx). A
// localPort <= 0 requests an ephemeral local port (kubectl's ":remote" form);
// the actual bound port is parsed from the readiness line and exposed via
// LocalPort — so parallel forwards never collide on a caller-chosen port.
//
// ctx bounds only the readiness handshake. The forwarded process itself is owned
// by the returned Handle and lives under its own (background-rooted) context: it
// is torn down solely by Handle.Close, never by ctx cancellation. This is
// deliberate — a forward routinely must outlive the Provision call that opened
// it (whose ctx carries a provisioning-only deadline), so binding the process to
// that ctx would kill live forwards the instant provisioning returned.
func (k *Kubectl) PortForward(ctx context.Context, target string, localPort, remotePort int) (*PortForward, error) {
	spec := fmt.Sprintf("%d:%d", localPort, remotePort)
	if localPort <= 0 {
		spec = fmt.Sprintf(":%d", remotePort) // let kubectl allocate an ephemeral local port
	}
	argv := append(k.base(), "port-forward", target, spec)
	procCtx, cancel := context.WithCancel(context.Background())
	proc, err := k.startPF(procCtx, argv)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start port-forward %s: %w", target, err)
	}
	pf := &PortForward{proc: proc, localPort: localPort, cancel: cancel, done: make(chan struct{})}

	ready := make(chan error, 1)
	go pf.watch(ready)

	select {
	case err := <-ready:
		if err != nil {
			pf.Close()
			return nil, fmt.Errorf("port-forward %s not ready: %w", target, err)
		}
		return pf, nil
	case <-ctx.Done():
		pf.Close()
		return nil, fmt.Errorf("port-forward %s cancelled: %w", target, ctx.Err())
	}
}

// PortForward is a running `kubectl port-forward` handle.
type PortForward struct {
	proc      Process
	localPort int
	cancel    context.CancelFunc
	done      chan struct{}
	dead      atomic.Bool
	closeOnce sync.Once
}

// LocalAddr is the loopback address callers connect to.
func (pf *PortForward) LocalAddr() string {
	return "127.0.0.1:" + strconv.Itoa(pf.localPort)
}

// LocalPort is the bound local port (the ephemeral one kubectl chose when an
// auto port was requested).
func (pf *PortForward) LocalPort() int { return pf.localPort }

// Alive reports whether the underlying kubectl process is still running. A dead
// forward's local port is refusing connections and must be re-established.
func (pf *PortForward) Alive() bool { return !pf.dead.Load() }

// Close terminates the forward. Safe to call repeatedly and concurrently.
func (pf *PortForward) Close() error {
	var err error
	pf.closeOnce.Do(func() {
		close(pf.done)
		pf.cancel()
		err = pf.proc.Kill()
	})
	return err
}

// watch reads kubectl's stdout for the "Forwarding from" readiness line (parsing
// the bound local port), keeps draining stdout so the pipe never blocks, and
// reaps the process on exit — flipping the handle to dead. It is the sole caller
// of proc.Wait.
func (pf *PortForward) watch(ready chan<- error) {
	sc := bufio.NewScanner(pf.proc.Stdout())
	signaled := false
	for sc.Scan() {
		if signaled {
			continue // drain remaining output
		}
		if strings.Contains(sc.Text(), "Forwarding from") {
			if port, ok := parseForwardPort(sc.Text()); ok {
				pf.localPort = port
			}
			signaled = true
			ready <- nil
		}
	}
	// Stream ended: the process is exiting. Reap it and mark the forward dead.
	waitErr := pf.proc.Wait()
	pf.dead.Store(true)
	if !signaled {
		if waitErr != nil {
			ready <- waitErr
		} else {
			ready <- fmt.Errorf("port-forward exited before forwarding")
		}
	}
}

// parseForwardPort extracts the local port from a kubectl readiness line like
// "Forwarding from 127.0.0.1:54123 -> 5432" (or an IPv6 "[::1]:54123 -> 5432").
func parseForwardPort(line string) (int, bool) {
	const marker = "Forwarding from "
	i := strings.Index(line, marker)
	if i < 0 {
		return 0, false
	}
	addr := line[i+len(marker):]
	if j := strings.Index(addr, " ->"); j >= 0 {
		addr = addr[:j]
	}
	_, portStr, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return 0, false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, false
	}
	return port, true
}

// --- os/exec-backed defaults ---

// OSRun is the default Runner: it shells out via os/exec with bounded capture.
func OSRun(ctx context.Context, stdin io.Reader, argv []string) (execpkg.Result, error) {
	if len(argv) == 0 {
		return execpkg.Result{}, fmt.Errorf("empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errb capBuffer
	out.limit = MaxCapture
	errb.limit = MaxCapture
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	res := execpkg.Result{
		Stdout:    out.String(),
		Stderr:    errb.String(),
		Truncated: out.truncated || errb.truncated,
	}
	if ctx.Err() != nil {
		res.ExitCode = -1
		return res, ctx.Err()
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		res.ExitCode = -1
		return res, err
	}
	res.ExitCode = 0
	return res, nil
}

// KubeconfigRunner returns the one-shot Runner for a given kubeconfig: an empty
// kubeconfig uses the shared ambient-authed OSRun; a set one shells each command
// with KUBECONFIG in the CHILD process env only (never global — parallel-safe),
// mirroring ybastack.ybaRunner / ybk8s.ybRunner. Shared so the cross-process
// reap paths do not each re-copy the same runner.
func KubeconfigRunner(kubeconfig string) Runner {
	if kubeconfig == "" {
		return OSRun
	}
	env := append(os.Environ(), "KUBECONFIG="+kubeconfig)
	return func(ctx context.Context, stdin io.Reader, argv []string) (execpkg.Result, error) {
		if len(argv) == 0 {
			return execpkg.Result{}, fmt.Errorf("empty argv")
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = env
		if stdin != nil {
			cmd.Stdin = stdin
		}
		var out, errb capBuffer
		out.limit = MaxCapture
		errb.limit = MaxCapture
		cmd.Stdout = &out
		cmd.Stderr = &errb
		err := cmd.Run()
		res := execpkg.Result{Stdout: out.String(), Stderr: errb.String(), Truncated: out.truncated || errb.truncated}
		if ctx.Err() != nil {
			res.ExitCode = -1
			return res, ctx.Err()
		}
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				res.ExitCode = ee.ExitCode()
				return res, nil
			}
			res.ExitCode = -1
			return res, err
		}
		return res, nil
	}
}

// osProc wraps os/exec.Cmd as a Process for PortForward.
type osProc struct {
	cmd *exec.Cmd
	out io.Reader
}

func osStart(ctx context.Context, argv []string) (Process, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout // kubectl writes the readiness line to stdout; merge anyway
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &osProc{cmd: cmd, out: stdout}, nil
}

func (p *osProc) Stdout() io.Reader { return p.out }
func (p *osProc) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}
func (p *osProc) Wait() error { return p.cmd.Wait() }

// --- helpers ---

// durationArg renders a timeout as a kubectl-friendly value (e.g. "300s").
func durationArg(d time.Duration) string {
	if d <= 0 {
		return "0"
	}
	return strconv.Itoa(int(d.Round(time.Second)/time.Second)) + "s"
}

// namespaceManifest builds a Namespace YAML with sorted labels for stable output.
func namespaceManifest(name string, labels map[string]string) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Namespace\nmetadata:\n")
	fmt.Fprintf(&b, "  name: %s\n", name)
	if len(labels) > 0 {
		b.WriteString("  labels:\n")
		for _, kv := range sortedKV(labels) {
			fmt.Fprintf(&b, "    %s: %q\n", kv[0], kv[1])
		}
	}
	return b.String()
}

// sortedKV returns m's entries sorted by key, for stable manifest output.
func sortedKV(m map[string]string) [][2]string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][2]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, [2]string{k, m[k]})
	}
	return out
}

// capBuffer is a bytes.Buffer capped at limit; once full it records truncation.
type capBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *capBuffer) String() string { return b.buf.String() }
