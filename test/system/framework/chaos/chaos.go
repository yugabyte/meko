// Package chaos is the fault-injection ("nemesis") engine: named, always-
// recoverable disruptions run against resources a test owns — YB pods on k8s or
// YB nodes on a VM/GCE host — while a workload runs. It mirrors the stress
// framework's src/apps/nemesis idea (network partition/slowdown/packet-loss,
// process kill/restart, pod/VM operations, plus a background NemesisContext that
// injects faults during a test) in Go, kept KISS.
//
// The engine knows nothing about the product beyond how to reach a target. All
// disruption is issued over the existing exec transports (exec/k8s for pods,
// exec/ssh for hosts) — no new dependencies. Two real target implementations
// exist (pod, host) and two real runtime pieces vary (ChaosAction, CloudVM), so
// those are interfaces per the framework's "interface only where two real
// implementations exist" rule; everything else is plain structs and functions.
//
// Every action obeys one invariant: it is ALWAYS recoverable, and Recover is
// guaranteed to run — even when Inject failed, and even when the run context is
// cancelled (recovery is rooted in context.Background(), mirroring the
// framework's teardown discipline). The NemesisLoop drives a seed-derived,
// reproducible sequence of actions and never touches a resource the test does
// not own.
package chaos

import (
	"context"
	"fmt"
	"strings"
	"time"

	execpkg "github.com/yugabyte/yb-amp/test/system/framework/exec"
	"github.com/yugabyte/yb-amp/test/system/framework/exec/k8s"
)

// Kind distinguishes the two real target backings.
type Kind int

const (
	// KindPod is a Kubernetes pod reached via exec/k8s (kubectl).
	KindPod Kind = iota
	// KindHost is a GCE/VM host reached via exec/ssh.
	KindHost
)

func (k Kind) String() string {
	switch k {
	case KindPod:
		return "pod"
	case KindHost:
		return "host"
	default:
		return "unknown"
	}
}

// ChaosAction is one recoverable disruption. Inject applies the fault to target;
// Recover reverses it. Recover MUST be idempotent and safe to call whether or
// not Inject succeeded — the loop always calls it, on its own background-rooted
// context, so a fault is never left applied. Analogous to a stress-framework
// Nemesis.act()/repair() pair.
type ChaosAction interface {
	Name() string
	Inject(ctx context.Context, target *Target) error
	Recover(ctx context.Context, target *Target) error
}

// CloudVM is the minimal instance stop/start contract the NodeStop action needs.
// The generic apps/cloud VM does not (yet) expose stop/start, so this is kept
// separate and small: a driver that grows the capability satisfies it
// structurally, and tests fake it directly. Rooted here so the chaos package
// depends on no cloud driver.
type CloudVM interface {
	Stop(ctx context.Context) error
	Start(ctx context.Context) error
	WaitRunning(ctx context.Context, timeout time.Duration) error
}

// Target is a resource a chaos action operates on. It resolves to a k8s pod
// (KindPod, with a namespace-scoped Kubectl for pod-level operations and a
// pod-bound exec) or an SSH host (KindHost, with an exec transport). Ownership
// is asserted by construction — only build targets for resources the test owns.
type Target struct {
	name      string
	kind      Kind
	exec      execpkg.RemoteExecutor // command transport (pod exec / ssh)
	kube      *k8s.Kubectl           // KindPod only: namespace-scoped, for delete/wait
	pod       string                 // KindPod only
	namespace string                 // KindPod only, for the ownership guard
	cloud     CloudVM                // optional: enables NodeStop
}

// NewPodTarget builds a pod target. kube is a namespace-scoped Kubectl; pod and
// container name the pod and (optionally) the container to exec into. The pod's
// namespace (from kube) is used by the loop's ownership guard.
func NewPodTarget(name string, kube *k8s.Kubectl, pod, container string) *Target {
	return &Target{
		name:      name,
		kind:      KindPod,
		exec:      kube.ForPod(pod, container),
		kube:      kube,
		pod:       pod,
		namespace: kube.Namespace(),
	}
}

// NewHostTarget builds a host target reached over the given exec transport
// (typically an *ssh.Client). The caller asserts the host is test-owned.
func NewHostTarget(name string, e execpkg.RemoteExecutor) *Target {
	return &Target{name: name, kind: KindHost, exec: e}
}

// WithCloud attaches a CloudVM so the NodeStop action can stop/start this
// target's instance. Returns the same target for chaining.
func (t *Target) WithCloud(vm CloudVM) *Target { t.cloud = vm; return t }

// Name is the target's logical name (used in logs/events).
func (t *Target) Name() string { return t.name }

// Kind reports whether the target is a pod or a host.
func (t *Target) Kind() Kind { return t.kind }

// run executes cmd on the target, prefixing sudo for hosts (pods usually exec as
// root; VMs run as an unprivileged login user). It errors on a non-zero exit so
// callers can surface a failed injection.
func (t *Target) run(ctx context.Context, cmd string) error {
	if t.kind == KindHost {
		cmd = "sudo " + cmd
	}
	res, err := t.exec.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("command %q exited %d: %s", cmd, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// runTolerant runs cmd but does not treat a non-zero exit as an error — used by
// Recover paths that must be idempotent (deleting a rule that may already be
// gone, tearing down a qdisc that may not exist).
func (t *Target) runTolerant(ctx context.Context, cmd string) error {
	if t.kind == KindHost {
		cmd = "sudo " + cmd
	}
	_, err := t.exec.Run(ctx, cmd)
	return err
}

// shellQuote wraps s in single quotes for safe use as one /bin/sh argument.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
