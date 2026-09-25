// Package k8s is the Kubernetes transport. It shells out to the ambient-authed
// `kubectl` binary — the same design choice as the ssh transport shelling its
// own client — rather than importing client-go (a very large dependency
// tree) into this standalone module. kubectl is itself a CLI client using the
// standard kubeconfig resolution, so ambient-auth "just works" and the local
// os/exec here is coordination only (issuing CLI calls to a remote cluster), not
// running any workload on the operator's machine.
//
// Kubectl provides cluster operations (Apply/Delete/Get/namespaces/wait/logs/
// port-forward). A Kubectl bound to a pod+container also implements
// exec.RemoteExecutor via `kubectl exec`, so it drops into the same env contract
// the SSH transport also satisfies.
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

	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
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
	logf    func(string, ...any) // nil → no command logging
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

// WithLogf enables debug logging of every kubectl command and its (truncated)
// output through f. nil (the default) logs nothing.
func (k *Kubectl) WithLogf(f func(string, ...any)) *Kubectl { k.logf = f; return k }

// maxLogTail bounds the per-stream output echoed into the debug log so a chatty
// command can't flood it — the full output still reaches callers via Result.
const maxLogTail = 2000

// CmdString renders an argv for a log line.
func CmdString(argv []string) string { return strings.Join(argv, " ") }

// LogTail formats a labeled, length-capped view of one output stream for the
// debug log; empty input yields "".
func LogTail(label, s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	if len(s) > maxLogTail {
		s = s[:maxLogTail] + fmt.Sprintf("…(+%d bytes)", len(s)-maxLogTail)
	}
	return fmt.Sprintf(" %s=%q", label, s)
}

// WithStarter overrides the long-lived starter (tests inject a fake).
func (k *Kubectl) WithStarter(s Starter) *Kubectl { k.startPF = s; return k }

// ForPod returns a shallow copy bound to pod/container so it satisfies
// exec.RemoteExecutor (`kubectl exec`). The original is unchanged.
// InNamespace returns a copy scoped to another namespace, for the occasional
// cross-namespace step (a co-located backing's pods live outside the control
// plane's namespace).
func (k *Kubectl) InNamespace(ns string) *Kubectl {
	cp := *k
	cp.cfg.Namespace = ns
	return &cp
}

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
	if k.logf != nil {
		k.logf("exec: %s", CmdString(argv))
	}
	res, err := k.run(ctx, stdin, argv)
	if k.logf != nil {
		k.logf("exec done (exit %d):%s%s", res.ExitCode, LogTail("out", res.Stdout), LogTail("err", res.Stderr))
	}
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
	return k.RunStdin(ctx, nil, cmd)
}

// RunStdin is Run with the command's stdin attached (`kubectl exec -i`).
func (k *Kubectl) RunStdin(ctx context.Context, stdin io.Reader, cmd string) (execpkg.Result, error) {
	if k.cfg.Pod == "" {
		return execpkg.Result{}, fmt.Errorf("k8s Run: no pod bound (use ForPod)")
	}
	extra := []string{"exec"}
	if stdin != nil {
		extra = append(extra, "-i")
	}
	extra = append(extra, k.cfg.Pod)
	if k.cfg.Container != "" {
		extra = append(extra, "-c", k.cfg.Container)
	}
	extra = append(extra, "--", "sh", "-c", cmd)
	argv := append(k.base(), extra...)
	res, err := k.run(ctx, stdin, argv)
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

// DeleteNow deletes a pod without a grace period: the container is killed
// outright instead of being asked to shut down. A graceful delete lets postgres
// terminate its connections and abort in-flight work tidily, which is the easy
// half of the fault; this is the other half.
func (k *Kubectl) DeleteNow(ctx context.Context, pod string) error {
	_, err := k.exec(ctx, nil, "delete", "pod", pod, "--ignore-not-found", "--force", "--grace-period=0")
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

// LabelNamespace sets one label on a namespace, overwriting any current value.
// Namespace-scoped commands carry -n; a namespace itself is cluster-scoped, so
// this drops it.
func (k *Kubectl) LabelNamespace(ctx context.Context, name, key, value string) error {
	nk := *k
	nk.cfg.Namespace = ""
	_, err := nk.exec(ctx, nil, "label", "namespace", name, fmt.Sprintf("%s=%s", key, value), "--overwrite")
	return err
}

// GetClusterScoped reads a cluster-scoped resource, dropping the -n that would name the namespace being asked about.
func (k *Kubectl) GetClusterScoped(ctx context.Context, kind, name, jsonpath string) (string, error) {
	nk := *k
	nk.cfg.Namespace = ""
	return nk.Get(ctx, kind, name, jsonpath)
}

// Label sets one label on a namespaced resource, overwriting any current value.
func (k *Kubectl) Label(ctx context.Context, kind, name, key, value string) error {
	_, err := k.exec(ctx, nil, "label", kind, name, fmt.Sprintf("%s=%s", key, value), "--overwrite")
	return err
}

// Unlabel removes one label from a namespaced resource, tolerating its absence.
func (k *Kubectl) Unlabel(ctx context.Context, kind, name, key string) error {
	res, err := k.exec(ctx, nil, "label", kind, name, key+"-")
	if err != nil && strings.Contains(res.Stderr, "not found") {
		return nil
	}
	return err
}

// AnnotateNamespace sets one annotation on a namespace, overwriting any current
// value. Annotations hold what a label value cannot — an RFC3339 timestamp's
// colons, for one.
func (k *Kubectl) AnnotateNamespace(ctx context.Context, name, key, value string) error {
	nk := *k
	nk.cfg.Namespace = ""
	_, err := nk.exec(ctx, nil, "annotate", "namespace", name, fmt.Sprintf("%s=%s", key, value), "--overwrite")
	return err
}

// Nodes returns every node's describe — conditions (MemoryPressure,
// DiskPressure, Ready), allocatable, and the pods each carries. Cluster-scoped,
// so it drops any -n.
//
// A pod that is Running and Ready with zero restarts on a starved node looks,
// from inside, exactly like the product hanging. This is what tells them apart.
func (k *Kubectl) Nodes(ctx context.Context) (string, error) {
	nk := *k
	nk.cfg.Namespace = ""
	res, err := nk.exec(ctx, nil, "describe", "nodes")
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}

// NodeUsage returns `kubectl top nodes` — actual CPU/memory use, as opposed to
// the requests `describe` reports. Needs metrics-server, so callers treat a
// failure as a skip rather than an error.
func (k *Kubectl) NodeUsage(ctx context.Context) (string, error) {
	nk := *k
	nk.cfg.Namespace = ""
	res, err := nk.exec(ctx, nil, "top", "nodes")
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
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

// RolloutStatus blocks until a workload's rollout completes (`kubectl rollout
// status <kind>/<name>`). It gates on the NEW ReplicaSet becoming ready, which a
// `condition=Available` wait does not: Available stays true on the old one for
// the whole rolling update.
func (k *Kubectl) RolloutStatus(ctx context.Context, kind, name string, timeout time.Duration) error {
	res, err := k.exec(ctx, nil, "rollout", "status", kind+"/"+name, "--timeout="+durationArg(timeout))
	if err != nil {
		return fmt.Errorf("%w%s", err, LogTail("out", res.Stdout))
	}
	return nil
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

// LogsAll returns up to tail lines per container of ALL of a pod's containers
// (`kubectl logs --all-containers --prefix`), so a multi-container pod's
// sidecars are captured too.
func (k *Kubectl) LogsAll(ctx context.Context, target string, tail int) (string, error) {
	return k.logsAll(ctx, target, tail, false)
}

// LogsAllPrevious is LogsAll for the PREVIOUS container instances (crash-loop
// evidence). It errors when no container of the pod ever restarted.
func (k *Kubectl) LogsAllPrevious(ctx context.Context, target string, tail int) (string, error) {
	return k.logsAll(ctx, target, tail, true)
}

func (k *Kubectl) logsAll(ctx context.Context, target string, tail int, previous bool) (string, error) {
	extra := []string{"logs", target, "--all-containers", "--prefix"}
	if previous {
		extra = append(extra, "--previous")
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

// Wide returns `kubectl get <kind> -o wide` — the at-a-glance listing
// (restarts, node, IP, images) diagnostics want alongside per-pod detail.
func (k *Kubectl) Wide(ctx context.Context, kind string) (string, error) {
	res, err := k.exec(ctx, nil, "get", kind, "-o", "wide")
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
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

// troubleMarkers are the describe lines that say why a pod is not running.
//
// "Startup probe failed" is here because a slow bootstrap no longer shows up as
// CrashLoopBackOff: the startupProbe suspends liveness while the compute is
// coming up (#960), so a pod that never finishes sits Running/0-1 with
// restartCount=0 and would otherwise return no marker at all.
var troubleMarkers = []string{"FailedScheduling", "Insufficient", "ImagePull", "CrashLoopBackOff",
	"FailedAttachVolume", "FailedMount", "OOMKilled", "Evicted", "untolerated taint",
	"Startup probe failed"}

// PodTrouble returns the most recent describe line that explains why a pod is
// not running. "Pending" on its own is unactionable — a report has to say
// whether it was capacity, an image, or a volume. Best-effort: "" when the
// describe fails or says nothing recognisable.
func (k *Kubectl) PodTrouble(ctx context.Context, pod string) string {
	out, err := k.Describe(ctx, "pod", pod)
	if err != nil {
		return ""
	}
	return pickTroubleLine(out)
}

func pickTroubleLine(describeOut string) string {
	var last string
	for _, line := range strings.Split(describeOut, "\n") {
		for _, m := range troubleMarkers {
			if strings.Contains(line, m) {
				last = strings.TrimSpace(line)
				break
			}
		}
	}
	if len(last) > 400 {
		last = last[:400] + "…"
	}
	return last
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
//
// UID and Restarts identify the pod ACROSS a window: a deleted pod comes back
// with Restarts reset to zero, and under a StatefulSet with the same name too,
// so only the UID separates "it was replaced" from "it never moved".
type PodState struct {
	Name     string
	Phase    string
	Ready    bool
	UID      string
	Restarts int
	// Unschedulable is the scheduler's PodScheduled=False message, e.g.
	// "0/47 nodes are available: 41 Insufficient cpu"; empty once placed.
	Unschedulable string
}

func (k *Kubectl) PodStates(ctx context.Context) ([]PodState, error) {
	const jsonpath = `{range .items[*]}{.metadata.name}{"\t"}{.status.phase}{"\t"}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\t"}{.metadata.uid}{"\t"}{range .status.containerStatuses[*]}{.restartCount}{","}{end}{"\t"}{range .status.conditions[?(@.type=="PodScheduled")]}{.reason}{"|"}{.message}{end}{"\n"}{end}`
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
		if len(fields) > 3 {
			st.UID = strings.TrimSpace(fields[3])
		}
		if len(fields) > 4 {
			for _, n := range strings.Split(fields[4], ",") {
				if v, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
					st.Restarts += v
				}
			}
		}
		if len(fields) > 5 {
			if reason, msg, ok := strings.Cut(strings.TrimSpace(fields[5]), "|"); ok && reason == "Unschedulable" {
				st.Unschedulable = strings.TrimSpace(msg)
			}
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
	pf := &PortForward{
		proc: proc, target: target, localPort: localPort, cancel: cancel,
		done: make(chan struct{}),
		logf: k.logf,
	}
	// Re-launch against the port kubectl actually bound, so the address every
	// caller already holds keeps working across a restart.
	pf.restart = func(rctx context.Context) (Process, error) {
		bound := pf.LocalPort()
		rspec := fmt.Sprintf("%d:%d", bound, remotePort)
		if bound <= 0 {
			rspec = fmt.Sprintf(":%d", remotePort)
		}
		return k.startPF(rctx, append(k.base(), "port-forward", target, rspec))
	}

	ready := make(chan error, 1)
	go pf.watch(ready)

	select {
	case err := <-ready:
		if err != nil {
			pf.Close()
			werr := fmt.Errorf("port-forward %s not ready: %w%s", target, err, pf.outputSuffix())
			if apiServerUnreachable(pf.Output()) {
				return nil, &APIServerUnreachableError{Err: werr}
			}
			return nil, werr
		}
		// An ephemeral request whose bound port could not be read leaves the handle
		// pointing at port 0. Callers would dial it and get a failure that names
		// neither kubectl nor the target, so fail here where both are known.
		if localPort <= 0 && pf.LocalPort() <= 0 {
			pf.Close()
			return nil, fmt.Errorf("port-forward %s reported ready without a local port%s", target, pf.outputSuffix())
		}
		return pf, nil
	case <-ctx.Done():
		pf.Close()
		return nil, fmt.Errorf("port-forward %s cancelled: %w%s", target, ctx.Err(), pf.outputSuffix())
	}
}

// PortForward is a running `kubectl port-forward` handle.
type PortForward struct {
	procMu    sync.Mutex // guards proc across a supervised restart
	proc      Process
	target    string
	localPort int
	cancel    context.CancelFunc
	done      chan struct{}
	dead      atomic.Bool
	ready     atomic.Bool
	closeOnce sync.Once
	portMu    sync.RWMutex

	// restart re-launches kubectl for this same target/ports. Set for every
	// forward the Kubectl factory builds; nil only in hand-built test handles,
	// which then behave exactly as before (one process, dead stays dead).
	restart    func(ctx context.Context) (Process, error)
	restarts   atomic.Int64
	restarting atomic.Bool
	logf       func(string, ...any)

	// out keeps the tail of what kubectl said. Everything that goes wrong with a
	// forward — "Unable to listen on port … address already in use", "lost
	// connection to pod", an expired credential — is said here and nowhere else,
	// and it used to be dropped on the floor: a dead forward reached the run as a
	// bare ECONNREFUSED with no cause attached.
	outMu sync.Mutex
	out   []string
}

// maxForwardOutput is how many of kubectl's most recent lines a handle keeps.
// The interesting line is always the last one or two.
const maxForwardOutput = 8

func (pf *PortForward) record(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	pf.outMu.Lock()
	pf.out = append(pf.out, line)
	if len(pf.out) > maxForwardOutput {
		pf.out = pf.out[len(pf.out)-maxForwardOutput:]
	}
	pf.outMu.Unlock()
}

// Output is the tail of kubectl's own output — the reason a forward failed to
// start or stopped working, when there is one.
func (pf *PortForward) Output() string {
	pf.outMu.Lock()
	defer pf.outMu.Unlock()
	return strings.Join(pf.out, "; ")
}

// outputSuffix renders Output for appending to an error message.
func (pf *PortForward) outputSuffix() string {
	if out := pf.Output(); out != "" {
		return " (kubectl: " + out + ")"
	}
	return ""
}

// Target is the kubectl target this forward was opened against.
func (pf *PortForward) Target() string { return pf.target }

// LocalAddr is the loopback address callers connect to.
func (pf *PortForward) LocalAddr() string {
	return "127.0.0.1:" + strconv.Itoa(pf.LocalPort())
}

// LocalPort is the bound local port (the ephemeral one kubectl chose when an
// auto port was requested). Read under a lock: the watcher goroutine sets it
// from kubectl's readiness line.
func (pf *PortForward) LocalPort() int {
	pf.portMu.RLock()
	defer pf.portMu.RUnlock()
	return pf.localPort
}

func (pf *PortForward) setLocalPort(port int) {
	pf.portMu.Lock()
	pf.localPort = port
	pf.portMu.Unlock()
}

// Alive reports whether the underlying kubectl process is still running. A dead
// forward's local port is refusing connections and must be re-established.
func (pf *PortForward) Alive() bool { return !pf.dead.Load() }

// Close terminates the forward. Safe to call repeatedly and concurrently.
func (pf *PortForward) Close() error {
	var err error
	pf.closeOnce.Do(func() {
		close(pf.done)
		pf.cancel()
		pf.procMu.Lock()
		proc := pf.proc
		pf.procMu.Unlock()
		err = proc.Kill()
	})
	return err
}

// watch reads kubectl's stdout for the "Forwarding from" readiness line (parsing
// the bound local port), keeps draining stdout so the pipe never blocks, and
// reaps the process on exit — flipping the handle to dead. It is the sole caller
// of proc.Wait.
func (pf *PortForward) watch(ready chan<- error) {
	for {
		exited := pf.readUntilExit(ready)
		if !pf.supervise(exited) {
			return
		}
	}
}

// supervise decides what happens after the kubectl process exits. A forward
// that was never ready, or one the caller Closed, is done. Otherwise the death
// is the failure this whole mechanism exists to absorb — kubectl port-forward
// does not reconnect on its own — so relaunch it, on the same local port, and
// keep the handle alive. Callers never observe the gap unless they dial during
// it, and their own retry covers that.
func (pf *PortForward) supervise(waitErr error) bool {
	if pf.restart == nil || !pf.ready.Load() {
		pf.dead.Store(true)
		return false
	}
	select {
	case <-pf.done:
		pf.dead.Store(true)
		return false
	default:
	}
	for attempt := 1; attempt <= portForwardRestartAttempts; attempt++ {
		select {
		case <-pf.done:
			pf.dead.Store(true)
			return false
		case <-time.After(portForwardRestartDelay):
		}
		procCtx, cancel := context.WithCancel(context.Background())
		proc, err := pf.restart(procCtx)
		if err != nil {
			cancel()
			pf.record(fmt.Sprintf("ampqa: port-forward %s restart attempt %d failed: %v", pf.target, attempt, err))
			continue
		}
		pf.procMu.Lock()
		pf.proc = proc
		oldCancel := pf.cancel
		pf.cancel = cancel
		pf.procMu.Unlock()
		oldCancel()
		pf.restarting.Store(false)
		n := pf.restarts.Add(1)
		pf.record(fmt.Sprintf("ampqa: port-forward %s died (%v); re-established on %s (restart %d)",
			pf.target, waitErr, pf.LocalAddr(), n))
		if pf.logf != nil {
			pf.logf("k8s: port-forward %s died (%v); re-established on %s (restart %d)",
				pf.target, waitErr, pf.LocalAddr(), n)
		}
		return true
	}
	pf.dead.Store(true)
	pf.record(fmt.Sprintf("ampqa: port-forward %s could not be re-established after %d attempts",
		pf.target, portForwardRestartAttempts))
	return false
}

// readUntilExit drains one kubectl process until its stdout closes, then reaps
// it and returns its wait error.
func (pf *PortForward) readUntilExit(ready chan<- error) error {
	pf.procMu.Lock()
	proc := pf.proc
	pf.procMu.Unlock()
	sc := bufio.NewScanner(proc.Stdout())
	for sc.Scan() {
		line := sc.Text()
		pf.record(line)
		if pf.ready.Load() {
			if forwardLostItsPod(line) {
				pf.killForRestart(proc, line)
			}
			continue // drain remaining output, keeping the tail for diagnostics
		}
		if strings.Contains(line, "Forwarding from") {
			if port, ok := parseForwardPort(line); ok {
				pf.setLocalPort(port)
			}
			pf.ready.Store(true)
			ready <- nil
		}
	}
	// Stream ended: the process is exiting. Reap it; supervise decides whether
	// this handle dies with it or gets a new process.
	waitErr := proc.Wait()
	if !pf.ready.Load() {
		if waitErr != nil {
			ready <- waitErr
		} else {
			waitErr = fmt.Errorf("port-forward exited before forwarding")
			ready <- waitErr
		}
	}
	return waitErr
}

// Restarts is how many times this forward has been re-established under the
// caller. Non-zero means the environment dropped it and the framework absorbed
// it — worth reporting, never worth failing a run over.
func (pf *PortForward) Restarts() int64 { return pf.restarts.Load() }

const (
	portForwardRestartAttempts = 10
	portForwardRestartDelay    = 500 * time.Millisecond
)

// forwardLostPodMarkers are what kubectl says about the FORWARD itself when its
// pod is gone: it stays up and keeps accepting, so every later request EOFs until
// a relaunch re-resolves the target.
var forwardLostPodMarkers = []string{
	"lost connection to pod",
	"error upgrading connection",
}

// forwardLostItsPod excludes kubectl's per-connection "an error occurred
// forwarding N -> M: ..." line first. The kubelet's reason lands inside it
// ("container not running (id)"), so matching on the reason would tear down a
// shared forward — and every other connection through it — over one dropped
// stream that the caller's own retry already covers.
func forwardLostItsPod(line string) bool {
	lower := strings.ToLower(line)
	if strings.Contains(lower, "an error occurred forwarding") {
		return false
	}
	for _, m := range forwardLostPodMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// killForRestart ends the current kubectl so watch/supervise relaunches it.
func (pf *PortForward) killForRestart(proc Process, line string) {
	if !pf.restarting.CompareAndSwap(false, true) {
		return
	}
	pf.record(fmt.Sprintf("ampqa: port-forward %s lost its pod (%s); relaunching", pf.target, strings.TrimSpace(line)))
	if pf.logf != nil {
		pf.logf("k8s: port-forward %s lost its pod (%s); relaunching", pf.target, strings.TrimSpace(line))
	}
	_ = proc.Kill()
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
