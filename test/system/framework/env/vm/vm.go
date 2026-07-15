// Package vm is the cloud-agnostic N-instance environment provider: it launches
// a set of cloud VMs (via the apps/cloud.VM contract — AWS today, GCP/Azure in
// later waves), waits until they run and accept SSH, and exposes them as an
// Environment whose Nodes() are per-instance RemoteExecutors. It is a HOST for a
// DB cluster (a yugabyted/yugabyte app runs a cluster over its nodes), not a
// YSQL target itself, so it does NOT implement PGAccessor.
//
// The cloud is chosen by config (env.vm.cloud, default "aws"); the provider
// itself is cloud-neutral — it depends only on cloud.VM and the plain cloud.*
// value types, never on a concrete driver. This is the UnifiedUniverse/
// UnifiedClient idea from the stress framework: one env, the cloud picked by a
// single knob.
//
// Lifetime (AGENTS.md §5): the SSH connections and instances are owned by the
// Env and released only in Teardown — the exec/ssh clients outlive the
// provision ctx by design. Teardown MUST actually terminate the instances
// (leaking VMs is the cardinal sin here); on a teardown error it still runs
// ReapByTag as a safety net and surfaces the error. Each provisioned env is
// recorded in the standing registry under its run tag so a later `reap` task can
// find leaks.
package vm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/cloud"
	"github.com/yugabyte/yb-amp/test/system/framework/core"
	"github.com/yugabyte/yb-amp/test/system/framework/env/registry"
	"github.com/yugabyte/yb-amp/test/system/framework/exec"
)

// DefaultNodeCount is the node count when unset — three, the minimal RF=3
// cluster host.
const DefaultNodeCount = 3

// DefaultCloud is the cloud selected when env.vm.cloud is unset. AWS is the only
// implemented driver in Wave 1; the shared dev-VPC defaults make it usable out
// of the box.
const DefaultCloud = "aws"

// cleanupTimeout bounds the background-rooted rollback (terminate + reap) a
// failed Provision runs. It is independent of the provision ctx so a cancelled
// or timed-out provision still cleans up the instances it launched.
const cleanupTimeout = 5 * time.Minute

// Config is the env.vm.* configuration. The per-cloud instance settings
// (apps.cloud.<cloud>.*) are read by cloud.New at client-build time, so this
// struct carries only the env-level, cloud-neutral knobs.
type Config struct {
	// Cloud selects the driver: "aws" (default), later "gcp"/"azure".
	Cloud string
	// NodeCount is how many instances to launch.
	NodeCount int
	// InstanceType overrides the driver's configured instance type when set;
	// empty → inherit the driver default (e.g. aws m7g.xlarge).
	InstanceType string
}

// FromConfig reads env.vm.* into a Config. The node_count and instance_type
// knobs also accept the legacy env.awsvm.* keys as a back-compat fallback (used
// only when the env.vm.* key is not set), so existing configs keep working.
func FromConfig(c *core.Config) Config {
	return Config{
		Cloud:        c.GetString("env.vm.cloud", DefaultCloud),
		NodeCount:    c.GetInt("env.vm.node_count", c.GetInt("env.awsvm.node_count", DefaultNodeCount)),
		InstanceType: firstNonEmpty(c.GetString("env.vm.instance_type", ""), c.GetString("env.awsvm.instance_type", "")),
	}
}

// Configured reports whether the provider is usable: a positive node count and
// a non-empty cloud name (the AWS defaults make the aws driver always usable, so
// this is effectively "node count resolves").
func (c Config) Configured() bool { return c.nodeCount() > 0 && c.cloud() != "" }

func (c Config) nodeCount() int {
	if c.NodeCount > 0 {
		return c.NodeCount
	}
	return DefaultNodeCount
}

func (c Config) cloud() string {
	if c.Cloud != "" {
		return c.Cloud
	}
	return DefaultCloud
}

// Provider provisions N-instance environments over a cloud.VM driver.
type Provider struct {
	cfg    Config
	client cloud.VM
	reg    *registry.Registry
	logf   func(format string, args ...any)
	newTag func() string
}

// Option customizes a Provider.
type Option func(*Provider)

// WithRegistry records each provisioned env in reg (keyed by run tag) so the
// reap task can find leaks. nil → not recorded.
func WithRegistry(reg *registry.Registry) Option { return func(p *Provider) { p.reg = reg } }

// WithLogf injects the log sink (default log.Printf).
func WithLogf(logf func(string, ...any)) Option { return func(p *Provider) { p.logf = logf } }

// New builds a Provider from cfg over the given cloud VM driver (built by the
// caller via cloud.New, or a fake in tests).
func New(cfg Config, client cloud.VM, opts ...Option) *Provider {
	p := &Provider{
		cfg:    cfg,
		client: client,
		logf:   log.Printf,
		newTag: randTag,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func (p *Provider) log(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}

// Provision launches the instances, waits until they run and accept SSH, opens
// a per-node SSH executor, and records the env in the registry. On any failure
// after launch it best-effort terminates and reaps so nothing leaks.
func (p *Provider) Provision(ctx context.Context, spec core.EnvSpec, seed *core.Seed) (core.Environment, error) {
	runTag := p.newTag()
	count := p.cfg.nodeCount()

	instances, err := p.client.RunInstances(ctx, cloud.RunSpec{
		Count:        count,
		RunTag:       runTag,
		InstanceType: p.cfg.InstanceType,
	})
	if err != nil {
		return nil, fmt.Errorf("vm: launch %d instances: %w", count, err)
	}

	// From here on, any failure must not leak the instances we just launched.
	// Rollback is rooted in a fresh, bounded context — NOT the provision ctx —
	// because a provision that failed BECAUSE ctx was cancelled/timed out would
	// otherwise short-circuit its own cleanup and orphan the instances.
	fail := func(e error) (core.Environment, error) {
		cctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		instIDs := ids(instances)
		if terr := p.client.Terminate(cctx, instIDs); terr != nil {
			p.log("vm: cleanup terminate %v after provision failure (best-effort): %v", instIDs, terr)
			if rerr := p.client.ReapByTag(cctx, runTag); rerr != nil {
				p.log("vm: reap-by-tag %s after provision failure (best-effort): %v", runTag, rerr)
			}
		}
		return nil, e
	}

	if err := p.client.WaitRunning(ctx, ids(instances)); err != nil {
		return fail(fmt.Errorf("vm: wait running: %w", err))
	}
	// Refresh to pick up public IPs assigned once running.
	refreshed, err := p.client.Describe(ctx, map[string]string{"tag:ampqa-run": runTag})
	if err != nil {
		return fail(fmt.Errorf("vm: describe after running: %w", err))
	}
	if len(refreshed) > 0 {
		instances = refreshed
	}

	meta := p.client.Meta()
	if err := p.client.WaitSSH(ctx, instances, meta.KeyFile); err != nil {
		return fail(fmt.Errorf("vm: wait ssh: %w", err))
	}

	conns := make([]cloud.Conn, 0, len(instances))
	for _, inst := range instances {
		conn, err := p.client.Connect(ctx, inst)
		if err != nil {
			for _, c := range conns {
				_ = c.Close()
			}
			return fail(fmt.Errorf("vm: connect %s: %w", inst.ID, err))
		}
		conns = append(conns, conn)
	}

	env := &Env{
		runTag:    runTag,
		instances: instances,
		conns:     conns,
		client:    p.client,
		logf:      p.logf,
		versions:  seedVersions(seed),
		handles: core.EnvHandles{
			Endpoint:   firstPrivateIP(instances),
			Containers: ids(instances),
		},
	}

	if p.reg != nil {
		entry, err := p.reg.Add(registry.Entry{
			Name:     "vm-" + runTag,
			Shape:    "vm",
			Provider: "vm",
			Data: map[string]string{
				"cloud":        p.cfg.cloud(),
				"run_tag":      runTag,
				"instance_ids": strings.Join(ids(instances), ","),
				"region":       meta.Region,
				"ssh_user":     meta.SSHUser,
				"ssh_key":      meta.KeyFile,
				"hosts":        strings.Join(privateIPs(instances), ","),
			},
		})
		if err != nil {
			for _, c := range conns {
				_ = c.Close()
			}
			return fail(fmt.Errorf("vm: record registry entry: %w", err))
		}
		env.regID = entry.ID
	}
	return env, nil
}

// Teardown terminates every launched instance (the whole point — no leaked
// VMs), closes the SSH connections, and removes the registry entry. If
// Terminate errors it still runs ReapByTag as a safety net and returns the
// original error.
func (p *Provider) Teardown(ctx context.Context, env core.Environment) error {
	e, ok := env.(*Env)
	if !ok {
		return fmt.Errorf("vm teardown: unexpected environment type %T", env)
	}
	for _, c := range e.conns {
		if err := c.Close(); err != nil {
			p.log("vm teardown: close ssh conn (best-effort): %v", err)
		}
	}
	termErr := p.client.Terminate(ctx, ids(e.instances))
	gone := termErr == nil
	if termErr != nil {
		p.log("vm teardown: terminate failed, running reap-by-tag safety net: %v", termErr)
		if rerr := p.client.ReapByTag(ctx, e.runTag); rerr != nil {
			p.log("vm teardown: reap-by-tag %s also failed (best-effort): %v", e.runTag, rerr)
		} else {
			gone = true // the reap net terminated them; nothing left to reap
		}
	}
	// Drop the standing record ONLY once the instances are actually gone. If both
	// terminate and the reap net failed they may still be alive, and the registry
	// entry is the only handle a later `reap` has to find and reclaim them.
	if gone && p.reg != nil && e.regID != "" {
		if _, err := p.reg.Remove(e.regID); err != nil {
			p.log("vm teardown: remove registry entry %s (best-effort): %v", e.regID, err)
		}
	}
	return termErr
}

// Attach is not supported: vm instances are disposable per-run hosts.
func (p *Provider) Attach(ctx context.Context, ref string) (core.Environment, error) {
	return nil, fmt.Errorf("vm env does not support attach")
}

// Env is a live N-instance environment.
type Env struct {
	runTag    string
	instances []cloud.Instance
	conns     []cloud.Conn
	client    cloud.VM
	logf      func(format string, args ...any)
	versions  core.DetectedVersions
	handles   core.EnvHandles
	regID     string
}

// Nodes returns a per-instance RemoteExecutor — the cluster host apps
// (yugabyted/yugabyte) run over these.
func (e *Env) Nodes() []exec.RemoteExecutor {
	out := make([]exec.RemoteExecutor, len(e.conns))
	for i, c := range e.conns {
		out[i] = c
	}
	return out
}

// NodeAddrs lists each instance's private IP (the address peers advertise on,
// and — via the driver's default reach=private setting — what SSH and the
// coordinator actually connect to; the dev VPC's security groups don't allow
// inbound from the public internet).
func (e *Env) NodeAddrs() []string { return privateIPs(e.instances) }

// PublicAddrs lists each instance's public IP. This is egress-only by default
// (outbound internet for artifact downloads) — NOT the coordinator's inbound
// reach, which uses NodeAddrs. Callers that opt into advertise=="public" use
// this for the cluster's advertised address instead, which requires the driver's
// reach=public too.
func (e *Env) PublicAddrs() []string {
	out := make([]string, len(e.instances))
	for i, inst := range e.instances {
		out[i] = inst.PublicIP
	}
	return out
}

// RunTag is the ampqa-run tag stamped on every instance (used by reap).
func (e *Env) RunTag() string { return e.runTag }

// ControllerURL is "": this env is a bare host set, not an amp control plane.
func (e *Env) ControllerURL() string { return "" }

// Versions reports the detected versions (from the seed, if any).
func (e *Env) Versions() core.DetectedVersions { return e.versions }

// Exec returns the first node's executor (chaos / direct commands).
func (e *Env) Exec() exec.RemoteExecutor {
	if len(e.conns) == 0 {
		return nil
	}
	return e.conns[0]
}

// Handles are the concrete identifiers of the env.
func (e *Env) Handles() core.EnvHandles { return e.handles }

// Collect pulls key host logs (cloud-init output, kernel ring buffer) off every
// node. Best-effort: unreadable nodes are logged and skipped, not fatal.
func (e *Env) Collect(ctx context.Context, into core.ArtifactSink) error {
	for i, c := range e.conns {
		host := e.instances[i].PrivateIP
		if host == "" {
			host = e.instances[i].ID
		}
		e.collectOne(ctx, into, c, host, "cloud-init.log", "tail -c 1000000 /var/log/cloud-init-output.log 2>/dev/null")
		e.collectOne(ctx, into, c, host, "dmesg.log", "dmesg 2>/dev/null | tail -n 2000")
	}
	return nil
}

func (e *Env) collectOne(ctx context.Context, into core.ArtifactSink, c exec.RemoteExecutor, host, name, cmd string) {
	res, err := c.Run(ctx, cmd)
	if err != nil {
		e.log("vm collect: %s on %s: %v", name, host, err)
		return
	}
	if strings.TrimSpace(res.Stdout) == "" {
		return
	}
	if err := into.Add(host+"/"+name, "log", []byte(res.Stdout)); err != nil {
		e.log("vm collect: add %s on %s: %v", name, host, err)
	}
}

func (e *Env) log(format string, args ...any) {
	if e.logf != nil {
		e.logf(format, args...)
	}
}

// --- helpers ---

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func ids(instances []cloud.Instance) []string {
	out := make([]string, 0, len(instances))
	for _, inst := range instances {
		if inst.ID != "" {
			out = append(out, inst.ID)
		}
	}
	return out
}

func privateIPs(instances []cloud.Instance) []string {
	out := make([]string, len(instances))
	for i, inst := range instances {
		out[i] = inst.PrivateIP
	}
	return out
}

func firstPrivateIP(instances []cloud.Instance) string {
	if len(instances) == 0 {
		return ""
	}
	return instances[0].PrivateIP
}

func seedVersions(seed *core.Seed) core.DetectedVersions {
	// The host set carries no product/YB version of its own; a cluster app
	// layered on top detects and records that. Kept as a seam for symmetry with
	// the other providers.
	return core.DetectedVersions{}
}

func randTag() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
