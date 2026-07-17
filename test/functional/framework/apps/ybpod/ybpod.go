// Package ybpod is the yugabyted-in-pod deploy App: it stands up a single-node
// (RF1) YugabyteDB inside one isolated namespace WITHOUT a promoted container
// image. Where apps/ybk8s deploys the upstream `yugabyte` helm chart — which
// needs the YB container image published at a tag (Docker Hub / quay), and so
// fails for any build whose image was never promoted — this backing depends only
// on the release TARBALL, which is always present in the S3 release bucket for
// every build.
//
// It is pure glue over three existing pieces (nothing here reimplements them):
//
//   - apps/clientpod (K8s shape) starts a bare Pod from a generic, PUBLIC base
//     image (default almalinux:8) and hands back a RemoteExecutor bound to it.
//   - apps/yugabyted drives `yugabyted start` over that executor, delivering the
//     el8 release tarball via the artifact system before start (Spec.Deploy).
//   - the artifact deployer fetches the tarball on the COORDINATOR (aws s3 cp,
//     ambient-authed) and pushes it into the pod with `kubectl cp` — so the pod
//     itself needs no AWS credentials, only glibc + tar/gzip (+ a Python 3.7
//     interpreter, which Start provisions via dnf; see the base-image note).
//
// Like apps/ybk8s, Deploy returns the two connection paths the amp foundation
// needs and Colocated() packages them as a ybbacking handle for ampstack's
// external backing:
//
//   - InClusterHostPorts — the YSQL Service's cluster-internal DNS
//     ("<service>.<ns>.svc.cluster.local:5433"), the path the amp controller +
//     endpoints (in-cluster) connect at.
//   - TailnetAddrs — the pod IP on :5433, reachable directly from this
//     coordinator over the Yugabyte tailnet (the framework's own workload and the
//     one-shot amp storage-DB create use it; the coordinator cannot resolve an
//     in-cluster .svc.cluster.local name).
//
// Isolation + no-leak (AGENTS.md §5/§8): the namespace is per-run and labelled
// managed-by=ybpod, so Teardown/Reap refuse any namespace they did not create;
// the pod uses ephemeral storage only (no PVC leak vector), so deleting the
// namespace reclaims everything.
package ybpod

import (
	"context"
	"fmt"
	"log"
	"path"
	"strings"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/clientpod"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/ybbacking"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/yugabyted"
	"github.com/yugabyte/yb-amp/test/functional/framework/artifact"
	"github.com/yugabyte/yb-amp/test/functional/framework/core"
	execpkg "github.com/yugabyte/yb-amp/test/functional/framework/exec"
	"github.com/yugabyte/yb-amp/test/functional/framework/exec/k8s"
)

// Defaults (core/defaults_test.go asserts the embedded default.yaml agrees).
const (
	DefaultNamespacePrefix = "ampqa-ybpod"

	// DefaultBaseImage is a generic, PUBLIC el8 base that can run the el8 release
	// tarball: it ships glibc, tar/gzip (for the remote untar) and dnf (used to
	// provision Python 3.7+, which yugabyted's launcher needs — almalinux:8's
	// stock python3 is 3.6). It is image-promotion-independent — the whole point
	// of this backing. Override via apps.ybpod.base_image; a private base needs
	// apps.ybpod.base_image_pull_secret (a pre-existing docker-registry Secret).
	DefaultBaseImage = "almalinux:8"

	// DefaultArch selects the S3 tarball's os/arch variant (see tarballFilename).
	// The k8s node the pod lands on MUST match this arch.
	DefaultArch = "x86_64"

	// DefaultInstallDir is the pod directory the release tarball lands in and
	// unpacks under; the yugabyted install root is <install_dir>/yugabyte-<base>.
	// Rooted under /root because the pod runs as root (unlike the awsvm ec2-user
	// path apps/yugabyted defaults to).
	DefaultInstallDir = "/root/yb-software"
	// DefaultBaseDir is yugabyted's --base_dir inside the pod (must be writable
	// by root, NOT the /home/ec2-user default apps/yugabyted assumes on a VM).
	DefaultBaseDir = "/root/yugabyte-data"

	DefaultServiceName  = "ybpod-ysql"
	DefaultYSQLPort     = 5433
	DefaultReadyTimeout = 10 * time.Minute

	// DefaultUser / DefaultDatabase are the YB superuser and maintenance database
	// a fresh yugabyted always provisions.
	DefaultUser     = "yugabyte"
	DefaultDatabase = "yugabyte"

	// podPythonSetupCmd provisions Python 3.7+ on the base image before any
	// yugabyted command. It is the non-sudo form of apps/yugabyted's default
	// (the pod runs as root, and a minimal base image has no sudo): probe first,
	// only dnf-install python39 when the stock interpreter is too old.
	podPythonSetupCmd = `python3 -c 'import dataclasses' 2>/dev/null || { dnf install -y python39 && { alternatives --set python3 /usr/bin/python3.9 2>/dev/null || alternatives --install /usr/bin/python3 python3 /usr/bin/python3.9 60; }; }`

	// Object/label names ybpod owns in a namespace. The managed-by label lets
	// Teardown/Reap refuse a namespace they did not create (mirrors ybk8s).
	ManagedByLabel = "ampqa-managed-by"
	ManagedByValue = "ybpod"

	// podSelector matches the clientpod-started pod (clientpod labels every pod
	// ampqa=1); the per-run namespace holds exactly one, so it selects it alone.
	podSelector = "ampqa=1"
)

// Config configures a yugabyted-in-pod deploy.
type Config struct {
	KubeContext string
	Kubeconfig  string // "" → ambient KUBECONFIG / in-cluster config

	NamespacePrefix string

	// BaseImage is the bare pod's image (default almalinux:8). BaseImagePullSecret
	// names a PRE-EXISTING docker-registry Secret in the namespace the kubelet uses
	// to pull a private BaseImage; "" (the default) → BaseImage is public.
	BaseImage           string
	BaseImagePullSecret string

	// Version is the YB release the pod runs (e.g. "2025.2.1.0-b141"); "" →
	// versions.yugabytedb (resolved by FromConfig). Arch selects the S3 tarball
	// variant ("x86_64" or "aarch64").
	Version string
	Arch    string

	InstallDir string
	BaseDir    string

	ServiceName  string
	YSQLPort     int
	ReadyTimeout time.Duration

	// ExtraNamespaceLabels are merged onto the namespace (e.g. a reaper stamp).
	ExtraNamespaceLabels map[string]string
}

// Getter is the minimal typed-config surface FromConfig needs (structurally
// satisfied by *core.Config, so this package imports core only for BaseVersion).
type Getter interface {
	GetString(key, def string) string
	GetInt(key string, def int) int
	GetDuration(key string, def time.Duration) time.Duration
}

// FromConfig reads apps.ybpod.* into a Config. Version falls back to
// versions.yugabytedb when apps.ybpod.version is empty.
func FromConfig(get Getter) Config {
	version := get.GetString("apps.ybpod.version", "")
	if version == "" {
		version = get.GetString("versions.yugabytedb", "")
	}
	return Config{
		KubeContext:         get.GetString("apps.ybpod.kubecontext", ""),
		Kubeconfig:          get.GetString("apps.ybpod.kubeconfig", ""),
		NamespacePrefix:     get.GetString("apps.ybpod.namespace_prefix", DefaultNamespacePrefix),
		BaseImage:           get.GetString("apps.ybpod.base_image", DefaultBaseImage),
		BaseImagePullSecret: get.GetString("apps.ybpod.base_image_pull_secret", ""),
		Version:             version,
		Arch:                get.GetString("apps.ybpod.arch", DefaultArch),
		InstallDir:          get.GetString("apps.ybpod.install_dir", DefaultInstallDir),
		BaseDir:             get.GetString("apps.ybpod.base_dir", DefaultBaseDir),
		ServiceName:         get.GetString("apps.ybpod.service_name", DefaultServiceName),
		YSQLPort:            get.GetInt("apps.ybpod.ysql_port", DefaultYSQLPort),
		ReadyTimeout:        get.GetDuration("apps.ybpod.ready_timeout", DefaultReadyTimeout),
	}
}

func applyDefaults(cfg *Config) {
	if cfg.NamespacePrefix == "" {
		cfg.NamespacePrefix = DefaultNamespacePrefix
	}
	if cfg.BaseImage == "" {
		cfg.BaseImage = DefaultBaseImage
	}
	if cfg.Arch == "" {
		cfg.Arch = DefaultArch
	}
	if cfg.InstallDir == "" {
		cfg.InstallDir = DefaultInstallDir
	}
	if cfg.BaseDir == "" {
		cfg.BaseDir = DefaultBaseDir
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = DefaultServiceName
	}
	if cfg.YSQLPort == 0 {
		cfg.YSQLPort = DefaultYSQLPort
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = DefaultReadyTimeout
	}
}

// Info is what a successful Deploy returns: the two connection paths plus the
// deploy's identity.
type Info struct {
	Namespace   string
	PodName     string
	Version     string
	ServiceName string
	YSQLPort    int
	User        string

	// TailnetConn is the coordinator-reachable YSQL target (the pod IP over the
	// tailnet). TailnetAddrs is the same as a host:port list.
	TailnetConn  pg.ConnInfo
	TailnetAddrs []string
	// InClusterHostPorts is the YSQL Service's cluster-internal DNS host:port —
	// the co-located path the amp controller connects at.
	InClusterHostPorts string
}

// Colocated packages the two paths as a ybbacking handle: the tailnet endpoint
// is the coordinator-reachable path (framework workload + one-shot storage-DB
// create), the in-cluster Service DNS is the amp controller/endpoints path.
// ybbacking.ExternalBacking routes it into an ampstack external backing.
func (i Info) Colocated() ybbacking.Colocated {
	return ybbacking.FromYBK8s(i.TailnetConn, i.TailnetAddrs, []string{i.InClusterHostPorts})
}

// artifactDeployer is the subset of *artifact.Deployer used, injectable so tests
// assert the tarball delivery without a real fetch/kubectl-cp. It matches the
// interface apps/yugabyted's WithDeployer takes.
type artifactDeployer interface {
	Deploy(ctx context.Context, execs []execpkg.RemoteExecutor, spec artifact.Spec, opts artifact.DeployOptions) error
}

// Deployer stands one yugabyted-in-pod up in one namespace and tears it down.
type Deployer struct {
	cfg     Config
	runTag  string
	ns      string
	podName string
	kube    *k8s.Kubectl
	logf    func(format string, args ...any)

	// Injected in tests / by composition; nil → apps/yugabyted's real defaults.
	ybDeployer artifactDeployer
	waitPG     func(ctx context.Context, ci pg.ConnInfo, timeout time.Duration) error

	// Live handles after Deploy, for Collect (best-effort log pull).
	pod *clientpod.K8s
	yb  *yugabyted.App
}

// New builds a Deployer for cfg using the real kubectl/aws binaries. runTag roots
// the per-run namespace + pod name and must be non-empty.
func New(cfg Config, runTag string) *Deployer {
	applyDefaults(&cfg)
	ns := namespaceFor(cfg.NamespacePrefix, runTag)
	runner := k8s.KubeconfigRunner(cfg.Kubeconfig)
	return &Deployer{
		cfg:     cfg,
		runTag:  runTag,
		ns:      ns,
		podName: podNameFor(runTag),
		kube:    k8s.New(k8s.Config{Context: cfg.KubeContext, Namespace: ns}).WithRunner(runner),
		logf:    log.Printf,
	}
}

// Reap tears a ybpod deploy down given only its namespace + kube settings — the
// separate-process `ampqa env rm` path, which has no live Deployer. It runs the
// SAME managed-by-guarded namespace delete Teardown does.
func Reap(ctx context.Context, kubeContext, kubeconfig, namespace string) error {
	if namespace == "" {
		return fmt.Errorf("ybpod reap: empty namespace")
	}
	runner := k8s.KubeconfigRunner(kubeconfig)
	d := &Deployer{
		cfg:  Config{KubeContext: kubeContext, Kubeconfig: kubeconfig},
		ns:   namespace,
		kube: k8s.New(k8s.Config{Context: kubeContext, Namespace: namespace}).WithRunner(runner),
		logf: log.Printf,
	}
	return d.Teardown(ctx)
}

// --- injection setters (composition + tests) ---

// WithKube replaces the namespace-bound kubectl (tests inject a scripted transport).
func (d *Deployer) WithKube(k *k8s.Kubectl) *Deployer { d.kube = k; return d }

// WithArtifactDeployer overrides the tarball deployer apps/yugabyted uses (tests
// inject a recorder; nil → the real artifact.Deployer).
func (d *Deployer) WithArtifactDeployer(dep artifactDeployer) *Deployer { d.ybDeployer = dep; return d }

// WithWaitPG overrides the YSQL readiness probe apps/yugabyted uses (tests inject
// a no-op; nil → the real pg.WaitReady).
func (d *Deployer) WithWaitPG(f func(context.Context, pg.ConnInfo, time.Duration) error) *Deployer {
	d.waitPG = f
	return d
}

// WithLogf overrides the logger (nil silences).
func (d *Deployer) WithLogf(f func(format string, args ...any)) *Deployer { d.logf = f; return d }

func (d *Deployer) log(format string, args ...any) {
	if d.logf != nil {
		d.logf(format, args...)
	}
}

// Namespace is the deploy's namespace.
func (d *Deployer) Namespace() string { return d.ns }

// Deploy starts the pod, deploys+starts yugabyted inside it over the tarball,
// exposes YSQL via a Service, and returns the two connection paths. On any step
// failure it leaves what it created in place (Teardown removes it).
func (d *Deployer) Deploy(ctx context.Context) (Info, error) {
	if d.runTag == "" {
		return Info{}, fmt.Errorf("ybpod: run tag is required (it roots the namespace and reap)")
	}
	if d.cfg.Version == "" {
		return Info{}, fmt.Errorf("ybpod: version is required (apps.ybpod.version or versions.yugabytedb)")
	}

	// 1. namespace (labelled so Teardown/reaper recognizes it).
	if err := d.kube.CreateNamespace(ctx, d.ns, namespaceLabels(d.cfg.ExtraNamespaceLabels)); err != nil {
		return Info{}, fmt.Errorf("create namespace %s: %w", d.ns, err)
	}

	// 2. bare pod from the public base image, kept alive by `sleep infinity` so we
	// can exec yugabyted into it (a bare image would otherwise exit immediately).
	d.pod = clientpod.NewK8s(d.kube)
	if err := d.pod.Start(ctx, clientpod.Spec{
		Name:             d.podName,
		Image:            d.cfg.BaseImage,
		Command:          []string{"sleep", "infinity"},
		ImagePullSecrets: pullSecrets(d.cfg.BaseImagePullSecret),
	}); err != nil {
		return Info{}, fmt.Errorf("start base pod: %w", err)
	}

	// 3. resolve the pod IP — yugabyted's --advertise_address and the
	// coordinator-reachable (tailnet) YSQL host.
	podIP, err := d.kube.Get(ctx, "pod", d.podName, "{.status.podIP}")
	if err != nil {
		return Info{}, fmt.Errorf("resolve pod IP: %w", err)
	}
	if podIP == "" {
		return Info{}, fmt.Errorf("pod %s has no IP", d.podName)
	}

	// 4. yugabyted over the pod executor: deliver the el8 tarball (coordinator
	// fetch + kubectl cp), then `yugabyted start` a single-node (RF1) cluster.
	d.yb = d.newYugabyted(podIP)
	if err := d.yb.Start(ctx); err != nil {
		return Info{}, fmt.Errorf("start yugabyted in pod: %w", err)
	}

	// 5. expose YSQL in-cluster via a Service selecting the pod on the YSQL port.
	if err := d.kube.Apply(ctx, serviceManifest(d.ns, d.cfg.ServiceName, d.cfg.YSQLPort)); err != nil {
		return Info{}, fmt.Errorf("apply YSQL service: %w", err)
	}

	// 6. wait until YSQL answers and yugabyted reports healthy.
	if err := d.yb.Wait(ctx); err != nil {
		return Info{}, fmt.Errorf("yugabyted never became ready: %w", err)
	}

	conn := d.yb.ConnInfo()
	return Info{
		Namespace:          d.ns,
		PodName:            d.podName,
		Version:            d.cfg.Version,
		ServiceName:        d.cfg.ServiceName,
		YSQLPort:           d.cfg.YSQLPort,
		User:               DefaultUser,
		TailnetConn:        conn,
		TailnetAddrs:       d.yb.YSQLAddrs(),
		InClusterHostPorts: d.inClusterHostPorts(),
	}, nil
}

// newYugabyted builds the yugabyted App over the pod executor, with the el8
// tarball Spec.Deploy and the pod-appropriate base/install dirs + python setup.
func (d *Deployer) newYugabyted(podIP string) *yugabyted.App {
	spec := yugabyted.Spec{
		InstallDir:   d.ybInstallDir(),
		BaseDir:      d.cfg.BaseDir,
		RF:           1,
		YSQLPort:     d.cfg.YSQLPort,
		ReadyTimeout: d.cfg.ReadyTimeout,
		Deploy: &yugabyted.DeploySpec{
			Artifact:  tarballSpec(d.cfg.Version, d.cfg.Arch),
			RemoteDir: d.cfg.InstallDir,
		},
	}
	node := yugabyted.Node{Exec: d.pod.Exec(), Advertise: podIP}
	opts := []yugabyted.Option{
		yugabyted.WithLogf(d.logf),
		yugabyted.WithPythonSetupCmd(podPythonSetupCmd),
	}
	if d.ybDeployer != nil {
		opts = append(opts, yugabyted.WithDeployer(d.ybDeployer))
	}
	if d.waitPG != nil {
		opts = append(opts, yugabyted.WithWaitPG(d.waitPG))
	}
	return yugabyted.New([]yugabyted.Node{node}, spec, opts...)
}

// ybInstallDir is the yugabyted install root: the tarball unpacks to a directory
// named yugabyte-<base-version> (build/os/arch suffix dropped) under InstallDir.
func (d *Deployer) ybInstallDir() string {
	return path.Join(d.cfg.InstallDir, extractedDirName(d.cfg.Version))
}

// inClusterHostPorts is the YSQL Service's cluster-internal DNS host:port.
func (d *Deployer) inClusterHostPorts() string {
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", d.cfg.ServiceName, d.ns, d.cfg.YSQLPort)
}

// Collect pulls the pod's yugabyted/master/tserver logs (via the yugabyted app)
// and the raw pod logs (via clientpod) into into. Best-effort; safe to call only
// after Deploy started them. Callers run it before Teardown (collect-before-kill).
func (d *Deployer) Collect(ctx context.Context, into apps.ArtifactSink) error {
	if d.yb != nil {
		if err := d.yb.Collect(ctx, into); err != nil {
			d.log("ybpod collect: yugabyted logs: %v", err)
		}
	}
	if d.pod != nil {
		if err := d.pod.CollectInto(ctx, into); err != nil {
			d.log("ybpod collect: pod logs: %v", err)
		}
	}
	return nil
}

// Teardown removes everything Deploy created, best-effort, on its own deadline: it
// refuses any namespace lacking THIS run's managed-by label, then deletes the
// namespace (which reclaims the pod + Service; the pod uses ephemeral storage, so
// there is no PVC leak vector).
func (d *Deployer) Teardown(ctx context.Context) error {
	if d.ns == "" {
		return fmt.Errorf("ybpod teardown: no namespace")
	}
	cluster := d.clusterKube()

	label, err := cluster.Get(ctx, "namespace", d.ns, "{.metadata.labels."+labelPath(ManagedByLabel)+"}")
	if err != nil {
		return fmt.Errorf("read managed-by label on %s: %w", d.ns, err)
	}
	if label != ManagedByValue {
		return fmt.Errorf("refusing to touch namespace %s: not ybpod-managed (%s=%q, want %q)",
			d.ns, ManagedByLabel, label, ManagedByValue)
	}
	if err := cluster.DeleteNamespace(ctx, d.ns); err != nil {
		return fmt.Errorf("delete namespace %s: %w", d.ns, err)
	}
	return nil
}

// clusterKube derives a namespace-unbound kubectl sharing this deployer's runner,
// for cluster-scoped operations (namespace get/delete).
func (d *Deployer) clusterKube() *k8s.Kubectl {
	return k8s.New(k8s.Config{Context: d.cfg.KubeContext}).WithRunner(d.kube.Runner())
}

// --- pure helpers ---

// osArch maps a config arch to the S3 release tarball's os/arch variant. The
// x86_64 build is published under the "centos" label, the aarch64 build under
// "almalinux8" — verified via `aws s3 ls s3://releases.yugabyte.com/<ver>/`.
// Both are el8-family and run on the almalinux:8 base image.
func osArch(arch string) string {
	if arch == "aarch64" {
		return "almalinux8-aarch64"
	}
	return "centos-x86_64"
}

// tarballFilename is the release tarball's S3 object name for a build+arch, e.g.
// "yugabyte-2025.2.1.0-b141-centos-x86_64.tar.gz".
func tarballFilename(version, arch string) string {
	return fmt.Sprintf("yugabyte-%s-%s.tar.gz", version, osArch(arch))
}

// tarballS3URL is the S3 URL of the release tarball: the directory is the full
// build version, the object is tarballFilename.
func tarballS3URL(version, arch string) string {
	return fmt.Sprintf("s3://releases.yugabyte.com/%s/%s", version, tarballFilename(version, arch))
}

// extractedDirName is the top-level directory the tarball unpacks to: just
// "yugabyte-<base-version>", with the build/os/arch suffix dropped.
func extractedDirName(version string) string {
	base := core.BaseVersion(version)
	if base == "" {
		base = version
	}
	return "yugabyte-" + base
}

// tarballSpec builds the artifact.Spec apps/yugabyted delivers: an S3 tarball
// fetched on the COORDINATOR (RemoteDownload false → aws s3 cp locally, then
// kubectl cp into the pod — so the pod needs no AWS creds) and untarred on the
// pod (RemoteUnzip).
func tarballSpec(version, arch string) artifact.Spec {
	return artifact.Spec{
		Name:        "ybpod-yugabytedb",
		Version:     extractedDirName(version),
		GlobPath:    tarballFilename(version, arch),
		URL:         tarballS3URL(version, arch),
		Type:        artifact.TypeS3,
		RemoteUnzip: true,
		Target:      "server",
	}
}

// serviceManifest is a ClusterIP Service exposing the YSQL port and selecting the
// clientpod pod (ampqa=1), labelled ampqa-managed so it is reaped with the ns.
func serviceManifest(namespace, name string, ysqlPort int) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
  labels:
    ampqa: "1"
    %s: %s
spec:
  selector:
    ampqa: "1"
  ports:
    - name: ysql
      port: %d
      targetPort: %d
`, name, namespace, ManagedByLabel, ManagedByValue, ysqlPort, ysqlPort)
}

// namespaceLabels are stamped on the namespace ybpod creates, so Teardown (and
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

// namespaceFor renders the tagged, reapable namespace name: "<prefix>-<runTag>".
func namespaceFor(prefix, runTag string) string {
	if prefix == "" {
		prefix = DefaultNamespacePrefix
	}
	return prefix + "-" + runTag
}

// podNameFor renders the per-run pod name.
func podNameFor(runTag string) string { return "ybpod-" + runTag }

// pullSecrets wraps a non-empty base-image pull secret name as a one-element list
// (clientpod's imagePullSecrets); "" → nil (public image).
func pullSecrets(name string) []string {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	return []string{name}
}

// labelPath escapes dots in a label key for a kubectl jsonpath segment.
func labelPath(key string) string { return strings.ReplaceAll(key, ".", `\.`) }
