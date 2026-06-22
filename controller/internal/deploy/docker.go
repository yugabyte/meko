package deploy

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DockerConfig configures the Docker deployer.
type DockerConfig struct {
	// EndpointImage is the single-container endpoint image (deploy/endpoint).
	EndpointImage string
	// Network is a user-defined Docker network shared by the controller and the
	// endpoints it launches, so the storage controller can reach an endpoint's
	// pageserver/safekeeper by container name (DNS) — no published-port juggling.
	Network string
	// ExternalHost is the host a *client* uses to reach a published port (the
	// compute). For a host-run e2e this is 127.0.0.1.
	ExternalHost string
	// SupervisorPath, when set, bind-mounts a host supervisor.py over the
	// endpoint image's bundled one. Dev-only affordance for when the image
	// hasn't been rebuilt with supervisor changes baked in; the daemon resolves
	// the path on the Docker host. Leave empty in production / k8s.
	SupervisorPath string
	// ReadyTimeout bounds how long CreateEndpoint waits for the compute.
	ReadyTimeout time.Duration
}

// DockerDeployer runs endpoints as Docker containers via the `docker` CLI (over
// the daemon socket mounted into the controller container). It is the Docker
// sibling of the Helm-backed KubernetesDeployer.
type DockerDeployer struct {
	cfg    DockerConfig
	logger *slog.Logger
}

// NewDockerDeployer returns a Deployer backed by the local Docker daemon.
func NewDockerDeployer(cfg DockerConfig, logger *slog.Logger) *DockerDeployer {
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 3 * time.Minute
	}
	return &DockerDeployer{cfg: cfg, logger: logger}
}

var _ Deployer = (*DockerDeployer)(nil)

const (
	endpointComputePort = 55433
	// Internal endpoint ports. With a shared network the storage controller
	// reaches these by container name, so they need no host remapping.
	endpointPSHTTPPort = 9898
	endpointPSPGPort   = 64000
	endpointSKHTTPPort = 7676
	endpointSKPGPort   = 5454
)

func (d *DockerDeployer) CreateEndpoint(ctx context.Context, spec EndpointSpec) (Endpoint, error) {
	d.rmForce(context.Background(), spec.Name)

	args := []string{"run", "-d", "--name", spec.Name}
	if d.cfg.Network != "" {
		args = append(args, "--network", d.cfg.Network)
	}
	args = append(args,
		"--add-host", "host.docker.internal:host-gateway",
		"-p", fmt.Sprintf("0:%d", endpointComputePort),
	)
	if d.cfg.SupervisorPath != "" {
		args = append(args, "-v",
			d.cfg.SupervisorPath+":/usr/local/bin/yb-endpoint-supervisor:ro")
	}

	env := map[string]string{
		"YB_HOST_PORTS":       spec.YB.HostPorts,
		"YB_USER":             spec.YB.User,
		"YB_PASSWORD":         spec.YB.Password,
		"YB_DBNAME":           spec.YB.DBName,
		"TENANT_ID":           spec.TenantID,
		"TIMELINE_ID":         spec.TimelineID,
		"ENDPOINT_PG_VERSION": strconv.Itoa(spec.PgVersion),
		"BOOTSTRAP":           strconv.FormatBool(spec.Bootstrap),
	}
	// Branch: relay the ancestor so the endpoint forks it instead of initdb'ing.
	if spec.AncestorTimelineID != "" {
		env["ANCESTOR_TIMELINE_ID"] = spec.AncestorTimelineID
		env["ANCESTOR_LSN"] = spec.AncestorLSN
	}
	if spec.StorageControllerURL != "" {
		// SC-coordinated: the controller reaches this endpoint's services by its
		// container name on the shared network, at their internal ports.
		env["STORAGE_CONTROLLER_URL"] = spec.StorageControllerURL
		env["SC_REACHABLE_HOST"] = spec.Name
		env["DEV_PROVISION_SCHEMA"] = "false" // the controller already provisioned it
		env["PAGESERVER_HTTP_PUB_PORT"] = strconv.Itoa(endpointPSHTTPPort)
		env["PAGESERVER_PG_PUB_PORT"] = strconv.Itoa(endpointPSPGPort)
		env["SAFEKEEPER_HTTP_PUB_PORT"] = strconv.Itoa(endpointSKHTTPPort)
		env["SAFEKEEPER_PG_PUB_PORT"] = strconv.Itoa(endpointSKPGPort)
	} else {
		env["DEV_PROVISION_SCHEMA"] = "true" // SC-free: the endpoint provisions the schema
	}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, d.cfg.EndpointImage)

	if _, err := d.docker(ctx, args...); err != nil {
		// `docker run -d` can fail after the container was created; don't leave it.
		d.rmForce(context.Background(), spec.Name)
		return Endpoint{}, fmt.Errorf("docker run endpoint %s: %w", spec.Name, err)
	}

	hostPort, err := d.publishedPort(ctx, spec.Name, endpointComputePort)
	if err != nil {
		d.rmForce(context.Background(), spec.Name)
		return Endpoint{}, err
	}
	addr := fmt.Sprintf("%s:%d", d.cfg.ExternalHost, hostPort)

	if err := d.waitReady(ctx, spec.Name); err != nil {
		d.logger.Error("endpoint failed to become ready; tearing it down",
			"endpoint", spec.Name, "error", err, "logs_tail", d.logsTail(ctx, spec.Name, 40))
		d.rmForce(context.Background(), spec.Name)
		return Endpoint{}, err
	}
	d.logger.Info("endpoint ready", "endpoint", spec.Name, "address", addr)
	return Endpoint{ID: spec.Name, Address: addr}, nil
}

func (d *DockerDeployer) DeleteEndpoint(_ context.Context, id string) error {
	d.rmForce(context.Background(), id)
	return nil
}

// waitReady blocks until the endpoint's supervisor reports the compute is up and
// its password is set (the marker it logs once Postgres accepts remote auth).
func (d *DockerDeployer) waitReady(ctx context.Context, name string) error {
	// LOAD-BEARING CONTRACT: this string must stay in sync with the line the
	// endpoint's deploy/endpoint/supervisor.py logs once the compute is up and
	// its password is set. We wait on the password-set marker (not just a TCP
	// accept) because clients connect with md5 auth. If you change either side,
	// change both — otherwise this silently times out and tears the endpoint down.
	const marker = "set cloud_admin password for remote auth"
	deadline := time.Now().Add(d.cfg.ReadyTimeout)
	for time.Now().Before(deadline) {
		if strings.Contains(d.logsTail(ctx, name, 200), marker) {
			return nil
		}
		if !d.running(ctx, name) {
			return fmt.Errorf("endpoint %s exited before becoming ready", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("endpoint %s not ready within %s", name, d.cfg.ReadyTimeout)
}

func (d *DockerDeployer) publishedPort(ctx context.Context, name string, containerPort int) (int, error) {
	out, err := d.docker(ctx, "port", name, fmt.Sprintf("%d/tcp", containerPort))
	if err != nil {
		return 0, fmt.Errorf("docker port %s %d: %w", name, containerPort, err)
	}
	// e.g. "0.0.0.0:49153\n[::]:49153" — take the first line's port.
	line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	idx := strings.LastIndex(line, ":")
	if idx < 0 {
		return 0, fmt.Errorf("unexpected `docker port` output: %q", out)
	}
	port, err := strconv.Atoi(strings.TrimSpace(line[idx+1:]))
	if err != nil {
		return 0, fmt.Errorf("parsing published port from %q: %w", line, err)
	}
	return port, nil
}

func (d *DockerDeployer) running(ctx context.Context, name string) bool {
	out, err := d.docker(ctx, "inspect", "-f", "{{.State.Running}}", name)
	return err == nil && strings.TrimSpace(out) == "true"
}

func (d *DockerDeployer) logsTail(ctx context.Context, name string, n int) string {
	// Bound each `docker logs` call so a hung daemon can't stall waitReady's loop.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "logs", "--tail", strconv.Itoa(n), name).CombinedOutput()
	if err != nil {
		return string(out)
	}
	// Keep it bounded line-wise too.
	var b strings.Builder
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		b.WriteString(sc.Text())
		b.WriteByte('\n')
	}
	return b.String()
}

func (d *DockerDeployer) rmForce(_ context.Context, name string) {
	_ = exec.Command("docker", "rm", "-f", name).Run()
}

func (d *DockerDeployer) docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
