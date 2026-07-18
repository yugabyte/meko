// Package dockerhost drives docker on a remote host through an
// exec.RemoteExecutor — no docker SDK and no local docker daemon. Every
// operation is a carefully quoted `docker …` command run over the executor.
package dockerhost

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// sortedKeys returns m's keys in deterministic order so generated commands are
// stable (and testable).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// shellQuote wraps s in single quotes, escaping embedded single quotes, so it is
// safe as one argument in a remote /bin/sh command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Host wraps a RemoteExecutor with docker-CLI helpers.
type Host struct {
	ex exec.RemoteExecutor
}

// New returns a Host driving docker over ex.
func New(ex exec.RemoteExecutor) *Host { return &Host{ex: ex} }

// PortMap publishes a container port on a host port.
type PortMap struct {
	Host      int
	Container int
}

// VolumeMount binds a host path (or named volume) to a container path.
type VolumeMount struct {
	Source string
	Target string
}

// ContainerSpec describes a container to run.
type ContainerSpec struct {
	Name        string
	Image       string
	Env         map[string]string
	Ports       []PortMap
	Volumes     []VolumeMount
	HostNetwork bool     // --network host (ignores Ports)
	Detach      bool     // -d
	ExtraArgs   []string // extra `docker run` flags (e.g. --add-host), already safe
	Command     []string
}

// Pull fetches an image.
func (h *Host) Pull(ctx context.Context, image string) error {
	return h.mustRun(ctx, fmt.Sprintf("docker pull %s", shellQuote(image)))
}

// RunContainer starts a container from spec and returns its id (or name).
func (h *Host) RunContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	args := []string{"docker", "run"}
	if spec.Detach {
		args = append(args, "-d")
	}
	if spec.Name != "" {
		args = append(args, "--name", shellQuote(spec.Name))
	}
	if spec.HostNetwork {
		args = append(args, "--network", "host")
	} else {
		for _, p := range spec.Ports {
			args = append(args, "-p", fmt.Sprintf("%d:%d", p.Host, p.Container))
		}
	}
	for _, k := range sortedKeys(spec.Env) {
		args = append(args, "-e", shellQuote(fmt.Sprintf("%s=%s", k, spec.Env[k])))
	}
	for _, v := range spec.Volumes {
		args = append(args, "-v", shellQuote(fmt.Sprintf("%s:%s", v.Source, v.Target)))
	}
	args = append(args, spec.ExtraArgs...)
	args = append(args, shellQuote(spec.Image))
	for _, c := range spec.Command {
		args = append(args, shellQuote(c))
	}
	res, err := h.ex.Run(ctx, strings.Join(args, " "))
	if err != nil {
		return "", fmt.Errorf("docker run %s: %w", spec.Name, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("docker run %s exited %d: %s", spec.Name, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if spec.Name != "" {
		return spec.Name, nil
	}
	return strings.TrimSpace(res.Stdout), nil
}

// StopRm stops and removes a container, best-effort (force, ignore missing).
func (h *Host) StopRm(ctx context.Context, name string) error {
	res, err := h.ex.Run(ctx, fmt.Sprintf("docker rm -f %s", shellQuote(name)))
	if err != nil {
		return fmt.Errorf("docker rm -f %s: %w", name, err)
	}
	if res.ExitCode != 0 && !strings.Contains(res.Stderr, "No such container") {
		return fmt.Errorf("docker rm -f %s exited %d: %s", name, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// Restart restarts a container in place (docker restart).
func (h *Host) Restart(ctx context.Context, name string) error {
	return h.mustRun(ctx, fmt.Sprintf("docker restart %s", shellQuote(name)))
}

// Logs returns up to tail lines of a container's logs (0 → all).
func (h *Host) Logs(ctx context.Context, name string, tail int) (string, error) {
	cmd := "docker logs"
	if tail > 0 {
		cmd += " --tail " + strconv.Itoa(tail)
	}
	cmd += " " + shellQuote(name)
	res, err := h.ex.Run(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("docker logs %s: %w", name, err)
	}
	// docker logs writes app stderr to the stream's stderr; merge both.
	return res.Stdout + res.Stderr, nil
}

// Exec runs a command inside a running container and returns its result.
func (h *Host) Exec(ctx context.Context, container string, cmd []string) (exec.Result, error) {
	args := []string{"docker", "exec", shellQuote(container)}
	for _, c := range cmd {
		args = append(args, shellQuote(c))
	}
	res, err := h.ex.Run(ctx, strings.Join(args, " "))
	if err != nil {
		return res, fmt.Errorf("docker exec %s: %w", container, err)
	}
	return res, nil
}

// Inspect reports whether a container exists and is running.
func (h *Host) Inspect(ctx context.Context, name string) (running bool, err error) {
	res, err := h.ex.Run(ctx, fmt.Sprintf("docker inspect -f {{.State.Running}} %s", shellQuote(name)))
	if err != nil {
		return false, fmt.Errorf("docker inspect %s: %w", name, err)
	}
	if res.ExitCode != 0 {
		// Non-zero means no such container — not an error, just not running.
		return false, nil
	}
	return strings.TrimSpace(res.Stdout) == "true", nil
}

// PollInterval is the cadence WaitHealthy polls at.
const PollInterval = 2 * time.Second

// WaitHealthy polls probe until it returns nil, ctx is cancelled, or the
// deadline passes. probe is an arbitrary readiness check (e.g. an in-container
// psql SELECT 1); ctx should carry the caller's deadline.
func (h *Host) WaitHealthy(ctx context.Context, name string, probe func(context.Context) error) error {
	var last error
	for {
		select {
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("wait healthy %s: %w (last probe: %v)", name, ctx.Err(), last)
			}
			return fmt.Errorf("wait healthy %s: %w", name, ctx.Err())
		default:
		}
		running, err := h.Inspect(ctx, name)
		if err != nil {
			return err
		}
		if running {
			if last = probe(ctx); last == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(PollInterval):
		}
	}
}

func (h *Host) mustRun(ctx context.Context, cmd string) error {
	res, err := h.ex.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("%s: %w", cmd, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s exited %d: %s", cmd, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}
