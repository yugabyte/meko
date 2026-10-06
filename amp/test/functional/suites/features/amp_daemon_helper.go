package features

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// The supervisor respawns a dead child after a 2s backoff; a pageserver or
// safekeeper that is not back inside this has not been respawned in place.
const daemonRespawnTimeout = 2 * time.Minute

// endpointDaemon is one supervised child inside a known endpoint pod.
type endpointDaemon struct {
	kube     *k8s.Kubectl
	pod      string
	uid      string
	comm     string
	restarts string
}

func (f *flow) endpointDaemon(ctx context.Context, ep controller.Endpoint, comm string) (*endpointDaemon, error) {
	kube := f.ampKube()
	if kube == nil {
		return nil, fmt.Errorf("%w: the %s process lives inside endpoint %s's pod and is reachable only by pod exec", errNoKubeContext, comm, ep.ID)
	}
	pod, err := (&kubeComputeRoller{kube: kube}).podName(ctx, ep.ID)
	if err != nil {
		return nil, err
	}
	uid, err := kube.Get(ctx, "pod", pod, "{.metadata.uid}")
	if err != nil {
		return nil, fmt.Errorf("read the uid of pod %s: %w", pod, err)
	}
	d := &endpointDaemon{kube: kube, pod: pod, uid: strings.TrimSpace(uid), comm: comm}
	if d.restarts, err = d.containerRestarts(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *endpointDaemon) containerRestarts(ctx context.Context) (string, error) {
	n, err := d.kube.Get(ctx, "pod", d.pod, fmt.Sprintf(`{.status.containerStatuses[?(@.name==%q)].restartCount}`, endpointPodContainer))
	if err != nil {
		return "", fmt.Errorf("read the %s container's restart count in pod %s: %w", endpointPodContainer, d.pod, err)
	}
	return strings.TrimSpace(n), nil
}

func (d *endpointDaemon) exec() *k8s.Kubectl { return d.kube.ForPod(d.pod, endpointPodContainer) }

func (d *endpointDaemon) pids(ctx context.Context) ([]int, error) {
	res, err := d.exec().Run(ctx, fmt.Sprintf(
		`for p in /proc/[0-9]*; do [ "$(cat $p/comm 2>/dev/null)" = %q ] && echo "${p#/proc/}"; done; exit 0`, d.comm))
	if err != nil {
		return nil, fmt.Errorf("list %s processes in pod %s: %w", d.comm, d.pod, err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("list %s processes in pod %s exited %d: %s", d.comm, d.pod, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	var out []int
	for _, f := range strings.Fields(res.Stdout) {
		if n, err := strconv.Atoi(f); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out, nil
}

func (d *endpointDaemon) signal(ctx context.Context, sig string, pids []int) error {
	parts := make([]string, len(pids))
	for i, p := range pids {
		parts[i] = strconv.Itoa(p)
	}
	res, err := d.exec().Run(ctx, "kill -"+sig+" "+strings.Join(parts, " "))
	if err != nil {
		return fmt.Errorf("send SIG%s to %s pid(s) %v in pod %s: %w", sig, d.comm, pids, d.pod, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("send SIG%s to %s pid(s) %v in pod %s exited %d: %s", sig, d.comm, pids, d.pod, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// running returns the daemon's pids, failing when there are none: every fault
// below needs a live process to aim at.
func (d *endpointDaemon) running(ctx context.Context) ([]int, error) {
	pids, err := d.pids(ctx)
	if err != nil {
		return nil, err
	}
	if len(pids) == 0 {
		return nil, fmt.Errorf("no %s process is running in pod %s, though the endpoint supervisor keeps one up", d.comm, d.pod)
	}
	return pids, nil
}

// kill SIGKILLs the given pids and returns once they are gone: the shell's exit
// code says the signal was sent, not that anything died.
func (d *endpointDaemon) kill(ctx context.Context, pids []int) error {
	if err := d.signal(ctx, "KILL", pids); err != nil {
		return err
	}
	err := pollUntil(ctx, 30*time.Second, time.Second, func() (bool, error) {
		now, err := d.pids(ctx)
		if err != nil {
			return false, err
		}
		return !slices.ContainsFunc(now, func(p int) bool { return slices.Contains(pids, p) }), nil
	})
	if err != nil {
		return fmt.Errorf("SIGKILL to %s pid(s) %v in pod %s: %w", d.comm, pids, d.pod, err)
	}
	return nil
}

// respawned waits for a new pid and requires the same pod and container: a
// supervisor that gave up is a different, weaker recovery.
func (d *endpointDaemon) respawned(ctx context.Context, old []int) ([]int, error) {
	var fresh []int
	err := pollUntil(ctx, daemonRespawnTimeout, time.Second, func() (bool, error) {
		now, err := d.pids(ctx)
		if err != nil {
			return false, nil
		}
		fresh = slices.DeleteFunc(now, func(p int) bool { return slices.Contains(old, p) })
		return len(fresh) > 0, nil
	})
	if err != nil {
		return nil, fmt.Errorf("the endpoint supervisor did not respawn %s in pod %s after its death: %w", d.comm, d.pod, err)
	}
	uid, err := d.kube.Get(ctx, "pod", d.pod, "{.metadata.uid}")
	if err != nil {
		return nil, fmt.Errorf("pod %s is gone after %s died: the supervisor should respawn a child in place, not hand the pod back to the kubelet: %w", d.pod, d.comm, err)
	}
	if strings.TrimSpace(uid) != d.uid {
		return nil, fmt.Errorf("pod %s was recreated (uid %s, was %s) after one %s death: the supervisor should respawn a child in place", d.pod, strings.TrimSpace(uid), d.uid, d.comm)
	}
	// A supervisor past its restart budget exits, and the kubelet restarts the
	// container in the same pod: new pids, same uid, but not a respawn.
	restarts, err := d.containerRestarts(ctx)
	if err != nil {
		return nil, err
	}
	if restarts != d.restarts {
		return nil, fmt.Errorf("the %s container in pod %s restarted (restartCount %s, was %s) after one %s death: the supervisor exited instead of respawning the child", endpointPodContainer, d.pod, restarts, d.restarts, d.comm)
	}
	return fresh, nil
}
