// Package cloud is the cloud-agnostic VM contract: one interface (VM) that
// captures the generic instance lifecycle every cloud backing needs — launch,
// wait-running, wait-SSH, describe, terminate, reap, connect — plus the plain
// value types that cross that boundary (RunSpec, Instance, Conn, Meta). The
// concrete per-cloud drivers live in subpackages (apps/cloud/aws today; gcp and
// azure land in later waves); New resolves one from a config-supplied cloud
// name, mirroring the stress framework's UnifiedClient idea (one contract, the
// cloud picked by configuration).
//
// Cloud-specific knobs — AMI/image, VPC/subnet, security groups, keypair name,
// per-cloud instance-type names — are NOT on the generic RunSpec; each driver
// reads them from its own apps.cloud.<cloud>.* config section (see aws.Config),
// so the generic type stays free of cloud leakage. RunSpec carries only what a
// caller genuinely varies per provision: how many, the instance size, the run
// tag, the name prefix, and the root volume size.
//
// This package imports the aws and gcp drivers (for New's adapters) but
// neither driver imports this package, so there is no cycle and each driver's
// runtime behavior is completely independent of this layer.
package cloud

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/cloud/aws"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/cloud/gcp"
	execpkg "github.com/yugabyte/yb-amp/test/system/framework/exec"
)

// Instance is one cloud VM the framework manages. It is the cloud-neutral
// projection of a provider's native instance record — enough for the env to
// wire SSH hosts, advertise addresses, and reap by age.
type Instance struct {
	ID         string
	PublicIP   string
	PrivateIP  string
	State      string
	LaunchTime time.Time
}

// RunSpec parameterizes a RunInstances call. It holds only the knobs a caller
// varies per provision; everything cloud-specific (image, network, keypair,
// per-cloud instance-type names) is resolved by the driver from its config.
type RunSpec struct {
	Count        int
	RunTag       string // roots ownership tagging and reap; required
	InstanceType string // "" → the driver's configured default
	NamePrefix   string // "" → the driver's default
	VolumeSizeGB int    // 0 → the driver's default
}

// Conn is an SSH connection to an instance: a RemoteExecutor plus Close, so the
// env that owns it can release it in Teardown. Every cloud reaches its
// instances over SSH the same way (exec/ssh), so this is genuinely cloud-neutral
// and identical to the aws driver's Conn — an aws.Conn satisfies it directly.
type Conn interface {
	execpkg.RemoteExecutor
	Close() error
}

// Meta is the cloud-neutral connection metadata the env records and reuses:
// which region the instances live in, the SSH user, and the private key file.
type Meta struct {
	Region  string
	SSHUser string
	KeyFile string
}

// VM is the cloud-agnostic instance-lifecycle contract. A driver satisfies it
// per cloud; callers (env/vm) depend only on this, so switching clouds is a
// config change, not a code change.
type VM interface {
	// RunInstances launches spec.Count instances, tagged for ownership/reap,
	// best-effort terminating any partial launch before returning an error.
	RunInstances(ctx context.Context, spec RunSpec) ([]Instance, error)
	// WaitRunning blocks until every id reaches the running state.
	WaitRunning(ctx context.Context, ids []string) error
	// WaitSSH polls each instance's reach IP :22 until it accepts a connection
	// or a bounded timeout passes (fail fast on an unreachable host).
	WaitSSH(ctx context.Context, instances []Instance, keyFile string) error
	// Describe lists instances matching filters (provider-native filter keys).
	Describe(ctx context.Context, filters map[string]string) ([]Instance, error)
	// Terminate terminates ids (idempotent; empty is a no-op).
	Terminate(ctx context.Context, ids []string) error
	// ReapByTag terminates every non-terminated instance of one run tag — the
	// safety net that guarantees a run's instances can always be cleaned up.
	ReapByTag(ctx context.Context, runTag string) error
	// ReapStale terminates managed autoclean instances older than olderThan —
	// the age-based safety net for instances whose run tag is unknown.
	ReapStale(ctx context.Context, olderThan time.Duration) error
	// Connect opens an SSH executor to an instance; ctx bounds the dial only,
	// the returned Conn outlives it and the caller owns Close.
	Connect(ctx context.Context, inst Instance) (Conn, error)
	// Meta returns the cloud-neutral connection metadata (region, ssh user, key).
	Meta() Meta
}

// ConfigGetter is the subset of *core.Config a driver reads. Taking an
// interface keeps this package (and the drivers) free of a core import.
type ConfigGetter interface {
	GetString(key, def string) string
	GetStringSlice(key string, def []string) []string
	GetDuration(key string, def time.Duration) time.Duration
}

// SecretResolver maps a secret NAME to its resolved value (matches
// core.Secrets.Value). Used to resolve optional explicit cloud credentials.
type SecretResolver func(name string) (string, error)

// New builds the VM driver for the named cloud, reading its
// apps.cloud.<cloud>.* config and resolving any optional explicit credentials
// via resolve. "" and "aws" resolve to the AWS driver, "gcp" to the GCP
// driver; "azure" returns a clear not-implemented error until Wave 4 lands.
func New(cloud string, c ConfigGetter, resolve SecretResolver) (VM, error) {
	switch strings.ToLower(strings.TrimSpace(cloud)) {
	case "", "aws":
		return newAWS(c, resolve)
	case "gcp":
		return newGCP(c, resolve)
	case "azure":
		return nil, fmt.Errorf("cloud %q not implemented yet (Wave 4)", cloud)
	default:
		return nil, fmt.Errorf("unknown cloud %q (want aws|gcp|azure)", cloud)
	}
}

// newAWS builds the AWS driver: read apps.cloud.aws.*, resolve the optional
// explicit static credentials (both-or-neither; a half-configured pair or a
// failing named secret is an error — the caller asked for explicit creds, so we
// must NOT silently fall back to the ambient chain), and wrap it in the adapter.
func newAWS(c ConfigGetter, resolve SecretResolver) (VM, error) {
	cfg := aws.FromConfig(c)
	access, secret, err := aws.ResolveCreds(c, aws.SecretResolver(resolve))
	if err != nil {
		return nil, err
	}
	cfg.AccessKey, cfg.SecretKey = access, secret
	return awsVM{c: aws.New(cfg)}, nil
}

// awsVM adapts *aws.Client (which keeps its own rich internal types) to the
// cloud.VM contract, translating the boundary value types both ways. The aws
// driver is untouched by this layer, so its behavior is identical whether it is
// used directly or through this adapter.
type awsVM struct{ c *aws.Client }

func (a awsVM) RunInstances(ctx context.Context, spec RunSpec) ([]Instance, error) {
	insts, err := a.c.RunInstances(ctx, aws.RunSpec{
		Count:        spec.Count,
		RunTag:       spec.RunTag,
		InstanceType: spec.InstanceType,
		NamePrefix:   spec.NamePrefix,
		VolumeSizeGB: spec.VolumeSizeGB,
	})
	return fromAWS(insts), err
}

func (a awsVM) WaitRunning(ctx context.Context, ids []string) error {
	return a.c.WaitRunning(ctx, ids)
}

func (a awsVM) WaitSSH(ctx context.Context, instances []Instance, keyFile string) error {
	return a.c.WaitSSH(ctx, toAWS(instances), keyFile)
}

func (a awsVM) Describe(ctx context.Context, filters map[string]string) ([]Instance, error) {
	insts, err := a.c.Describe(ctx, filters)
	return fromAWS(insts), err
}

func (a awsVM) Terminate(ctx context.Context, ids []string) error {
	return a.c.Terminate(ctx, ids)
}

func (a awsVM) ReapByTag(ctx context.Context, runTag string) error {
	return a.c.ReapByTag(ctx, runTag)
}

// ReapByTagConfirmed exposes the driver's confirmed reap (count of instances
// acted on) through the adapter — NOT part of the cloud.VM interface, so a
// confirmation-aware caller (env/reap) type-asserts for it while fakes need not
// implement it.
func (a awsVM) ReapByTagConfirmed(ctx context.Context, runTag string) (int, error) {
	return a.c.ReapByTagConfirmed(ctx, runTag)
}

func (a awsVM) ReapStale(ctx context.Context, olderThan time.Duration) error {
	return a.c.ReapStale(ctx, olderThan)
}

func (a awsVM) Connect(ctx context.Context, inst Instance) (Conn, error) {
	// aws.Conn's method set matches cloud.Conn, so the value is assignable.
	conn, err := a.c.Connect(ctx, toAWSOne(inst))
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (a awsVM) Meta() Meta {
	cfg := a.c.Config()
	return Meta{Region: cfg.Region, SSHUser: cfg.SSHUser, KeyFile: cfg.KeyFile}
}

func fromAWS(in []aws.Instance) []Instance {
	if in == nil {
		return nil
	}
	out := make([]Instance, len(in))
	for i, x := range in {
		out[i] = Instance{ID: x.ID, PublicIP: x.PublicIP, PrivateIP: x.PrivateIP, State: x.State, LaunchTime: x.LaunchTime}
	}
	return out
}

func toAWS(in []Instance) []aws.Instance {
	if in == nil {
		return nil
	}
	out := make([]aws.Instance, len(in))
	for i, x := range in {
		out[i] = toAWSOne(x)
	}
	return out
}

func toAWSOne(x Instance) aws.Instance {
	return aws.Instance{ID: x.ID, PublicIP: x.PublicIP, PrivateIP: x.PrivateIP, State: x.State, LaunchTime: x.LaunchTime}
}

// newGCP builds the GCP driver: read apps.cloud.gcp.*, resolve the optional
// explicit service-account key file (empty → ambient gcloud auth chain, same
// default-to-ambient idea as aws), and wrap it in the adapter.
func newGCP(c ConfigGetter, resolve SecretResolver) (VM, error) {
	cfg := gcp.FromConfig(c)
	keyFile, err := gcp.ResolveServiceAccountKeyFile(c, gcp.SecretResolver(resolve))
	if err != nil {
		return nil, err
	}
	cfg.ServiceAccountKeyFile = keyFile
	return gcpVM{c: gcp.New(cfg)}, nil
}

// gcpVM adapts *gcp.Client (which keeps its own rich internal types) to the
// cloud.VM contract, translating the boundary value types both ways. The gcp
// driver is untouched by this layer, so its behavior is identical whether it
// is used directly or through this adapter.
type gcpVM struct{ c *gcp.Client }

func (g gcpVM) RunInstances(ctx context.Context, spec RunSpec) ([]Instance, error) {
	insts, err := g.c.RunInstances(ctx, gcp.RunSpec{
		Count:        spec.Count,
		RunTag:       spec.RunTag,
		InstanceType: spec.InstanceType,
		NamePrefix:   spec.NamePrefix,
		VolumeSizeGB: spec.VolumeSizeGB,
	})
	return fromGCP(insts), err
}

func (g gcpVM) WaitRunning(ctx context.Context, ids []string) error {
	return g.c.WaitRunning(ctx, ids)
}

func (g gcpVM) WaitSSH(ctx context.Context, instances []Instance, keyFile string) error {
	return g.c.WaitSSH(ctx, toGCP(instances), keyFile)
}

func (g gcpVM) Describe(ctx context.Context, filters map[string]string) ([]Instance, error) {
	insts, err := g.c.Describe(ctx, filters)
	return fromGCP(insts), err
}

func (g gcpVM) Terminate(ctx context.Context, ids []string) error {
	return g.c.Terminate(ctx, ids)
}

func (g gcpVM) ReapByTag(ctx context.Context, runTag string) error {
	return g.c.ReapByTag(ctx, runTag)
}

// ReapByTagConfirmed exposes the driver's confirmed reap through the adapter (see
// awsVM.ReapByTagConfirmed).
func (g gcpVM) ReapByTagConfirmed(ctx context.Context, runTag string) (int, error) {
	return g.c.ReapByTagConfirmed(ctx, runTag)
}

func (g gcpVM) ReapStale(ctx context.Context, olderThan time.Duration) error {
	return g.c.ReapStale(ctx, olderThan)
}

func (g gcpVM) Connect(ctx context.Context, inst Instance) (Conn, error) {
	// gcp.Conn's method set matches cloud.Conn, so the value is assignable.
	conn, err := g.c.Connect(ctx, toGCPOne(inst))
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (g gcpVM) Meta() Meta {
	cfg := g.c.Config()
	// Region carries the zone: GCE's per-instance operations (describe,
	// delete) are zone-scoped, so the zone is the locality an env needs to
	// reconnect — the closest cloud-neutral fit for the Region field.
	return Meta{Region: cfg.Zone, SSHUser: cfg.SSHUser, KeyFile: cfg.KeyFile}
}

func fromGCP(in []gcp.Instance) []Instance {
	if in == nil {
		return nil
	}
	out := make([]Instance, len(in))
	for i, x := range in {
		out[i] = Instance{ID: x.ID, PublicIP: x.PublicIP, PrivateIP: x.PrivateIP, State: x.State, LaunchTime: x.LaunchTime}
	}
	return out
}

func toGCP(in []Instance) []gcp.Instance {
	if in == nil {
		return nil
	}
	out := make([]gcp.Instance, len(in))
	for i, x := range in {
		out[i] = toGCPOne(x)
	}
	return out
}

func toGCPOne(x Instance) gcp.Instance {
	return gcp.Instance{ID: x.ID, PublicIP: x.PublicIP, PrivateIP: x.PrivateIP, State: x.State, LaunchTime: x.LaunchTime}
}
