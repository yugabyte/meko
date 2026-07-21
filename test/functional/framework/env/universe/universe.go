// Package universe is the YB-cluster-as-target environment provider
// (architecture: a YugabyteDB universe/cluster used directly as the test
// TARGET, not the amp control plane). Unlike the docker/sharedk8s/exclusivek8s
// providers — which stand up amp — this one exposes a plain YSQL endpoint so pg
// and workloadgen tests run against a real YugabyteDB.
//
// Four modes, selected by env.universe.mode:
//
//   - attach_yba: resolve an EXISTING YBA universe by name/id (apps/yba) and
//     expose its YSQL endpoint. Teardown is a no-op unless env.universe.owned
//     is true (never delete someone else's universe by default).
//   - attach_address: a directly-configured host:port list is the YSQL target
//     (attach to any running YB / yugabyted). Teardown is always a no-op.
//   - provision_yugabyted: the framework brings up a fresh yugabyted cluster on
//     configured ssh nodes (apps/yugabyted), exposes its endpoint, and Destroys
//     it on teardown.
//   - provision_yba: the framework stands up its OWN YBA in k8s (apps/ybastack),
//     creates an AWS provider + a universe on it (apps/yba), and exposes that
//     universe's YSQL endpoint. This mode is always owned: teardown deletes the
//     universe (terminating its cloud nodes) THEN tears the YBA down, LIFO. Every
//     created resource is registered for cleanup the instant it exists, so a
//     partial provision (YBA up, universe-create failed) still tears the YBA
//     down — the incremental cleanup is rooted in context.Background() so a
//     cancelled provision cleans up regardless. Requires the provision deps
//     wired via WithProvisionYBA (apps.ybastack.* / apps.yba.* + a secret
//     resolver).
//
// The env implements the PGEndpoint accessor convention (docker/sharedk8s) so
// pg-based tests type-assert to it; there is no amp controller, so
// ControllerURL() is "".
package universe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	osexec "os/exec"
	"strconv"
	"strings"
	"time"

	awscloud "github.com/yugabyte/amp/test/functional/framework/apps/cloud/aws"
	gcpcloud "github.com/yugabyte/amp/test/functional/framework/apps/cloud/gcp"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/yba"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybastack"
	"github.com/yugabyte/amp/test/functional/framework/apps/yugabyted"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
	sshx "github.com/yugabyte/amp/test/functional/framework/exec/ssh"
)

// Modes.
const (
	ModeAttachYBA          = "attach_yba"
	ModeAttachAddress      = "attach_address"
	ModeProvisionYugabyted = "provision_yugabyted"
	ModeProvisionYBA       = "provision_yba"
)

// Defaults.
const (
	DefaultYSQLPort = 5433
	ybUser          = "yugabyte"
	ybDatabase      = "yugabyte"

	// provisionCleanupBudget bounds the incremental cleanup that runs when a
	// provision_yba flow fails partway (YBA up, universe-create failed). It is
	// rooted in context.Background() so a cancelled provision still cleans up.
	provisionCleanupBudget = 20 * time.Minute
	// provisionTeardownBudget bounds a full provision_yba Env teardown (delete
	// universe + tear the YBA down), independent of the caller's ctx.
	provisionTeardownBudget = 25 * time.Minute
	// provisionUniverseReadyTimeout bounds the wait for a created universe to
	// become healthy — an RF3 create (provision nodes → install software → start
	// master/tserver → WaitForMasterLeader/TServerHeartBeats → UniverseUpdate
	// Succeeded) is ~15-25 min, so allow generous headroom.
	provisionUniverseReadyTimeout = 45 * time.Minute
)

// Config is the env.universe.* configuration.
type Config struct {
	Mode  string
	Owned bool // attach_yba: true → Teardown deletes the universe

	// attach_yba
	YBAURL      string
	YBAToken    string
	YBACustomer string
	UniverseRef string // universe name or UUID
	InsecureTLS bool   // YBA is commonly self-signed

	// attach_address
	Addresses []string // "host:port" list (or bare "host" → default port)

	// provision_yugabyted
	Hosts      []string // ssh + advertise hosts
	SSHUser    string
	SSHKey     string
	SSHPort    int
	InstallDir string
	BaseDir    string
	DataDir    string
	RF         int

	YSQLPort int
	Product  string // fallback product version label
}

// FromConfig reads env.universe.* from a framework Config.
func FromConfig(c *core.Config) Config {
	return Config{
		Mode:  c.GetString("env.universe.mode", ""),
		Owned: c.GetBool("env.universe.owned", false),

		YBAURL:      c.GetString("env.universe.yba.url", ""),
		YBAToken:    c.GetString("env.universe.yba.api_token", ""),
		YBACustomer: c.GetString("env.universe.yba.customer_uuid", ""),
		UniverseRef: c.GetString("env.universe.yba.universe", ""),
		InsecureTLS: c.GetBool("env.universe.yba.insecure_tls", true),

		Addresses: c.GetStringSlice("env.universe.addresses", nil),

		Hosts:      c.GetStringSlice("env.universe.yugabyted.hosts", nil),
		SSHUser:    c.GetString("env.universe.yugabyted.ssh_user", ""),
		SSHKey:     c.GetString("env.universe.yugabyted.ssh_key", ""),
		SSHPort:    c.GetInt("env.universe.yugabyted.ssh_port", 0),
		InstallDir: c.GetString("env.universe.yugabyted.install_dir", ""),
		BaseDir:    c.GetString("env.universe.yugabyted.base_dir", ""),
		DataDir:    c.GetString("env.universe.yugabyted.data_dir", ""),
		RF:         c.GetInt("env.universe.yugabyted.rf", 0),

		YSQLPort: c.GetInt("env.universe.ysql_port", DefaultYSQLPort),
		Product:  c.GetString("versions.product", ""),
	}
}

// Configured reports whether the provider is usable (a mode is set). Used to
// gate provider selection.
func (c Config) Configured() bool { return c.Mode != "" }

func (c Config) ysqlPort() int {
	if c.YSQLPort != 0 {
		return c.YSQLPort
	}
	return DefaultYSQLPort
}

// transport is what provision_yugabyted needs from an ssh connection.
type transport interface {
	exec.RemoteExecutor
	Close() error
}

// ybaStack is the apps/ybastack.Deployer surface the provision_yba path drives:
// stand a YBA up (Deploy → Info) and tear it (namespace + suffixed CRBs) down.
// An interface so unit tests substitute a fake without shelling kubectl/helm.
type ybaStack interface {
	Deploy(ctx context.Context) (ybastack.Info, error)
	Teardown(ctx context.Context) error
}

// ybaProvisioner is the apps/yba.Client subset the provision_yba path drives to
// create a provider + universe on the freshly-deployed YBA and later delete the
// universe. *yba.Client satisfies it; unit tests substitute a fake.
type ybaProvisioner interface {
	EnsureAWSProvider(ctx context.Context, spec yba.ProviderSpec) (string, error)
	EnsureK8sProvider(ctx context.Context, spec yba.K8sProviderSpec) (string, error)
	EnsureGCPProvider(ctx context.Context, spec yba.GCPProviderSpec) (string, error)
	SetYbcEnabledForProvider(ctx context.Context, providerUUID string, enabled bool) error
	EnsureRelease(ctx context.Context, version, arch, tarballURL string) error
	AddInstanceType(ctx context.Context, providerUUID string, spec yba.InstanceTypeSpec) error
	ProviderAccessKeyCode(ctx context.Context, providerUUID string) (string, error)
	RegionUUIDsForCodes(ctx context.Context, providerUUID string, codes ...string) ([]string, error)
	CreateUniverseFromSpec(ctx context.Context, s yba.UniverseSpec) (yba.TaskRef, error)
	WaitTask(ctx context.Context, taskUUID string) (yba.TaskStatus, error)
	WaitUniverseReady(ctx context.Context, ref string, timeout time.Duration) error
	GetUniverse(ctx context.Context, ref string) (yba.Universe, error)
	ForceDeleteUniverseAndWait(ctx context.Context, universeID string) (yba.TaskRef, error)
}

// Provider provisions/attaches YB-universe targets.
type Provider struct {
	cfg  Config
	logf func(format string, args ...any)

	// Injected in tests; defaults wired in New.
	newYBA    func(cfg Config) *yba.Client
	dial      func(ctx context.Context, cfg Config, host string) (transport, error)
	waitPG    func(ctx context.Context, ci pg.ConnInfo, timeout time.Duration) error
	detectVer func(ctx context.Context, ci pg.ConnInfo) string

	// provision_yba deps. provisionCfg/resolveSecret are wired via
	// WithProvisionYBA (nil for the other modes); the seams below default to the
	// real ybastack/yba constructors + spec builders and are overridden by unit
	// tests to run the flow fake-only (no disk, no network).
	//
	// providerType selects the universe backing cloud: "aws" (default — VM nodes
	// via an EC2 provider), "gcp" (VM nodes on GCE), or "kubernetes" (a universe
	// standing up ON the same k8s cluster the YBA runs in, co-located). It is read
	// from apps.yba.provider_type in WithProvisionYBA.
	provisionCfg         *core.Config
	resolveSecret        func(name string) (string, error)
	providerType         string
	newStack             func(runTag string) (ybaStack, error)
	newProvisioner       func(info ybastack.Info) ybaProvisioner
	buildProviderSpec    func() (yba.ProviderSpec, error)
	buildUniverseSpec    func() (yba.UniverseSpec, error)
	buildK8sProviderSpec func() (yba.K8sProviderSpec, error)
	buildK8sUniverseSpec func() (yba.UniverseSpec, error)
	buildGCPProviderSpec func() (yba.GCPProviderSpec, error)
	buildGCPUniverseSpec func() (yba.UniverseSpec, error)
	// reapNodes terminates every EC2 instance tagged ampqa-run=<runTag> via the
	// provider's own AWS creds — the airtight AWS teardown backstop. A seam so
	// tests inject a fake (no real AWS).
	reapNodes func(ctx context.Context, region, accessKey, secretKey, runTag string) error
	// reapNamespaces deletes any leftover YBA-created universe namespace in the
	// target k8s cluster — the airtight KUBERNETES teardown backstop (the analogue
	// of reapNodes). A seam so tests inject a fake (no real kubectl).
	reapNamespaces func(ctx context.Context, kubeContext, kubeconfig, universeName string) error
	// reapGCENodes deletes every GCE instance labeled ampqa-run=<runTag> in the
	// provider's project/zone via gcloud — the airtight GCP teardown backstop (the
	// analogue of reapNodes). It authenticates gcloud with the SAME service-account
	// key file the provider used. A seam so tests inject a fake (no real gcloud).
	reapGCENodes func(ctx context.Context, project, zone, credsFile, runTag string) error
	// resolveK8sYSQLAddrs returns the tserver POD IPs (as host:port) of a
	// kubernetes universe over the target cluster — the operator-reachable
	// backing endpoint. YBA reports a k8s node's private_ip as an in-cluster
	// address (pod DNS / ClusterIP) the operator can't reach over the tailnet,
	// whereas pod IPs are tailnet-routable (mirrors apps/ybk8s). A seam so tests
	// inject a fake (no real kubectl).
	resolveK8sYSQLAddrs func(ctx context.Context, kubeContext, kubeconfig, universeName string, ysqlPort int) ([]string, error)
}

// Option customizes a Provider.
type Option func(*Provider)

// WithLogf overrides the log sink (default log.Printf). Pass a masked sink so a
// resolved secret never survives into a log line.
func WithLogf(logf func(format string, args ...any)) Option {
	return func(p *Provider) { p.logf = logf }
}

// WithProvisionYBA wires the deps the provision_yba mode needs beyond
// env.universe.*: the raw config (read for apps.ybastack.* / apps.yba.*) and a
// secret resolver (quay pull-secret + AWS creds, by name). The other modes
// ignore it; unit tests inject the narrow newStack/newProvisioner fakes instead.
func WithProvisionYBA(get *core.Config, resolve func(name string) (string, error)) Option {
	return func(p *Provider) {
		p.provisionCfg = get
		p.resolveSecret = resolve
		if get != nil {
			p.providerType = get.GetString("apps.yba.provider_type", "aws")
		}
	}
}

// New builds a Provider from cfg.
func New(cfg Config, opts ...Option) *Provider {
	p := &Provider{
		cfg:          cfg,
		logf:         log.Printf,
		newYBA:       defaultNewYBA,
		dial:         defaultDial,
		waitPG:       pg.WaitReady,
		detectVer:    detectYBVersion,
		providerType: "aws",
	}
	p.newStack = p.defaultNewStack
	p.newProvisioner = p.defaultNewProvisioner
	p.buildProviderSpec = p.defaultBuildProviderSpec
	p.buildUniverseSpec = p.defaultBuildUniverseSpec
	p.buildK8sProviderSpec = p.defaultBuildK8sProviderSpec
	p.buildK8sUniverseSpec = p.defaultBuildK8sUniverseSpec
	p.buildGCPProviderSpec = p.defaultBuildGCPProviderSpec
	p.buildGCPUniverseSpec = p.defaultBuildGCPUniverseSpec
	p.reapNodes = defaultReapNodes
	p.reapNamespaces = defaultReapNamespaces
	p.reapGCENodes = defaultReapGCENodes
	p.resolveK8sYSQLAddrs = defaultResolveK8sYSQLAddrs
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

func defaultNewYBA(cfg Config) *yba.Client {
	hc := &http.Client{Timeout: 60 * time.Second}
	if cfg.InsecureTLS {
		hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // YBA is commonly self-signed; opt-in via config
	}
	return yba.New(cfg.YBAURL, cfg.YBACustomer, cfg.YBAToken, yba.WithHTTPClient(hc))
}

func defaultDial(ctx context.Context, cfg Config, host string) (transport, error) {
	return sshx.Dial(ctx, sshx.Config{
		Host:    host,
		Port:    cfg.SSHPort,
		User:    cfg.SSHUser,
		KeyPath: cfg.SSHKey,
	})
}

// defaultNewStack builds the real ybastack Deployer for the provision_yba mode.
// It needs the config + secret resolver WithProvisionYBA supplied.
func (p *Provider) defaultNewStack(runTag string) (ybaStack, error) {
	if p.provisionCfg == nil {
		return nil, fmt.Errorf("provision_yba: no config wired (call WithProvisionYBA)")
	}
	if p.resolveSecret == nil {
		return nil, fmt.Errorf("provision_yba: no secret resolver wired (call WithProvisionYBA)")
	}
	return ybastack.New(ybastack.FromConfig(p.provisionCfg), runTag, p.resolveSecret).WithLogf(p.logf), nil
}

// defaultNewProvisioner builds a real *yba.Client pointed at the freshly-deployed
// YBA (its URL/customer/token from Info), with an insecure-TLS transport when
// configured (YBA is commonly self-signed).
func (p *Provider) defaultNewProvisioner(info ybastack.Info) ybaProvisioner {
	hc := &http.Client{Timeout: 60 * time.Second}
	if p.cfg.InsecureTLS {
		hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // YBA is commonly self-signed; opt-in via config
	}
	return yba.New(info.URL, info.CustomerUUID, info.APIToken, yba.WithHTTPClient(hc), yba.WithLogf(p.logf))
}

// defaultBuildProviderSpec builds the AWS provider spec from apps.yba.aws.*
// (resolving the AWS creds by name and reading the shared keypair off disk).
func (p *Provider) defaultBuildProviderSpec() (yba.ProviderSpec, error) {
	if p.provisionCfg == nil || p.resolveSecret == nil {
		return yba.ProviderSpec{}, fmt.Errorf("provision_yba: no config/secret resolver wired (call WithProvisionYBA)")
	}
	return yba.BuildAWSProviderSpec(p.provisionCfg, p.resolveSecret)
}

// defaultBuildUniverseSpec builds the universe spec from apps.yba.universe.*.
func (p *Provider) defaultBuildUniverseSpec() (yba.UniverseSpec, error) {
	if p.provisionCfg == nil {
		return yba.UniverseSpec{}, fmt.Errorf("provision_yba: no config wired (call WithProvisionYBA)")
	}
	return yba.BuildUniverseSpec(p.provisionCfg), nil
}

// defaultBuildK8sProviderSpec builds the kubernetes provider spec from
// apps.yba.k8s.* (resolving the kubeconfig + pull-secret by named secret).
func (p *Provider) defaultBuildK8sProviderSpec() (yba.K8sProviderSpec, error) {
	if p.provisionCfg == nil || p.resolveSecret == nil {
		return yba.K8sProviderSpec{}, fmt.Errorf("provision_yba: no config/secret resolver wired (call WithProvisionYBA)")
	}
	return yba.BuildK8sProviderSpec(p.provisionCfg, p.resolveSecret)
}

// defaultBuildK8sUniverseSpec builds the k8s universe spec from apps.yba.k8s.*.
func (p *Provider) defaultBuildK8sUniverseSpec() (yba.UniverseSpec, error) {
	if p.provisionCfg == nil {
		return yba.UniverseSpec{}, fmt.Errorf("provision_yba: no config wired (call WithProvisionYBA)")
	}
	return yba.BuildK8sUniverseSpec(p.provisionCfg), nil
}

// defaultBuildGCPProviderSpec builds the gcp provider spec from apps.yba.gcp.*
// (resolving the service-account credentials by named secret and reading the
// shared keypair off disk when configured).
func (p *Provider) defaultBuildGCPProviderSpec() (yba.GCPProviderSpec, error) {
	if p.provisionCfg == nil || p.resolveSecret == nil {
		return yba.GCPProviderSpec{}, fmt.Errorf("provision_yba: no config/secret resolver wired (call WithProvisionYBA)")
	}
	return yba.BuildGCPProviderSpec(p.provisionCfg, p.resolveSecret)
}

// defaultBuildGCPUniverseSpec builds the gcp universe spec from apps.yba.gcp.*.
func (p *Provider) defaultBuildGCPUniverseSpec() (yba.UniverseSpec, error) {
	if p.provisionCfg == nil {
		return yba.UniverseSpec{}, fmt.Errorf("provision_yba: no config wired (call WithProvisionYBA)")
	}
	return yba.BuildGCPUniverseSpec(p.provisionCfg), nil
}

// Provision stands up (or attaches to) the target per the configured mode.
func (p *Provider) Provision(ctx context.Context, spec core.EnvSpec, seed *core.Seed) (core.Environment, error) {
	switch p.cfg.Mode {
	case ModeAttachYBA:
		return p.provisionAttachYBA(ctx, p.cfg.UniverseRef)
	case ModeAttachAddress:
		return p.provisionAttachAddress()
	case ModeProvisionYugabyted:
		return p.provisionYugabyted(ctx)
	case ModeProvisionYBA:
		return p.provisionYBA(ctx, seed)
	default:
		return nil, fmt.Errorf("env.universe.mode %q unknown (want attach_yba|attach_address|provision_yugabyted|provision_yba)", p.cfg.Mode)
	}
}

func (p *Provider) provisionAttachYBA(ctx context.Context, ref string) (core.Environment, error) {
	if p.cfg.YBAURL == "" || p.cfg.YBACustomer == "" {
		return nil, fmt.Errorf("attach_yba: env.universe.yba.url and customer_uuid are required")
	}
	if ref == "" {
		return nil, fmt.Errorf("attach_yba: env.universe.yba.universe (name or id) is required")
	}
	cl := p.newYBA(p.cfg)
	u, err := cl.GetUniverse(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("attach universe %q: %w", ref, err)
	}
	ci, addrs, err := yba.YSQLConnInfo(u)
	if err != nil {
		return nil, err
	}
	version := u.Version()
	if version == "" {
		version = p.cfg.Product
	}
	return &Env{
		mode:       ModeAttachYBA,
		pg:         ci,
		nodeAddrs:  addrs,
		versions:   core.DetectedVersions{YB: version, Product: p.cfg.Product},
		yba:        cl,
		universeID: u.UniverseUUID,
		owned:      p.cfg.Owned,
		handles:    core.EnvHandles{Endpoint: u.UniverseUUID},
	}, nil
}

func (p *Provider) provisionAttachAddress() (core.Environment, error) {
	if len(p.cfg.Addresses) == 0 {
		return nil, fmt.Errorf("attach_address: env.universe.addresses is required")
	}
	host, port := splitAddr(p.cfg.Addresses[0], p.cfg.ysqlPort())
	if host == "" {
		return nil, fmt.Errorf("attach_address: first address %q has no host", p.cfg.Addresses[0])
	}
	addrs := make([]string, len(p.cfg.Addresses))
	for i, a := range p.cfg.Addresses {
		h, po := splitAddr(a, p.cfg.ysqlPort())
		addrs[i] = net.JoinHostPort(h, strconv.Itoa(po))
	}
	return &Env{
		mode:      ModeAttachAddress,
		pg:        pg.ConnInfo{Host: host, Port: port, User: ybUser, Database: ybDatabase},
		nodeAddrs: addrs,
		versions:  core.DetectedVersions{Product: p.cfg.Product},
		handles:   core.EnvHandles{Endpoint: net.JoinHostPort(host, strconv.Itoa(port))},
	}, nil
}

func (p *Provider) provisionYugabyted(ctx context.Context) (core.Environment, error) {
	if len(p.cfg.Hosts) == 0 {
		return nil, fmt.Errorf("provision_yugabyted: env.universe.yugabyted.hosts is required")
	}
	var transports []transport
	fail := func(e error) (core.Environment, error) {
		for _, tr := range transports {
			_ = tr.Close()
		}
		return nil, e
	}
	nodes := make([]yugabyted.Node, 0, len(p.cfg.Hosts))
	for _, host := range p.cfg.Hosts {
		tr, err := p.dial(ctx, p.cfg, host)
		if err != nil {
			return fail(fmt.Errorf("connect node %s: %w", host, err))
		}
		transports = append(transports, tr)
		nodes = append(nodes, yugabyted.Node{Exec: tr, Advertise: host})
	}
	app := yugabyted.New(nodes, yugabyted.Spec{
		InstallDir: p.cfg.InstallDir,
		BaseDir:    p.cfg.BaseDir,
		DataDir:    p.cfg.DataDir,
		RF:         p.cfg.RF,
		YSQLPort:   p.cfg.ysqlPort(),
	}, yugabyted.WithLogf(p.logf), yugabyted.WithWaitPG(p.waitPG))

	if err := app.Start(ctx); err != nil {
		return fail(err)
	}
	if err := app.Wait(ctx); err != nil {
		return fail(fmt.Errorf("yugabyted cluster never became ready: %w", err))
	}
	return &Env{
		mode:       ModeProvisionYugabyted,
		pg:         app.ConnInfo(),
		nodeAddrs:  p.cfg.Hosts,
		versions:   core.DetectedVersions{YB: p.detectVer(ctx, app.ConnInfo()), Product: p.cfg.Product},
		app:        app,
		transports: transports,
		exec:       nodes[0].Exec,
		handles:    core.EnvHandles{Endpoint: nodes[0].Advertise, Containers: p.cfg.Hosts},
	}, nil
}

// provisionYBA stands up an owned YBA (apps/ybastack), creates an AWS provider +
// a universe on it (apps/yba), and exposes that universe's YSQL endpoint. Every
// created resource is registered for incremental cleanup the instant it exists,
// so a failure past that point (or a cancelled ctx) still tears down what was
// created — the cleanup is rooted in context.Background(). On full success the
// created resources are owned by the returned Env, whose Teardown deletes the
// universe then tears the YBA down (LIFO).
func (p *Provider) provisionYBA(ctx context.Context, seed *core.Seed) (core.Environment, error) {
	runTag := provisionRunTag(seed)
	k8s := p.providerType == "kubernetes"
	gcp := p.providerType == "gcp"

	// Build the specs first: a config/secret error must fail before anything is
	// created (nothing to clean up yet). The kubernetes path builds a k8s provider
	// + universe spec (no cloud creds); the gcp path a GCE provider; the default
	// AWS path an EC2 provider.
	var (
		provSpec    yba.ProviderSpec
		k8sProvSpec yba.K8sProviderSpec
		gcpProvSpec yba.GCPProviderSpec
		uniSpec     yba.UniverseSpec
		err         error
	)
	switch {
	case k8s:
		k8sProvSpec, err = p.buildK8sProviderSpec()
		if err != nil {
			return nil, fmt.Errorf("provision_yba: build k8s provider spec: %w", err)
		}
		uniSpec, err = p.buildK8sUniverseSpec()
		if err != nil {
			return nil, fmt.Errorf("provision_yba: build k8s universe spec: %w", err)
		}
		uniSpec.ProviderCode = "kubernetes"
	case gcp:
		gcpProvSpec, err = p.buildGCPProviderSpec()
		if err != nil {
			return nil, fmt.Errorf("provision_yba: build gcp provider spec: %w", err)
		}
		uniSpec, err = p.buildGCPUniverseSpec()
		if err != nil {
			return nil, fmt.Errorf("provision_yba: build gcp universe spec: %w", err)
		}
		uniSpec.ProviderCode = "gcp"
	default:
		provSpec, err = p.buildProviderSpec()
		if err != nil {
			return nil, fmt.Errorf("provision_yba: build provider spec: %w", err)
		}
		uniSpec, err = p.buildUniverseSpec()
		if err != nil {
			return nil, fmt.Errorf("provision_yba: build universe spec: %w", err)
		}
		uniSpec.ProviderCode = "aws"
	}
	uniSpec.RunTag = runTag

	stack, err := p.newStack(runTag)
	if err != nil {
		return nil, err
	}

	// Incremental cleanup: each created resource pushes its teardown here the
	// instant it exists. failure runs them LIFO on a fresh background deadline so
	// even a cancelled ctx cleans up. Never reached on full success (the Env owns
	// the resources instead).
	var created []func(context.Context) error
	failure := func(e error) (core.Environment, error) {
		cctx, cancel := context.WithTimeout(context.Background(), provisionCleanupBudget)
		defer cancel()
		for i := len(created) - 1; i >= 0; i-- {
			if cerr := created[i](cctx); cerr != nil {
				p.log("provision_yba cleanup (best-effort): %v", cerr)
			}
		}
		return nil, e
	}

	// A single gated teardown, armed BEFORE Deploy. Deploy creates the namespace/
	// pull-secret/CRBs as its first steps and leaves them on any later failure, so
	// its cleanup must be armed even when Deploy errors. Once a universe exists it
	// is force-deleted FIRST and the YBA is torn down only once the nodes are gone
	// (teardownProvisionedYBA). provClient/universeID/namespace are filled in as
	// provisioning progresses; the closure reads them at cleanup time.
	// reap is the airtight backstop, independent of the universe UUID or the YBA
	// being reachable, bound now (all inputs known before Deploy) so teardown can
	// always sweep our resources even if universe-resolve or the YBA force-delete
	// failed. AWS: terminate EVERY EC2 instance tagged ampqa-run=<runTag> via AWS.
	// gcp: delete EVERY GCE instance labeled ampqa-run=<runTag> via gcloud (using
	// the provider's own SA key). kubernetes: delete any leftover YBA-created
	// universe namespace in the target cluster (the same GKE cluster the YBA runs
	// in — apps.ybastack.kubecontext).
	universeName := uniSpec.UniverseName()
	// The YBA control plane ALWAYS runs in k8s (ybastack), regardless of the
	// universe backing cloud, so its kubecontext identifies both the k8s
	// namespace backstop (kubernetes universes) and the ybastack YBA namespace a
	// separate-process teardown must reap.
	kubeContext, kubeconfig := "", ""
	if p.provisionCfg != nil {
		kubeContext = p.provisionCfg.GetString("apps.ybastack.kubecontext", "")
		kubeconfig = p.provisionCfg.GetString("apps.ybastack.kubeconfig", "")
	}
	var reap func(context.Context) error
	switch {
	case k8s:
		reap = func(c context.Context) error {
			return p.reapNamespaces(c, kubeContext, kubeconfig, universeName)
		}
	case gcp:
		reap = func(c context.Context) error {
			return p.reapGCENodes(c, gcpProvSpec.Project, gcpProvSpec.Zone, gcpProvSpec.CredentialsFile, runTag)
		}
	default:
		reap = func(c context.Context) error {
			return p.reapNodes(c, uniSpec.Region, provSpec.AccessKey, provSpec.SecretKey, runTag)
		}
	}

	var (
		provClient   ybaProvisioner
		universeID   string
		ybaNamespace string
	)
	created = append(created, func(c context.Context) error {
		return teardownProvisionedYBA(c, provClient, stack, universeID, universeName, runTag, reap, ybaNamespace, p.log)
	})

	info, err := stack.Deploy(ctx)
	if err != nil {
		return failure(fmt.Errorf("provision_yba: deploy yba: %w", err))
	}
	ybaNamespace = info.Namespace
	p.log("provision_yba: yba up at %s (namespace %s, yb image registry %s)", info.URL, info.Namespace, info.YBImageRegistry)

	cl := p.newProvisioner(info)
	provClient = cl
	// VM clouds (AWS + GCP): import the DB release for the universe version+arch —
	// a freshly-deployed YBA has no artifact for it, so universe-create would 500
	// ("no artifact found for architecture <arch>"). k8s universes pull their DB
	// image from the provider's kubernetesImageRegistry instead, so there is no
	// tarball release to import. Idempotent — a present artifact is a no-op.
	if !k8s {
		arch := provSpec.Arch
		if gcp {
			arch = gcpProvSpec.Arch
		}
		if err := cl.EnsureRelease(ctx, uniSpec.YBSoftwareVersion, arch, uniSpec.ReleaseURL); err != nil {
			return failure(fmt.Errorf("provision_yba: ensure release: %w", err))
		}
	}
	var providerUUID string
	switch {
	case k8s:
		providerUUID, err = cl.EnsureK8sProvider(ctx, k8sProvSpec)
		if err != nil {
			return failure(fmt.Errorf("provision_yba: ensure k8s provider: %w", err))
		}
	case gcp:
		providerUUID, err = cl.EnsureGCPProvider(ctx, gcpProvSpec)
		if err != nil {
			return failure(fmt.Errorf("provision_yba: ensure gcp provider: %w", err))
		}
	default:
		providerUUID, err = cl.EnsureAWSProvider(ctx, provSpec)
		if err != nil {
			return failure(fmt.Errorf("provision_yba: ensure aws provider: %w", err))
		}
	}
	// The provider is idempotent shared infra (reused by name), so it is NOT
	// registered for deletion; it is reclaimed when the YBA namespace is torn
	// down anyway.
	// Set ybc.provider.enabled to match the requested YBC mode BEFORE create:
	// disabled (the default) makes YBA skip YBC install + the WaitForYbcServer
	// subtask that otherwise times out pinging a yb-controller the foundation
	// test neither installs nor needs (workloads run via amp compute).
	if err := cl.SetYbcEnabledForProvider(ctx, providerUUID, uniSpec.EnableYbc); err != nil {
		return failure(fmt.Errorf("provision_yba: set ybc.provider.enabled: %w", err))
	}
	// AWS only: register the universe's instance type on the provider (YBA does
	// not auto-populate an API-created AWS provider's instance-type catalog, so
	// universe-create would NPE on a null InstanceType). A GCP provider DOES
	// auto-populate its machine-type catalog from the GCE API on create
	// (GCPInitializer), so no registration is needed. A kubernetes universe has no
	// instance type at all — pods, not VMs.
	if !k8s && !gcp {
		if err := cl.AddInstanceType(ctx, providerUUID, yba.InstanceTypeSpec{
			Code:      uniSpec.InstanceType,
			NumCores:  uniSpec.NumCores,
			MemSizeGB: uniSpec.MemSizeGB,
			Arch:      provSpec.Arch,
		}); err != nil {
			return failure(fmt.Errorf("provision_yba: register instance type: %w", err))
		}
	}
	// VM clouds: resolve the provider's SSH access key so the universe references
	// it (AWS launches EC2 with KeyName=None otherwise; GCP wires the reused
	// keypair). A GCP provider created with no keypair (the default) has no access
	// key — YBA then uses project/OS-login SSH keys — so the resolve is skipped
	// unless a keypair was configured. A kubernetes universe has no SSH key.
	if !k8s {
		needAccessKey := !gcp || gcpProvSpec.KeyPairName != ""
		if needAccessKey {
			accessKeyCode, err := cl.ProviderAccessKeyCode(ctx, providerUUID)
			if err != nil {
				return failure(fmt.Errorf("provision_yba: resolve access key: %w", err))
			}
			uniSpec.AccessKeyCode = accessKeyCode
		}
	}
	regionUUIDs, err := cl.RegionUUIDsForCodes(ctx, providerUUID, uniSpec.Region)
	if err != nil {
		return failure(fmt.Errorf("provision_yba: resolve region uuids: %w", err))
	}
	uniSpec.ProviderUUID = providerUUID
	uniSpec.RegionUUIDs = regionUUIDs

	// Create the universe, then set universeID BEFORE waiting: the create task
	// launches the cloud nodes DURING the wait, so a wait/task failure must still
	// reap them. Once universeID is set, the gated teardown above force-deletes
	// the universe (terminating its nodes) before the YBA is torn down — a failed
	// create that already launched nodes is no longer orphaned.
	ref, err := cl.CreateUniverseFromSpec(ctx, uniSpec)
	if err != nil {
		return failure(fmt.Errorf("provision_yba: create universe: %w", err))
	}
	universeID = ref.ResourceUUID
	if universeID == "" {
		// Some YBA builds don't return the universe UUID in the create task ref;
		// resolve it by our ampqa-<runTag> name (the record exists the instant
		// create is accepted). Capturing it here — before the wait — keeps the
		// teardown able to force-delete the universe if the task later fails.
		if u, gerr := cl.GetUniverse(ctx, universeName); gerr == nil {
			universeID = u.UniverseUUID
		}
	}
	if _, err := cl.WaitTask(ctx, ref.TaskUUID); err != nil {
		return failure(fmt.Errorf("provision_yba: create universe %q: %w", universeName, err))
	}

	ref2 := universeID
	if ref2 == "" {
		ref2 = universeName
	}
	// The create task completing is necessary but not sufficient — confirm the
	// universe itself is HEALTHY (updateSucceeded && !updateInProgress) before
	// provisioning is declared done, so the backing store is genuinely up (nodes
	// provisioned, software installed, master/tserver live) and not half-built.
	if err := cl.WaitUniverseReady(ctx, ref2, provisionUniverseReadyTimeout); err != nil {
		return failure(fmt.Errorf("provision_yba: universe not healthy: %w", err))
	}
	u, err := cl.GetUniverse(ctx, ref2)
	if err != nil {
		return failure(fmt.Errorf("provision_yba: resolve created universe %q: %w", ref2, err))
	}
	if universeID == "" {
		universeID = u.UniverseUUID
	}
	ci, addrs, err := yba.YSQLConnInfo(u)
	if err != nil {
		return failure(fmt.Errorf("provision_yba: %w", err))
	}
	var inClusterAddrs []string
	if k8s {
		// YBA reports a k8s universe node's private_ip as an in-cluster address
		// (pod DNS / ClusterIP) the operator can't reach over the tailnet; only
		// the framework's OPERATOR-side layer-1 path is affected (amp's own deploy
		// reaches the same universe via in-cluster DNS and is fine). Re-target the
		// backing endpoint at the tserver POD IPs, which ARE tailnet-routable, but
		// KEEP the in-cluster private_ip addrs: an amp control plane co-located in
		// this cluster dials the universe over those, not the pod IPs (the split
		// ybbacking.Colocated draws — controller via in-cluster DNS, coordinator
		// create via tailnet pod IPs).
		inClusterAddrs = addrs
		podAddrs, perr := p.resolveK8sYSQLAddrs(ctx, kubeContext, kubeconfig, universeName, ci.Port)
		if perr != nil {
			return failure(fmt.Errorf("provision_yba: resolve k8s universe tserver pod IPs: %w", perr))
		}
		host, port := splitAddr(podAddrs[0], ci.Port)
		ci.Host, ci.Port, addrs = host, port, podAddrs
	}
	version := u.Version()
	if version == "" {
		version = p.cfg.Product
	}
	return &Env{
		mode:            ModeProvisionYBA,
		pg:              ci,
		nodeAddrs:       addrs,
		inClusterAddrs:  inClusterAddrs,
		versions:        core.DetectedVersions{YB: version, Product: p.cfg.Product},
		provStack:       stack,
		provClient:      cl,
		provNamespace:   info.Namespace,
		provUniverse:    universeName,
		provRunTag:      runTag,
		provReap:        reap,
		universeID:      universeID,
		owned:           true,
		ybImageRegistry: info.YBImageRegistry,
		handles:         core.EnvHandles{Endpoint: universeID},

		provProviderType: p.providerType,
		provRegion:       uniSpec.Region,
		provKubeContext:  kubeContext,
		provKubeconfig:   kubeconfig,
		provHelmRelease:  info.HelmRelease,
		provCRBs:         info.ClusterRoleBindings,
	}, nil
}

// provisionRunTag derives the per-run tag that roots the ybastack namespace and
// the universe name. Seed-derived (stable, DNS-safe hex) when a seed is present;
// a wall-clock fallback keeps it unique when none is (e.g. a bare unit call).
func provisionRunTag(seed *core.Seed) string {
	if seed != nil {
		return fmt.Sprintf("%x", uint64(seed.Value()))
	}
	return fmt.Sprintf("%x", uint64(time.Now().UnixNano()))
}

// detectYBVersion best-effort live-detects the backing version via SELECT
// version(), extracting the YugabyteDB build YSQL embeds in the response
// ("PostgreSQL ...-YB-X.Y.Z.W-bN ..." — core.ExtractYBVersionMarker) rather
// than recording the raw prose string, so the result is Compare-able by
// VersionSpec.Qualifies instead of silently parsing to garbage. Returns "" on
// any failure (connect, query, or no marker) — the sentinel Qualifies already
// treats as "don't enforce this bound" — so a detection miss never blocks a
// run, only widens the gate.
func detectYBVersion(ctx context.Context, ci pg.ConnInfo) string {
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
	v, ok := core.ExtractYBVersionMarker(strings.TrimSpace(raw))
	if !ok {
		return ""
	}
	return v
}

// teardownProvisionedYBA safely tears a provision_yba env down and enforces the
// cardinal invariant: NEVER remove the managing YBA while any of this run's nodes
// may still be running. Sequence:
//  1. Prefer a clean YBA force-delete of the universe (removes the universe +
//     nodes + volumes). The UUID is resolved by our ampqa-<runTag> name if it
//     was not captured. Best-effort — the tag sweep below is the real guarantee.
//  2. BACKSTOP (always runs, regardless of UUID/YBA state): terminate EVERY EC2
//     instance tagged ampqa-run=<runTag> directly via AWS. This is the airtight
//     guard against orphans — it does not depend on a resolved UUID or a
//     reachable YBA.
//  3. Tear the YBA down ONLY after the tag sweep succeeds. If the sweep FAILS
//     (an AWS error — nodes may still run), KEEP the YBA up and loudly error;
//     stranding running EC2 with no manager is the worst possible outcome.
//
// defaultReapNodes terminates every EC2 instance tagged ampqa-run=<runTag>
// using the provider's own resolved AWS creds (the same creds that launched
// them), region-scoped — the real teardown backstop.
func defaultReapNodes(ctx context.Context, region, accessKey, secretKey, runTag string) error {
	client := awscloud.New(awscloud.Config{Region: region, AccessKey: accessKey, SecretKey: secretKey})
	return client.ReapByTag(ctx, runTag)
}

// defaultReapGCENodes deletes every GCE instance labeled ampqa-run=<runTag>
// across the provider's whole PROJECT/REGION via gcloud (apps/cloud/gcp
// ReapByTag) — the GCP teardown backstop, the analogue of defaultReapNodes.
// ReapByTag discovers region-wide and deletes each node in its OWN zone, so the
// sweep catches universe nodes wherever YBA placed them in the region, not just
// the configured zone (which is only a fallback for a zone-less describe). It
// authenticates gcloud with the SAME service-account key file the provider used
// (credsFile); an empty credsFile falls back to the ambient gcloud auth chain.
// The GCE label value is the run tag sanitized identically on both sides (a hex
// run tag is a no-op), so the sweep matches the labels YBA stamps on the nodes.
func defaultReapGCENodes(ctx context.Context, project, zone, credsFile, runTag string) error {
	client := gcpcloud.New(gcpcloud.Config{Project: project, Zone: zone, ServiceAccountKeyFile: credsFile})
	return client.ReapByTag(ctx, runTag)
}

// defaultReapNamespaces is the KUBERNETES teardown backstop (the analogue of
// defaultReapNodes): after the universe force-delete — which itself asks YBA to
// remove the pods/namespace it created — it deletes any leftover namespace in
// the target cluster named after the universe, so a failed force-delete or an
// unreachable YBA never orphans the universe's pods/PVCs. The universe name is
// ampqa-<runTag> (seed-derived, unique), so an exact or "<name>-" prefix match
// is safe; YBA names the universe namespace after the universe.
func defaultReapNamespaces(ctx context.Context, kubeContext, kubeconfig, universeName string) error {
	if universeName == "" {
		return fmt.Errorf("reap-namespaces: empty universe name")
	}
	kube := k8s.New(k8s.Config{Context: kubeContext}).WithRunner(k8sReapRunner(kubeconfig))
	out, err := kube.GetSelector(ctx, "namespaces", "", "{.items[*].metadata.name}")
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}
	var bad []string
	for _, ns := range strings.Fields(out) {
		if ns == universeName || strings.HasPrefix(ns, universeName+"-") {
			if derr := kube.DeleteNamespace(ctx, ns); derr != nil {
				bad = append(bad, fmt.Sprintf("delete namespace %s: %v", ns, derr))
			}
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s", strings.Join(bad, "; "))
	}
	return nil
}

// k8sTServerNameSubstr identifies a YugabyteDB universe's tserver pods BY NAME.
// A YBA-created universe names its tserver pods "<release>-yb-tserver-N" and
// does NOT stamp them with the raw yugabyte chart's app=yb-tserver label (YBA
// deploys YB via its own helm values, a different label scheme), so matching on
// the pod name is robust to that difference. Master pods are "…-yb-master-N",
// which does not contain this substring, so they are excluded.
const k8sTServerNameSubstr = "yb-tserver"

// defaultResolveK8sYSQLAddrs resolves a kubernetes universe's tserver POD IPs
// (as host:port) over the target cluster — the operator-reachable backing
// endpoint. YBA names the universe's namespace after the universe (the
// nodePrefix embeds the seed-unique universe name, e.g. yb-dev-ampqa-<hash>),
// so its pods are found by listing namespaces whose name contains universeName
// and reading each tserver pod's IP there. Pod IPs are tailnet-routable from
// the operator; the in-cluster DNS/ClusterIP YBA reports in private_ip is not
// (mirrors apps/ybk8s.resolveTailnetAddrs).
func defaultResolveK8sYSQLAddrs(ctx context.Context, kubeContext, kubeconfig, universeName string, ysqlPort int) ([]string, error) {
	return k8sTServerAddrs(ctx, kubeContext, k8sReapRunner(kubeconfig), universeName, ysqlPort)
}

// k8sTServerAddrs is the runner-injectable core of defaultResolveK8sYSQLAddrs
// (a seam so unit tests script kubectl without touching a real cluster).
func k8sTServerAddrs(ctx context.Context, kubeContext string, runner k8s.Runner, universeName string, ysqlPort int) ([]string, error) {
	if universeName == "" {
		return nil, fmt.Errorf("resolve k8s ysql addrs: empty universe name")
	}
	if ysqlPort == 0 {
		ysqlPort = DefaultYSQLPort
	}
	cluster := k8s.New(k8s.Config{Context: kubeContext}).WithRunner(runner)
	nsOut, err := cluster.GetSelector(ctx, "namespaces", "", "{.items[*].metadata.name}")
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	var (
		matched []string // namespaces containing the universe name
		seen    []string // every pod name seen in those namespaces (for a clear error)
		addrs   []string
	)
	for _, ns := range strings.Fields(nsOut) {
		if !strings.Contains(ns, universeName) {
			continue
		}
		matched = append(matched, ns)
		nsKube := k8s.New(k8s.Config{Context: kubeContext, Namespace: ns}).WithRunner(runner)
		// name + IP per pod; match tservers BY NAME (robust to YBA's label scheme).
		out, err := nsKube.GetSelector(ctx, "pods", "",
			`{range .items[*]}{.metadata.name}{" "}{.status.podIP}{"\n"}{end}`)
		if err != nil {
			return nil, fmt.Errorf("list pods in namespace %s: %w", ns, err)
		}
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) == 0 {
				continue
			}
			name := f[0]
			seen = append(seen, name)
			if !strings.Contains(name, k8sTServerNameSubstr) || len(f) < 2 {
				continue
			}
			addrs = append(addrs, fmt.Sprintf("%s:%d", f[1], ysqlPort))
		}
	}
	if len(addrs) == 0 {
		if len(matched) == 0 {
			return nil, fmt.Errorf("no k8s namespace on the target cluster contains the universe name %q", universeName)
		}
		return nil, fmt.Errorf("no tserver pod (name containing %q, with a pod IP) found in namespace(s) %v for universe %q; pods seen: %v",
			k8sTServerNameSubstr, matched, universeName, seen)
	}
	return addrs, nil
}

// k8sReapRunner returns the kubectl one-shot runner for the namespace backstop.
// With a kubeconfig it shells kubectl with KUBECONFIG set in the child env only
// (never global — parallel-safe); without one it uses the ambient-authed
// k8s.OSRun. Mirrors ybastack.ybaRunner.
func k8sReapRunner(kubeconfig string) k8s.Runner {
	if kubeconfig == "" {
		return k8s.OSRun
	}
	env := append(os.Environ(), "KUBECONFIG="+kubeconfig)
	return func(ctx context.Context, stdin io.Reader, argv []string) (exec.Result, error) {
		if len(argv) == 0 {
			return exec.Result{}, fmt.Errorf("empty argv")
		}
		cmd := osexec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = env
		if stdin != nil {
			cmd.Stdin = stdin
		}
		var out, errb strings.Builder
		cmd.Stdout = &out
		cmd.Stderr = &errb
		rerr := cmd.Run()
		res := exec.Result{Stdout: out.String(), Stderr: errb.String()}
		if ctx.Err() != nil {
			res.ExitCode = -1
			return res, ctx.Err()
		}
		if rerr != nil {
			var ee *osexec.ExitError
			if errors.As(rerr, &ee) {
				res.ExitCode = ee.ExitCode()
				return res, nil
			}
			res.ExitCode = -1
			return res, rerr
		}
		return res, nil
	}
}

func teardownProvisionedYBA(ctx context.Context, provClient ybaProvisioner, stack ybaStack, universeID, universeName, runTag string, reap func(context.Context) error, namespace string, log func(string, ...any)) error {
	id := universeID
	if id == "" && universeName != "" && provClient != nil {
		if u, err := provClient.GetUniverse(ctx, universeName); err == nil && u.UniverseUUID != "" {
			id = u.UniverseUUID
		}
	}
	if id != "" && provClient != nil {
		if _, err := provClient.ForceDeleteUniverseAndWait(ctx, id); err != nil {
			log("provision_yba teardown: YBA force-delete of universe %s failed; the ampqa-run=%s tag sweep is the backstop: %v", id, runTag, err)
		} else {
			log("provision_yba teardown: universe %s deleted (cloud nodes terminated)", id)
		}
	}
	// Airtight backstop, independent of UUID/YBA: terminate this run's nodes by tag.
	if reap != nil {
		if err := reap(ctx); err != nil {
			return fmt.Errorf("tag sweep (ampqa-run=%s) FAILED — keeping the YBA up (namespace %s) so this run's nodes are not orphaned; reap manually: %w", runTag, namespace, err)
		}
	}
	return stack.Teardown(ctx)
}

// Teardown deletes/destroys only what this provider owns: an owned YBA universe
// (attach_yba + owned), or a provisioned yugabyted cluster. Attached targets
// are left untouched.
func (p *Provider) Teardown(ctx context.Context, env core.Environment) error {
	e, ok := env.(*Env)
	if !ok {
		return fmt.Errorf("universe teardown: unexpected environment type %T", env)
	}
	switch e.mode {
	case ModeAttachYBA:
		if !e.owned {
			return nil
		}
		if _, err := e.yba.DeleteUniverseAndWait(ctx, e.universeID); err != nil {
			return fmt.Errorf("delete owned universe %s: %w", e.universeID, err)
		}
		return nil
	case ModeProvisionYugabyted:
		err := e.app.Destroy(ctx, discardSink{})
		for _, tr := range e.transports {
			if cerr := tr.Close(); cerr != nil {
				p.log("universe teardown: close node transport (best-effort): %v", cerr)
			}
		}
		return err
	case ModeProvisionYBA:
		// On an independent background deadline so a cancelled run still cleans up.
		// The universe is force-deleted FIRST (terminating its cloud nodes) and the
		// YBA is torn down ONLY once the nodes are gone — see teardownProvisionedYBA.
		tctx, cancel := context.WithTimeout(context.Background(), provisionTeardownBudget)
		defer cancel()
		return teardownProvisionedYBA(tctx, e.provClient, e.provStack, e.universeID, e.provUniverse, e.provRunTag, e.provReap, e.provNamespace, p.log)
	default:
		return nil
	}
}

// Attach resolves a YBA universe by ref (attach_yba mode only). Other modes do
// not support attach-by-ref.
func (p *Provider) Attach(ctx context.Context, ref string) (core.Environment, error) {
	if p.cfg.Mode != ModeAttachYBA {
		return nil, fmt.Errorf("universe attach: only supported in attach_yba mode (mode=%q)", p.cfg.Mode)
	}
	return p.provisionAttachYBA(ctx, ref)
}

// Env is a live YB-universe target.
type Env struct {
	mode      string
	pg        pg.ConnInfo
	nodeAddrs []string
	// inClusterAddrs is set for a kubernetes provision_yba universe only: the
	// tserver in-cluster addresses (YBA's private_ip — pod DNS / ClusterIP) that
	// an amp control plane co-located in this cluster dials, distinct from the
	// tailnet pod IPs in pg/nodeAddrs the operator uses. Empty for VM universes,
	// whose addresses are reachable both ways.
	inClusterAddrs []string
	versions       core.DetectedVersions
	handles        core.EnvHandles

	// attach_yba
	yba        *yba.Client
	universeID string
	owned      bool

	// provision_yugabyted
	app        *yugabyted.App
	transports []transport
	exec       exec.RemoteExecutor

	// provision_yba
	provStack       ybaStack
	provClient      ybaProvisioner
	provNamespace   string                      // ampqa-yba-<runTag> YBA control-plane namespace
	provUniverse    string                      // ampqa-<runTag> universe name (teardown fallback)
	provRunTag      string                      // ampqa-run tag value (teardown sweep)
	provReap        func(context.Context) error // tag-based node sweep backstop
	ybImageRegistry string

	// Serializable reap handles for a separate-process teardown (ProvisionedHandles).
	provProviderType string // "aws" | "gcp" | "kubernetes"
	provRegion       string // informational (cloud backings)
	provKubeContext  string // the ybastack cluster (YBA ns + any k8s universe ns)
	provKubeconfig   string
	provHelmRelease  string   // the REAL YBA helm release (chart-scoped RBAC name)
	provCRBs         []string // ybastack suffixed ClusterRoleBindings
}

// ProvisionedHandles are the serializable ownership handles a separate-process
// teardown (env/reap, via `ampqa env rm`) needs to reap a provision_yba env
// without the in-memory teardown closure: the YBA control-plane namespace (always
// in k8s, via ybastack) plus — per the universe backing — a k8s universe
// namespace (ProviderType "kubernetes") or the cloud run tag (aws/gcp). It
// carries NO secrets; the reaper re-resolves credentials from config. The zero
// value means "not a provision_yba env" (nothing here to reap this way).
type ProvisionedHandles struct {
	ProviderType string
	RunTag       string
	Region       string
	KubeContext  string
	Kubeconfig   string
	YBANamespace string
	HelmRelease  string // the REAL YBA helm release (chart-scoped RBAC is named after it)
	UniverseName string
	CRBs         []string
}

// ProvisionedHandles exposes the reap handles for a provision_yba env; the zero
// value for any other mode.
func (e *Env) ProvisionedHandles() ProvisionedHandles {
	if e.mode != ModeProvisionYBA {
		return ProvisionedHandles{}
	}
	return ProvisionedHandles{
		ProviderType: e.provProviderType,
		RunTag:       e.provRunTag,
		Region:       e.provRegion,
		KubeContext:  e.provKubeContext,
		Kubeconfig:   e.provKubeconfig,
		YBANamespace: e.provNamespace,
		HelmRelease:  e.provHelmRelease,
		UniverseName: e.provUniverse,
		CRBs:         e.provCRBs,
	}
}

// Mode reports the env.universe.mode this Env was provisioned/attached under
// (one of the Mode* constants) — callers use it to tell a real-infra
// provisioning mode (provision_yugabyted, provision_yba) apart from an
// attach mode that merely points at something already running.
func (e *Env) Mode() string { return e.mode }

// PGEndpoint lets pg-based tests reach the universe (docker env's convention).
func (e *Env) PGEndpoint() pg.ConnInfo { return e.pg }

// NodeAddrs lists every tserver host:port the target exposes.
func (e *Env) NodeAddrs() []string { return e.nodeAddrs }

// InClusterAddrs lists the tserver in-cluster host:port addresses for a
// kubernetes provision_yba universe — the co-located path an amp control plane
// running in the SAME cluster dials (YBA's private_ip / pod DNS), as opposed to
// the tailnet pod IPs NodeAddrs exposes for the operator. It is nil for VM
// universes (whose addresses are reachable both ways) and every attach mode, so
// a caller reads a non-empty slice as "this backing needs the co-located split".
func (e *Env) InClusterAddrs() []string { return e.inClusterAddrs }

// ControllerURL is "": the target is a YB cluster, not an amp control plane.
func (e *Env) ControllerURL() string           { return "" }
func (e *Env) Versions() core.DetectedVersions { return e.versions }
func (e *Env) Handles() core.EnvHandles        { return e.handles }

// Exec returns the bootstrap node's executor for provisioned clusters (chaos /
// direct commands); nil for attached targets with no shell access.
func (e *Env) Exec() exec.RemoteExecutor { return e.exec }

// Collect gathers cluster logs. For a provisioned yugabyted cluster this pulls
// per-node logs via the app; for attached targets it is a no-op (we don't own
// the nodes' log access).
func (e *Env) Collect(ctx context.Context, into core.ArtifactSink) error {
	if e.mode == ModeProvisionYugabyted && e.app != nil {
		return e.app.Collect(ctx, into)
	}
	return nil
}

// discardSink drops collected blobs — used when teardown Destroy runs without a
// caller-provided sink (the scheduler collects separately before teardown).
type discardSink struct{}

func (discardSink) Add(string, string, []byte) error { return nil }

// splitAddr parses host:port, defaulting the port when absent/unparseable.
func splitAddr(addr string, defPort int) (string, int) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", defPort
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, defPort
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return host, defPort
	}
	return host, port
}
