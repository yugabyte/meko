// Package ybk8s is the YugabyteDB-in-Kubernetes deploy App: it stands up (and
// tears down) a YugabyteDB cluster inside a single, isolated namespace using the
// upstream `yugabyte` helm chart, and hands back the two connection paths the
// co-located amp foundation test needs. It is the sibling of apps/ybastack —
// same shape, same kubectl/helm-shelled (ambient-authed) transport, same
// guaranteed-teardown discipline — but it deploys the YugabyteDB data plane
// (yb-master + yb-tserver StatefulSets) rather than a YBA control plane.
//
// It exists for the cheapest fully-GCP co-located mode: YugabyteDB and the amp
// control plane run in ONE GKE cluster, so amp↔YB traffic never leaves the
// cluster (no tailnet hop, no cross-cloud egress). Deploy exposes TWO paths:
//
//   - InClusterHostPorts — the tserver Service's cluster-internal DNS
//     ("<service>.<ns>.svc.cluster.local:5433"). This is the co-located path
//     the amp controller + endpoints (in-cluster pods) connect at.
//   - TailnetAddrs — the tserver pod IPs on :5433, reachable directly from this
//     coordinator over the Yugabyte tailnet (the same way ybastack reaches a
//     pod IP). This is the path the FRAMEWORK's own layer-1 workload — and the
//     one-shot amp storage-DB create — use, because the coordinator cannot
//     resolve an in-cluster .svc.cluster.local name.
//
// Isolation and no-leak are the load-bearing properties (AGENTS.md §3/§5/§8):
// the namespace is per-run (ampqa-yb-<runTag>) and labelled managed-by, so
// Teardown refuses any namespace it did not create; and because a StatefulSet's
// PVCs are NOT reclaimed by helm uninstall (and can outlive a namespace delete
// in some configs), Teardown deletes them explicitly — the leak vector here,
// the analogue of ybastack's cluster-scoped-CRB backstop.
package ybk8s

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	execpkg "github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// Defaults (core/defaults_test.go asserts the embedded default.yaml agrees).
const (
	DefaultNamespacePrefix   = "ampqa-yb"
	DefaultImageRegistry     = "yugabytedb/yugabyte"
	DefaultImagePullRegistry = "quay.io"

	// DefaultImagePullSecretName is the k8s Secret object created in the YB
	// namespace and referenced as the chart's Image.pullSecretName when a private
	// registry pull credential is configured (apps.ybk8s.image_pull_secret).
	DefaultImagePullSecretName = "yb-pull-secret"

	DefaultReplicationFactor = 1
	DefaultStorageClass      = "standard"
	DefaultStorageSize       = "10Gi"
	DefaultServiceName       = "yb-tserver-service"
	DefaultYSQLPort          = 5433
	DefaultMasterCPU         = "0.5"
	DefaultMasterMemory      = "1Gi"
	DefaultTServerCPU        = "0.5"
	DefaultTServerMemory     = "2Gi"
	DefaultReach             = ReachTailnet
	DefaultHelmTimeout       = 30 * time.Minute

	// DefaultUser is the YB superuser the framework and amp connect as; the
	// cluster's always-present maintenance database is likewise "yugabyte".
	DefaultUser = "yugabyte"

	// Chart-fixed object names/labels (the upstream `yugabyte` chart). Not
	// configurable — they are what the chart renders regardless of values.
	masterStatefulSet  = "yb-master"
	tserverStatefulSet = "yb-tserver"
	tserverPodSelector = "app=yb-tserver"
)

// ReachMode selects how the deployed cluster's tailnet endpoint is resolved for
// the framework's own (coordinator-side) workload.
type ReachMode string

const (
	// ReachTailnet (default) resolves the tserver pod IPs and reaches YSQL on
	// them directly over the tailnet — pod IPs are routable from this
	// coordinator (an in-cluster Service ClusterIP is not, so the pod IP is the
	// reliable path, mirroring ybastack's pod-IP fallback).
	ReachTailnet ReachMode = "tailnet"
)

// Config configures a YugabyteDB-in-k8s deploy.
type Config struct {
	KubeContext string
	Kubeconfig  string // "" → ambient KUBECONFIG / in-cluster config

	NamespacePrefix string
	ChartVersion    string // "" → versions.yugabytedb (resolved by FromConfig)
	ImageRegistry   string // chart Image.repository
	ImageTag        string // chart Image.tag; "" → ChartVersion

	// ImagePullSecret is the NAME of a declared config secret (secrets: section)
	// whose value is a "<user>:<pass>" private-registry token. "" (the default) →
	// no image-pull secret is created, so the public-image path is unchanged.
	// ImagePullRegistry is the registry that token authenticates (the
	// dockerconfigjson "auths" key, e.g. "quay.io"); ImagePullSecretName is the
	// k8s Secret object created in the namespace and set as Image.pullSecretName.
	ImagePullSecret     string
	ImagePullRegistry   string
	ImagePullSecretName string

	ReplicationFactor int
	MasterReplicas    int // 0 → ReplicationFactor
	TServerReplicas   int // 0 → ReplicationFactor

	StorageClass string
	StorageSize  string
	ServiceName  string // tserver Service name (its cluster-internal DNS + YSQL port)
	YSQLPort     int

	MasterCPU     string
	MasterMemory  string
	TServerCPU    string
	TServerMemory string

	Reach       ReachMode
	HelmRelease string // "" → "<namespace>"
	HelmTimeout time.Duration

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

// FromConfig reads apps.ybk8s.* into a Config. ChartVersion falls back to
// versions.yugabytedb when apps.ybk8s.chart_version is empty; ImageTag falls
// back to the resolved ChartVersion.
func FromConfig(get Getter) Config {
	chartVersion := get.GetString("apps.ybk8s.chart_version", "")
	if chartVersion == "" {
		chartVersion = get.GetString("versions.yugabytedb", "")
	}
	imageTag := get.GetString("apps.ybk8s.image_tag", "")
	if imageTag == "" {
		imageTag = chartVersion
	}
	return Config{
		// Fall back to the k8s context the run already provides (sharedk8s /
		// ampstack) so a self-provisioning test needs no ybk8s-specific context.
		KubeContext: get.GetString("apps.ybk8s.kubecontext",
			get.GetString("env.sharedk8s.context", get.GetString("env.ampstack.context", ""))),
		Kubeconfig:          get.GetString("apps.ybk8s.kubeconfig", ""),
		NamespacePrefix:     get.GetString("apps.ybk8s.namespace_prefix", DefaultNamespacePrefix),
		ChartVersion:        chartVersion,
		ImageRegistry:       get.GetString("apps.ybk8s.image_registry", DefaultImageRegistry),
		ImageTag:            imageTag,
		ImagePullSecret:     get.GetString("apps.ybk8s.image_pull_secret", ""),
		ImagePullRegistry:   get.GetString("apps.ybk8s.image_pull_registry", DefaultImagePullRegistry),
		ImagePullSecretName: get.GetString("apps.ybk8s.image_pull_secret_name", DefaultImagePullSecretName),
		ReplicationFactor:   get.GetInt("apps.ybk8s.replication_factor", DefaultReplicationFactor),
		MasterReplicas:      get.GetInt("apps.ybk8s.master_replicas", 0),
		TServerReplicas:     get.GetInt("apps.ybk8s.tserver_replicas", 0),
		StorageClass:        get.GetString("apps.ybk8s.storage_class", DefaultStorageClass),
		StorageSize:         get.GetString("apps.ybk8s.storage_size", DefaultStorageSize),
		ServiceName:         get.GetString("apps.ybk8s.service_name", DefaultServiceName),
		YSQLPort:            get.GetInt("apps.ybk8s.ysql_port", DefaultYSQLPort),
		MasterCPU:           get.GetString("apps.ybk8s.master_cpu", DefaultMasterCPU),
		MasterMemory:        get.GetString("apps.ybk8s.master_memory", DefaultMasterMemory),
		TServerCPU:          get.GetString("apps.ybk8s.tserver_cpu", DefaultTServerCPU),
		TServerMemory:       get.GetString("apps.ybk8s.tserver_memory", DefaultTServerMemory),
		Reach:               ReachMode(get.GetString("apps.ybk8s.reach", string(DefaultReach))),
		HelmRelease:         get.GetString("apps.ybk8s.helm_release", ""),
		HelmTimeout:         get.GetDuration("apps.ybk8s.helm_timeout", DefaultHelmTimeout),
	}
}

func applyDefaults(cfg *Config) {
	if cfg.NamespacePrefix == "" {
		cfg.NamespacePrefix = DefaultNamespacePrefix
	}
	if cfg.ImageRegistry == "" {
		cfg.ImageRegistry = DefaultImageRegistry
	}
	if cfg.ImageTag == "" {
		cfg.ImageTag = cfg.ChartVersion
	}
	if cfg.ImagePullRegistry == "" {
		cfg.ImagePullRegistry = DefaultImagePullRegistry
	}
	if cfg.ImagePullSecretName == "" {
		cfg.ImagePullSecretName = DefaultImagePullSecretName
	}
	if cfg.ReplicationFactor <= 0 {
		cfg.ReplicationFactor = DefaultReplicationFactor
	}
	if cfg.MasterReplicas <= 0 {
		cfg.MasterReplicas = cfg.ReplicationFactor
	}
	if cfg.TServerReplicas <= 0 {
		cfg.TServerReplicas = cfg.ReplicationFactor
	}
	if cfg.StorageClass == "" {
		cfg.StorageClass = DefaultStorageClass
	}
	if cfg.StorageSize == "" {
		cfg.StorageSize = DefaultStorageSize
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = DefaultServiceName
	}
	if cfg.YSQLPort == 0 {
		cfg.YSQLPort = DefaultYSQLPort
	}
	if cfg.MasterCPU == "" {
		cfg.MasterCPU = DefaultMasterCPU
	}
	if cfg.MasterMemory == "" {
		cfg.MasterMemory = DefaultMasterMemory
	}
	if cfg.TServerCPU == "" {
		cfg.TServerCPU = DefaultTServerCPU
	}
	if cfg.TServerMemory == "" {
		cfg.TServerMemory = DefaultTServerMemory
	}
	if cfg.Reach == "" {
		cfg.Reach = DefaultReach
	}
	if cfg.HelmTimeout <= 0 {
		cfg.HelmTimeout = DefaultHelmTimeout
	}
}

// Info is what a successful Deploy returns: the two connection paths plus the
// deploy's identity.
type Info struct {
	Namespace   string
	HelmRelease string
	Version     string // the image tag deployed
	ServiceName string
	YSQLPort    int
	User        string

	// InClusterHostPorts is the tserver Service's cluster-internal DNS host:port
	// ("<service>.<ns>.svc.cluster.local:<port>") — the co-located path the amp
	// controller + endpoints connect at.
	InClusterHostPorts string
	// TailnetAddrs are the tserver pod IPs on :<port>, reachable from this
	// coordinator over the tailnet — the path the framework's layer-1 workload
	// and the one-shot amp storage-DB create use.
	TailnetAddrs []string
}

// Deployer stands one YugabyteDB cluster up in one namespace and tears it down.
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
}

// SecretResolver maps a secret NAME to its resolved value. It matches
// core.Secrets.Value; taking a func keeps this package free of a core import
// (the apps/* convention). A resolver is needed only when
// apps.ybk8s.image_pull_secret names a private-registry credential.
type SecretResolver func(name string) (string, error)

// New builds a Deployer for cfg using the real kubectl/helm/aws binaries. runTag
// roots the per-run namespace and must be non-empty.
func New(cfg Config, runTag string) *Deployer {
	applyDefaults(&cfg)
	ns := namespaceFor(cfg.NamespacePrefix, runTag)
	release := helmReleaseFor(cfg.HelmRelease, ns)
	runner := ybRunner(cfg.Kubeconfig)
	return &Deployer{
		cfg:      cfg,
		runTag:   runTag,
		ns:       ns,
		release:  release,
		kube:     k8s.New(k8s.Config{Context: cfg.KubeContext, Namespace: ns}).WithRunner(runner),
		helmRun:  runner,
		logf:     log.Printf,
		download: defaultChartDownload,
	}
}

// Reap tears a ybk8s deploy down given only its namespace + kube settings — the
// separate-process `ampqa env rm` path, which reconstructs the reap from the
// registry record and has no live Deployer. It rebuilds a namespace-bound
// Deployer (the release defaults off the namespace exactly as New derived it
// when apps.ybk8s.helm_release was unset — the standing-env case) and runs the
// SAME Teardown (managed-by guard, helm uninstall, PVC delete, namespace delete),
// so there is one teardown implementation. kubeContext/kubeconfig target the
// cluster the deploy ran on.
func Reap(ctx context.Context, kubeContext, kubeconfig, namespace string) error {
	if namespace == "" {
		return fmt.Errorf("ybk8s reap: empty namespace")
	}
	runner := k8s.KubeconfigRunner(kubeconfig)
	d := &Deployer{
		cfg:     Config{KubeContext: kubeContext, Kubeconfig: kubeconfig},
		ns:      namespace,
		release: helmReleaseFor("", namespace),
		kube:    k8s.New(k8s.Config{Context: kubeContext, Namespace: namespace}).WithRunner(runner),
		helmRun: runner,
		logf:    log.Printf,
	}
	return d.Teardown(ctx)
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

// WithSecretResolver wires the resolver used to look up the private-registry
// pull credential named by apps.ybk8s.image_pull_secret (production callers pass
// core.Secrets.Value). Unset → any configured image_pull_secret is a Deploy
// error; the default public-image path needs no resolver.
func (d *Deployer) WithSecretResolver(f SecretResolver) *Deployer { d.resolveSecret = f; return d }

// WithLogf overrides the logger (nil silences).
func (d *Deployer) WithLogf(f func(format string, args ...any)) *Deployer { d.logf = f; return d }

func (d *Deployer) log(format string, args ...any) {
	if d.logf != nil {
		d.logf(format, args...)
	}
}

// Namespace is the deploy's namespace.
func (d *Deployer) Namespace() string { return d.ns }

// Deploy stands the cluster up and returns its two connection paths. On any
// step failure it leaves what it created in place (Teardown removes it) and
// returns the error.
func (d *Deployer) Deploy(ctx context.Context) (Info, error) {
	if d.runTag == "" {
		return Info{}, fmt.Errorf("ybk8s: run tag is required (it roots the namespace and reap)")
	}
	if d.cfg.ChartVersion == "" {
		return Info{}, fmt.Errorf("ybk8s: chart version is required (apps.ybk8s.chart_version or versions.yugabytedb)")
	}

	// 1. namespace (labelled so Teardown/reaper recognizes it).
	if err := d.kube.CreateNamespace(ctx, d.ns, namespaceLabels(d.cfg.ExtraNamespaceLabels)); err != nil {
		return Info{}, fmt.Errorf("create namespace %s: %w", d.ns, err)
	}

	// 1b. private-registry image-pull secret (no-op unless image_pull_secret set).
	// Applied before the chart install so the tserver/master pods can reference it.
	if err := d.ensurePullSecret(ctx); err != nil {
		return Info{}, err
	}

	// 2. chart: download + extract.
	chartDir, cleanup, err := d.fetchChart(ctx)
	if err != nil {
		return Info{}, err
	}
	defer cleanup()

	// 3. helm install --wait.
	if err := d.helmInstall(ctx, chartDir); err != nil {
		return Info{}, err
	}

	// 4. wait the tserver StatefulSet Ready (YSQL is serving once its pods are).
	want := fmt.Sprintf("jsonpath={.status.readyReplicas}=%d", d.cfg.TServerReplicas)
	if err := d.kube.WaitFor(ctx, "statefulset/"+tserverStatefulSet, want, d.cfg.HelmTimeout); err != nil {
		return Info{}, fmt.Errorf("tserver statefulset never became ready: %w", err)
	}

	// 5. resolve the coordinator-reachable (tailnet) tserver addresses.
	tailnet, err := d.resolveTailnetAddrs(ctx)
	if err != nil {
		return Info{}, err
	}

	return Info{
		Namespace:          d.ns,
		HelmRelease:        d.release,
		Version:            d.cfg.ImageTag,
		ServiceName:        d.cfg.ServiceName,
		YSQLPort:           d.cfg.YSQLPort,
		User:               DefaultUser,
		InClusterHostPorts: d.inClusterHostPorts(),
		TailnetAddrs:       tailnet,
	}, nil
}

// inClusterHostPorts is the tserver Service's cluster-internal DNS host:port —
// the co-located path amp connects at.
func (d *Deployer) inClusterHostPorts() string {
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", d.cfg.ServiceName, d.ns, d.cfg.YSQLPort)
}

// resolveTailnetAddrs returns every tserver pod IP on the YSQL port — the
// coordinator reaches these directly over the tailnet (a ClusterIP is not
// routable from here, so the pod IP is the reliable path — see ReachTailnet).
func (d *Deployer) resolveTailnetAddrs(ctx context.Context) ([]string, error) {
	out, err := d.kube.GetSelector(ctx, "pod", tserverPodSelector, "{.items[*].status.podIP}")
	if err != nil {
		return nil, fmt.Errorf("list tserver pod IPs: %w", err)
	}
	var addrs []string
	for _, ip := range strings.Fields(out) {
		if ip != "" {
			addrs = append(addrs, fmt.Sprintf("%s:%d", ip, d.cfg.YSQLPort))
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no tserver pod IPs for selector %s", tserverPodSelector)
	}
	return addrs, nil
}

// fetchChart downloads the yugabyte chart tarball and extracts it, returning the
// chart directory plus a cleanup func for the temp extraction dir. Unlike
// ybastack, no values.yaml patch is needed — every override goes via helm --set.
func (d *Deployer) fetchChart(ctx context.Context) (string, func(), error) {
	tmp, err := os.MkdirTemp("", "ampqa-ybk8s-chart-")
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
	return chartDir, cleanup, nil
}

// helmSets builds the --set-string map for the install: replica counts and the
// replication_factor gflag (RF is driven by the master count + the master
// gflag), the image, storage, and small resource requests (cheap), and
// enableLoadBalancer=false so the Services stay ClusterIP (co-located; reached
// via pod IP over the tailnet).
func (d *Deployer) helmSets() map[string]string {
	sets := map[string]string{
		"enableLoadBalancer":               "false",
		"Image.repository":                 d.cfg.ImageRegistry,
		"Image.tag":                        d.cfg.ImageTag,
		"replicas.master":                  strconv.Itoa(d.cfg.MasterReplicas),
		"replicas.tserver":                 strconv.Itoa(d.cfg.TServerReplicas),
		"gflags.master.replication_factor": strconv.Itoa(d.cfg.ReplicationFactor),
		"storage.master.storageClass":      d.cfg.StorageClass,
		"storage.tserver.storageClass":     d.cfg.StorageClass,
		"storage.master.size":              d.cfg.StorageSize,
		"storage.tserver.size":             d.cfg.StorageSize,
		"resource.master.requests.cpu":     d.cfg.MasterCPU,
		"resource.master.requests.memory":  d.cfg.MasterMemory,
		"resource.tserver.requests.cpu":    d.cfg.TServerCPU,
		"resource.tserver.requests.memory": d.cfg.TServerMemory,
	}
	// Point the chart at the in-namespace pull secret only when one is configured
	// (the chart's Image.pullSecretName — verified in the yugabyte chart).
	if d.cfg.ImagePullSecret != "" {
		sets["Image.pullSecretName"] = d.cfg.ImagePullSecretName
	}
	return sets
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
	res, err := d.helmRun(ctx, nil, d.helmInstallArgs(chartDir))
	if err != nil {
		return fmt.Errorf("helm install: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("helm install exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// ensurePullSecret applies the private-registry image-pull Secret into the YB
// namespace when apps.ybk8s.image_pull_secret names a declared credential. It is
// a no-op for the default public-image path (empty name). The named secret's
// value is a "<user>:<pass>" registry token; it is resolved via the injected
// resolver, encoded into a dockerconfigjson, and applied — the token itself is
// never logged, only its base64 form reaches the manifest bytes on stdin.
func (d *Deployer) ensurePullSecret(ctx context.Context) error {
	if d.cfg.ImagePullSecret == "" {
		return nil
	}
	if d.resolveSecret == nil {
		return fmt.Errorf("ybk8s: image_pull_secret %q set but no secret resolver available (WithSecretResolver)", d.cfg.ImagePullSecret)
	}
	token, err := d.resolveSecret(d.cfg.ImagePullSecret)
	if err != nil {
		return fmt.Errorf("resolve image_pull_secret %q: %w", d.cfg.ImagePullSecret, err)
	}
	b64, err := dockerConfigJSONB64(d.cfg.ImagePullRegistry, token)
	if err != nil {
		return fmt.Errorf("build image-pull secret: %w", err)
	}
	if err := d.kube.Apply(ctx, pullSecretManifest(d.ns, d.cfg.ImagePullSecretName, b64)); err != nil {
		return fmt.Errorf("apply image-pull secret: %w", err)
	}
	return nil
}

// Teardown removes everything Deploy created, best-effort, on its own
// (caller-supplied) deadline: it refuses any namespace lacking THIS run's
// managed-by label, then helm-uninstalls the release, explicitly deletes the
// StatefulSet PVCs (the leak vector — a StatefulSet's PVCs survive both helm
// uninstall and, in some storage configs, a namespace delete), and finally
// deletes the namespace. It only ever touches the ampqa-yb-<runTag> namespace
// it created and the PVCs inside it.
func (d *Deployer) Teardown(ctx context.Context) error {
	if d.ns == "" {
		return fmt.Errorf("ybk8s teardown: no namespace")
	}
	cluster := d.clusterKube()

	label, err := cluster.Get(ctx, "namespace", d.ns, "{.metadata.labels."+labelPath(ManagedByLabel)+"}")
	if err != nil {
		return fmt.Errorf("read managed-by label on %s: %w", d.ns, err)
	}
	if label != ManagedByValue {
		return fmt.Errorf("refusing to touch namespace %s: not ybk8s-managed (%s=%q, want %q)",
			d.ns, ManagedByLabel, label, ManagedByValue)
	}

	var errs []string

	// helm uninstall first (removes the chart's namespaced objects cleanly); a
	// missing release is not fatal — the namespace delete below is the backstop.
	if res, herr := d.helmRun(ctx, nil, d.helmUninstallArgs()); herr != nil {
		errs = append(errs, fmt.Sprintf("helm uninstall %s: %v", d.release, herr))
	} else if res.ExitCode != 0 && !strings.Contains(res.Stderr, "not found") {
		errs = append(errs, fmt.Sprintf("helm uninstall %s exited %d: %s", d.release, res.ExitCode, strings.TrimSpace(res.Stderr)))
	}

	// Explicitly reclaim the StatefulSet PVCs (helm uninstall leaves them).
	if err := d.deletePVCs(ctx); err != nil {
		errs = append(errs, err.Error())
	}

	if err := cluster.DeleteNamespace(ctx, d.ns); err != nil {
		errs = append(errs, fmt.Sprintf("delete namespace %s: %v", d.ns, err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("ybk8s teardown: %s", strings.Join(errs, "; "))
	}
	return nil
}

// helmUninstallArgs builds the `helm uninstall` argv (namespace-scoped).
func (d *Deployer) helmUninstallArgs() []string {
	argv := []string{"helm", "uninstall", d.release, "--namespace", d.ns, "--ignore-not-found"}
	if d.cfg.KubeContext != "" {
		argv = append(argv, "--kube-context", d.cfg.KubeContext)
	}
	return argv
}

// deletePVCs deletes every PersistentVolumeClaim the chart's volumeClaimTemplates
// stamped for this release — the leak vector. The chart's "yugabyte.labels" helper
// (templates/_helpers.tpl) puts heritage/release/chart/component on the PVCs, NOT
// app=... (that label only lands on the StatefulSet/pods via a separate
// "yugabyte.applabel" helper); component is also identical across the master and
// tserver StatefulSets, so release=<release> is the one PVC-stamped label that is
// both chart-guaranteed and unique to this install, and it covers both PVC sets in
// a single selector. It lists the names first (the k8s transport has no
// delete-by-selector), then deletes each; a namespace with no matching PVC is a
// no-op.
func (d *Deployer) deletePVCs(ctx context.Context) error {
	selector := "release=" + d.release
	names, err := d.kube.GetSelector(ctx, "pvc", selector, "{.items[*].metadata.name}")
	if err != nil {
		return fmt.Errorf("list pvcs (%s): %v", selector, err)
	}
	var bad []string
	for _, name := range strings.Fields(names) {
		if name == "" {
			continue
		}
		if err := d.kube.Delete(ctx, "persistentvolumeclaim", name); err != nil {
			bad = append(bad, fmt.Sprintf("delete pvc %s: %v", name, err))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s", strings.Join(bad, "; "))
	}
	return nil
}

// clusterKube derives a namespace-unbound kubectl sharing this deployer's runner,
// for cluster-scoped operations (namespace get/delete).
func (d *Deployer) clusterKube() *k8s.Kubectl {
	return k8s.New(k8s.Config{Context: d.cfg.KubeContext}).WithRunner(d.kube.Runner())
}

// --- object/label names + pure helpers ---

// Object/label names ybk8s owns in a namespace. The managed-by label lets
// Teardown refuse a namespace it did not create (mirrors ybastack/ampstack).
const (
	ManagedByLabel = "ampqa-managed-by"
	ManagedByValue = "ybk8s"
)

// dockerConfigJSONB64 builds the base64 .dockerconfigjson body of a
// docker-registry Secret for a PRIVATE registry (e.g. quay.io) from a
// "<user>:<pass>" login token: the docker "auth" field is base64(token) — the
// token base64'd WHOLE, NOT the ECR base64("AWS:"+token) form (see
// apps/ampstack.dockerConfigJSONB64). registry is the "auths" key. This is the
// shape the stress framework pulls quay.io/yugabyte/yugabyte-itest images with.
func dockerConfigJSONB64(registry, token string) (string, error) {
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("image-pull token is empty")
	}
	if registry == "" {
		registry = DefaultImagePullRegistry
	}
	auth := base64.StdEncoding.EncodeToString([]byte(token))
	cfg := map[string]any{
		"auths": map[string]any{
			registry: map[string]any{"auth": auth, "email": ""},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// pullSecretManifest renders a dockerconfigjson image-pull Secret from a
// pre-encoded .dockerconfigjson value, labelled ampqa-managed so it is reaped
// with the namespace. Mirrors ybastack/ampstack.pullSecretManifest.
func pullSecretManifest(namespace, name, dockerConfigB64 string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
  labels:
    ampqa: "1"
    %s: %s
type: kubernetes.io/dockerconfigjson
data:
  .dockerconfigjson: %s
`, name, namespace, ManagedByLabel, ManagedByValue, dockerConfigB64)
}

// namespaceFor renders the tagged, reapable namespace name: "<prefix>-<runTag>".
func namespaceFor(prefix, runTag string) string {
	if prefix == "" {
		prefix = DefaultNamespacePrefix
	}
	return prefix + "-" + runTag
}

// helmReleaseFor derives the helm release name from the namespace when none is
// configured (the namespace is already per-run unique).
func helmReleaseFor(release, namespace string) string {
	if release != "" {
		return release
	}
	return namespace
}

// shortVersion is the first three dot components of a version string
// ("2025.2.1.0-b141" → "2025.2.1"), matching the chart tarball filename.
func shortVersion(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) <= 3 {
		return version
	}
	return strings.Join(parts[:3], ".")
}

// chartS3URL builds the S3 URL of the yugabyte helm chart tarball for a version
// (same bucket ybastack uses for yugaware): the directory is the full version,
// the tarball filename carries the short (three-component) version.
func chartS3URL(version string) string {
	return fmt.Sprintf("s3://releases.yugabyte.com/%s/helm/yugabyte-%s.tgz", version, shortVersion(version))
}

// namespaceLabels are stamped on the namespace ybk8s creates, so Teardown (and
// any reaper) can recognize and safely reclaim it.
func namespaceLabels(extra map[string]string) map[string]string {
	out := map[string]string{
		"ampqa":        "1",
		ManagedByLabel: ManagedByValue,
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// labelPath escapes dots in a label key for a kubectl jsonpath segment.
func labelPath(key string) string { return strings.ReplaceAll(key, ".", `\.`) }

// sortedSetStringArgs renders a stable, sorted "--set-string k=v" argv from a
// map, so the helm install command is deterministic.
func sortedSetStringArgs(kv map[string]string) []string {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	// small map: insertion sort keeps output stable without importing sort.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	out := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		out = append(out, "--set-string", k+"="+kv[k])
	}
	return out
}

// durationArg renders a timeout as a helm-friendly value (e.g. "30m0s").
func durationArg(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	return d.String()
}

// FirstHostPort splits the first "host:port" of a CSV into a host and port,
// defaulting the port to 5433 when absent/unparsable. Exposed so a caller
// (the foundation provisioner) can build a pg.ConnInfo from TailnetAddrs
// without re-implementing the parse.
func FirstHostPort(csv string) (string, int) {
	first := csv
	if i := strings.IndexByte(csv, ','); i >= 0 {
		first = csv[:i]
	}
	first = strings.TrimSpace(first)
	host, portStr, err := net.SplitHostPort(first)
	if err != nil {
		return first, DefaultYSQLPort
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return host, DefaultYSQLPort
	}
	return host, port
}

// --- os/exec-backed defaults ---

// ybRunner returns the kubectl/helm one-shot runner. With a kubeconfig it shells
// each command with KUBECONFIG set in the child process env (never global —
// parallel-safe); without one it uses the shared ambient-authed k8s.OSRun.
func ybRunner(kubeconfig string) k8s.Runner {
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
