package foundation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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
	noWaits         bool
	// interrupt, when non-nil, abandons the steps that do not write as soon as
	// it closes. See WithInterrupt.
	interrupt    <-chan struct{}
	pollInterval time.Duration
	// dsnOpts are the consumer's own libpq parameters, supplied by connstr
	// flags. Empty is the common case and means "whatever the universe
	// implies".
	dsnOpts DSNOptions
	// httpGet is the seam for the one probe that is not a kubectl call, for the
	// same reason Runner exists: it has to be recordable without a cluster.
	httpGet HTTPGetter
	// inCluster routes that probe at the yugaware Service instead of exec'ing
	// curl in the pod. Defaulted from KUBERNETES_SERVICE_HOST, which the kubelet
	// sets in every pod, so the binary does the right thing in both places with
	// nothing to configure.
	inCluster bool
}

// HTTPGetter performs one GET and returns the status code and body.
type HTTPGetter func(ctx context.Context, url string) (int, string, error)

// httpGet is the real one. The timeout matches the `curl -m 15` it replaces: a
// probe that cannot answer quickly is inconclusive, and the caller treats it
// that way.
func httpGet(ctx context.Context, url string) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	return res.StatusCode, string(body), err
}

// certificateTimeout bounds waitCertificate. Registering a CA with YBA is a
// metadata operation — seconds, or it is not going to happen — so it does not
// get the universe's tens-of-minutes budget.
const certificateTimeout = 5 * time.Minute

// WithDSNOptions supplies the libpq parameters the consumer owns — its sslmode
// and its own certificate paths — for the DSN Connect reports. See DSNOptions.
func (d *Deployer) WithDSNOptions(o DSNOptions) *Deployer {
	d.dsnOpts = o
	return d
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
		httpGet:         httpGet,
		inCluster:       os.Getenv("KUBERNETES_SERVICE_HOST") != "",
	}
}

// WithHTTPGetter replaces the HTTP seam, and WithInCluster overrides the
// autodetect. Both exist for tests, and the second also for the case autodetect
// cannot see: a binary running in a pod in one cluster against another through a
// kubeconfig, where the yugaware Service name resolves to the wrong place or to
// nothing.
func (d *Deployer) WithHTTPGetter(g HTTPGetter) *Deployer {
	d.httpGet = g
	return d
}

func (d *Deployer) WithInCluster(in bool) *Deployer {
	d.inCluster = in
	return d
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
	// Before anything reaches the cluster, because both flags are create-time
	// only: a universe that turns out encrypted by default cannot be edited out
	// of it, and this is the last moment the answer is still cheap.
	for _, f := range d.spec.InheritedEncryption() {
		switch f {
		case "enableClientToNodeEncrypt":
			d.log("warning: universe.%s is not set, so the CRD's default applies and CLIENT "+
				"connections will be encrypted. The DSN this reports is sslmode=%s to match. "+
				"Set it explicitly to choose; it is create-time only, so changing it later "+
				"rebuilds the universe", f, d.spec.SSLMode())
		default:
			d.log("warning: universe.%s is not set, so the CRD's default applies and the tserver "+
				"RPC port will be encrypted. Set it explicitly to choose; it is create-time "+
				"only, so changing it later rebuilds the universe", f)
		}
	}
	// A placementInfo whose zones Validate could not find is one it did not
	// check, and the cross-check against the provider is the whole reason that
	// check exists — YBA rejects a zone mismatch as a failed task tens of
	// minutes into a bring-up. Reported rather than refused: which shapes the
	// CRD accepts is YBA's to decide and moves between releases, so an unknown
	// one is more likely a release this predates than a mistake, and the API
	// server rejects a genuinely wrong one on apply.
	if d.spec.PlacementUnchecked() {
		d.log("warning: universe.placementInfo is set but no zone code was found under " +
			"regions[].zones[], so it was NOT checked against the provider's zones. A zone the " +
			"provider does not have fails as a YBA task tens of minutes from now, not here")
	}
	for _, bin := range []string{"kubectl", "helm"} {
		if _, err := d.run(ctx, nil, []string{bin, "version", "--client"}); err != nil {
			return fmt.Errorf("%s not usable on PATH: %w", bin, err)
		}
	}
	if err := d.checkEncryptionMatchesTheUniverse(ctx); err != nil {
		return err
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

	// Same contract for the Secrets a TelemetryProvider references. Missing, the
	// operator leaves the provider in Error with the reason only in YBA's log,
	// and the universe's telemetry section then never reconciles — a universe
	// that looks entirely healthy while exporting nothing.
	for _, name := range d.spec.TelemetryProviderSecrets() {
		res, err := d.kubectl(ctx, nil, "get", "secret", name, "-o", "name")
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("secret %q, referenced by a TelemetryProvider, does not exist in "+
				"namespace %s: create it before deploying (the environment's install hint prints "+
				"the `kubectl create secret` line), or drop the telemetry from the spec — the "+
				"provider would otherwise sit in Error and nothing would be exported",
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

// WithoutWaits returns once the spec is APPLIED rather than once the foundation
// is USABLE.
//
// For a caller that converges repeatedly. YBA retries every reference it cannot
// resolve — a provider or certificate that is missing makes the universe create
// throw and requeue, and a telemetry provider that is not Ready yet is picked up
// by the next informer resync — so the waits buy a one-shot caller a finished
// foundation, and buy a looping one only a slower loop.
//
// It drops more than the waits themselves: checkTServerService needs the
// universe up, so it cannot run either. The caller gets a converge that has
// written every CR and nothing more, and is expected to read the CRs back
// itself. Up and Apply then return a nil Info whenever the address is not
// resolvable yet, with no error.
//
// Prune still runs, and still deletes what the spec dropped. It does not need
// the universe settled: a CR YBA cannot yet release is refused, marked and
// requeued by YBA itself rather than being lost. What prune skips here is
// WAITING for the deletions to land — see pruneDeclared.
//
// installYBA's own wait is NOT dropped. The CRs are rejected until YBA is
// serving and has its single customer, so skipping that one would only move the
// failure.
func (d *Deployer) WithoutWaits() *Deployer {
	d.noWaits = true
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

// step is one unit of a converge. The sequence is a list rather than a chain of
// calls so that a failure can name what it skipped — see IncompleteError.
// StepID identifies a converge step for machines. The `name` beside it is prose
// written for an error message and is free to be reworded; an ID is not, because
// a caller reporting progress publishes it and anything reading that report then
// depends on the spelling.
type StepID string

const (
	StepPreflight      StepID = "preflight"
	StepCRDs           StepID = "install-crds"
	StepNamespace      StepID = "ensure-namespace"
	StepYBA            StepID = "install-yba"
	StepApplyResources StepID = "apply-resources"
	StepWaitUniverse   StepID = "wait-universe"
	StepPrune          StepID = "prune"
	StepTServerService StepID = "verify-tserver-service"
)

// Steps is every step a converge can run, in order. A given run is a SUBSET:
// --no-prune drops one and --apply-only drops two, so a caller showing progress
// takes its total from the run rather than from len(Steps).
var Steps = []StepID{
	StepPreflight,
	StepCRDs,
	StepNamespace,
	StepYBA,
	StepApplyResources,
	StepWaitUniverse,
	StepPrune,
	StepTServerService,
}

type step struct {
	id   StepID
	name string
	run  func(context.Context) error
	// mutating says this step WRITES to the cluster, which decides whether an
	// interrupt may cut it short. See WithInterrupt.
	mutating bool
}

// ErrInterrupted is a converge abandoned partway because the caller signalled
// that its input is stale. It is not a failure and wants no backoff: nothing is
// half-written, and the next converge starts from the spec that superseded this
// one.
var ErrInterrupted = errors.New("converge interrupted: the spec it was converging has been superseded")

// WithInterrupt lets a looping caller abandon a converge when a newer spec
// arrives, which is what stops one bad spec holding the loop for the whole
// universe timeout.
//
// Only the steps that do not WRITE are abandoned. A mutating step runs to
// completion even after ch closes, because being cut off midway is how the
// damage happens: killing `helm upgrade --install` is exactly what leaves the
// release in `pending-install`, which no later converge can get past without
// uninstalling it first. The waits, the preflight and the Service check hold no
// such state, so dropping them costs nothing but the time already spent.
//
// So an interrupt is not a cancel. A caller that needs the converge to STOP —
// a pod being terminated — cancels the context instead, and accepts the wedge
// that implies.
func (d *Deployer) WithInterrupt(ch <-chan struct{}) *Deployer {
	d.interrupt = ch
	return d
}

// interrupted reports whether the interrupt has already fired, which is how a
// cancelled step is told apart from a genuine failure.
func (d *Deployer) interrupted() bool {
	if d.interrupt == nil {
		return false
	}
	select {
	case <-d.interrupt:
		return true
	default:
		return false
	}
}

// stepContext derives the context one step runs under: the converge's own for a
// mutating step, and one that also ends on the interrupt for the rest.
func (d *Deployer) stepContext(ctx context.Context, s step) (context.Context, context.CancelFunc) {
	if s.mutating {
		return ctx, func() {}
	}
	return d.abandonable(ctx)
}

// abandonable derives a context that also ends when the interrupt fires.
//
// Used for whole steps that write nothing, and for the POLLING half of a step
// that does: once a delete has been requested the cluster finishes it whether
// or not anything is still watching, so waiting for it is abandonable even
// though requesting it was not.
func (d *Deployer) abandonable(ctx context.Context) (context.Context, context.CancelFunc) {
	if d.interrupt == nil {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-d.interrupt:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// convergeSteps is the list this converge will run, which depends on the
// options: --apply-only drops the wait and the Service check, --no-prune drops
// the prune.
func (d *Deployer) convergeSteps() []step {
	steps := []step{
		{id: StepPreflight, name: "preflight", run: d.Preflight},
		{id: StepCRDs, name: "install the operator CRDs", run: d.applyCRDs, mutating: true},
		{id: StepNamespace, name: "ensure the namespace", run: d.ensureNamespace, mutating: true},
		{id: StepYBA, name: "install YBA", run: d.installYBA, mutating: true},
		{id: StepApplyResources, name: "apply the custom resources", run: d.applyCRs, mutating: true},
	}
	if !d.noWaits {
		steps = append(steps, step{id: StepWaitUniverse,
			name: "wait for the universe to settle", run: d.waitUniverse})
	}
	if !d.noPrune {
		// After the applies, and it does not need the universe settled. One
		// edit can both stop the universe using a KMSConfig and drop the key,
		// and deleting the config first is safe: KMSConfigReconciler refuses a
		// delete while EncryptionAtRestUtil.configInUse, writes `InUse` to the
		// CR and requeues it with incrementRetry false — forever. The finalizer
		// holds the object until the disable task has actually released it. So
		// the ordering is YBA's, and YBA does it.
		steps = append(steps, step{id: StepPrune,
			name: "prune objects the spec no longer declares", run: d.pruneDeclared, mutating: true})
	}
	if !d.noWaits {
		// Reads the universe's own Service, so it cannot run before the
		// universe is up. It goes with the wait rather than being separately
		// optional.
		steps = append(steps, step{id: StepTServerService,
			name: "verify the tserver Service", run: d.checkTServerService})
	}
	return steps
}

func (d *Deployer) converge(ctx context.Context, initial bool) (*Info, error) {
	steps := d.convergeSteps()

	for i, s := range steps {
		// The interrupt is checked BETWEEN steps as well as inside one. A step
		// already running is cut short only when it writes nothing, but once
		// the spec is stale nothing NEW starts, whether it writes or not:
		// every step is create-or-apply, so stopping between two of them
		// strands nothing, and the next converge runs the lot from the spec
		// that superseded this one. The previous step is the last that ran, so
		// it is the one the error names.
		if i > 0 && d.interrupted() {
			return nil, stopAt(steps[i-1], steps[i:], ErrInterrupted)
		}
		stepCtx, cancel := d.stepContext(ctx, s)
		err := s.run(stepCtx)
		cancel()
		if err == nil {
			continue
		}
		// An interrupt is reported as one whatever the step said. A cancelled
		// step fails with whatever it happened to be doing — a poll reporting
		// a context error, a kubectl that was killed — and none of that is
		// worth showing, or backing off over.
		if d.interrupted() {
			err = ErrInterrupted
		}
		// Nothing has run yet when the FIRST step fails, so there is no
		// half-built foundation to report — just the reason. It is the
		// position that makes it safe, not the step: several later steps are
		// read-only too, but they run after ones that are not.
		if i == 0 {
			return nil, fmt.Errorf("%s: %w", s.name, err)
		}
		return nil, stopAt(s, steps[i+1:], err)
	}
	info, err := d.Connect(ctx)
	if err != nil {
		if d.noWaits {
			// Expected rather than exceptional here: the Service the address
			// comes from is created by the universe, which this converge did
			// not wait for. The apply still succeeded, so report nil and let
			// the caller ask again on its next pass.
			d.log("connection details are not available yet: %v", err)
			return nil, nil
		}
		return nil, fmt.Errorf("read the connection details: %w", err)
	}
	if initial {
		d.log("foundation %q is up in namespace %s", d.spec.UniverseName(), d.spec.Namespace)
	} else {
		d.log("foundation %q converged in namespace %s", d.spec.UniverseName(), d.spec.Namespace)
	}
	return info, nil
}

// IncompleteError is a converge that aborted partway, naming the steps after
// the failure that therefore did not run.
//
// It exists because the steps that get skipped are invisible: the universe can
// be Ready and every pod healthy while `prune` and `checkTServerService` never
// ran, and both omissions surface much later and somewhere else — as an object
// the spec stopped declaring that is still live, or as a consumer handed an
// address with no Service behind it.
//
// It also says the foundation is resumable, which is not obvious: every step is
// create-or-apply, so Apply picks up from where this stopped.
type IncompleteError struct {
	Step    string
	Skipped []string
	Err     error

	// ID and SkippedIDs are the same two things for a machine. Step and Skipped
	// stay prose because they are what the message prints; a caller that
	// publishes progress reads these instead, so rewording a step name cannot
	// break it.
	ID         StepID
	SkippedIDs []StepID
}

func (e *IncompleteError) Error() string {
	// The last step failing skips nothing, so there is no list to print. The
	// foundation is still incomplete and still resumable.
	steps := ""
	if len(e.Skipped) > 0 {
		steps = "these steps did not run: " + strings.Join(e.Skipped, ", ") + "\n"
	}
	return fmt.Sprintf("%s: %v\nthe foundation is incomplete — %sconverging it again resumes "+
		"from here; every step is create-or-apply", e.Step, e.Err, steps)
}

func (e *IncompleteError) Unwrap() error { return e.Err }

// stopAt reports a converge that ended at `at`, with `rest` the steps after it
// that therefore did not run.
func stopAt(at step, rest []step, err error) *IncompleteError {
	inc := &IncompleteError{Step: at.name, ID: at.id, Err: err}
	for _, s := range rest {
		inc.Skipped = append(inc.Skipped, s.name)
		inc.SkippedIDs = append(inc.SkippedIDs, s.id)
	}
	return inc
}

// pruneDeclared removes the objects this foundation owns that the spec no
// longer declares.
func (d *Deployer) pruneDeclared(ctx context.Context) error {
	manifests, err := d.spec.Render()
	if err != nil {
		return err
	}
	return d.prune(ctx, manifests)
}

func (d *Deployer) ensureNamespace(ctx context.Context) error {
	// The namespace may be SHARED with the consumer's own workloads (amp keeps
	// them apart, in FOUNDATION_NS; other consumers need not), so it is created
	// if absent and otherwise left exactly as it is. The managed-by label records only that a
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

// checkEncryptionMatchesTheUniverse refuses a spec whose encryption disagrees
// with the universe it would converge.
//
// Both flags are create-time only, so a disagreement is not something a converge
// can settle: the CRD's `self == oldSelf` rejects the apply, and before that
// this deployer would report a DSN for a universe that does not exist — the
// `no pg_hba.conf entry` crash-loop, or its mirror image, `require` against a
// plaintext database.
//
// The way in is an UPGRADE. A deployer that defaulted both flags to false built
// plaintext universes from specs that said nothing, and wrote the false into the
// CR. This one reads an absent flag as the CRD's true, so the same spec now
// means the opposite. The fix is one line per flag in the spec, and this is what
// says so instead of letting the apply fail forty minutes later.
func (d *Deployer) checkEncryptionMatchesTheUniverse(ctx context.Context) error {
	res, err := d.kubectl(ctx, nil, "get", "ybuniverse", d.spec.UniverseName(),
		"-o", "jsonpath={.spec.enableClientToNodeEncrypt} {.spec.enableNodeToNodeEncrypt}")
	if err != nil || res.ExitCode != 0 {
		// No universe yet, or no cluster to ask: nothing to disagree with.
		return nil
	}
	// Strictly two booleans or nothing: a jsonpath that matched no field, or a
	// CR shape this does not recognise, is not evidence of a disagreement.
	live := strings.Fields(res.Stdout)
	if len(live) != 2 {
		return nil
	}
	for _, v := range live {
		if v != "true" && v != "false" {
			return nil
		}
	}
	for i, f := range []string{"enableClientToNodeEncrypt", "enableNodeToNodeEncrypt"} {
		on := live[i] == "true"
		if on == d.spec.encryptionOn(f) {
			continue
		}
		stated := "says nothing, so the CRD's default (true) applies"
		if _, set := d.spec.universe()[f]; set {
			stated = fmt.Sprintf("says %v", d.spec.universe()[f])
		}
		return fmt.Errorf("universe %s has %s=%v and the spec %s: the field is create-time only, "+
			"so converging this would be rejected by the CRD and every DSN reported meanwhile "+
			"describes the wrong universe. Write %s: %v in the spec to match what is there, or "+
			"build a new universe",
			d.spec.UniverseName(), f, on, stated, f, on)
	}
	return nil
}
