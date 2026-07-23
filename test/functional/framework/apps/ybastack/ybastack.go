// Package ybastack is the YugabyteDB-Anywhere-in-Kubernetes deploy App: it
// stands up (and tears down) a full YBA control plane inside a single, isolated
// namespace and hands back a token-authenticated *yba.Client pointed at it. It
// is the sibling of apps/ampstack — same shape, same kubectl/helm-shelled
// (ambient-authed) transport, same guaranteed-teardown discipline — but for YBA
// instead of the amp controller. It exists because there is no standing dev YBA
// to attach to: the framework must be able to STAND UP its own.
//
// The flow mirrors the stress framework's task_create_k8s_yugaware +
// yugaware_installer recipe: pull the yugaware helm chart tarball from S3, patch
// its values.yaml image registry + pull-secret, create the namespace / service
// account / quay pull-secret / (optional) universe-management RBAC, `helm
// install --wait`, then reach the platform and bootstrap the first customer +
// API token (apps/yba.Bootstrap).
//
// Isolation is the load-bearing property (AGENTS.md §3/§8): the namespace is
// per-run (ampqa-yba-<runTag>), and every ClusterRoleBinding the RBAC ships —
// cluster-scoped objects with fixed names that two parallel YBAs would clobber —
// is renamed with the namespace suffix and deleted explicitly in Teardown (a
// namespace delete never reclaims a cluster-scoped object). ybastack only ever
// touches namespaces/bindings it created.
//
// The platform is reached like ampstack's controller (ControllerReach): tailnet
// (default) resolves the yugaware Service's cluster-internal address (ClusterIP,
// falling back to the pod IP) and talks to it directly over the tailnet — no
// kubectl port-forward, nothing to die mid-run; loadbalancer reads the Service's
// external LoadBalancer ingress IP instead (the stress path).
package ybastack

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/yba"
	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// Defaults (core/defaults_test.go asserts the embedded default.yaml agrees).
const (
	DefaultNamespacePrefix = "ampqa-yba"
	DefaultImageRegistry   = "quay.io/yugabyte/yugaware-itest"
	DefaultYBImageRegistry = "quay.io/yugabyte/yugabyte-itest"
	DefaultQuaySecret      = "quay_images"
	DefaultPullSecretName  = "quay-secret"
	DefaultServiceAccount  = "yw-sa"
	DefaultStorageClass    = "standard"
	DefaultReach           = ReachTailnet
	DefaultServicePort     = 80
	DefaultHelmTimeout     = 40 * time.Minute
	DefaultBootstrapName   = "demo"
	DefaultBootstrapEmail  = "demo@yugabyte.com"
	// DefaultBootstrapPassword is the well-known non-secret YBA dev default. It is
	// a config default only — a real deployment overrides it via config, never a
	// literal in a caller.
	DefaultBootstrapPassword = "Password#123"
	DefaultBootstrapTimeout  = 10 * time.Minute
)

// ReachMode selects how the deployed YBA is reached.
type ReachMode string

const (
	// ReachTailnet (default) resolves the yugaware Service's cluster-internal
	// address (ClusterIP, falling back to the pod IP on its container target
	// port) and talks to it directly.
	ReachTailnet ReachMode = "tailnet"
	// ReachLoadBalancer reads the Service's external LoadBalancer ingress IP (the
	// stress recipe's path).
	ReachLoadBalancer ReachMode = "loadbalancer"
)

// Config configures a YBA-in-k8s deploy.
type Config struct {
	KubeContext string
	Kubeconfig  string // "" → ambient KUBECONFIG / in-cluster config

	NamespacePrefix string
	ChartVersion    string // "" → versions.yugabytedb (resolved by FromConfig)
	ImageRegistry   string // chart image registry (yugaware-itest)
	YBImageRegistry string // universe image registry (yugabyte-itest) — for the provider wave, not the install
	QuaySecret      string // NAME of the secret holding the quay pull credential
	PullSecretName  string // in-cluster image-pull Secret name
	ServiceAccount  string
	StorageClass    string
	RBACDir         string // optional: universe-management RBAC templates to apply (CRBs suffixed per namespace)

	Reach       ReachMode
	ServiceName string // "" → "<release>-yugaware-ui"
	ServicePort int
	PodSelector string // "" → "app=<release>-yugaware"
	HelmRelease string // "" → "<namespace>-yugaware"
	HelmTimeout time.Duration
	InsecureTLS bool

	BootstrapName     string
	BootstrapEmail    string
	BootstrapPassword string
	BootstrapTimeout  time.Duration

	// ExtraNamespaceLabels are merged onto the namespace (e.g. a reaper stamp).
	ExtraNamespaceLabels map[string]string
}

// Getter is the minimal typed-config surface FromConfig needs (structurally
// satisfied by *core.Config, so this package imports no core — the apps/*
// convention).
type Getter interface {
	GetString(key, def string) string
	GetInt(key string, def int) int
	GetBool(key string, def bool) bool
	GetDuration(key string, def time.Duration) time.Duration
}

// SecretResolver maps a secret NAME to its resolved value (matches
// core.Secrets.Value); a func keeps this package free of a core import.
type SecretResolver func(name string) (string, error)

// FromConfig reads apps.ybastack.* into a Config. ChartVersion falls back to
// versions.yugabytedb when apps.ybastack.chart_version is empty. KubeContext
// falls back to env.ampstack.context (when neither ybastack kubecontext nor
// kubeconfig is set) so a YBA-provisioned backing co-locates in the same
// cluster as amp without duplicate config — mirroring ybk8s.
func FromConfig(get Getter) Config {
	chartVersion := get.GetString("apps.ybastack.chart_version", "")
	if chartVersion == "" {
		chartVersion = get.GetString("versions.yugabytedb", "")
	}
	kubeContext := get.GetString("apps.ybastack.kubecontext", "")
	kubeconfig := get.GetString("apps.ybastack.kubeconfig", "")
	if kubeContext == "" && kubeconfig == "" {
		kubeContext = get.GetString("env.ampstack.context", "")
	}
	return Config{
		KubeContext:       kubeContext,
		Kubeconfig:        kubeconfig,
		NamespacePrefix:   get.GetString("apps.ybastack.namespace_prefix", DefaultNamespacePrefix),
		ChartVersion:      chartVersion,
		ImageRegistry:     get.GetString("apps.ybastack.image_registry", DefaultImageRegistry),
		YBImageRegistry:   get.GetString("apps.ybastack.yb_image_registry", DefaultYBImageRegistry),
		QuaySecret:        get.GetString("apps.ybastack.quay_secret", DefaultQuaySecret),
		PullSecretName:    get.GetString("apps.ybastack.pull_secret_name", DefaultPullSecretName),
		ServiceAccount:    get.GetString("apps.ybastack.service_account", DefaultServiceAccount),
		StorageClass:      get.GetString("apps.ybastack.storage_class", DefaultStorageClass),
		RBACDir:           get.GetString("apps.ybastack.rbac_dir", ""),
		Reach:             ReachMode(get.GetString("apps.ybastack.reach", string(DefaultReach))),
		ServiceName:       get.GetString("apps.ybastack.service_name", ""),
		ServicePort:       get.GetInt("apps.ybastack.service_port", DefaultServicePort),
		PodSelector:       get.GetString("apps.ybastack.pod_selector", ""),
		HelmRelease:       get.GetString("apps.ybastack.helm_release", ""),
		HelmTimeout:       get.GetDuration("apps.ybastack.helm_timeout", DefaultHelmTimeout),
		InsecureTLS:       get.GetBool("apps.ybastack.insecure_tls", true),
		BootstrapName:     get.GetString("apps.ybastack.bootstrap.name", DefaultBootstrapName),
		BootstrapEmail:    get.GetString("apps.ybastack.bootstrap.email", DefaultBootstrapEmail),
		BootstrapPassword: get.GetString("apps.ybastack.bootstrap.password", DefaultBootstrapPassword),
		BootstrapTimeout:  get.GetDuration("apps.ybastack.bootstrap_timeout", DefaultBootstrapTimeout),
	}
}

func applyDefaults(cfg *Config) {
	if cfg.NamespacePrefix == "" {
		cfg.NamespacePrefix = DefaultNamespacePrefix
	}
	if cfg.ImageRegistry == "" {
		cfg.ImageRegistry = DefaultImageRegistry
	}
	if cfg.YBImageRegistry == "" {
		cfg.YBImageRegistry = DefaultYBImageRegistry
	}
	if cfg.QuaySecret == "" {
		cfg.QuaySecret = DefaultQuaySecret
	}
	if cfg.PullSecretName == "" {
		cfg.PullSecretName = DefaultPullSecretName
	}
	if cfg.ServiceAccount == "" {
		cfg.ServiceAccount = DefaultServiceAccount
	}
	if cfg.StorageClass == "" {
		cfg.StorageClass = DefaultStorageClass
	}
	if cfg.Reach == "" {
		cfg.Reach = DefaultReach
	}
	if cfg.ServicePort == 0 {
		cfg.ServicePort = DefaultServicePort
	}
	if cfg.HelmTimeout <= 0 {
		cfg.HelmTimeout = DefaultHelmTimeout
	}
	if cfg.BootstrapName == "" {
		cfg.BootstrapName = DefaultBootstrapName
	}
	if cfg.BootstrapEmail == "" {
		cfg.BootstrapEmail = DefaultBootstrapEmail
	}
	if cfg.BootstrapPassword == "" {
		cfg.BootstrapPassword = DefaultBootstrapPassword
	}
	if cfg.BootstrapTimeout <= 0 {
		cfg.BootstrapTimeout = DefaultBootstrapTimeout
	}
}

// Info is what a successful Deploy returns — exactly what env/universe's
// yba.* needs to reach the freshly stood-up YBA.
type Info struct {
	URL                 string
	APIToken            string
	CustomerUUID        string
	Version             string
	Namespace           string
	HelmRelease         string
	YBImageRegistry     string   // universe image registry, for the provider wave
	ClusterRoleBindings []string // namespace-suffixed CRBs Teardown must delete
}

// Deployer stands one YBA up in one namespace and tears it down.
type Deployer struct {
	cfg     Config
	runTag  string
	ns      string
	release string
	kube    *k8s.Kubectl
	helmRun k8s.Runner
	logf    func(format string, args ...any)

	// Injected in tests / by composition; defaults wired in New.
	download      func(ctx context.Context, s3URL, destDir string) (string, error)
	resolveSecret SecretResolver
	newClient     func(url string) *yba.Client
	bootstrap     func(ctx context.Context, cl *yba.Client, creds yba.BootstrapCreds, timeout time.Duration) (yba.BootstrapResult, error)

	mu          sync.Mutex
	createdCRBs []string
}

// New builds a Deployer for cfg using the real kubectl/helm/aws binaries. runTag
// roots the per-run namespace and must be non-empty; resolve resolves the quay
// pull-secret by name.
func New(cfg Config, runTag string, resolve SecretResolver) *Deployer {
	applyDefaults(&cfg)
	ns := namespaceFor(cfg.NamespacePrefix, runTag)
	release := helmReleaseFor(cfg.HelmRelease, ns)
	runner := ybaRunner(cfg.Kubeconfig)
	d := &Deployer{
		cfg:           cfg,
		runTag:        runTag,
		ns:            ns,
		release:       release,
		kube:          k8s.New(k8s.Config{Context: cfg.KubeContext, Namespace: ns}).WithRunner(runner),
		helmRun:       runner,
		logf:          log.Printf,
		download:      defaultChartDownload,
		resolveSecret: resolve,
	}
	d.newClient = d.defaultNewClient
	d.bootstrap = func(ctx context.Context, cl *yba.Client, creds yba.BootstrapCreds, timeout time.Duration) (yba.BootstrapResult, error) {
		return cl.Bootstrap(ctx, creds, timeout)
	}
	return d
}

// Reap tears a ybastack YBA down given only its namespace, the REAL helm release
// name, the namespace-suffixed ClusterRoleBindings Deploy recorded, and kube
// settings — the separate-process `ampqa env rm` path, which reconstructs the
// reap from the registry record and has no live Deployer. The release MUST be the
// one Deploy used: the yugaware chart names its cluster-scoped RBAC (a
// ClusterRole + ClusterRoleBinding) after the release, and a namespace delete
// never reclaims cluster-scoped objects — so reaping with the wrong release name
// (e.g. the namespace-derived default when apps.ybastack.helm_release was set)
// LEAKS that RBAC. An empty release falls back to the namespace-derived default
// (the case where helm_release was unset). It runs the SAME Teardown as the live
// Deployer, so there is one teardown implementation.
func Reap(ctx context.Context, kubeContext, kubeconfig, namespace, release string, crbs []string) error {
	if namespace == "" {
		return fmt.Errorf("ybastack reap: empty namespace")
	}
	return reapDeployer(kubeContext, kubeconfig, namespace, release, k8s.KubeconfigRunner(kubeconfig), crbs).Teardown(ctx)
}

// reapDeployer builds the namespace-bound Deployer Reap tears down. Split out (and
// the runner injected) so a unit test can drive the reap against a scripted
// kubectl and assert it targets the recorded release.
func reapDeployer(kubeContext, kubeconfig, namespace, release string, runner k8s.Runner, crbs []string) *Deployer {
	return &Deployer{
		cfg:         Config{KubeContext: kubeContext, Kubeconfig: kubeconfig},
		ns:          namespace,
		release:     helmReleaseFor(release, namespace),
		kube:        k8s.New(k8s.Config{Context: kubeContext, Namespace: namespace}).WithRunner(runner),
		helmRun:     runner,
		logf:        log.Printf,
		createdCRBs: append([]string(nil), crbs...),
	}
}

// --- injection setters (composition + tests) ---

// WithKube replaces the namespace-bound kubectl (tests inject a scripted transport).
func (d *Deployer) WithKube(k *k8s.Kubectl) *Deployer { d.kube = k; return d }

// WithHelmRunner overrides the runner used for `helm install`.
func (d *Deployer) WithHelmRunner(r k8s.Runner) *Deployer { d.helmRun = r; return d }

// WithDownload overrides the chart tarball download (tests inject a local file).
func (d *Deployer) WithDownload(f func(ctx context.Context, s3URL, destDir string) (string, error)) *Deployer {
	d.download = f
	return d
}

// WithClientFactory overrides how a *yba.Client is built from a URL.
func (d *Deployer) WithClientFactory(f func(url string) *yba.Client) *Deployer {
	d.newClient = f
	return d
}

// WithBootstrap overrides the token-bootstrap step (tests inject a no-network stub).
func (d *Deployer) WithBootstrap(f func(ctx context.Context, cl *yba.Client, creds yba.BootstrapCreds, timeout time.Duration) (yba.BootstrapResult, error)) *Deployer {
	d.bootstrap = f
	return d
}

// WithLogf overrides the logger (nil silences). Pass a masked sink so a resolved
// secret never survives into a log line.
func (d *Deployer) WithLogf(f func(format string, args ...any)) *Deployer {
	d.logf = f
	if d.kube != nil {
		d.kube.WithLogf(f)
	}
	return d
}

// Namespace is the deploy's namespace.
func (d *Deployer) Namespace() string { return d.ns }

// Deploy stands the YBA up and returns the reachable, token-bootstrapped Info.
// On any step failure it leaves what it created in place (Teardown removes it)
// and returns the error.
func (d *Deployer) Deploy(ctx context.Context) (Info, error) {
	if d.runTag == "" {
		return Info{}, fmt.Errorf("ybastack: run tag is required (it roots the namespace and reap)")
	}
	if d.cfg.ChartVersion == "" {
		return Info{}, fmt.Errorf("ybastack: chart version is required (apps.ybastack.chart_version or versions.yugabytedb)")
	}

	// 1. namespace (labelled so Teardown/reaper recognizes it).
	if err := d.kube.CreateNamespace(ctx, d.ns, namespaceLabels(d.cfg.ExtraNamespaceLabels)); err != nil {
		return Info{}, fmt.Errorf("create namespace %s: %w", d.ns, err)
	}

	// 2. service account.
	if err := d.kube.Apply(ctx, serviceAccountManifest(d.ns, d.cfg.ServiceAccount)); err != nil {
		return Info{}, fmt.Errorf("apply service account %s: %w", d.cfg.ServiceAccount, err)
	}

	// 3. quay image-pull secret.
	if err := d.ensurePullSecret(ctx); err != nil {
		return Info{}, err
	}

	// 4. optional universe-management RBAC (ClusterRoleBindings suffixed per ns).
	if err := d.applyRBAC(ctx); err != nil {
		return Info{}, err
	}

	// 5. chart: download + extract + patch values.yaml.
	chartDir, cleanup, err := d.fetchChart(ctx)
	if err != nil {
		return Info{}, err
	}
	defer cleanup()

	// 6. helm install --wait.
	if err := d.helmInstall(ctx, chartDir); err != nil {
		return Info{}, err
	}

	// 7. reach the platform (tailnet direct address, or the LB ingress IP).
	url, err := d.resolveURL(ctx)
	if err != nil {
		return Info{}, err
	}

	// 8. bootstrap the first customer + durable API token (retry-until-up).
	cl := d.newClient(url)
	res, err := d.bootstrap(ctx, cl, yba.BootstrapCreds{
		Name:     d.cfg.BootstrapName,
		Email:    d.cfg.BootstrapEmail,
		Password: d.cfg.BootstrapPassword,
	}, d.cfg.BootstrapTimeout)
	if err != nil {
		return Info{}, fmt.Errorf("bootstrap yba at %s: %w", url, err)
	}

	d.mu.Lock()
	crbs := append([]string(nil), d.createdCRBs...)
	d.mu.Unlock()
	return Info{
		URL:                 url,
		APIToken:            res.APIToken,
		CustomerUUID:        res.CustomerUUID,
		Version:             res.Version,
		Namespace:           d.ns,
		HelmRelease:         d.release,
		YBImageRegistry:     d.cfg.YBImageRegistry,
		ClusterRoleBindings: crbs,
	}, nil
}

// ensurePullSecret resolves the quay credential by secret NAME and applies the
// in-cluster dockerconfigjson image-pull Secret.
func (d *Deployer) ensurePullSecret(ctx context.Context) error {
	if d.resolveSecret == nil {
		return fmt.Errorf("ybastack: no secret resolver to resolve quay secret %q", d.cfg.QuaySecret)
	}
	value, err := d.resolveSecret(d.cfg.QuaySecret)
	if err != nil {
		return fmt.Errorf("resolve quay secret %q: %w", d.cfg.QuaySecret, err)
	}
	b64, err := quayDockerConfigJSONB64(value, registryHost(d.cfg.ImageRegistry))
	if err != nil {
		return fmt.Errorf("build quay pull secret: %w", err)
	}
	if err := d.kube.Apply(ctx, pullSecretManifest(d.ns, d.cfg.PullSecretName, b64)); err != nil {
		return fmt.Errorf("apply quay pull secret: %w", err)
	}
	return nil
}

// applyRBAC reads the universe-management RBAC templates (when configured),
// fills their placeholders, suffixes every ClusterRoleBinding name with the
// namespace (so parallel YBAs never clobber a cluster-scoped binding), applies
// them, and records the suffixed names for Teardown. No RBACDir → no-op (the
// yugaware helm chart provisions its own namespaced RBAC).
func (d *Deployer) applyRBAC(ctx context.Context) error {
	if d.cfg.RBACDir == "" {
		return nil
	}
	entries, err := os.ReadDir(d.cfg.RBACDir)
	if err != nil {
		return fmt.Errorf("read rbac dir %s: %w", d.cfg.RBACDir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(d.cfg.RBACDir, e.Name()))
		if err != nil {
			return fmt.Errorf("read rbac file %s: %w", e.Name(), err)
		}
		filled := replaceRBACPlaceholders(string(raw), d.ns, d.cfg.ServiceAccount)
		manifest, names, err := suffixClusterRoleBindings(filled, d.ns)
		if err != nil {
			return fmt.Errorf("suffix cluster role bindings in %s: %w", e.Name(), err)
		}
		if err := d.kube.Apply(ctx, manifest); err != nil {
			return fmt.Errorf("apply rbac %s: %w", e.Name(), err)
		}
		d.mu.Lock()
		d.createdCRBs = append(d.createdCRBs, names...)
		d.mu.Unlock()
	}
	return nil
}

// fetchChart downloads the yugaware chart tarball, extracts it, patches its
// values.yaml, and returns the chart directory plus a cleanup func for the temp
// extraction dir.
func (d *Deployer) fetchChart(ctx context.Context) (string, func(), error) {
	tmp, err := os.MkdirTemp("", "ampqa-yba-chart-")
	if err != nil {
		return "", func() {}, fmt.Errorf("chart temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }

	tgz, err := d.download(ctx, chartS3URL(d.cfg.ChartVersion), tmp)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("download chart: %w", err)
	}
	chartDir, err := extractTarGz(tgz, tmp)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("extract chart: %w", err)
	}
	valuesPath := filepath.Join(chartDir, "values.yaml")
	content, err := os.ReadFile(valuesPath)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("read chart values.yaml: %w", err)
	}
	patched := patchValuesYAML(string(content), d.cfg.ImageRegistry, d.cfg.PullSecretName)
	if err := os.WriteFile(valuesPath, []byte(patched), 0o600); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("write chart values.yaml: %w", err)
	}
	return chartDir, cleanup, nil
}

// helmSets builds the --set-string map for the install.
func (d *Deployer) helmSets() map[string]string {
	return map[string]string{
		"yugaware.storageClass":     d.cfg.StorageClass,
		"yugaware.image.repository": d.cfg.ImageRegistry,
		"image.repository":          d.cfg.ImageRegistry,
	}
}

// helmInstallArgs builds the `helm install --wait` argv.
func (d *Deployer) helmInstallArgs(chartDir string) []string {
	argv := []string{
		"helm", "install", d.release, chartDir,
		"--namespace", d.ns,
		"--wait",
		"--timeout", durationArg(d.cfg.HelmTimeout),
	}
	if d.cfg.KubeContext != "" {
		argv = append(argv, "--kube-context", d.cfg.KubeContext)
	}
	argv = append(argv, sortedSetStringArgs(d.helmSets())...)
	return argv
}

func (d *Deployer) helmInstall(ctx context.Context, chartDir string) error {
	argv := d.helmInstallArgs(chartDir)
	if d.logf != nil {
		d.logf("exec: %s", k8s.CmdString(argv))
	}
	res, err := d.helmRun(ctx, nil, argv)
	if d.logf != nil {
		d.logf("exec done (exit %d):%s%s", res.ExitCode, k8s.LogTail("out", res.Stdout), k8s.LogTail("err", res.Stderr))
	}
	if err != nil {
		return fmt.Errorf("helm install: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("helm install exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// serviceName / podSelector default off the release name when unconfigured.
func (d *Deployer) serviceName() string {
	if d.cfg.ServiceName != "" {
		return d.cfg.ServiceName
	}
	return d.release + "-yugaware-ui"
}

func (d *Deployer) podSelector() string {
	if d.cfg.PodSelector != "" {
		return d.cfg.PodSelector
	}
	return "app=" + d.release + "-yugaware"
}

// resolveURL resolves how the platform is reached. loadbalancer reads the
// Service's external LoadBalancer ingress IP; tailnet resolves the Service's
// cluster-internal address (ClusterIP that answers, else the pod IP on its
// container target port) — the direct, port-forward-free path (mirrors
// ampstack.resolveControllerAddr).
func (d *Deployer) resolveURL(ctx context.Context) (string, error) {
	if d.cfg.Reach == ReachLoadBalancer {
		ip, err := d.kube.Get(ctx, "svc", d.serviceName(), "{.status.loadBalancer.ingress[0].ip}")
		if err != nil {
			return "", fmt.Errorf("read load-balancer ip of svc %s: %w", d.serviceName(), err)
		}
		if ip = strings.TrimSpace(ip); ip == "" {
			return "", fmt.Errorf("svc %s has no load-balancer ingress ip yet", d.serviceName())
		}
		return d.urlFor(ip), nil
	}
	if ip, err := d.kube.Get(ctx, "svc", d.serviceName(), "{.spec.clusterIP}"); err == nil {
		if ip = strings.TrimSpace(ip); ip != "" && ip != "None" && d.probe(ctx, ip) {
			return d.urlFor(ip), nil
		}
	}
	podIP, err := d.kube.GetSelector(ctx, "pod", d.podSelector(), "{.items[0].status.podIP}")
	if err != nil {
		return "", fmt.Errorf("yba ClusterIP unreachable; pod IP lookup failed: %w", err)
	}
	if podIP = strings.TrimSpace(podIP); podIP == "" {
		return "", fmt.Errorf("yba not reachable: no ClusterIP and no pod for selector %s", d.podSelector())
	}
	// The pod listens on the Service's targetPort (the container port), not the
	// Service port — reaching podIP:ServicePort would hit a closed port. The
	// Service VIP maps ServicePort→targetPort but is not routable from outside
	// the cluster, so the direct-to-pod path must use targetPort itself.
	return fmt.Sprintf("http://%s:%d", podIP, d.podTargetPort(ctx)), nil
}

// podTargetPort returns the container port the yugaware Service forwards
// ServicePort to (its targetPort) — the port the pod itself listens on. Falls
// back to ServicePort when the targetPort can't be read as a number.
func (d *Deployer) podTargetPort(ctx context.Context) int {
	jp := fmt.Sprintf("{.spec.ports[?(@.port==%d)].targetPort}", d.cfg.ServicePort)
	tp, err := d.kube.Get(ctx, "svc", d.serviceName(), jp)
	if err == nil {
		if n, perr := strconv.Atoi(strings.TrimSpace(tp)); perr == nil && n > 0 {
			return n
		}
	}
	return d.cfg.ServicePort
}

// probe reports whether the YBA at ip answers its version endpoint.
func (d *Deployer) probe(ctx context.Context, ip string) bool {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := d.newClient(d.urlFor(ip)).AppVersion(pctx)
	return err == nil
}

func (d *Deployer) urlFor(host string) string {
	return fmt.Sprintf("http://%s:%d", host, d.cfg.ServicePort)
}

// defaultNewClient builds a *yba.Client (empty customer/token — the pre-bootstrap
// state) with an insecure-TLS http.Client, so a self-signed https YBA is
// reachable. Plain http (the in-cluster port-80 default) works too.
func (d *Deployer) defaultNewClient(url string) *yba.Client {
	hc := &http.Client{Timeout: 30 * time.Second}
	if d.cfg.InsecureTLS {
		hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // YBA is commonly self-signed
	}
	return yba.New(url, "", "", yba.WithHTTPClient(hc), yba.WithLogf(d.logf))
}

// Teardown removes everything Deploy created, best-effort and LIFO, on its own
// (caller-supplied) deadline: the namespace-suffixed ClusterRoleBindings first
// (cluster-scoped — a namespace delete never reclaims them, so this is the real
// leak vector), then the namespace itself. It deletes the namespace ONLY after
// confirming the ampqa managed-by label, so a mistargeted namespace is never
// destroyed, and only ever touches bindings it suffixed with THIS namespace.
func (d *Deployer) Teardown(ctx context.Context) error {
	if d.ns == "" {
		return fmt.Errorf("ybastack teardown: no namespace")
	}
	var errs []string

	d.mu.Lock()
	crbs := append([]string(nil), d.createdCRBs...)
	d.mu.Unlock()
	cluster := d.clusterKube()
	for _, name := range crbs {
		// Only ever delete a binding this run suffixed with its own namespace.
		if !strings.HasSuffix(name, "-"+d.ns) {
			continue
		}
		if err := cluster.Delete(ctx, "clusterrolebinding", name); err != nil {
			errs = append(errs, fmt.Sprintf("delete clusterrolebinding %s: %v", name, err))
		}
	}

	// The yugaware chart itself creates cluster-scoped RBAC named after the helm
	// release (a ClusterRole + ClusterRoleBinding). A namespace delete does NOT
	// reap cluster-scoped objects, so remove them explicitly. The release name
	// carries this run's unique namespace, so it is ours; Delete is
	// --ignore-not-found, so an absent object (chart never installed) is a no-op.
	for _, kind := range []string{"clusterrolebinding", "clusterrole"} {
		if err := cluster.Delete(ctx, kind, d.release); err != nil {
			errs = append(errs, fmt.Sprintf("delete %s %s: %v", kind, d.release, err))
		}
	}

	label, err := cluster.Get(ctx, "namespace", d.ns, "{.metadata.labels."+labelPath(ManagedByLabel)+"}")
	if err != nil {
		errs = append(errs, fmt.Sprintf("read managed-by label on %s: %v", d.ns, err))
	} else if label != ManagedByValue {
		errs = append(errs, fmt.Sprintf("refusing to delete namespace %s: not ybastack-managed (%s=%q)", d.ns, ManagedByLabel, label))
	} else if err := cluster.DeleteNamespace(ctx, d.ns); err != nil {
		errs = append(errs, fmt.Sprintf("delete namespace %s: %v", d.ns, err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("ybastack teardown: %s", strings.Join(errs, "; "))
	}
	return nil
}

// clusterKube derives a namespace-unbound kubectl sharing this deployer's runner,
// for cluster-scoped operations (namespace + ClusterRoleBinding get/delete).
func (d *Deployer) clusterKube() *k8s.Kubectl {
	return k8s.New(k8s.Config{Context: d.cfg.KubeContext}).WithRunner(d.kube.Runner())
}

// --- os/exec-backed defaults ---

// ybaRunner returns the kubectl/helm one-shot runner. With a kubeconfig it shells
// each command with KUBECONFIG set in the child process env (never global —
// parallel-safe); without one it uses the shared ambient-authed k8s.OSRun.
func ybaRunner(kubeconfig string) k8s.Runner {
	if kubeconfig == "" {
		return k8s.OSRun
	}
	env := append(os.Environ(), "KUBECONFIG="+kubeconfig)
	return func(ctx context.Context, stdin io.Reader, argv []string) (execpkg.Result, error) {
		if len(argv) == 0 {
			return execpkg.Result{}, fmt.Errorf("empty argv")
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = env
		if stdin != nil {
			cmd.Stdin = stdin
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
			if ee, ok := err.(*exec.ExitError); ok {
				res.ExitCode = ee.ExitCode()
				return res, nil
			}
			res.ExitCode = -1
			return res, err
		}
		return res, nil
	}
}

// defaultChartDownload shells `aws s3 cp <s3URL> <destDir>/` (ambient-authed) and
// returns the local tarball path.
func defaultChartDownload(ctx context.Context, s3URL, destDir string) (string, error) {
	dest := filepath.Join(destDir, filepath.Base(s3URL))
	cmd := exec.CommandContext(ctx, "aws", "s3", "cp", s3URL, dest)
	var errb strings.Builder
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("aws s3 cp %s: %w: %s", s3URL, err, strings.TrimSpace(errb.String()))
	}
	return dest, nil
}

// extractTarGz extracts a .tgz into destDir and returns the path of the single
// top-level chart directory it contains.
func extractTarGz(tgzPath, destDir string) (string, error) {
	f, err := os.Open(tgzPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	topDir := ""
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		clean := filepath.Clean(hdr.Name)
		if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			continue // never write outside destDir (tar traversal guard)
		}
		target := filepath.Join(destDir, clean)
		if top := strings.SplitN(clean, string(filepath.Separator), 2)[0]; topDir == "" && top != "" {
			topDir = top
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			if err := writeFileFromTar(target, tr); err != nil {
				return "", err
			}
		}
	}
	if topDir == "" {
		return "", fmt.Errorf("chart tarball %s had no entries", tgzPath)
	}
	return filepath.Join(destDir, topDir), nil
}

func writeFileFromTar(target string, tr *tar.Reader) error {
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	// Bound each file to guard against a decompression bomb.
	if _, err := io.Copy(out, io.LimitReader(tr, 64<<20)); err != nil {
		return err
	}
	return nil
}

// durationArg renders a timeout as a helm/kubectl-friendly value (e.g. "40m0s").
func durationArg(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	return d.String()
}
