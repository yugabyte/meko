// Package clientpod is the client-pod mechanic (architecture §6): launching a
// workload image next to the environment under test, never on the
// coordinator. Two shapes share one interface — Docker (a detached container
// on a remote docker host) and K8s (a bare Pod on a cluster) — so a workload
// driver or test can address either uniformly. Stop always collects logs
// before killing (apps.StopWithCollect — the report_kill lesson).
package clientpod

import (
	"bufio"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/apps"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/dockerhost"
	"github.com/yugabyte/yb-amp/test/system/framework/exec"
	"github.com/yugabyte/yb-amp/test/system/framework/exec/k8s"
)

// Spec describes a client-pod workload to launch.
type Spec struct {
	Name    string
	Image   string
	Command []string
	Env     map[string]string
	// ImagePullSecrets (K8s shape only) names pre-existing docker-registry
	// Secrets the kubelet uses to pull a private Image (e.g. an amp/quay
	// workload image). Empty → the image is public.
	ImagePullSecrets []string
}

// ClientPod is the uniform lifecycle both shapes implement.
type ClientPod interface {
	Start(ctx context.Context, spec Spec) error
	// Follow reads the pod/container's current logs and calls cb once per
	// line. It is a snapshot follow (not a live stream) — sufficient for
	// progress polling from the coordinator without holding an open
	// connection; call it repeatedly to poll.
	Follow(ctx context.Context, cb func(line string)) error
	CollectInto(ctx context.Context, into apps.ArtifactSink) error
	// Stop always collects into before stopping (apps.StopWithCollect).
	Stop(ctx context.Context, into apps.ArtifactSink) error
}

func followLines(logs string, cb func(line string)) error {
	sc := bufio.NewScanner(strings.NewReader(logs))
	// Log lines can be long (workload progress dumps); default bufio.Scanner
	// caps at 64KiB per line, which is too easy to hit here.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		cb(sc.Text())
	}
	return sc.Err()
}

// --- Docker shape ---

// Docker runs a client-pod workload as a detached container on a docker host
// reached over a RemoteExecutor (env/docker's shape).
type Docker struct {
	host *dockerhost.Host
	name string
}

// NewDocker binds a client pod to a docker host.
func NewDocker(host *dockerhost.Host) *Docker { return &Docker{host: host} }

// Start launches spec as a detached container.
func (d *Docker) Start(ctx context.Context, spec Spec) error {
	if spec.Name == "" {
		return fmt.Errorf("clientpod docker: spec.Name is required")
	}
	d.name = spec.Name
	_, err := d.host.RunContainer(ctx, dockerhost.ContainerSpec{
		Name:    spec.Name,
		Image:   spec.Image,
		Detach:  true,
		Env:     spec.Env,
		Command: spec.Command,
	})
	if err != nil {
		return fmt.Errorf("start client pod %s: %w", spec.Name, err)
	}
	return nil
}

// Follow reads the container's current logs and calls cb per line.
func (d *Docker) Follow(ctx context.Context, cb func(line string)) error {
	logs, err := d.host.Logs(ctx, d.name, 0)
	if err != nil {
		return fmt.Errorf("follow %s: %w", d.name, err)
	}
	return followLines(logs, cb)
}

// CollectInto fetches the container's full logs into into.
func (d *Docker) CollectInto(ctx context.Context, into apps.ArtifactSink) error {
	logs, err := d.host.Logs(ctx, d.name, 0)
	if err != nil {
		return fmt.Errorf("collect %s: %w", d.name, err)
	}
	return into.Add(d.name+"/clientpod.log", "log", []byte(logs))
}

// Stop always collects logs first, then removes the container.
func (d *Docker) Stop(ctx context.Context, into apps.ArtifactSink) error {
	return apps.StopWithCollect(ctx, into, d.CollectInto, func(ctx context.Context) error {
		return d.host.StopRm(ctx, d.name)
	})
}

// --- K8s shape ---

// DefaultReadyTimeout bounds the pod-ready wait after apply.
const DefaultReadyTimeout = 2 * time.Minute

// K8s runs a client-pod workload as a bare Pod, labeled ampqa=1 (for the
// reaper), on a cluster reached through kubectl.
type K8s struct {
	kube         *k8s.Kubectl
	name         string
	ReadyTimeout time.Duration // 0 → DefaultReadyTimeout
}

// NewK8s binds a client pod to a cluster/namespace (kube is already scoped to
// both via its Config).
func NewK8s(kube *k8s.Kubectl) *K8s { return &K8s{kube: kube} }

// Start applies a bare Pod manifest for spec and waits until it is Ready.
func (k *K8s) Start(ctx context.Context, spec Spec) error {
	if spec.Name == "" {
		return fmt.Errorf("clientpod k8s: spec.Name is required")
	}
	k.name = spec.Name
	if err := k.kube.Apply(ctx, podManifest(spec)); err != nil {
		return fmt.Errorf("apply client pod %s: %w", spec.Name, err)
	}
	timeout := k.ReadyTimeout
	if timeout == 0 {
		timeout = DefaultReadyTimeout
	}
	if err := k.kube.WaitFor(ctx, "pod/"+spec.Name, "condition=Ready", timeout); err != nil {
		return fmt.Errorf("wait ready %s: %w", spec.Name, err)
	}
	return nil
}

// Exec returns a RemoteExecutor bound to the started pod, so a workload driver
// (sampleapp, benchbase) or an artifact deploy can run commands and copy files
// INTO the long-lived client pod. Valid only after Start (which sets the pod
// name); before that the returned executor's Run errors that no pod is bound.
func (k *K8s) Exec() exec.RemoteExecutor { return k.kube.ForPod(k.name, "") }

// Follow reads the pod's current logs and calls cb per line.
func (k *K8s) Follow(ctx context.Context, cb func(line string)) error {
	logs, err := k.kube.Logs(ctx, "pod/"+k.name, "", 0)
	if err != nil {
		return fmt.Errorf("follow %s: %w", k.name, err)
	}
	return followLines(logs, cb)
}

// CollectInto fetches the pod's full logs into into.
func (k *K8s) CollectInto(ctx context.Context, into apps.ArtifactSink) error {
	logs, err := k.kube.Logs(ctx, "pod/"+k.name, "", 0)
	if err != nil {
		return fmt.Errorf("collect %s: %w", k.name, err)
	}
	return into.Add(k.name+"/clientpod.log", "log", []byte(logs))
}

// Stop always collects logs first, then deletes the pod.
func (k *K8s) Stop(ctx context.Context, into apps.ArtifactSink) error {
	return apps.StopWithCollect(ctx, into, k.CollectInto, func(ctx context.Context) error {
		return k.kube.Delete(ctx, "pod", k.name)
	})
}

// podManifest is a minimal bare Pod carrying spec's image/command/env,
// labeled ampqa=1 so the reaper can find every client pod it ever launched.
func podManifest(spec Spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Pod\nmetadata:\n  name: %s\n  labels:\n    ampqa: \"1\"\n", spec.Name)
	b.WriteString("spec:\n  restartPolicy: Never\n")
	if len(spec.ImagePullSecrets) > 0 {
		b.WriteString("  imagePullSecrets:\n")
		for _, s := range spec.ImagePullSecrets {
			fmt.Fprintf(&b, "    - name: %s\n", s)
		}
	}
	b.WriteString("  containers:\n")
	fmt.Fprintf(&b, "    - name: %s\n      image: %s\n", spec.Name, spec.Image)
	if len(spec.Command) > 0 {
		b.WriteString("      command:\n")
		for _, c := range spec.Command {
			fmt.Fprintf(&b, "        - %q\n", c)
		}
	}
	if len(spec.Env) > 0 {
		b.WriteString("      env:\n")
		for _, k := range sortedKeys(spec.Env) {
			fmt.Fprintf(&b, "        - name: %s\n          value: %q\n", k, spec.Env[k])
		}
	}
	return b.String()
}

// sortedKeys returns m's keys in deterministic order so generated manifests
// are stable (and testable).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
