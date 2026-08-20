package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// DockerVoyagerConfig configures Voyager containers launched through the Docker
// CLI. DockerBin defaults to "docker".
type DockerVoyagerConfig struct {
	Image              string
	Network            string
	DBConnectionString string
	DockerBin          string
}

// DockerVoyagerJobRunner materializes Voyager runs as containers backed by a
// deterministic named volume.
type DockerVoyagerJobRunner struct {
	cfg DockerVoyagerConfig
}

// NewDockerVoyagerJobRunner constructs the Docker implementation.
func NewDockerVoyagerJobRunner(cfg DockerVoyagerConfig) *DockerVoyagerJobRunner {
	if cfg.DockerBin == "" {
		cfg.DockerBin = "docker"
	}
	return &DockerVoyagerJobRunner{cfg: cfg}
}

var _ VoyagerJobRunner = (*DockerVoyagerJobRunner)(nil)

func (r *DockerVoyagerJobRunner) StartRun(ctx context.Context, spec VoyagerRunSpec) (RunRef, error) {
	job, err := buildVoyagerJobSpec(spec, r.cfg.DBConnectionString)
	if err != nil {
		return RunRef{}, err
	}
	if r.cfg.Image == "" {
		return RunRef{}, errors.New("Docker Voyager image is required")
	}

	// `docker volume create NAME` is create-if-absent and preserves the first
	// creation's settings, which gives assess and migrate the same export data.
	if result, err := r.docker(ctx, "volume", "create", job.Volume); err != nil {
		// This command has no credential-bearing arguments, so stderr is safe
		// to retain in the returned error.
		return RunRef{}, fmt.Errorf("create Docker Voyager volume: %w", dockerCommandError(err, result.stderr))
	}

	if id, ok := r.containerID(ctx, job.Name); ok {
		return r.runRef(id, job), nil
	}

	args := []string{"run", "-d", "--name", job.Name}
	if r.cfg.Network != "" {
		args = append(args, "--network", r.cfg.Network)
	}
	args = append(args, "--mount", "type=volume,src="+job.Volume+",dst="+voyagerExportDir)
	for _, env := range job.Env {
		args = append(args, "--env", env.Name+"="+env.Value)
	}
	args = append(args, "--entrypoint", "/bin/bash", r.cfg.Image, "-c", job.Command)

	result, err := r.docker(ctx, args...)
	if err == nil && strings.TrimSpace(result.stdout) != "" {
		return r.runRef(strings.TrimSpace(result.stdout), job), nil
	}

	// A concurrent replay can win between inspect and run. Never surface Docker
	// stderr here: command arguments contain the source and target passwords.
	if existingID, ok := r.containerID(ctx, job.Name); ok {
		return r.runRef(existingID, job), nil
	}
	return RunRef{}, errors.New("start Docker Voyager container")
}

func (r *DockerVoyagerJobRunner) RunStatus(ctx context.Context, ref RunRef) (RunState, error) {
	if ref.DeployerType != VoyagerDeployerDocker || ref.ContainerID == "" {
		return "", errors.New("invalid Docker Voyager run reference")
	}
	result, err := r.docker(ctx, "container", "inspect", "--format", "{{json .State}}", ref.ContainerID)
	if err != nil {
		// This inspect command has no credentials, so its stderr is safe to
		// include for diagnosis.
		return "", fmt.Errorf("inspect Docker Voyager container: %w", dockerCommandError(err, result.stderr))
	}
	return parseDockerContainerState([]byte(strings.TrimSpace(result.stdout)))
}

func (r *DockerVoyagerJobRunner) runRef(id string, job voyagerJobSpec) RunRef {
	return RunRef{
		DeployerType: VoyagerDeployerDocker,
		ContainerID:  id,
		Volume:       job.Volume,
	}
}

func (r *DockerVoyagerJobRunner) containerID(ctx context.Context, name string) (string, bool) {
	result, err := r.docker(ctx, "container", "inspect", "--format", "{{.Id}}", name)
	id := strings.TrimSpace(result.stdout)
	return id, err == nil && id != ""
}

type dockerCommandResult struct {
	stdout string
	stderr string
}

// docker captures both command streams without embedding arguments in its
// errors. Callers decide whether stderr is safe to surface: docker run carries
// credential-bearing environment values and must discard it, while volume and
// inspect commands can include it for diagnosis.
func (r *DockerVoyagerJobRunner) docker(ctx context.Context, args ...string) (dockerCommandResult, error) {
	cmd := exec.CommandContext(ctx, r.cfg.DockerBin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return dockerCommandResult{stdout: stdout.String(), stderr: stderr.String()}, ctx.Err()
		}
		return dockerCommandResult{stdout: stdout.String(), stderr: stderr.String()}, errors.New("Docker command failed")
	}
	return dockerCommandResult{stdout: stdout.String(), stderr: stderr.String()}, nil
}

func dockerCommandError(err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, stderr)
}

func parseDockerContainerState(body []byte) (RunState, error) {
	var state struct {
		Status   string `json:"Status"`
		Running  bool   `json:"Running"`
		ExitCode int    `json:"ExitCode"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return "", errors.New("decode Docker Voyager container state")
	}
	if state.Running {
		return RunStateRunning, nil
	}
	switch state.Status {
	case "created":
		return RunStatePending, nil
	case "running", "restarting", "paused":
		return RunStateRunning, nil
	case "exited":
		if state.ExitCode == 0 {
			return RunStateSucceeded, nil
		}
		return RunStateFailed, nil
	case "dead", "removing":
		return RunStateFailed, nil
	default:
		return RunStatePending, nil
	}
}
