// Package gcp is the GCE instance-lifecycle app: it launches, waits on,
// describes, connects to, and terminates GCE instances by shelling the
// `gcloud` CLI — the same design choice as apps/cloud/aws (no
// google-cloud-compute SDK dependency in this standalone module) and the
// kubectl/helm/docker transports.
//
// Auth is the one place gcp genuinely differs from aws: aws instances are
// ambient-authed by default (the aws CLI's own credential chain). GCE needs an
// active gcloud identity too, and by default this app leaves that to whatever
// `gcloud auth` state is already active on the machine running it (ambient,
// same idea as aws). A caller that wants an explicit service account instead
// sets apps.cloud.gcp.service_account_secret to a secrets: name; the resolved
// value (a PATH to the service-account JSON — see ResolveServiceAccountKeyFile)
// is injected into the shelled gcloud process via
// CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE. This is deliberately NOT
// `gcloud auth activate-service-account`, which mutates the shared, global
// gcloud config directory — a real hazard for parallel-by-design tests
// (AGENTS.md §3) running under different service accounts at once. The env var
// scopes the credential to one child process only, never argv, never logged.
//
// Hygiene (AGENTS.md §8) mirrors aws: RunInstances reuses an EXISTING SSH
// public key (apps.cloud.gcp.public_key_file) injected into instance metadata
// (`ssh-keys`) — never `gcloud compute os-login ssh-keys create/add`, never a
// freshly generated keypair — and bakes the mandatory ownership labels
// (yb_owner/yb_task/yb_dept/autoclean plus ampqa/ampqa-run) into every
// instance. ReapByTag/ReapStale find and terminate leaked instances by label.
package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	execpkg "github.com/yugabyte/yb-amp/test/functional/framework/exec"
	sshx "github.com/yugabyte/yb-amp/test/functional/framework/exec/ssh"
)

// Canonical GCP dev-project settings sourced from the stress framework's
// region_configs.py (CLOUD_DEFAULTS["gcp"] and GCP_REGIONS["us-west1"]) — the
// shared dev project, VPC, and subnet this account already uses. Image and SSH
// key material are NOT sourced from there (stress bakes custom images and
// relies on an operator-supplied key with no per-cloud constant of its own),
// so those stay documented-empty until a real live-run value is supplied.
const (
	DefaultProject    = "yugabyte"
	DefaultRegion     = "us-west1"
	DefaultZone       = "us-west1-b"
	DefaultNetwork    = "yugabyte-network"
	DefaultSubnetwork = "subnet-us-west1"
	DefaultDiskType   = "pd-standard"
	DefaultSSHUser    = "centos"

	// DefaultMachineType is a reasonable general-purpose default (4 vCPU,
	// e2 standard tier) — NOT sourced from stress (which resolves machine type
	// dynamically per instance_type, no fixed constant); pick a project-specific
	// one via apps.cloud.gcp.machine_type if this doesn't fit.
	DefaultMachineType = "e2-standard-4"

	// DefaultImage/DefaultImageFamily/DefaultImageProject: no generic public
	// image was identified (stress's custom images are baked and
	// project-specific, not a reusable generic default) — one of image or
	// image_family must be filled before a live run.
	DefaultImage        = ""
	DefaultImageFamily  = ""
	DefaultImageProject = ""

	// DefaultKeyFile/DefaultPublicKeyFile: no GCP-specific SSH key path exists
	// in stress (it relies on a generic operator-supplied path plus keys baked
	// into its custom images) — must be filled before a live run.
	DefaultKeyFile       = ""
	DefaultPublicKeyFile = ""

	DefaultVolumeSizeGB = 100

	// DefaultOwner is the yb_owner label / name-prefix value when neither the
	// top-level `owner` config key nor apps.cloud.gcp.name_prefix is set. A
	// neutral default so the framework never bakes in a specific person's
	// username; override per run with -o owner=<name>.
	DefaultOwner = "ampqa"

	// DefaultReach mirrors aws: instances reach each other and the coordinator
	// over the private (internal) IP; "public" is an explicit opt-out.
	DefaultReach = "private"

	// DefaultSSHWaitTimeout bounds WaitSSH so an unreachable host fails fast
	// instead of hanging for the caller's whole provision deadline.
	DefaultSSHWaitTimeout = 4 * time.Minute
)

// Config is the apps.cloud.gcp.* configuration.
type Config struct {
	Project      string
	Zone         string
	Region       string
	Network      string
	Subnetwork   string
	DiskType     string
	MachineType  string
	Image        string // a SPECIFIC image name (`--image`); "" → use ImageFamily
	ImageFamily  string // an image FAMILY (`--image-family`, always current); takes precedence over Image
	ImageProject string // the project owning Image/ImageFamily (`--image-project`)

	SSHUser       string
	KeyFile       string // private key path for SSH (~ expanded by exec/ssh)
	PublicKeyFile string // EXISTING public key injected into instance metadata — NEVER minted (~ expanded here)

	// Owner is stamped as the yb_owner label on every instance (see
	// mandatoryLabels) and, when NamePrefix is unset, used as the name prefix.
	Owner string
	// NamePrefix prefixes every launched instance's name: "<prefix>-<run>-<n>".
	// "" (the default) falls back to Owner.
	NamePrefix string

	// Reach selects the address used for SSH/coordinator traffic: "private"
	// (default) uses the internal/tailnet IP; "public" opts into the external one.
	Reach string
	// SSHWaitTimeout bounds WaitSSH (see DefaultSSHWaitTimeout).
	SSHWaitTimeout time.Duration

	// ServiceAccountKeyFile is the RESOLVED path to a service-account JSON key
	// (see ResolveServiceAccountKeyFile), already resolved by the caller. Empty
	// (the default) → the ambient `gcloud auth` chain is used, exactly like aws
	// defaults to the ambient AWS credential chain.
	ServiceAccountKeyFile string
}

// ConfigGetter is the subset of *core.Config this package reads. Taking an
// interface keeps apps/cloud/gcp free of a core import (the apps/* convention).
type ConfigGetter interface {
	GetString(key, def string) string
	GetStringSlice(key string, def []string) []string
	GetDuration(key string, def time.Duration) time.Duration
}

// SecretResolver maps a secret NAME to its resolved value. It matches
// core.Secrets.Value; taking a func keeps this package free of a core import.
type SecretResolver func(name string) (string, error)

// ResolveServiceAccountKeyFile reads apps.cloud.gcp.service_account_secret (a
// secret NAME, not a value) and resolves it via resolve into a filesystem path
// to a service-account JSON key. Empty (the default) → ("", nil): keep the
// ambient gcloud auth chain, mirroring aws's ResolveCreds default. The secret
// is expected to be declared as an env-variant secret whose env var holds a
// PATH (e.g. GCP_KEY=/path/to/sa.json) — for that variant core.Secrets.Value
// and core.Secrets.Path return the identical string, so the ordinary
// SecretResolver (Value-shaped) already yields the path a caller needs.
func ResolveServiceAccountKeyFile(c ConfigGetter, resolve SecretResolver) (string, error) {
	name := c.GetString("apps.cloud.gcp.service_account_secret", "")
	if name == "" {
		return "", nil
	}
	if resolve == nil {
		return "", fmt.Errorf("gcp service account: service_account_secret=%q set but no secret resolver available", name)
	}
	path, err := resolve(name)
	if err != nil {
		return "", fmt.Errorf("gcp service_account_secret %q: %w", name, err)
	}
	return path, nil
}

// FromConfig reads apps.cloud.gcp.* into a Config, applying the canonical
// defaults.
func FromConfig(c ConfigGetter) Config {
	owner := c.GetString("owner", DefaultOwner)
	return Config{
		Project:        c.GetString("apps.cloud.gcp.project", DefaultProject),
		Zone:           c.GetString("apps.cloud.gcp.zone", DefaultZone),
		Region:         c.GetString("apps.cloud.gcp.region", DefaultRegion),
		Network:        c.GetString("apps.cloud.gcp.network", DefaultNetwork),
		Subnetwork:     c.GetString("apps.cloud.gcp.subnetwork", DefaultSubnetwork),
		DiskType:       c.GetString("apps.cloud.gcp.disk_type", DefaultDiskType),
		MachineType:    c.GetString("apps.cloud.gcp.machine_type", DefaultMachineType),
		Image:          c.GetString("apps.cloud.gcp.image", DefaultImage),
		ImageFamily:    c.GetString("apps.cloud.gcp.image_family", DefaultImageFamily),
		ImageProject:   c.GetString("apps.cloud.gcp.image_project", DefaultImageProject),
		SSHUser:        c.GetString("apps.cloud.gcp.ssh_user", DefaultSSHUser),
		KeyFile:        c.GetString("apps.cloud.gcp.key_file", DefaultKeyFile),
		PublicKeyFile:  c.GetString("apps.cloud.gcp.public_key_file", DefaultPublicKeyFile),
		Owner:          owner,
		NamePrefix:     c.GetString("apps.cloud.gcp.name_prefix", owner),
		Reach:          c.GetString("apps.cloud.gcp.reach", DefaultReach),
		SSHWaitTimeout: c.GetDuration("apps.cloud.gcp.ssh_wait_timeout", DefaultSSHWaitTimeout),
	}
}

// authEnv returns the gcloud auth environment variable to inject into the
// shelled gcloud process, or nil when no explicit service account is
// configured (the ambient `gcloud auth` chain is used). Deliberately NOT
// `gcloud auth activate-service-account` — see the package doc for why that
// would be a parallel-safety hazard.
func (c Config) authEnv() []string {
	if c.ServiceAccountKeyFile == "" {
		return nil
	}
	return []string{"CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE=" + c.ServiceAccountKeyFile}
}

func (c Config) withDefaults() Config {
	if c.Project == "" {
		c.Project = DefaultProject
	}
	if c.Zone == "" {
		c.Zone = DefaultZone
	}
	if c.Region == "" {
		c.Region = DefaultRegion
	}
	if c.Network == "" {
		c.Network = DefaultNetwork
	}
	if c.Subnetwork == "" {
		c.Subnetwork = DefaultSubnetwork
	}
	if c.DiskType == "" {
		c.DiskType = DefaultDiskType
	}
	if c.MachineType == "" {
		c.MachineType = DefaultMachineType
	}
	if c.SSHUser == "" {
		c.SSHUser = DefaultSSHUser
	}
	if c.Owner == "" {
		c.Owner = DefaultOwner
	}
	if c.NamePrefix == "" {
		c.NamePrefix = c.Owner
	}
	if c.Reach == "" {
		c.Reach = DefaultReach
	}
	if c.SSHWaitTimeout <= 0 {
		c.SSHWaitTimeout = DefaultSSHWaitTimeout
	}
	return c
}

// Runner runs one `gcloud` invocation to completion. Injected in tests to
// record argv and return canned output; the default shells out via os/exec.
type Runner func(ctx context.Context, argv []string) (execpkg.Result, error)

// Conn is an SSH connection to an instance: a RemoteExecutor plus Close, so the
// env that owns it can release it in Teardown.
type Conn interface {
	execpkg.RemoteExecutor
	Close() error
}

// sshDialer opens an SSH connection to a host. Injected in tests.
type sshDialer func(ctx context.Context, cfg sshx.Config) (Conn, error)

// portProbe reports whether host:port accepts a TCP connection. Injected in
// tests so WaitSSH needs no real network.
type portProbe func(ctx context.Context, host string, port int) error

// pubKeyReader reads the CONTENTS of an existing public key file. Injected in
// tests so RunInstances needs no real file on disk.
type pubKeyReader func(path string) (string, error)

// Instance is one GCE instance the app manages. ID is the instance NAME (GCE's
// natural per-instance handle for describe/delete — unlike EC2, gcloud
// operates on the resource name, not a separate opaque instance id). Zone is
// the instance's ACTUAL zone as reported by describe/list: RunInstances
// launches into the driver's single configured zone, but reap (ReapByTag /
// ReapStale) can turn up instances a DIFFERENT owner placed elsewhere in the
// region — a YBA GCP-provider universe spreads its nodes across the region's
// zones — so each instance must be deleted in its own zone, not the configured
// one.
type Instance struct {
	ID         string
	Zone       string
	PublicIP   string
	PrivateIP  string
	State      string
	LaunchTime time.Time
}

// Client drives GCE instance lifecycle over the gcloud CLI.
type Client struct {
	cfg     Config
	run     Runner
	dialSSH sshDialer
	probe   portProbe
	readKey pubKeyReader
	logf    func(format string, args ...any)
}

// Option customizes a Client.
type Option func(*Client)

// WithRunner overrides the gcloud-CLI runner (tests inject a scripted recorder).
func WithRunner(r Runner) Option { return func(c *Client) { c.run = r } }

// WithLogf injects the log sink (default log.Printf).
func WithLogf(logf func(string, ...any)) Option { return func(c *Client) { c.logf = logf } }

// WithSSHDialer overrides the SSH dialer used by Connect (tests inject a fake).
func WithSSHDialer(d func(ctx context.Context, cfg sshx.Config) (Conn, error)) Option {
	return func(c *Client) { c.dialSSH = sshDialer(d) }
}

// WithPortProbe overrides the TCP reachability probe used by WaitSSH.
func WithPortProbe(p func(ctx context.Context, host string, port int) error) Option {
	return func(c *Client) { c.probe = portProbe(p) }
}

// WithPubKeyReader overrides the public-key file reader used by RunInstances.
func WithPubKeyReader(r func(path string) (string, error)) Option {
	return func(c *Client) { c.readKey = pubKeyReader(r) }
}

// New builds a Client from cfg (defaults applied).
func New(cfg Config, opts ...Option) *Client {
	c := &Client{
		cfg:     cfg.withDefaults(),
		dialSSH: defaultSSHDial,
		probe:   tcpProbe,
		readKey: osReadPubKey,
		logf:    log.Printf,
	}
	c.run = c.osRun
	for _, o := range opts {
		o(c)
	}
	return c
}

// Config returns the resolved config (defaults applied).
func (c *Client) Config() Config { return c.cfg }

func (c *Client) log(format string, args ...any) {
	if c.logf != nil {
		c.logf(format, args...)
	}
}

// RunSpec parameterizes a RunInstances call.
type RunSpec struct {
	Count        int
	RunTag       string // stamped as ampqa-run=<RunTag> label and into every name
	InstanceType string // "" → cfg.MachineType
	NamePrefix   string // "" → cfg.NamePrefix (Owner-derived)
	VolumeSizeGB int    // 0 → DefaultVolumeSizeGB
}

// RunInstances launches spec.Count instances (one gcloud call each, so every
// instance gets its own indexed name), each with the mandatory ownership
// labels, and — when apps.cloud.gcp.public_key_file is configured — the shared
// EXISTING public key injected into instance metadata (never a freshly
// generated or OS-Login-minted key). On a partial failure it best-effort
// terminates whatever already launched, so a failed provision never leaks.
func (c *Client) RunInstances(ctx context.Context, spec RunSpec) ([]Instance, error) {
	if spec.Count <= 0 {
		return nil, fmt.Errorf("run-instances: count must be > 0, got %d", spec.Count)
	}
	if spec.RunTag == "" {
		return nil, fmt.Errorf("run-instances: RunTag is required (it roots labeling and reap)")
	}
	instances := make([]Instance, 0, spec.Count)
	for i := 0; i < spec.Count; i++ {
		inst, err := c.runOne(ctx, spec, i)
		if err != nil {
			if len(instances) > 0 {
				ids := instanceIDs(instances)
				if terr := c.Terminate(ctx, ids); terr != nil {
					c.log("gcp: rollback terminate of %v after launch failure (best-effort): %v", ids, terr)
				}
			}
			return nil, fmt.Errorf("run-instances #%d: %w", i, err)
		}
		instances = append(instances, inst)
	}
	return instances, nil
}

func (c *Client) runOne(ctx context.Context, spec RunSpec, i int) (Instance, error) {
	machineType := spec.InstanceType
	if machineType == "" {
		machineType = c.cfg.MachineType
	}
	prefix := spec.NamePrefix
	if prefix == "" {
		prefix = c.cfg.NamePrefix
	}
	volume := spec.VolumeSizeGB
	if volume == 0 {
		volume = DefaultVolumeSizeGB
	}
	name := sanitizeGCEName(fmt.Sprintf("%s-%s-%d", prefix, spec.RunTag, i))

	pubKey, err := c.pubKey()
	if err != nil {
		return Instance{}, err
	}

	argv := []string{
		"gcloud", "compute", "instances", "create", name,
		"--project", c.cfg.Project,
		"--zone", c.cfg.Zone,
		"--machine-type", machineType,
		"--network", c.cfg.Network,
		"--subnet", c.cfg.Subnetwork,
		"--boot-disk-size", fmt.Sprintf("%dGB", volume),
		"--boot-disk-type", c.cfg.DiskType,
		"--labels", labelsFlag(mandatoryLabels(spec.RunTag, c.cfg.Owner)),
		"--format", "json",
	}
	// An image FAMILY (--image-family) always resolves to the family's current
	// image and is the standard GCE idiom (e.g. almalinux-8); a specific --image
	// name is the pin-an-exact-build alternative. Family wins when both are set.
	switch {
	case c.cfg.ImageFamily != "":
		argv = append(argv, "--image-family", c.cfg.ImageFamily)
	case c.cfg.Image != "":
		argv = append(argv, "--image", c.cfg.Image)
	}
	if c.cfg.ImageProject != "" {
		argv = append(argv, "--image-project", c.cfg.ImageProject)
	}
	if pubKey != "" {
		argv = append(argv, "--metadata", "ssh-keys="+c.cfg.SSHUser+":"+pubKey)
	}

	res, err := c.run(ctx, argv)
	if err != nil {
		return Instance{}, err
	}
	if res.ExitCode != 0 {
		return Instance{}, fmt.Errorf("gcloud instances create exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	var out []instanceJSON
	if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil {
		return Instance{}, fmt.Errorf("parse instances create output: %w", err)
	}
	if len(out) == 0 {
		return Instance{}, fmt.Errorf("instances create returned no instance")
	}
	return out[0].toInstance(), nil
}

// pubKey reads the configured public key file's contents, or returns "" when
// none is configured (the instance then relies on whatever access mechanism —
// a baked-in authorized key, OS Login configured out of band — the image
// already provides).
func (c *Client) pubKey() (string, error) {
	if c.cfg.PublicKeyFile == "" {
		return "", nil
	}
	content, err := c.readKey(expandHome(c.cfg.PublicKeyFile))
	if err != nil {
		return "", fmt.Errorf("read gcp public key file %q: %w", c.cfg.PublicKeyFile, err)
	}
	return strings.TrimSpace(content), nil
}

// WaitRunning blocks until every id reaches the RUNNING status, polling
// `gcloud compute instances describe` (gcloud has no single blocking
// wait-until-running call the way `aws ec2 wait instance-running` does).
// Empty ids is a no-op; bounded only by ctx — the caller's provision deadline.
func (c *Client) WaitRunning(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if err := c.waitOneRunning(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) waitOneRunning(ctx context.Context, id string) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait-running %s: %w", id, ctx.Err())
		default:
		}
		argv := []string{"gcloud", "compute", "instances", "describe", id,
			"--project", c.cfg.Project, "--zone", c.cfg.Zone, "--format", "json"}
		res, err := c.run(ctx, argv)
		if err != nil {
			return fmt.Errorf("gcloud instances describe %s: %w", id, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("gcloud instances describe %s exit %d: %s", id, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		var ij instanceJSON
		if err := json.Unmarshal([]byte(res.Stdout), &ij); err != nil {
			return fmt.Errorf("parse instances describe output for %s: %w", id, err)
		}
		if ij.Status == "RUNNING" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait-running %s: %w (last status %s)", id, ctx.Err(), ij.Status)
		case <-time.After(2 * time.Second):
		}
	}
}

// WaitSSH polls each instance's reach IP (reachIP — the private/internal IP by
// default) until a real SSH auth handshake succeeds, ctx is cancelled, or its
// own SSHWaitTimeout budget passes — whichever is soonest. keyFile must be set.
//
// It waits for a genuine authenticated connection, not just an open :22,
// because on GCE the guest agent provisions the metadata SSH key (creates the
// ssh_user and writes authorized_keys) tens of seconds AFTER sshd starts
// accepting connections — a bare TCP probe returns while auth would still be
// rejected, and the caller's single Connect would then race key provisioning.
func (c *Client) WaitSSH(ctx context.Context, instances []Instance, keyFile string) error {
	if keyFile == "" {
		return fmt.Errorf("wait-ssh: no key file configured")
	}
	timeout := c.cfg.SSHWaitTimeout
	if timeout <= 0 {
		timeout = DefaultSSHWaitTimeout
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, inst := range instances {
		host := c.reachIP(inst)
		if host == "" {
			return fmt.Errorf("wait-ssh: instance %s has no reachable IP", inst.ID)
		}
		if err := c.waitOneSSH(wctx, inst, host, keyFile); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) waitOneSSH(ctx context.Context, inst Instance, host, keyFile string) error {
	var last error
	for {
		select {
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("wait-ssh %s (%s): %w (last: %v)", inst.ID, host, ctx.Err(), last)
			}
			return fmt.Errorf("wait-ssh %s (%s): %w", inst.ID, host, ctx.Err())
		default:
		}
		// TCP first (cheap), then a real auth handshake (dial with the key, then
		// close) — see WaitSSH's doc for why an open port alone is insufficient.
		if last = c.probe(ctx, host, 22); last == nil {
			var conn Conn
			if conn, last = c.dialSSH(ctx, sshx.Config{Host: host, User: c.cfg.SSHUser, KeyPath: keyFile}); last == nil {
				_ = conn.Close()
				return nil
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// reachIP returns the address used to reach inst for SSH/coordinator traffic:
// the internal IP by default (the tailnet path), falling back to the external
// IP if the internal one is unset (a describe race), or the external IP
// outright when Reach is "public".
func (c *Client) reachIP(inst Instance) string {
	if c.cfg.Reach == "public" {
		return inst.PublicIP
	}
	if inst.PrivateIP != "" {
		return inst.PrivateIP
	}
	return inst.PublicIP
}

// Describe lists instances matching filters. Filter keys follow the
// "label:<key>"→value convention (rendered as a `labels.<key>=<value>` gcloud
// filter clause) plus the special key "status" for a comma-separated list of
// GCE instance statuses (rendered as an OR'd status= clause); all clauses are
// AND'd together.
func (c *Client) Describe(ctx context.Context, filters map[string]string) ([]Instance, error) {
	argv := []string{"gcloud", "compute", "instances", "list", "--project", c.cfg.Project, "--format", "json"}
	if expr := filterExpr(filters); expr != "" {
		argv = append(argv, "--filter", expr)
	}
	res, err := c.run(ctx, argv)
	if err != nil {
		return nil, fmt.Errorf("gcloud instances list: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("gcloud instances list exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	var out []instanceJSON
	if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil {
		return nil, fmt.Errorf("parse instances list output: %w", err)
	}
	instances := make([]Instance, 0, len(out))
	for _, ij := range out {
		instances = append(instances, ij.toInstance())
	}
	return instances, nil
}

// Terminate deletes ids (idempotent): empty ids is a no-op, and an
// already-gone instance is not an error. All ids are assumed to live in the
// driver's single configured zone — true for RunInstances' own launches and
// its rollback. Reap paths, whose instances can span the region, use
// terminateInstances (per-instance zone) instead.
func (c *Client) Terminate(ctx context.Context, ids []string) error {
	return c.terminateInZone(ctx, c.cfg.Zone, ids)
}

// terminateInstances deletes instances in THEIR OWN zones. Reap turns up
// instances placed anywhere in the region (a YBA GCP-provider universe spreads
// its nodes across zones), and gcloud's `instances delete` takes a single
// --zone, so this groups by each instance's actual zone and issues one delete
// per zone. An instance whose zone is unknown (a describe that omitted it)
// falls back to the configured zone. Idempotent per zone.
func (c *Client) terminateInstances(ctx context.Context, instances []Instance) error {
	byZone := map[string][]string{}
	for _, inst := range instances {
		if inst.ID == "" {
			continue
		}
		zone := inst.Zone
		if zone == "" {
			zone = c.cfg.Zone
		}
		byZone[zone] = append(byZone[zone], inst.ID)
	}
	for _, zone := range sortedZoneKeys(byZone) {
		if err := c.terminateInZone(ctx, zone, byZone[zone]); err != nil {
			return err
		}
	}
	return nil
}

// terminateInZone deletes ids in one zone (idempotent): empty ids is a no-op,
// and an already-gone instance is not an error.
func (c *Client) terminateInZone(ctx context.Context, zone string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	argv := append([]string{"gcloud", "compute", "instances", "delete",
		"--project", c.cfg.Project, "--zone", zone, "--quiet"}, ids...)
	res, err := c.run(ctx, argv)
	if err != nil {
		return fmt.Errorf("gcloud instances delete: %w", err)
	}
	if res.ExitCode != 0 {
		if strings.Contains(res.Stderr, "was not found") || strings.Contains(res.Stderr, "Not Found") {
			return nil
		}
		return fmt.Errorf("gcloud instances delete exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// nonTerminalStatuses lists every GCE instance status delete-worthy for reap
// (everything except TERMINATED, which is already gone).
const nonTerminalStatuses = "provisioning,staging,running,repairing,stopping,suspending,suspended"

// ReapByTag terminates every non-terminated instance labeled
// ampqa-run=<runTag> — the safety net that guarantees a run's instances can
// always be cleaned up even if the normal teardown path was missed. Discovery
// (Describe) is region-wide and termination is per-instance-zone, so this reaps
// nodes in ANY zone of the region — critical for a YBA GCP-provider universe,
// whose nodes YBA places across the region's zones, not the one configured zone.
func (c *Client) ReapByTag(ctx context.Context, runTag string) error {
	_, err := c.ReapByTagConfirmed(ctx, runTag)
	return err
}

// ReapByTagConfirmed is ReapByTag plus the COUNT of matching instances it acted
// on, so a reap in a possibly-divergent region/credential context can tell
// "reaped N instances" (positive confirmation) apart from "described ZERO" —
// which means the reap looked in the wrong place and must not count as a reclaim.
func (c *Client) ReapByTagConfirmed(ctx context.Context, runTag string) (int, error) {
	if runTag == "" {
		return 0, fmt.Errorf("reap-by-tag: empty run tag")
	}
	instances, err := c.Describe(ctx, map[string]string{
		"label:ampqa-run": sanitizeLabelValue(runTag),
		"status":          nonTerminalStatuses,
	})
	if err != nil {
		return 0, err
	}
	if err := c.terminateInstances(ctx, instances); err != nil {
		return 0, err
	}
	return len(instances), nil
}

// ReapStale terminates any running ampqa=1 autoclean=true instance whose
// creation time is older than olderThan — the age-based safety net for
// instances whose run tag is unknown. Like ReapByTag it is region-wide: stale
// instances are deleted in their own zones, wherever in the region they landed.
func (c *Client) ReapStale(ctx context.Context, olderThan time.Duration) error {
	instances, err := c.Describe(ctx, map[string]string{
		"label:ampqa":     "1",
		"label:autoclean": "true",
		"status":          nonTerminalStatuses,
	})
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-olderThan)
	var stale []Instance
	for _, inst := range instances {
		if !inst.LaunchTime.IsZero() && inst.LaunchTime.Before(cutoff) {
			stale = append(stale, inst)
		}
	}
	return c.terminateInstances(ctx, stale)
}

// Connect opens an SSH RemoteExecutor to an instance's reach IP using the
// configured ssh_user and key_file. ctx bounds the dial only; the returned
// Conn outlives it, so the caller owns Close.
func (c *Client) Connect(ctx context.Context, inst Instance) (Conn, error) {
	host := c.reachIP(inst)
	if host == "" {
		return nil, fmt.Errorf("connect: instance %s has no reachable IP", inst.ID)
	}
	return c.dialSSH(ctx, sshx.Config{
		Host:    host,
		User:    c.cfg.SSHUser,
		KeyPath: c.cfg.KeyFile,
	})
}

// --- helpers ---

type instanceJSON struct {
	Name              string `json:"name"`
	Status            string `json:"status"`
	Zone              string `json:"zone"` // a full zone URL; shortZone extracts the name
	CreationTimestamp string `json:"creationTimestamp"`
	NetworkInterfaces []struct {
		NetworkIP     string `json:"networkIP"`
		AccessConfigs []struct {
			NatIP string `json:"natIP"`
		} `json:"accessConfigs"`
	} `json:"networkInterfaces"`
}

func (ij instanceJSON) toInstance() Instance {
	inst := Instance{ID: ij.Name, State: ij.Status, Zone: shortZone(ij.Zone)}
	if len(ij.NetworkInterfaces) > 0 {
		ni := ij.NetworkInterfaces[0]
		inst.PrivateIP = ni.NetworkIP
		if len(ni.AccessConfigs) > 0 {
			inst.PublicIP = ni.AccessConfigs[0].NatIP
		}
	}
	if t, err := time.Parse(time.RFC3339Nano, ij.CreationTimestamp); err == nil {
		inst.LaunchTime = t
	}
	return inst
}

// shortZone extracts the zone NAME from gcloud's fully-qualified zone URL
// (".../zones/us-west1-a" → "us-west1-a"); a bare name or empty string is
// returned unchanged.
func shortZone(zoneURL string) string {
	if i := strings.LastIndex(zoneURL, "/"); i >= 0 {
		return zoneURL[i+1:]
	}
	return zoneURL
}

// sortedZoneKeys returns a map's keys in deterministic order (stable argv for
// per-zone delete calls). Insertion sort keeps the package sort-import-free,
// matching sortedKeys; the zone set is tiny.
func sortedZoneKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}

func instanceIDs(instances []Instance) []string {
	ids := make([]string, 0, len(instances))
	for _, inst := range instances {
		if inst.ID != "" {
			ids = append(ids, inst.ID)
		}
	}
	return ids
}

// mandatory ownership labels baked into every instance (AGENTS.md §8). yb_owner
// comes from Config.Owner (see FromConfig); the rest mirror aws's tags exactly
// so ownership reads identically across clouds.
const (
	labelTask = "stress-dev"
	labelDept = "qa"
)

func mandatoryLabels(runTag, owner string) map[string]string {
	return map[string]string{
		"yb_owner":  sanitizeLabelValue(owner),
		"yb_task":   labelTask,
		"yb_dept":   labelDept,
		"autoclean": "true",
		"ampqa":     "1",
		"ampqa-run": sanitizeLabelValue(runTag),
	}
}

// labelsFlag renders labels as the comma-separated `--labels` value, in
// deterministic (sorted-key) order.
func labelsFlag(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for _, k := range sortedKeys(labels) {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ",")
}

// sanitizeLabelValue enforces GCP label VALUE rules: lowercase letters,
// digits, underscores, dashes only; max 63 chars. (Only label KEYS must start
// with a letter; every key this app uses already does.)
func sanitizeLabelValue(v string) string {
	v = strings.ToLower(v)
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	s := b.String()
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}

// sanitizeGCEName enforces GCE instance NAME rules: lowercase letters, digits,
// dashes only; must start with a letter; must not end with a dash; max 63
// chars.
func sanitizeGCEName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	if out == "" || out[0] < 'a' || out[0] > 'z' {
		out = "i-" + out
		if len(out) > 63 {
			out = out[:63]
		}
	}
	return out
}

// filterExpr renders filters into a gcloud `--filter` boolean expression.
func filterExpr(filters map[string]string) string {
	if len(filters) == 0 {
		return ""
	}
	var parts []string
	for _, k := range sortedKeys(filters) {
		v := filters[k]
		switch {
		case k == "status":
			var alt []string
			for _, s := range strings.Split(v, ",") {
				alt = append(alt, fmt.Sprintf("status=%s", strings.ToUpper(strings.TrimSpace(s))))
			}
			parts = append(parts, "("+strings.Join(alt, " OR ")+")")
		case strings.HasPrefix(k, "label:"):
			parts = append(parts, fmt.Sprintf("labels.%s=%s", strings.TrimPrefix(k, "label:"), v))
		default:
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
		}
	}
	return strings.Join(parts, " AND ")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// simple insertion sort keeps generated argv stable without importing sort
	// twice across the package; small maps only.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}

// expandHome expands a leading ~ to the current user's home directory (mirrors
// exec/ssh's own key-path expansion; duplicated here in this tiny, local form
// rather than importing core, keeping this app free of a core import).
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if u, err := user.Current(); err == nil {
			if p == "~" {
				return u.HomeDir
			}
			return filepath.Join(u.HomeDir, p[2:])
		}
	}
	return p
}

// osReadPubKey reads a public key file's contents from disk (coordination
// only — the file itself is the operator's already-existing key).
func osReadPubKey(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// osRun shells the gcloud CLI locally (coordination only — API calls to GCP).
// When an explicit service account is configured its key file is injected
// into the child process environment (never argv, never logged); otherwise
// the child inherits the ambient gcloud auth chain.
func (c *Client) osRun(ctx context.Context, argv []string) (execpkg.Result, error) {
	if len(argv) == 0 {
		return execpkg.Result{}, fmt.Errorf("empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if extra := c.cfg.authEnv(); len(extra) > 0 {
		cmd.Env = append(os.Environ(), extra...)
	}
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	res := execpkg.Result{Stdout: out.String(), Stderr: errb.String()}
	if ctx.Err() != nil {
		res.ExitCode = -1
		return res, ctx.Err()
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		res.ExitCode = -1
		return res, err
	}
	return res, nil
}

func defaultSSHDial(ctx context.Context, cfg sshx.Config) (Conn, error) {
	return sshx.Dial(ctx, cfg)
}

func tcpProbe(ctx context.Context, host string, port int) error {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprintf("%d", port)))
	if err != nil {
		return err
	}
	return conn.Close()
}
