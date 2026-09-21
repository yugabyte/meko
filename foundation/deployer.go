package foundation

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Timeouts. Generous on purpose: a yugaware install pulls a large image and
// builds its own Postgres schema, and a universe create schedules master and
// tserver pods with PVCs. ampqa runs the same chart with a 40-minute helm
// timeout, so anything much shorter fails on a cold cluster rather than on a
// real problem.
const (
	DefaultYBATimeout      = 40 * time.Minute
	DefaultUniverseTimeout = 45 * time.Minute
	DefaultPollInterval    = 15 * time.Second
)

// Deployer applies a Spec to a cluster. Every external command goes through
// Runner, so the whole thing is exercised in tests with no cluster.
type Deployer struct {
	spec *Spec
	run  Runner
	log  func(format string, args ...any)

	ybaTimeout      time.Duration
	universeTimeout time.Duration
	noPrune         bool
	pollInterval    time.Duration
}

// New builds a Deployer for spec. A nil runner uses the real binaries.
func New(spec *Spec, run Runner) *Deployer {
	if run == nil {
		run = Exec
	}
	return &Deployer{
		spec:            spec,
		run:             run,
		log:             func(string, ...any) {},
		ybaTimeout:      DefaultYBATimeout,
		universeTimeout: DefaultUniverseTimeout,
		pollInterval:    DefaultPollInterval,
	}
}

// WithLogger sends progress to log. A bring-up takes tens of minutes, so
// silence is indistinguishable from a hang.
func (d *Deployer) WithLogger(log func(format string, args ...any)) *Deployer {
	d.log = log
	return d
}

// WithTimeouts overrides the waits. Zero leaves a default in place.
func (d *Deployer) WithTimeouts(yba, universe, poll time.Duration) *Deployer {
	if yba > 0 {
		d.ybaTimeout = yba
	}
	if universe > 0 {
		d.universeTimeout = universe
	}
	if poll > 0 {
		d.pollInterval = poll
	}
	return d
}

// HelmRelease is the release name for this foundation's yugaware install.
//
// It carries the namespace, and that is load-bearing rather than cosmetic: the
// yugaware chart creates CLUSTER-SCOPED RBAC named after the helm release, and
// cluster-scoped objects have no namespace to isolate them. Two foundations on
// one cluster with the same release name would clobber each other's
// ClusterRole and ClusterRoleBinding — and CI runs concurrent per-PR
// namespaces. Deriving the name from the namespace makes those objects unique
// per foundation and safely identifiable at teardown
// (amp/test/functional/framework/apps/ybastack/ybastack.go documents the same
// hazard).
func (s *Spec) HelmRelease() string {
	return s.Namespace + "-yugaware"
}

// kubectl runs kubectl in this foundation's namespace.
func (d *Deployer) kubectl(ctx context.Context, stdin []byte, args ...string) (Result, error) {
	return d.kubectlIn(ctx, d.spec.Namespace, stdin, args...)
}

// kubectlIn runs in a named namespace. Needed because the universe's own
// objects — the tserver Service, its pods — live wherever the provider zones'
// kubeNamespace points, which is not necessarily the foundation's namespace.
func (d *Deployer) kubectlIn(ctx context.Context, namespace string, stdin []byte,
	args ...string) (Result, error) {
	argv := append(d.kubectlBase(), "-n", namespace)
	return d.run(ctx, stdin, append(argv, args...))
}

// kubectlUniverse runs in the namespace the DB pods are in.
func (d *Deployer) kubectlUniverse(ctx context.Context, stdin []byte, args ...string) (Result, error) {
	return d.kubectlIn(ctx, d.spec.UniverseNamespace(), stdin, args...)
}

// kubectlCluster runs kubectl with no namespace, for cluster-scoped objects.
func (d *Deployer) kubectlCluster(ctx context.Context, stdin []byte, args ...string) (Result, error) {
	return d.run(ctx, stdin, append(d.kubectlBase(), args...))
}

func (d *Deployer) kubectlBase() []string {
	argv := []string{"kubectl"}
	if d.spec.KubeContext != "" {
		argv = append(argv, "--context", d.spec.KubeContext)
	}
	return argv
}

func (d *Deployer) helm(ctx context.Context, args ...string) (Result, error) {
	argv := []string{"helm"}
	if d.spec.KubeContext != "" {
		argv = append(argv, "--kube-context", d.spec.KubeContext)
	}
	return d.run(ctx, nil, append(argv, args...))
}

// Preflight checks what would otherwise fail deep into a bring-up. Each failure
// names the fix, because the alternative is a helm timeout forty minutes later
// with a pod-scheduling error buried in it.
func (d *Deployer) Preflight(ctx context.Context) error {
	for _, bin := range []string{"kubectl", "helm"} {
		if _, err := d.run(ctx, nil, []string{bin, "version", "--client"}); err != nil {
			return fmt.Errorf("%s not usable on PATH: %w", bin, err)
		}
	}
	res, err := d.kubectlCluster(ctx, nil, "get", "namespace", "-o", "name")
	if err != nil {
		return fmt.Errorf("reach the cluster: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cannot list namespaces (context %q): %s",
			d.spec.KubeContext, strings.TrimSpace(res.Stderr))
	}
	if err := d.spec.checkDefaultUser(); err != nil {
		return err
	}
	if err := d.checkLocalPaths(); err != nil {
		return err
	}
	if err := d.checkHelmVersion(ctx); err != nil {
		return err
	}
	if err := d.checkK8sVersion(ctx); err != nil {
		return err
	}

	// Every storage class the spec names must exist and bind
	// WaitForFirstConsumer. An Immediate class provisions the PVC before the pod
	// is scheduled, which zone-pins the ~4.5 CPU yugaware pod to whatever zone
	// the volume landed in and then fails to schedule it.
	//
	// Collected from the spec rather than read off one field, because a storage
	// class can be set in several places and the PROVIDER's per-zone value wins
	// over the universe's — so checking only the universe would pass a spec
	// whose real class does not exist.
	for _, sc := range storageClasses(d.spec) {
		res, err := d.kubectlCluster(ctx, nil, "get", "storageclass", sc,
			"-o", "jsonpath={.volumeBindingMode}")
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("storage class %q not found: create it, or name one that exists "+
				"(provider zone kubernetesStorageClass, or a volume's storageClass)", sc)
		}
		if mode := strings.TrimSpace(res.Stdout); mode != "WaitForFirstConsumer" {
			d.log("warning: storage class %s binds %s, not WaitForFirstConsumer; "+
				"the yugaware pod may fail to schedule after its PVC zone-pins it", sc, mode)
		}
	}

	// Every pull secret the spec names must already exist: nothing here creates
	// one. Missing, it is not a deploy error — the pods are admitted and sit in
	// ImagePullBackOff, so the failure presents as a wait that never ends with
	// the reason two `kubectl describe`s away.
	for _, name := range d.spec.PullSecrets() {
		res, err := d.kubectl(ctx, nil, "get", "secret", name, "-o", "name")
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("pull secret %q does not exist in namespace %s: create it before "+
				"deploying (amp/deploy/up-gke.sh mints it from PULL_SECRET), or drop the "+
				"reference from the spec — the images would otherwise sit in ImagePullBackOff",
				name, d.spec.Namespace)
		}
	}
	return nil
}

// WithoutPrune leaves objects the spec no longer declares on the cluster.
//
// An escape hatch, not a mode: the default has to be that the spec is the
// source of truth, or removing a key means something different from adding
// one.
func (d *Deployer) WithoutPrune() *Deployer {
	d.noPrune = true
	return d
}

// Up stands a foundation up from nothing and returns how to reach it.
func (d *Deployer) Up(ctx context.Context) (*Info, error) {
	return d.converge(ctx, true)
}

// Apply converges an existing foundation onto the spec — the day-2 verb. Scale,
// software upgrade, gflags and disk grow are all edits to the spec followed by
// this.
//
// It shares Up's path rather than having one of its own: every step is
// create-or-apply, so the difference is only which steps are expected to be
// no-ops. A separate code path would be a second thing to keep correct.
func (d *Deployer) Apply(ctx context.Context) (*Info, error) {
	return d.converge(ctx, false)
}

func (d *Deployer) converge(ctx context.Context, initial bool) (*Info, error) {
	if err := d.Preflight(ctx); err != nil {
		return nil, err
	}
	if err := d.applyCRDs(ctx); err != nil {
		return nil, err
	}
	if err := d.ensureNamespace(ctx); err != nil {
		return nil, err
	}
	if err := d.installYBA(ctx); err != nil {
		return nil, err
	}
	if err := d.applyCRs(ctx); err != nil {
		return nil, err
	}
	if err := d.waitUniverse(ctx); err != nil {
		return nil, err
	}
	// After the universe has SETTLED, not merely after the applies. One edit
	// can both stop the universe using a KMSConfig and drop the key; disabling
	// encryption at rest is a YBA task, so deleting the config before that task
	// finishes races a reference that is still live.
	if !d.noPrune {
		manifests, err := d.spec.Render()
		if err != nil {
			return nil, err
		}
		if err := d.prune(ctx, manifests); err != nil {
			return nil, err
		}
	}
	if err := d.checkTServerService(ctx); err != nil {
		return nil, err
	}
	if err := d.ensureDatabases(ctx); err != nil {
		return nil, err
	}
	info, err := d.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if initial {
		d.log("foundation %q is up in namespace %s", d.spec.UniverseName(), d.spec.Namespace)
	} else {
		d.log("foundation %q converged in namespace %s", d.spec.UniverseName(), d.spec.Namespace)
	}
	return info, nil
}

func (d *Deployer) ensureNamespace(ctx context.Context) error {
	// The namespace is SHARED with the consumer's own workloads (the amp
	// controller and its endpoints run here too), so it is created if absent and
	// otherwise left exactly as it is. The managed-by label records only that a
	// foundation lives here; it is deliberately not an ownership claim, because
	// Down must never delete a namespace holding someone else's pods.
	res, err := d.kubectlCluster(ctx, nil, "get", "namespace", d.spec.Namespace, "-o", "name")
	if err != nil {
		return err
	}
	if res.ExitCode == 0 {
		return nil
	}
	d.log("creating namespace %s", d.spec.Namespace)
	if _, err := d.kubectlCluster(ctx, nil, "create", "namespace", d.spec.Namespace); err != nil {
		return err
	}
	_, err = d.kubectlCluster(ctx, nil, "label", "namespace", d.spec.Namespace,
		ManagedByLabel+"="+ManagedByValue, "--overwrite")
	return err
}
