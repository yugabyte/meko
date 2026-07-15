// Package aws is the EC2 instance-lifecycle app: it launches, waits on,
// describes, connects to, and terminates AWS instances by shelling the
// ambient-authed `aws` CLI — the same design choice as the kubectl/helm/docker
// transports (no aws-sdk-go dependency in this standalone module). The local
// os/exec here is coordination only (issuing API calls to AWS), not running any
// workload on the operator's machine.
//
// Hygiene is the point of this app (AGENTS.md §8): it reuses the shared
// `yb-dev-aws-2` keypair and NEVER creates one, bakes the mandatory ownership
// tags (yb_owner/yb_task/yb_dept/autoclean plus ampqa/ampqa-run) into every
// instance, and offers ReapByTag/ReapStale so leaked instances can always be
// found and terminated.
package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	execpkg "github.com/yugabyte/yb-amp/test/system/framework/exec"
	sshx "github.com/yugabyte/yb-amp/test/system/framework/exec/ssh"
)

// Canonical AWS test-instance settings (AWS_TEST_INSTANCES.md — the shared
// us-west-2 dev VPC). These are genuine defaults so an awsvm env is usable out
// of the box; core/defaults_test.go asserts default.yaml agrees.
const (
	DefaultRegion       = "us-west-2"
	DefaultSubnet       = "subnet-11798e59"
	DefaultKeyName      = "yb-dev-aws-2"
	DefaultKeyFile      = "~/.ssh/yb-dev-aws-2.pem"
	DefaultAMI          = "ami-0eb514f4c0f885334" // AlmaLinux 9 ARM (aarch64)
	DefaultInstanceType = "m7g.xlarge"
	DefaultSSHUser      = "ec2-user"
	DefaultVolumeSizeGB = 100

	// DefaultOwner is the yb_owner tag / name-prefix value when neither the
	// top-level `owner` config key nor apps.cloud.aws.name_prefix is set. A
	// neutral default so the framework never bakes in a specific person's
	// username; override per run with -o owner=<name>.
	DefaultOwner = "ampqa"

	// DefaultReach is "private": the dev VPC's security groups allow inbound
	// SSH only from the tailnet/VPC CIDRs, not the public internet, so the
	// coordinator MUST reach instances over their private IP. "public" is an
	// opt-out for environments where the public IP genuinely is reachable.
	DefaultReach = "private"

	// DefaultSSHWaitTimeout bounds WaitSSH so a host on an unreachable network
	// path (wrong reach setting, SG misconfiguration) fails fast and lets
	// teardown reclaim the instance, instead of hanging for the caller's whole
	// (much longer) provision deadline.
	DefaultSSHWaitTimeout = 4 * time.Minute
)

// DefaultSecurityGroups is the canonical SG set for the shared dev VPC.
var DefaultSecurityGroups = []string{
	"sg-0427247c274837cdc",
	"sg-0ab0af5e93728f73a",
	"sg-05b6d45d3ab93c852",
	"sg-0271332bdd9fa1313",
}

// Mandatory ownership tags baked into every instance. Name and ampqa-run are
// per-run; yb_owner comes from Config.Owner (see FromConfig); the rest are
// fixed. An instance missing these is subject to account cleanup — tagging is
// not optional (AGENTS.md §8).
const (
	tagTask = "stress-dev"
	tagDept = "qa"
)

// Config is the apps.cloud.aws.* configuration.
type Config struct {
	Region         string
	Subnet         string
	SecurityGroups []string
	KeyName        string // existing keypair — NEVER minted by this app
	KeyFile        string // private key path for SSH (~ expanded by exec/ssh)
	AMI            string
	InstanceType   string
	SSHUser        string
	// Owner is stamped as the yb_owner tag on every instance (see
	// mandatoryTags) and, when NamePrefix is unset, used as the name prefix.
	Owner string
	// NamePrefix prefixes every launched instance's Name tag: "<prefix>-<run>-
	// <n>". "" (the default) falls back to Owner.
	NamePrefix string
	// Reach selects the address used for SSH/coordinator traffic: "private"
	// (default) uses the tailnet/VPC private IP the security groups actually
	// allow inbound; "public" opts into the public IP instead.
	Reach string
	// SSHWaitTimeout bounds WaitSSH (see DefaultSSHWaitTimeout).
	SSHWaitTimeout time.Duration
	// AccessKey/SecretKey are OPTIONAL explicit static credentials, already
	// RESOLVED from named secrets by the caller (see ResolveCreds). Both empty
	// (the default) → the `aws` CLI uses its ambient credential chain (env,
	// profile, instance role) exactly as before. When set they are exported as
	// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY into the shelled aws process's
	// environment only — never placed on argv and never logged.
	AccessKey string
	SecretKey string
}

// ConfigGetter is the subset of *core.Config this package reads. Taking an
// interface keeps apps/cloud/aws free of a core import (the apps/* convention).
type ConfigGetter interface {
	GetString(key, def string) string
	GetStringSlice(key string, def []string) []string
	GetDuration(key string, def time.Duration) time.Duration
}

// SecretResolver maps a secret NAME to its resolved value. It matches
// core.Secrets.Value; taking a func keeps this package free of a core import.
type SecretResolver func(name string) (string, error)

// ResolveCreds reads apps.cloud.aws.access_key_secret / secret_key_secret
// (secret NAMES, not values) from c and resolves them via resolve into explicit
// static credentials for Config.AccessKey/SecretKey. Both names empty (the
// default) → ("", "", nil): keep the ambient credential chain. Setting exactly
// one is a configuration error — partial static credentials never authenticate.
// A named secret that fails to resolve is an error too: explicit creds were
// requested, so the caller must NOT silently fall back to ambient.
func ResolveCreds(c ConfigGetter, resolve SecretResolver) (accessKey, secretKey string, err error) {
	accessName := c.GetString("apps.cloud.aws.access_key_secret", "")
	secretName := c.GetString("apps.cloud.aws.secret_key_secret", "")
	if accessName == "" && secretName == "" {
		return "", "", nil
	}
	if accessName == "" || secretName == "" {
		return "", "", fmt.Errorf("aws static credentials: set BOTH apps.cloud.aws.access_key_secret and secret_key_secret, or neither (got access=%q secret=%q)", accessName, secretName)
	}
	if resolve == nil {
		return "", "", fmt.Errorf("aws static credentials: access_key_secret/secret_key_secret set but no secret resolver available")
	}
	if accessKey, err = resolve(accessName); err != nil {
		return "", "", fmt.Errorf("aws access_key_secret %q: %w", accessName, err)
	}
	if secretKey, err = resolve(secretName); err != nil {
		return "", "", fmt.Errorf("aws secret_key_secret %q: %w", secretName, err)
	}
	return accessKey, secretKey, nil
}

// FromConfig reads apps.cloud.aws.* into a Config, applying the canonical
// defaults.
func FromConfig(c ConfigGetter) Config {
	owner := c.GetString("owner", DefaultOwner)
	return Config{
		Region:         c.GetString("apps.cloud.aws.region", DefaultRegion),
		Subnet:         c.GetString("apps.cloud.aws.subnet", DefaultSubnet),
		SecurityGroups: c.GetStringSlice("apps.cloud.aws.security_groups", DefaultSecurityGroups),
		KeyName:        c.GetString("apps.cloud.aws.key_name", DefaultKeyName),
		KeyFile:        c.GetString("apps.cloud.aws.key_file", DefaultKeyFile),
		AMI:            c.GetString("apps.cloud.aws.ami", DefaultAMI),
		InstanceType:   c.GetString("apps.cloud.aws.instance_type", DefaultInstanceType),
		SSHUser:        c.GetString("apps.cloud.aws.ssh_user", DefaultSSHUser),
		Owner:          owner,
		NamePrefix:     c.GetString("apps.cloud.aws.name_prefix", owner),
		Reach:          c.GetString("apps.cloud.aws.reach", DefaultReach),
		SSHWaitTimeout: c.GetDuration("apps.cloud.aws.ssh_wait_timeout", DefaultSSHWaitTimeout),
	}
}

// credEnv returns the AWS credential environment variables to inject into the
// shelled aws process, or nil when no explicit static creds are configured (the
// ambient chain is used). Both keys are set together — partial creds are never
// injected (ResolveCreds already rejects a half-configured pair).
func (c Config) credEnv() []string {
	if c.AccessKey == "" || c.SecretKey == "" {
		return nil
	}
	return []string{
		"AWS_ACCESS_KEY_ID=" + c.AccessKey,
		"AWS_SECRET_ACCESS_KEY=" + c.SecretKey,
	}
}

func (c Config) withDefaults() Config {
	if c.Region == "" {
		c.Region = DefaultRegion
	}
	if c.Subnet == "" {
		c.Subnet = DefaultSubnet
	}
	if len(c.SecurityGroups) == 0 {
		c.SecurityGroups = DefaultSecurityGroups
	}
	if c.KeyName == "" {
		c.KeyName = DefaultKeyName
	}
	if c.KeyFile == "" {
		c.KeyFile = DefaultKeyFile
	}
	if c.AMI == "" {
		c.AMI = DefaultAMI
	}
	if c.InstanceType == "" {
		c.InstanceType = DefaultInstanceType
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

// Runner runs one `aws` invocation to completion. Injected in tests to record
// argv and return canned output; the default shells out via os/exec.
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

// Instance is one EC2 instance the app manages.
type Instance struct {
	ID         string
	PublicIP   string
	PrivateIP  string
	State      string
	LaunchTime time.Time
}

// Client drives EC2 instance lifecycle over the aws CLI.
type Client struct {
	cfg     Config
	run     Runner
	dialSSH sshDialer
	probe   portProbe
	logf    func(format string, args ...any)
}

// Option customizes a Client.
type Option func(*Client)

// WithRunner overrides the aws-CLI runner (tests inject a scripted recorder).
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

// New builds a Client from cfg (defaults applied).
func New(cfg Config, opts ...Option) *Client {
	c := &Client{
		cfg:     cfg.withDefaults(),
		dialSSH: defaultSSHDial,
		probe:   tcpProbe,
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
	RunTag       string // stamped as ampqa-run=<RunTag> and into every Name
	InstanceType string // "" → cfg.InstanceType
	NamePrefix   string // "" → cfg.NamePrefix (Owner-derived)
	VolumeSizeGB int    // 0 → DefaultVolumeSizeGB
}

// RunInstances launches spec.Count instances (one aws call each, so every
// instance gets its own indexed Name tag), each with a public IP (egress only —
// outbound internet for artifact downloads; the dev VPC's security groups do
// not allow inbound from the public internet, so reachIP is what SSH/coordinator
// traffic actually uses), the SHARED key_name (never a freshly-minted keypair),
// and the mandatory ownership tags. On a partial failure it best-effort
// terminates whatever already launched before returning, so a failed provision
// never leaks.
func (c *Client) RunInstances(ctx context.Context, spec RunSpec) ([]Instance, error) {
	if spec.Count <= 0 {
		return nil, fmt.Errorf("run-instances: count must be > 0, got %d", spec.Count)
	}
	if spec.RunTag == "" {
		return nil, fmt.Errorf("run-instances: RunTag is required (it roots tagging and reap)")
	}
	instances := make([]Instance, 0, spec.Count)
	for i := 0; i < spec.Count; i++ {
		inst, err := c.runOne(ctx, spec, i)
		if err != nil {
			if len(instances) > 0 {
				ids := instanceIDs(instances)
				if terr := c.Terminate(ctx, ids); terr != nil {
					c.log("aws: rollback terminate of %v after launch failure (best-effort): %v", ids, terr)
				}
			}
			return nil, fmt.Errorf("run-instances #%d: %w", i, err)
		}
		instances = append(instances, inst)
	}
	return instances, nil
}

func (c *Client) runOne(ctx context.Context, spec RunSpec, i int) (Instance, error) {
	instanceType := spec.InstanceType
	if instanceType == "" {
		instanceType = c.cfg.InstanceType
	}
	prefix := spec.NamePrefix
	if prefix == "" {
		prefix = c.cfg.NamePrefix
	}
	volume := spec.VolumeSizeGB
	if volume == 0 {
		volume = DefaultVolumeSizeGB
	}
	name := fmt.Sprintf("%s-%s-%d", prefix, spec.RunTag, i)

	argv := []string{
		"aws", "ec2", "run-instances",
		"--region", c.cfg.Region,
		"--image-id", c.cfg.AMI,
		"--instance-type", instanceType,
		"--key-name", c.cfg.KeyName, // reuse the shared keypair — NEVER create one
		"--subnet-id", c.cfg.Subnet,
		"--associate-public-ip-address",
		"--count", "1",
		"--block-device-mappings", blockDeviceMapping(volume),
		"--tag-specifications", tagSpecification(mandatoryTags(name, spec.RunTag, c.cfg.Owner)),
		"--output", "json",
	}
	// security-group-ids takes a space-separated list as distinct argv elements.
	argv = append(argv, "--security-group-ids")
	argv = append(argv, c.cfg.SecurityGroups...)

	res, err := c.run(ctx, argv)
	if err != nil {
		return Instance{}, err
	}
	if res.ExitCode != 0 {
		return Instance{}, fmt.Errorf("aws run-instances exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	var out struct {
		Instances []instanceJSON `json:"Instances"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil {
		return Instance{}, fmt.Errorf("parse run-instances output: %w", err)
	}
	if len(out.Instances) == 0 {
		return Instance{}, fmt.Errorf("run-instances returned no instance")
	}
	return out.Instances[0].toInstance(), nil
}

// WaitRunning blocks until every id reaches the running state via
// `aws ec2 wait instance-running`. Empty ids is a no-op.
func (c *Client) WaitRunning(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	argv := append([]string{"aws", "ec2", "wait", "instance-running", "--region", c.cfg.Region, "--instance-ids"}, ids...)
	res, err := c.run(ctx, argv)
	if err != nil {
		return fmt.Errorf("aws wait instance-running: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("aws wait instance-running exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// WaitSSH polls each instance's reach IP (reachIP — the private IP by default)
// :22 until it accepts a connection, ctx is cancelled, or its own
// SSHWaitTimeout budget passes — whichever is soonest, so an unreachable host
// (wrong reach setting, SG misconfiguration) fails fast instead of hanging for
// the caller's whole provision deadline. keyFile must be set — SSH access is
// impossible without it, so a missing key is a configuration error surfaced
// here rather than as an opaque dial failure later.
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
		if err := c.waitOneSSH(wctx, inst, host); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) waitOneSSH(ctx context.Context, inst Instance, host string) error {
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
		last = c.probe(ctx, host, 22)
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// reachIP returns the address used to reach inst for SSH/coordinator traffic.
// The dev VPC's security groups allow inbound only from tailnet/VPC CIDRs, so
// the default ("private" or unset) is the private IP; "public" opts into the
// public IP (which otherwise exists purely for the instance's own outbound
// internet access — artifact downloads — not for inbound reach). Falls back to
// the public IP if the private IP is unset, so a host describe race never
// blocks connecting outright.
func (c *Client) reachIP(inst Instance) string {
	if c.cfg.Reach == "public" {
		return inst.PublicIP
	}
	if inst.PrivateIP != "" {
		return inst.PrivateIP
	}
	return inst.PublicIP
}

// Describe lists instances matching filters (Name→Value, joined into
// `--filters Name=<k>,Values=<v>`). Terminated instances are included by AWS
// unless a state filter excludes them; callers pass instance-state-name to
// scope.
func (c *Client) Describe(ctx context.Context, filters map[string]string) ([]Instance, error) {
	argv := []string{"aws", "ec2", "describe-instances", "--region", c.cfg.Region, "--output", "json"}
	if len(filters) > 0 {
		argv = append(argv, "--filters")
		for _, k := range sortedKeys(filters) {
			argv = append(argv, fmt.Sprintf("Name=%s,Values=%s", k, filters[k]))
		}
	}
	res, err := c.run(ctx, argv)
	if err != nil {
		return nil, fmt.Errorf("aws describe-instances: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("aws describe-instances exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	var out struct {
		Reservations []struct {
			Instances []instanceJSON `json:"Instances"`
		} `json:"Reservations"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil {
		return nil, fmt.Errorf("parse describe-instances output: %w", err)
	}
	var instances []Instance
	for _, r := range out.Reservations {
		for _, ij := range r.Instances {
			instances = append(instances, ij.toInstance())
		}
	}
	return instances, nil
}

// Terminate terminates ids (idempotent): empty ids is a no-op, and an
// already-gone instance (InvalidInstanceID.NotFound) is not an error.
func (c *Client) Terminate(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	argv := append([]string{"aws", "ec2", "terminate-instances", "--region", c.cfg.Region, "--output", "json", "--instance-ids"}, ids...)
	res, err := c.run(ctx, argv)
	if err != nil {
		return fmt.Errorf("aws terminate-instances: %w", err)
	}
	if res.ExitCode != 0 {
		if strings.Contains(res.Stderr, "InvalidInstanceID.NotFound") {
			return nil
		}
		return fmt.Errorf("aws terminate-instances exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// ReapByTag terminates every non-terminated instance tagged ampqa-run=<runTag>
// — the safety net that guarantees a run's instances can always be cleaned up
// even if the normal teardown path was missed.
func (c *Client) ReapByTag(ctx context.Context, runTag string) error {
	_, err := c.ReapByTagConfirmed(ctx, runTag)
	return err
}

// ReapByTagConfirmed is ReapByTag plus the COUNT of matching instances it acted
// on. A caller reaping in a possibly-divergent region/credential context uses
// the count to tell "reaped N instances" (positive confirmation) apart from
// "described ZERO" — which, when the region or creds may be defaulted, means the
// reap looked in the WRONG place and must NOT be treated as a successful reclaim.
func (c *Client) ReapByTagConfirmed(ctx context.Context, runTag string) (int, error) {
	if runTag == "" {
		return 0, fmt.Errorf("reap-by-tag: empty run tag")
	}
	instances, err := c.Describe(ctx, map[string]string{
		"tag:ampqa-run":       runTag,
		"instance-state-name": "pending,running,stopping,stopped",
	})
	if err != nil {
		return 0, err
	}
	ids := instanceIDs(instances)
	if err := c.Terminate(ctx, ids); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// ReapStale terminates any running ampqa=1 autoclean=true instance whose launch
// time is older than olderThan — the age-based safety net for instances whose
// run tag is unknown (e.g. an operator lost track of a run).
func (c *Client) ReapStale(ctx context.Context, olderThan time.Duration) error {
	instances, err := c.Describe(ctx, map[string]string{
		"tag:ampqa":           "1",
		"tag:autoclean":       "true",
		"instance-state-name": "pending,running,stopping,stopped",
	})
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-olderThan)
	var stale []string
	for _, inst := range instances {
		if !inst.LaunchTime.IsZero() && inst.LaunchTime.Before(cutoff) {
			stale = append(stale, inst.ID)
		}
	}
	return c.Terminate(ctx, stale)
}

// Connect opens an SSH RemoteExecutor to an instance's reach IP (reachIP — the
// private IP by default) using the configured ssh_user and key_file. ctx bounds
// the dial only; the returned Conn outlives it (exec/ssh.Dial's contract), so
// the caller owns Close.
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
	InstanceID       string    `json:"InstanceId"`
	PublicIPAddress  string    `json:"PublicIpAddress"`
	PrivateIPAddress string    `json:"PrivateIpAddress"`
	LaunchTime       time.Time `json:"LaunchTime"`
	State            struct {
		Name string `json:"Name"`
	} `json:"State"`
}

func (ij instanceJSON) toInstance() Instance {
	return Instance{
		ID:         ij.InstanceID,
		PublicIP:   ij.PublicIPAddress,
		PrivateIP:  ij.PrivateIPAddress,
		State:      ij.State.Name,
		LaunchTime: ij.LaunchTime,
	}
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

type kv struct{ Key, Value string }

func mandatoryTags(name, runTag, owner string) []kv {
	return []kv{
		{"Name", name},
		{"yb_owner", owner},
		{"yb_task", tagTask},
		{"yb_dept", tagDept},
		{"autoclean", "true"},
		{"ampqa", "1"},
		{"ampqa-run", runTag},
	}
}

// tagSpecification renders the --tag-specifications value for run-instances.
func tagSpecification(tags []kv) string {
	parts := make([]string, len(tags))
	for i, t := range tags {
		parts[i] = fmt.Sprintf("{Key=%s,Value=%s}", t.Key, t.Value)
	}
	return "ResourceType=instance,Tags=[" + strings.Join(parts, ",") + "]"
}

func blockDeviceMapping(sizeGB int) string {
	return fmt.Sprintf(`[{"DeviceName":"/dev/sda1","Ebs":{"VolumeSize":%d,"VolumeType":"gp3"}}]`, sizeGB)
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

// osRun shells the aws CLI locally (coordination only — API calls to AWS). When
// explicit static credentials are configured they are injected into the child
// process environment (never argv, never logged); otherwise the child inherits
// the ambient credential chain.
func (c *Client) osRun(ctx context.Context, argv []string) (execpkg.Result, error) {
	if len(argv) == 0 {
		return execpkg.Result{}, fmt.Errorf("empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if extra := c.cfg.credEnv(); len(extra) > 0 {
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
