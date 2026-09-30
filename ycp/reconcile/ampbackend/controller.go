package ampbackend

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"meko/ycp/internal/ampclient"
	"meko/ycp/internal/api"
	"meko/ycp/reconcile"
)

// Runner runs the helm CLI: one command, stdout back, a non-zero exit as an
// error carrying stderr. It is an interface so the controller arm is testable
// without a cluster, and an exec seam rather than helm.sh/helm/v3 because linking
// the SDK would pull client-go and apimachinery into a module that deliberately
// has neither.
//
// stdin is what `-f -` reads: helm treats a values file named "-" as stdin, which
// is how this passes a whole values document without writing a temp file. Nil for
// every command that takes none.
type Runner interface {
	Run(ctx context.Context, stdin []byte, args ...string) ([]byte, error)
}

// ControllerConfig is everything the controller kind needs that lives outside the
// controller's REST API: the helm client, the release to upgrade, and a read of
// the controller Deployment's rollout.
//
// Whether any given environment wants its controller version managed is desired
// state's business, not the backend's: the row is emitted either way and carries
// Preexisting, so an environment that declares no controller simply sees it
// reported and never converged.
type ControllerConfig struct {
	Helm Runner
	// Rollout reports whether the named Deployment has finished rolling out, and
	// the image tag its pod template carries. The tag is what a declared version
	// is compared against: the controller's own /version reports the binary's
	// stamp, which is a different string from the image tag an operator declares.
	Rollout func(ctx context.Context, name string) (bool, string, error)
	// Release is the helm release the controller runs as; the chart does not fix
	// it, the operator does.
	Release   string
	Namespace string
	// Chart is the reference helm resolves: an OCI ref, or a local directory for a
	// cluster test running an unpublished chart.
	Chart string
	// Deployment is the controller's Deployment, whose rollout gates readiness.
	Deployment string
	// Upgrades runs the `helm upgrade` itself. Nil runs it in this process through
	// Helm, which only a test should do: see Upgrader.
	Upgrades Upgrader
}

// Upgrader runs a `helm upgrade` somewhere that outlives this process, and reports
// on it afterwards.
//
// The agent is a member of the release it upgrades, so the upgrade replaces the
// agent's own pod. helm waits for the release's hooks even without --wait, and a
// client killed while it waits never records the revision, leaving the release in
// pending-upgrade for good — which blocks every later upgrade. A run that is not
// in this pod, and a record of it this pod's replacement can read, is what an
// upgrade needs to finish.
type Upgrader interface {
	// Start runs `helm <args> -f <values>` as the run named name. Starting a name
	// that is still running does nothing, so a retry after a crash is safe; one
	// that already finished is run again — Start is only called when the release
	// does not carry these values, so a finished run of them did not stick.
	Start(ctx context.Context, name string, args []string, values []byte) error
	// Latest is the newest run for this release, or the zero UpgradeRun when there
	// is none.
	Latest(ctx context.Context) (UpgradeRun, error)
	// Remove deletes a run and its values.
	Remove(ctx context.Context, name string) error
}

// UpgradeRun is one upgrade run as Upgrader.Latest reports it.
type UpgradeRun struct {
	Name string
	// State is "running", "succeeded" or "failed"; "" when there is no run.
	State string
	// Message is what helm said on a failed run.
	Message string
}

// Upgrade run states.
const (
	UpgradeRunning   = "running"
	UpgradeSucceeded = "succeeded"
	UpgradeFailed    = "failed"
)

// controllerState is the config plus the version last declared for it. Readiness
// is "running what the spec asks", and List cannot see the spec, so the target is
// remembered here as Update goes past.
type controllerState struct {
	cfg ControllerConfig
	// target is guarded by Backend.mu.
	target string
}

// controllerSpec is a controller resource's desired spec. values are chart
// overrides carried verbatim, kept as raw JSON so a nested override is compared
// and re-sent exactly as declared.
type controllerSpec struct {
	Version      string                     `json:"version"`
	ChartVersion string                     `json:"chart_version"`
	Values       map[string]json.RawMessage `json:"values"`
}

func controllerSpecOf(r api.DesiredResource) (controllerSpec, error) {
	var spec controllerSpec
	err := unmarshalSpec(r, &spec)
	return spec, err
}

func (b *Backend) controllerTarget() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.ctl.target
}

func (b *Backend) setControllerTarget(v string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ctl.target = v
}

// controllerActual reports the controller singleton, and whether this agent
// manages it at all.
//
// Nothing here may fail the way List fails. A controller upgrade restarts the
// controller, so /version is unreachable at exactly the moment this kind is being
// converged — and a List error aborts the whole tick for every resource, which
// would freeze all reconciliation for the length of every upgrade. An unreachable
// controller is therefore Ready=false in state "upgrading", which is also the
// truth.
//
// The row is emitted on every successful List, healthy or not, and whether or not
// desired state declares a controller. Emitting it unconditionally is what
// anything gated on the controller being Ready needs, because that gate reads the
// actual map and never desired — a child whose parent is missing here is deferred
// forever, silently.
//
// Reporting a resource nobody declared is safe because the row says what it is:
// Preexisting, so the engine never plans a Delete for it. Without that the
// ordinary GC rule would collect it, this kind's Delete refuses (as it must), and
// the refusal would latch a permanent drift row on an environment where nothing
// is wrong.
//
// Ready needs a target, so it stays false until an Update has been past. That
// costs one tick after an agent restart and is what keeps an ordering gate
// honest: nothing that waits on the controller should start against one still on
// the old version just because this process has forgotten which version was asked
// for. An undeclared row sits at that same not-ready unmanaged state forever,
// which is correct and is why the engine does not time it (trackStuck).
func (b *Backend) controllerActual(ctx context.Context) (reconcile.Actual, bool) {
	key := reconcile.ResourceKey{Kind: KindController, Name: controllerName}
	running, verr := b.readControllerVersion(ctx)
	rolled, deployed, rerr := b.ctl.cfg.Rollout(ctx, b.ctl.cfg.Deployment)
	target := b.controllerTarget()

	// Readiness is three facts: the pod template carries the declared tag, the
	// rollout is complete, and the controller answers. The reported version is an
	// attr rather than a comparand - it is the binary's own stamp, not the tag.
	a := reconcile.Actual{Key: key, State: controllerStateUpgrading, Preexisting: true}
	switch {
	case verr != nil || rerr != nil || !rolled:
	case target == "":
		a.State = controllerStateUnmanaged
	case deployed == target:
		a.State = controllerStateReady
		a.Ready = true
	}
	// Same reason as the fleet row: no create, so the generation diff is the only
	// lever, and a first declaration's generation is 0. updateController decides.
	a.ObservedGen = reconcile.GenerationNever
	a.Attrs = attrPut(attrPut(attrPut(nil, "version", running), "image_tag", deployed), "target_version", target)
	return a, true
}

// States the controller row reports. A closed set, because trackStuck restarts a
// resource's timeline whenever State changes.
const (
	controllerStateReady     = "ready"
	controllerStateUpgrading = "upgrading"
	controllerStateUnmanaged = "unmanaged"
)

// readControllerVersion reads the running controller's own version.
func (b *Backend) readControllerVersion(ctx context.Context) (string, error) {
	var v ampclient.Version
	if err := b.getJSON(ctx, func(c context.Context) (*http.Response, error) {
		return b.api.GetVersion(c)
	}, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// updateController moves the controller release to the declared version. Create
// is the same call: the release always exists — this agent is running inside it —
// so a create only means List has not emitted a row yet.
//
// This kind claims no generation: the deployed values are readable off the
// release and the running version off the controller itself, so convergence is
// proved by reading rather than by an echo. An echo would be a second record of
// something already visible, and a weaker one — it cannot see drift it did not
// cause, where a read can. So it returns nil only when the release carries the
// declared values, the rollout is complete, and the controller is answering
// again — answering, not answering the declared version: /version reports the
// binary's ldflags stamp, which is a different string from the image tag a spec
// declares, so comparing the two would never converge.
//
// It does not go through applyUpdate: helm is not a controller task, so there is
// no task id for the next tick to read. The verdict comes from the next tick's
// observation instead.
func (b *Backend) updateController(ctx context.Context, r api.DesiredResource) error {
	if r.Name != controllerName {
		return reconcile.Permanent(fmt.Errorf(
			"controller %q: the controller is a singleton and must be declared as %q", r.Name, controllerName))
	}
	spec, err := controllerSpecOf(r)
	if err != nil {
		return err
	}
	version := strings.TrimSpace(spec.Version)
	if version == "" {
		return reconcile.Permanent(fmt.Errorf("controller %q: spec.version is required — there is no controller to run without an image tag", r.Name))
	}
	b.setControllerTarget(version)

	if err := b.priorUpgradeRun(ctx); err != nil {
		return err
	}
	if err := b.priorRelease(ctx); err != nil {
		return err
	}
	// Read the release BEFORE running helm. A tick that lands mid-rollout must find
	// its own upgrade already deployed and wait, or every tick bumps the revision
	// again and the rollout never settles.
	deployed, err := b.deployedValues(ctx)
	if err != nil {
		return err
	}
	if diff := valuesDiffer(deployed, version, spec.Values); diff != "" {
		if err := b.helmUpgrade(ctx, version, spec, deployed); err != nil {
			return err
		}
		return reconcile.Pending(fmt.Errorf("helm upgrade of %q issued (%s)", b.ctl.cfg.Release, diff))
	}
	if spec.ChartVersion != "" {
		// A packaged chart is already one version, and helm SILENTLY IGNORES --version
		// against an archive reference rather than refusing it. So a declaration that
		// disagrees with the archive cannot be satisfied, and without this it loops
		// forever: the upgrade is issued, deploys the archive's own version, the next
		// tick still sees a mismatch, and issues it again. Permanent, because no
		// number of retries changes what is inside the .tgz.
		if isChartArchive(b.ctl.cfg.Chart) {
			if cur, err := b.releaseChartVersion(ctx); err == nil && cur != "" && cur != spec.ChartVersion {
				return reconcile.Permanent(fmt.Errorf(
					"controller %q: spec.chart_version is %s but this agent's chart reference is the archive %q, which is version %s — "+
						"helm ignores --version for an archive, so point the agent at a chart repository or declare the version the archive carries",
					r.Name, spec.ChartVersion, b.ctl.cfg.Chart, cur))
			}
		}
		// Only when helm can answer: an older helm has no `get metadata`, and a chart
		// version nobody can read must not re-run the upgrade every tick.
		if cur, err := b.releaseChartVersion(ctx); err == nil && cur != "" && cur != spec.ChartVersion {
			if err := b.helmUpgrade(ctx, version, spec, deployed); err != nil {
				return err
			}
			return reconcile.Pending(fmt.Errorf("helm upgrade of %q issued (chart %s -> %s)",
				b.ctl.cfg.Release, cur, spec.ChartVersion))
		}
	}

	rolled, deployedTag, err := b.ctl.cfg.Rollout(ctx, b.ctl.cfg.Deployment)
	if err != nil {
		return fmt.Errorf("read controller rollout: %w", err)
	}
	if !rolled {
		return reconcile.Pending(fmt.Errorf("controller rollout to %s is still in progress", version))
	}
	if deployedTag != version {
		return reconcile.Pending(fmt.Errorf("controller pod template carries %s, waiting for %s", deployedTag, version))
	}
	// Only that the controller answers, not what it answers: /version reports the
	// binary's stamped version, which is not the image tag the spec declares.
	if _, err := b.readControllerVersion(ctx); err != nil {
		return reconcile.Pending(fmt.Errorf("controller is not answering /version yet: %w", err))
	}
	return nil
}

// deployedValues reads the release's user-supplied values — what --reuse-values
// carries forward, and so what the next upgrade would be a change to. A release
// with none renders `null`, which decodes to a nil map.
func (b *Backend) deployedValues(ctx context.Context) (map[string]any, error) {
	out, err := b.ctl.cfg.Helm.Run(ctx, nil, "get", "values", b.ctl.cfg.Release, "--namespace", b.ctl.cfg.Namespace, "--output", "json")
	if err != nil {
		return nil, fmt.Errorf("read deployed values of %q: %w", b.ctl.cfg.Release, err)
	}
	var vals map[string]any
	if err := json.Unmarshal(out, &vals); err != nil {
		return nil, fmt.Errorf("decode deployed values of %q: %w", b.ctl.cfg.Release, err)
	}
	return vals, nil
}

func (b *Backend) releaseChartVersion(ctx context.Context) (string, error) {
	out, err := b.ctl.cfg.Helm.Run(ctx, nil, "get", "metadata", b.ctl.cfg.Release, "--namespace", b.ctl.cfg.Namespace, "--output", "json")
	if err != nil {
		return "", err
	}
	var meta struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &meta); err != nil {
		return "", err
	}
	return meta.Version, nil
}

// priorRelease is the verdict on whatever helm last did to this release, read
// before anything is issued — the same shape as priorTask on the task path, with
// `helm status` standing in for GET /tasks/{id} and the release name for the task
// id. There is only ever one operation in flight per release, which helm enforces
// itself, so no id is needed to name it.
//
// Without this the values comparison below is the only in-flight check, and it
// answers the wrong question after a crash. The agent is a member of the release
// it upgrades, so its own rollout kills the helm client; if that lands after helm
// wrote the pending revision but before the manifests were submitted, the release
// sits in pending-upgrade with the NEW values already recorded. The comparison
// then short-circuits, the rollout check finds the OLD pod template, and the tick
// reports Pending — forever, with only StuckAfter's "no progress" to show for it,
// rather than the one thing an operator needs to hear: run `helm rollback`.
//
// A status that cannot be read is not fatal: helm may be older than
// `status --output json`, and refusing to upgrade because the status was
// unreadable would wedge the kind on a cosmetic problem.
func (b *Backend) priorRelease(ctx context.Context) error {
	cfg := b.ctl.cfg
	out, err := cfg.Helm.Run(ctx, nil, "status", cfg.Release, "--namespace", cfg.Namespace, "--output", "json")
	if err != nil {
		return nil // no release yet, or a helm that cannot answer: let the upgrade decide
	}
	var st struct {
		Info struct {
			Status      string `json:"status"`
			Description string `json:"description"`
		} `json:"info"`
	}
	if jerr := json.Unmarshal(out, &st); jerr != nil {
		return nil
	}
	switch st.Info.Status {
	case "pending-install", "pending-upgrade", "pending-rollback":
		// Somebody's operation is still running — ours from a previous tick, or a
		// human's. Either way helm refuses a concurrent one, so reporting it in
		// flight is both true and the only thing that does not turn into a failed
		// verb every tick.
		return reconcile.Pending(fmt.Errorf("helm release %q is %s: %s",
			cfg.Release, st.Info.Status, st.Info.Description))
	case "failed":
		// Retryable rather than permanent: a failed release is upgradable, and the
		// engine's backoff is the right cadence for retrying it. The description is
		// helm's own, which is the sentence an operator needs.
		return fmt.Errorf("helm release %q is failed and needs attention (%s); "+
			"an upgrade over a failed release may not recover it — `helm rollback %s` is usually what does",
			cfg.Release, st.Info.Description, cfg.Release)
	}
	return nil
}

// isChartArchive reports whether a chart reference names a packaged chart rather
// than a repository entry. An archive carries exactly one version, which is why
// --version does not apply to it.
//
// Suffix match on the reference, because that is all helm itself goes on: a
// local path, an https URL and an OCI ref are told apart by shape, and only the
// first two can end in .tgz.
func isChartArchive(ref string) bool {
	return strings.HasSuffix(ref, ".tgz") || strings.HasSuffix(ref, ".tar.gz")
}

// helmUpgrade moves the release: through cfg.Upgrades, which runs helm outside
// this pod (see Upgrader), or in this process when a test left it nil. Either
// way without --wait: the rollout is read back on the next tick, quite possibly
// by the replacement pod.
//
// Everything that varies goes in one values document rather than the argv; the
// argv carries only the release, the chart and a declared chart_version, so no
// declared value lands in a Job spec.
func (b *Backend) helmUpgrade(ctx context.Context, version string, spec controllerSpec, deployed map[string]any) error {
	cfg := b.ctl.cfg
	args := []string{"upgrade", cfg.Release, cfg.Chart, "--namespace", cfg.Namespace}
	// Not for an archive: helm ignores it there, and passing a flag that is silently
	// dropped is how the argv stops describing what actually ran.
	if spec.ChartVersion != "" && !isChartArchive(cfg.Chart) {
		args = append(args, "--version", spec.ChartVersion)
	}
	vals, err := upgradeValues(deployed, version, spec.Values)
	if err != nil {
		return fmt.Errorf("build values for %q: %w", cfg.Release, err)
	}
	doc, err := json.Marshal(vals)
	if err != nil {
		return fmt.Errorf("encode values for %q: %w", cfg.Release, err)
	}
	if cfg.Upgrades == nil {
		if _, err := cfg.Helm.Run(ctx, doc, append(args, "-f", "-")...); err != nil {
			return fmt.Errorf("helm upgrade %q: %w", cfg.Release, err)
		}
		return nil
	}
	name := upgradeRunName(cfg.Release, args, doc)
	if err := cfg.Upgrades.Start(ctx, name, args, doc); err != nil {
		return fmt.Errorf("start helm upgrade %q (%s): %w", cfg.Release, name, err)
	}
	return nil
}

// upgradeValues is the whole values document an upgrade sends.
//
// It starts from the release's own user-supplied values, so the overrides the
// operator installed with (registry, storage class, the API key Secret) survive
// an upgrade the desired state does not restate. NOT --reuse-values, which reuses
// the last release's COMPUTED values and so masks every default the newer chart
// adds; `helm get values` returns the user-supplied set alone, so handing that
// back lets the new chart's defaults through underneath it.
//
// Onto that go the per-image tags, not the chart-global imageTag: imageTag is only
// the FALLBACK for an image with no tag of its own, so on an install that pins
// per-image tags — which the deployed environments do — setting it moves nothing.
// Then the declared overrides, each merged into the subtree at its dotted key the
// way --set-json merges, so declaring one key of a table keeps the rest of it.
func upgradeValues(deployed map[string]any, version string, values map[string]json.RawMessage) (map[string]any, error) {
	out := map[string]any{}
	if len(deployed) > 0 {
		raw, err := json.Marshal(deployed)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
	}
	for _, k := range controllerImageTagKeys {
		mergeAt(out, strings.Split(k, "."), version)
	}
	for _, k := range sortedValueKeys(values) {
		var v any
		if err := json.Unmarshal(values[k], &v); err != nil {
			return nil, reconcile.Rejected(fmt.Errorf("values.%s is not valid JSON: %w", k, err))
		}
		mergeAt(out, strings.Split(k, "."), v)
	}
	return out, nil
}

// mergeAt sets val at the path, creating tables on the way, and merges a table
// into a table already there instead of replacing it.
func mergeAt(dst map[string]any, path []string, val any) {
	for _, seg := range path[:len(path)-1] {
		next, _ := dst[seg].(map[string]any)
		if next == nil {
			next = map[string]any{}
			dst[seg] = next
		}
		dst = next
	}
	leaf := path[len(path)-1]
	if m, ok := val.(map[string]any); ok {
		if into, ok := dst[leaf].(map[string]any); ok {
			for k, v := range m {
				mergeAt(into, []string{k}, v)
			}
			return
		}
	}
	dst[leaf] = val
}

// upgradeRunName names a run by what it runs, so a restart that starts the same
// upgrade again finds the one already there. Kept well under the 63 characters a
// Job's name has to fit, because the Job controller stamps it into a label.
func upgradeRunName(release string, args []string, doc []byte) string {
	h := sha256.New()
	h.Write([]byte(strings.Join(args, "\x00")))
	h.Write([]byte{0})
	h.Write(doc)
	if len(release) > 40 {
		release = release[:40]
	}
	return fmt.Sprintf("%s-upgrade-%x", strings.TrimRight(release, "-"), h.Sum(nil)[:5])
}

// priorUpgradeRun is the verdict on the upgrade last started — by this process or
// the pod it replaced: Pending while it runs, and helm's own error, once, when it
// failed. A failed run is removed after it is reported, so priorRelease speaks
// for the release from then on. A finished one is left to expire.
func (b *Backend) priorUpgradeRun(ctx context.Context) error {
	cfg := b.ctl.cfg
	if cfg.Upgrades == nil {
		return nil
	}
	run, err := cfg.Upgrades.Latest(ctx)
	if err != nil {
		return fmt.Errorf("read the last helm upgrade of %q: %w", cfg.Release, err)
	}
	switch run.State {
	case UpgradeRunning:
		return reconcile.Pending(fmt.Errorf("helm upgrade of %q is running (%s)", cfg.Release, run.Name))
	case UpgradeFailed:
		if err := cfg.Upgrades.Remove(ctx, run.Name); err != nil {
			return fmt.Errorf("helm upgrade of %q failed (%s: %s), and removing the run failed: %w",
				cfg.Release, run.Name, run.Message, err)
		}
		return fmt.Errorf("helm upgrade of %q failed (%s): %s", cfg.Release, run.Name, run.Message)
	}
	return nil
}

// controllerImageTagKeys are the images a controller version moves. The agent is
// in the list because it ships as a component of the controller chart: a
// controller upgrade upgrades the reconciler with it, which is the whole reason
// there is no separate reconciler kind.
//
// The release runs two more images from the same stream — the MCP server and the
// UI, both enabled by default and both resolving mcp.image.tag / ui.image.tag
// through the same per-image-tag-first rule. They are deliberately NOT moved:
// neither is in the upgrade path this kind exists for, and moving them would make
// a controller upgrade a fleet-wide image bump by side effect. The cost is that a
// release upgraded this way is version-skewed across those two pods, and nothing
// here reports it — valuesDiffer checks only the keys above, and readiness reads
// the controller Deployment's rollout alone. Whoever adds them should add them to
// this list and to the convergence check together.
var controllerImageTagKeys = []string{
	"controller.image.tag",
	"agent.image.tag",
	// The endpoint image is NOT a side effect — it is the point. A controller
	// release's endpoint tag is the pin every endpoint it creates runs, and the
	// endpoint roll converges the fleet onto it. _helpers.tpl resolves that tag
	// from controller.endpoint.image.tag first, so an upgrade setting only the two
	// above leaves AMP_ENDPOINT_IMAGE_TAG where it was: the new controller's pin
	// equals the old one and the roll finds a fleet already converged, reporting
	// success having moved nothing.
	"controller.endpoint.image.tag",
}

// valuesDiffer names the first declared value the release is not already
// deployed with, or "" when it carries all of them.
func valuesDiffer(deployed map[string]any, version string, values map[string]json.RawMessage) string {
	for _, k := range controllerImageTagKeys {
		if got := valueAt(deployed, k); !sameValue(got, version) {
			return fmt.Sprintf("%s %v -> %q", k, got, version)
		}
	}
	for _, k := range sortedValueKeys(values) {
		var want any
		if err := json.Unmarshal(values[k], &want); err != nil {
			return fmt.Sprintf("values.%s is not valid JSON", k)
		}
		if !contains(valueAt(deployed, k), want) {
			return fmt.Sprintf("values.%s differs", k)
		}
	}
	return ""
}

// valueAt walks a dotted helm key into the deployed values, the way --set
// addresses one.
func valueAt(deployed map[string]any, key string) any {
	var cur any = deployed
	for _, seg := range strings.Split(key, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[seg]
		if !ok {
			return nil
		}
	}
	return cur
}

// contains reports whether deployed already carries want — containment, not
// equality, because --set-json merges a declared subtree into what the release
// already has rather than replacing it. Comparing a declared `{"endpoint":
// {"storageClass": "x"}}` against the whole deployed `controller` table by
// equality can never match, so every tick would re-run helm.
func contains(deployed, want any) bool {
	wm, ok := want.(map[string]any)
	if !ok {
		return sameValue(deployed, want)
	}
	dm, ok := deployed.(map[string]any)
	if !ok {
		return false
	}
	for k, v := range wm {
		if !contains(dm[k], v) {
			return false
		}
	}
	return true
}

// sameValue compares two scalars as helm reports them. `--set` type-infers, so a
// numeric image tag comes back as a float rather than the string it was sent as,
// and a type-strict comparison would re-run the upgrade every tick.
func sameValue(deployed, want any) bool {
	if reflect.DeepEqual(deployed, want) {
		return true
	}
	if deployed == nil || want == nil {
		return false
	}
	switch want.(type) {
	case string, float64, bool, json.Number:
		return fmt.Sprint(deployed) == fmt.Sprint(want)
	}
	return false
}

func sortedValueKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
