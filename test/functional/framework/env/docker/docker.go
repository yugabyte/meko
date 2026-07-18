// Package docker is the single-container amp environment provider
// (architecture §5.3): it drives a REMOTE docker host over SSH to run the
// endpoint image (which packs the whole data path) plus a YugabyteDB container,
// waits until Postgres answers on the compute port, and tears the per-test
// containers down. Nothing runs on the operator's machine.
package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/dockerhost"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	sshx "github.com/yugabyte/amp/test/functional/framework/exec/ssh"
)

// Default ports and images.
const (
	DefaultComputePortBase = 55433
	DefaultYBPortBase      = 5433
	DefaultYBImage         = "yugabytedb/yugabyte:latest"
	computePassword        = "cloud_admin"
	computeUser            = "cloud_admin"
	computeDB              = "postgres"
	ybUser                 = "yugabyte"
	// computeReadyTimeout bounds waiting for the endpoint's Postgres to serve
	// again after a compute restart.
	computeReadyTimeout = 3 * time.Minute
	// hostGateway lets the endpoint container reach the docker host's published
	// YB port as host.docker.internal.
	hostGateway = "host.docker.internal:host-gateway"
)

// Config is the env.docker.* configuration.
type Config struct {
	Host            string
	SSHUser         string
	SSHKey          string
	SSHPort         int
	EndpointImage   string
	YBImage         string
	ComputePortBase int
	YBPortBase      int
	// YBMasterGflags / YBTserverGflags are CSV flag=value lists forwarded to
	// yugabyted's --master_flags / --tserver_flags (env.docker.yb_gflags
	// feeds both sides).
	YBMasterGflags  string
	YBTserverGflags string
	YBShared        bool   // one YB across tests on the host, else fresh per test
	NameToken       string // uniquifier so parallel runs on one host don't collide
}

// yugabytedCommand is the container start command, with optional extra
// master/tserver gflags (CSV flag=value lists).
func yugabytedCommand(masterGflags, tserverGflags string) []string {
	cmd := []string{"bin/yugabyted", "start", "--background=false"}
	if masterGflags != "" {
		cmd = append(cmd, "--master_flags="+masterGflags)
	}
	if tserverGflags != "" {
		cmd = append(cmd, "--tserver_flags="+tserverGflags)
	}
	return cmd
}

// joinGflags merges CSV flag=value lists, dropping empty parts.
func joinGflags(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ",")
}

// FromConfig reads the env.docker.* namespace from a framework Config.
func FromConfig(c *core.Config) Config {
	cfg := Config{
		Host:            c.GetString("env.docker.host", ""),
		SSHUser:         c.GetString("env.docker.ssh_user", ""),
		SSHKey:          c.GetString("env.docker.ssh_key", ""),
		SSHPort:         c.GetInt("env.docker.ssh_port", 0),
		EndpointImage:   c.GetString("env.docker.endpoint_image", ""),
		YBImage:         c.GetString("env.docker.yb_image", DefaultYBImage),
		ComputePortBase: c.GetInt("env.docker.compute_port_base", DefaultComputePortBase),
		YBPortBase:      c.GetInt("env.docker.yb_port_base", DefaultYBPortBase),
		YBMasterGflags: joinGflags(c.GetString("env.docker.yb_gflags", ""),
			c.GetString("env.docker.yb_master_gflags", "")),
		YBTserverGflags: joinGflags(c.GetString("env.docker.yb_gflags", ""),
			c.GetString("env.docker.yb_tserver_gflags", "")),
		YBShared:  c.GetBool("env.docker.yb_shared", true),
		NameToken: c.GetString("env.docker.name_token", ""),
	}
	return cfg
}

// Configured reports whether a docker host is set (used to gate test selection).
func (c Config) Configured() bool { return c.Host != "" }

// transport is what the provider needs from a connection: command execution
// plus a Close. *ssh.Client satisfies it; tests inject a scripted fake.
type transport interface {
	exec.RemoteExecutor
	Close() error
}

// Provider provisions single-container amp environments on a remote docker host.
type Provider struct {
	cfg     Config
	counter atomic.Int64
	dial    func(ctx context.Context, cfg Config) (transport, error)
	logf    func(format string, args ...any)

	// detectProduct is injected in tests (a canned result, no real dial); New
	// wires the real SELECT-version()-over-pg implementation.
	detectProduct func(ctx context.Context, ci pg.ConnInfo) string
}

// New builds a Provider from cfg, dialing over SSH.
func New(cfg Config) *Provider {
	if cfg.ComputePortBase == 0 {
		cfg.ComputePortBase = DefaultComputePortBase
	}
	if cfg.YBPortBase == 0 {
		cfg.YBPortBase = DefaultYBPortBase
	}
	if cfg.YBImage == "" {
		cfg.YBImage = DefaultYBImage
	}
	if cfg.NameToken == "" {
		cfg.NameToken = randToken()
	}
	return &Provider{
		cfg:           cfg,
		dial:          sshDial,
		logf:          log.Printf,
		detectProduct: defaultDetectProductVersion,
	}
}

func sshDial(ctx context.Context, cfg Config) (transport, error) {
	return sshx.Dial(ctx, sshx.Config{
		Host:    cfg.Host,
		Port:    cfg.SSHPort,
		User:    cfg.SSHUser,
		KeyPath: cfg.SSHKey,
	})
}

func (p *Provider) log(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}

// allocation is the per-test set of distinct names/ports. Allocation is
// counter-driven, so it is collision-free by construction across parallel tests.
type allocation struct {
	index        int
	endpointName string
	ybName       string
	computePort  int
	ybPort       int
}

func (p *Provider) alloc() allocation {
	i := int(p.counter.Add(1) - 1)
	a := allocation{
		index:        i,
		endpointName: fmt.Sprintf("ampqa-ep-%s-%d", p.cfg.NameToken, i),
		computePort:  p.cfg.ComputePortBase + i,
	}
	if p.cfg.YBShared {
		a.ybName = fmt.Sprintf("ampqa-yb-%s-shared", p.cfg.NameToken)
		a.ybPort = p.cfg.YBPortBase
	} else {
		a.ybName = fmt.Sprintf("ampqa-yb-%s-%d", p.cfg.NameToken, i)
		a.ybPort = p.cfg.YBPortBase + i + 1
	}
	return a
}

// Provision connects to the docker host, ensures YugabyteDB is up, starts the
// endpoint with DEV_PROVISION_SCHEMA, and waits until psql answers.
func (p *Provider) Provision(ctx context.Context, spec core.EnvSpec, seed *core.Seed) (core.Environment, error) {
	if p.cfg.EndpointImage == "" {
		return nil, fmt.Errorf("env.docker.endpoint_image is required")
	}
	a := p.alloc()
	tr, err := p.dial(ctx, p.cfg)
	if err != nil {
		return nil, fmt.Errorf("connect docker host %s: %w", p.cfg.Host, err)
	}
	host := dockerhost.New(tr)

	// From the first container this call STARTS, any later failure must not leak
	// it: fail() StopRm's every container this call started (most recent first),
	// then closes the transport. A shared YB that was ALREADY running is never
	// tracked, so it is never torn down by a rollback.
	var started []string
	mark := func(name string) { started = append(started, name) }
	fail := func(e error) (core.Environment, error) {
		for i := len(started) - 1; i >= 0; i-- {
			if serr := host.StopRm(ctx, started[i]); serr != nil {
				p.log("docker env: rollback StopRm %s after provision failure (best-effort): %v", started[i], serr)
			}
		}
		_ = tr.Close()
		return nil, e
	}

	if err := p.ensureYB(ctx, host, a, mark); err != nil {
		return fail(err)
	}

	ybHostPorts := fmt.Sprintf("host.docker.internal:%d", a.ybPort)
	if _, err := host.RunContainer(ctx, dockerhost.ContainerSpec{
		Name:      a.endpointName,
		Image:     p.cfg.EndpointImage,
		Detach:    true,
		ExtraArgs: []string{"--add-host", hostGateway},
		Ports:     []dockerhost.PortMap{{Host: a.computePort, Container: a.computePort}},
		Env: map[string]string{
			"YB_HOST_PORTS":        ybHostPorts,
			"YB_USER":              ybUser,
			"DEV_PROVISION_SCHEMA": "true",
			"COMPUTE_PORT":         fmt.Sprintf("%d", a.computePort),
			"COMPUTE_PASSWORD":     computePassword,
		},
	}); err != nil {
		return fail(err)
	}
	mark(a.endpointName)

	if err := host.WaitHealthy(ctx, a.endpointName, func(ctx context.Context) error {
		return p.probeCompute(ctx, host, a)
	}); err != nil {
		return fail(fmt.Errorf("endpoint %s never became ready: %w", a.endpointName, err))
	}

	computePG := pg.ConnInfo{
		Host:     p.cfg.Host,
		Port:     a.computePort,
		User:     computeUser,
		Password: computePassword,
		Database: computeDB,
	}
	// Live-detect the amp product build from the endpoint's own compute
	// Postgres (there is no amp controller in this shape at all, so the image
	// tag is the only static fallback); fall back to the tag when the compute
	// Postgres isn't reachable yet or its response isn't a bare version.
	product := imageTag(p.cfg.EndpointImage)
	productLive := false
	if v := p.detectProduct(ctx, computePG); v != "" {
		product, productLive = v, true
	}
	ybVersion := p.detectYBVersion(ctx, host, a.ybName)
	p.log("docker env: versions detected — product=%q (live=%v) yb=%q (live=%v)", product, productLive, ybVersion, ybVersion != "")

	env := &Env{
		transport:    tr,
		host:         host,
		endpointName: a.endpointName,
		ybName:       a.ybName,
		ybShared:     p.cfg.YBShared,
		versions: core.DetectedVersions{
			Product: product,
			YB:      ybVersion,
		},
		pg:        computePG,
		waitReady: pg.WaitReady,
		handles: core.EnvHandles{
			Endpoint:   a.endpointName,
			Containers: []string{a.endpointName, a.ybName},
		},
	}
	return env, nil
}

// ensureYB starts YugabyteDB when needed: always fresh in per-test mode, or when
// the shared container is not already running. mark records a container this
// call actually STARTED so Provision's rollback tears it down on a later failure
// — a shared YB that was already running is not marked (it must survive).
func (p *Provider) ensureYB(ctx context.Context, host *dockerhost.Host, a allocation, mark func(string)) error {
	if p.cfg.YBShared {
		running, err := host.Inspect(ctx, a.ybName)
		if err != nil {
			return err
		}
		if running {
			return nil
		}
	}
	if _, err := host.RunContainer(ctx, dockerhost.ContainerSpec{
		Name:    a.ybName,
		Image:   p.cfg.YBImage,
		Detach:  true,
		Ports:   []dockerhost.PortMap{{Host: a.ybPort, Container: DefaultYBPortBase}},
		Command: yugabytedCommand(p.cfg.YBMasterGflags, p.cfg.YBTserverGflags),
	}); err != nil {
		return fmt.Errorf("start YugabyteDB %s: %w", a.ybName, err)
	}
	mark(a.ybName)
	if err := host.WaitHealthy(ctx, a.ybName, func(ctx context.Context) error {
		return p.probeYB(ctx, host, a.ybName)
	}); err != nil {
		return fmt.Errorf("YugabyteDB %s never became ready: %w", a.ybName, err)
	}
	return nil
}

func (p *Provider) probeCompute(ctx context.Context, host *dockerhost.Host, a allocation) error {
	sql := fmt.Sprintf("PGPASSWORD=%s psql -h 127.0.0.1 -p %d -U %s -d %s -tAc 'SELECT 1'",
		computePassword, a.computePort, computeUser, computeDB)
	res, err := host.Exec(ctx, a.endpointName, []string{"bash", "-c", sql})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("psql probe exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func (p *Provider) probeYB(ctx context.Context, host *dockerhost.Host, ybName string) error {
	sql := fmt.Sprintf("ysqlsh -h 127.0.0.1 -p %d -U %s -tAc 'SELECT 1'", DefaultYBPortBase, ybUser)
	res, err := host.Exec(ctx, ybName, []string{"bash", "-c", sql})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("ysqlsh probe exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// detectYBVersion best-effort live-detects the backing YugabyteDB version by
// running `yugabyted version` inside the live yugabyted container. The output
// is validated as a bare version (core.LooksLikeVersion) before being
// trusted, so a build that ever changes the command's output shape falls back
// to "" (Qualifies' documented "don't enforce this bound" sentinel) rather
// than recording unparseable text.
func (p *Provider) detectYBVersion(ctx context.Context, host *dockerhost.Host, ybName string) string {
	res, err := host.Exec(ctx, ybName, []string{"bash", "-c", "yugabyted version"})
	if err != nil || res.ExitCode != 0 {
		p.log("docker env: YB version detection failed for %s (best-effort): %v", ybName, err)
		return ""
	}
	v := strings.TrimSpace(res.Stdout)
	if !core.LooksLikeVersion(v) {
		return ""
	}
	return v
}

// defaultDetectProductVersion best-effort live-detects the amp product build
// via SELECT version() against the endpoint's own compute Postgres — the
// endpoint image has no other queryable version surface reachable from the
// framework. Accepted only when the raw response is itself a bare version
// (core.LooksLikeVersion), never the free-form Postgres banner text; a miss
// (endpoint not ready yet, or its Postgres reports prose) returns "" so the
// caller falls back to the image tag.
func defaultDetectProductVersion(ctx context.Context, ci pg.ConnInfo) string {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client, err := pg.Connect(cctx, ci)
	if err != nil {
		return ""
	}
	defer client.Close()
	raw, err := client.QueryString(cctx, "SELECT version()")
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(raw)
	if !core.LooksLikeVersion(v) {
		return ""
	}
	return v
}

// Teardown removes the per-test endpoint (and per-test YB), leaving a shared YB.
// Errors are logged, not returned fatally.
func (p *Provider) Teardown(ctx context.Context, env core.Environment) error {
	e, ok := env.(*Env)
	if !ok {
		return fmt.Errorf("docker teardown: unexpected environment type %T", env)
	}
	if err := e.host.StopRm(ctx, e.endpointName); err != nil {
		p.log("docker env: teardown of endpoint %s failed (best-effort): %v", e.endpointName, err)
	}
	if !e.ybShared {
		if err := e.host.StopRm(ctx, e.ybName); err != nil {
			p.log("docker env: teardown of YB %s failed (best-effort): %v", e.ybName, err)
		}
	}
	return e.transport.Close()
}

// Attach is not supported for the disposable single-container shape.
func (p *Provider) Attach(ctx context.Context, ref string) (core.Environment, error) {
	return nil, fmt.Errorf("docker single-container env does not support attach")
}

// Env is a live single-container amp environment.
type Env struct {
	transport    transport
	host         *dockerhost.Host
	endpointName string
	ybName       string
	ybShared     bool
	versions     core.DetectedVersions
	pg           pg.ConnInfo
	handles      core.EnvHandles

	// waitReady blocks until the endpoint's Postgres accepts connections; it is
	// pg.WaitReady in production and stubbed in unit tests (no real DB to dial).
	waitReady func(ctx context.Context, ci pg.ConnInfo, timeout time.Duration) error
}

// PGAccessor is the optional interface apps type-assert to reach the endpoint's
// Postgres. core stays untouched; this lives in the provider package.
type PGAccessor interface {
	PGEndpoint() pg.ConnInfo
}

// DockerControl is the optional interface tests type-assert to drive the
// underlying containers (e.g. restart the endpoint).
type DockerControl interface {
	DockerHost() *dockerhost.Host
	EndpointContainer() string
}

func (e *Env) ControllerURL() string           { return "" }
func (e *Env) Versions() core.DetectedVersions { return e.versions }
func (e *Env) Exec() exec.RemoteExecutor       { return e.transport }
func (e *Env) Handles() core.EnvHandles        { return e.handles }
func (e *Env) PGEndpoint() pg.ConnInfo         { return e.pg }
func (e *Env) DockerHost() *dockerhost.Host    { return e.host }
func (e *Env) EndpointContainer() string       { return e.endpointName }

// RestartCompute restarts the endpoint container in place and blocks until its
// Postgres serves again — the compute-restart capability the restart regression
// tests depend on. The data lives in the backing YugabyteDB, so it survives.
func (e *Env) RestartCompute(ctx context.Context) error {
	if err := e.host.Restart(ctx, e.endpointName); err != nil {
		return fmt.Errorf("restart endpoint container %s: %w", e.endpointName, err)
	}
	return e.waitReady(ctx, e.pg, computeReadyTimeout)
}

// Collect fetches the endpoint and YB container logs into the sink.
func (e *Env) Collect(ctx context.Context, into core.ArtifactSink) error {
	for _, name := range []string{e.endpointName, e.ybName} {
		logs, err := e.host.Logs(ctx, name, 0)
		if err != nil {
			return fmt.Errorf("collect logs %s: %w", name, err)
		}
		if err := into.Add(name+"/docker.log", "log", []byte(logs)); err != nil {
			return err
		}
	}
	return nil
}

func randToken() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// imageTag returns the tag portion of an image ref, or "" if untagged. A ':' is
// only a tag separator when no '/' follows it (registry host ports keep a '/').
func imageTag(image string) string {
	i := strings.LastIndexByte(image, ':')
	if i < 0 {
		return ""
	}
	if strings.ContainsRune(image[i+1:], '/') {
		return ""
	}
	return image[i+1:]
}
