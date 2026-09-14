// Package foundation stands a YugabyteDB universe up with the YugabyteDB
// Kubernetes Operator, and hands its consumers the address they connect at.
//
// A FOUNDATION is one YugabyteDB universe plus everything running on top of it
// (amp/controller/architecture/ac-highlvl.md). amp keeps all durable controller
// state in it. What this package replaces is a hand-rolled single-node
// `yugabyted` StatefulSet inlined as a bash heredoc in amp/deploy/up{,-gke}.sh:
// single-node, RF-1, no rolling upgrade, no backups, no lifecycle management.
//
// The spec file is the source of truth and an update is a re-apply of it. That
// is deliberate: the CRs are already a declarative surface with server-side
// apply and a status the operator reconciles level-triggered, so wrapping them
// in an imperative API would be less Kubernetes-native, not more. `render`
// emits the manifests and applies nothing, which is what makes a change
// reviewable as a diff and the output consumable by GitOps.
//
// # The spec IS the CRs
//
// `provider:` and `universe:` are the YBProvider and YBUniverse `spec` bodies
// VERBATIM, and `yba.values:` is yugaware chart values verbatim. None of the
// three is a translated subset: this package defaults a few keys and validates
// a few, and does not own the field list.
//
// The alternative is what it replaces — a Go struct per CR mapped field by
// field, extended every time someone needs a field nobody modelled, and
// re-extended on every operator release. With passthrough, `chartVersion` alone
// decides which release's fields are legal and a new field needs no code.
//
// Nothing here re-checks those bodies against the CRD's schema. `kubectl apply`
// sends strict field validation, so the API server rejects an undeclared field,
// a wrong type and a bad enum itself (".spec.numNodez: field not declared in
// schema"), and a copy of that schema in this repo would need regenerating on
// every chart bump to say the same thing slightly sooner. The cost is that a
// passthrough typo surfaces on apply rather than in a cluster-less test.
//
// kubectl and helm stay shelled ambient-authed binaries rather than linked
// SDKs (the ampqa convention), and every one of them goes through Runner, so
// the whole deployer is testable without a cluster.
package foundation

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Defaults, shared by the examples and the golden-file tests.
const (
	DefaultChartVersion = "2026.1.1"
	DefaultChartRepo    = "yugabytedb/yugaware"
	DefaultUniverseName = "foundation"
	DefaultProviderName = "foundation"
	DefaultYSQLPort     = 5433

	// TServerRPCPort is the yb-tserver RPC port the compute hot path speaks
	// pggate to, alongside YSQL on the same Service. It is NOT the YSQL port:
	// a network policy that opens only YSQL leaves the control plane working
	// and silently breaks the hot path (amp/compute/architecture/high-level.md
	// "The hot path needs direct tserver RPC access").
	TServerRPCPort = 9100

	// TServerEndpointName is the serviceEndpoints entry asking the chart for one
	// ClusterIP over the tserver pods carrying BOTH client ports. The chart
	// creates the Service; nothing here does.
	//
	// Both ports on one Service is what lets a single address drive both compute
	// lanes: the YSQL pool connects to the host as given, and the direct-RPC
	// lane derives the same host on TServerRPCPort (ysql_client's
	// effective_tserver_rpc_addrs, which appends 9100 to each host_ports HOST
	// when tserver_rpc_addrs is unset — and nothing in the deploy path can set
	// it, so the derivation is the only route).
	//
	// Deliberately NOT the chart's own "yb-tserver-service" name: our override
	// replaces the default endpoint list, and if it ever failed to apply, the
	// chart's default endpoint of that name would occupy it as a LoadBalancer
	// with no 9100. A distinct name makes the Service's presence proof that our
	// override took.
	TServerEndpointName = "yb-tserver-conn"

	// TServerServiceLabels finds that Service. It is DISCOVERED, not named,
	// and the reason is worth stating because the name looks predictable and
	// is not: a Namespaced-scope endpoint is named
	// "<commonLabels.app.kubernetes.io/part-of | \"namespaced\">-<endpoint
	// name>", and YBA always sets that label — to the universe name it
	// generated, suffix and all. So the real Service came out
	// "foundation-1954916679-yb-tserver-conn" where this once expected
	// "namespaced-yb-tserver-conn", and a name-based lookup found nothing at
	// all on a universe that was otherwise perfectly healthy.
	//
	// These two labels are set from values WE control — the template writes
	// `serviceName: <endpoint name>` and `scope: <scope>` — so they identify
	// the Service no matter what YBA prefixes it with.
	TServerServiceLabels = "serviceName=" + TServerEndpointName + ",scope=Namespaced"
)

// ChartSource is where the yugaware chart comes from.
type ChartSource string

const (
	// ChartRepo pulls from the public helm repo, which carries RELEASED
	// versions only.
	ChartRepo ChartSource = "repo"
	// ChartS3 pulls the release tarball, which is the only place unreleased
	// itest builds exist (the path ampqa's apps/ybastack uses).
	ChartS3 ChartSource = "s3"
	// ChartPath is a local tarball or directory — air-gapped runs, and a chart
	// under test.
	ChartPath ChartSource = "path"
)

// Spec is a foundation, as declared in the YAML file that is its source of
// truth. Every day-2 change is an edit to this and a re-apply.
type Spec struct {
	KubeContext  string      `yaml:"kubeContext"`
	Namespace    string      `yaml:"namespace"`
	ChartVersion string      `yaml:"chartVersion"`
	ChartSource  ChartSource `yaml:"chartSource"`
	// CRDPath overrides where the operator's CRD bundle comes from — a local
	// copy for an air-gapped cluster or a CI run with no egress to GitHub.
	// Empty uses CRDURL(ChartVersion).
	CRDPath string `yaml:"crdPath"`

	YBA YBASpec `yaml:"yba"`

	// Provider is YBProvider.spec verbatim, plus a `name` key for the object's
	// metadata.
	//
	// A k8s provider carries what the operator's auto-provider cannot infer and
	// what the universe cannot override: the DB image registry (k8s universes
	// pull the DB image from a registry rather than a release tarball), its pull
	// secret, and the per-zone storage class — which takes PRECEDENCE over any
	// storageClass on the universe's volumes, so this is where a storage class
	// actually gets decided.
	//
	// Empty means "use a preexisting provider", and then universe.providerName
	// must name it.
	Provider map[string]any `yaml:"provider"`

	// Universe is YBUniverse.spec verbatim. See the package doc.
	Universe map[string]any `yaml:"universe"`

	// Databases are created after the universe reports Ready. The amp
	// controller's db.Connect targets yb_storage AT STARTUP, so it must exist
	// before the controller rolls out.
	Databases []string `yaml:"databases"`
}

// YBASpec configures the yugaware (YBA) control plane install.
type YBASpec struct {
	// Chart is the repo reference, S3 version, or local path, per ChartSource.
	Chart string `yaml:"chart"`
	// Values are yugaware chart values verbatim, written to a values file and
	// passed as `helm -f` — the chart's whole surface, with no field list here
	// to keep in step with it.
	//
	// The settings this deployer requires are applied as `--set` AFTER the file
	// so they win, since helm gives --set precedence over -f. That is
	// deliberate: kubernetesOperatorEnabled and rbac.create are load-bearing,
	// and a values file that quietly disabled either would leave a YBA with no
	// operator and a CR nothing reconciles.
	Values map[string]any `yaml:"values"`
}

// Load parses a Spec from YAML and fills in defaults.
//
// KnownFields is on for the Spec's own keys, so a typo in `namespace` or
// `chartVersion` is an error rather than a silently ignored field. It
// deliberately does not reach inside provider/universe/values, which are
// passthrough — a typo there is caught by the API server on apply.
func Load(data []byte) (*Spec, error) {
	var s Spec
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse spec: %w", err)
	}
	s.withDefaults()
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Spec) withDefaults() {
	if s.ChartVersion == "" {
		s.ChartVersion = DefaultChartVersion
	}
	if s.ChartSource == "" {
		s.ChartSource = ChartRepo
	}
	if s.YBA.Chart == "" && s.ChartSource == ChartRepo {
		s.YBA.Chart = DefaultChartRepo
	}
	if s.Universe == nil {
		s.Universe = map[string]any{}
	}
	setDefault(s.Universe, "universeName", DefaultUniverseName)

	// A provider block means we create one, so the universe has to reference it
	// by whatever name it got. Without a block, the operator uses the
	// providerName already set (Validate requires one).
	if len(s.Provider) > 0 {
		setDefault(s.Provider, "name", DefaultProviderName)
		setDefault(s.Universe, "providerName", s.Provider["name"])
	}
	s.universeDefaults()
}

// universeDefaults fills in the universe fields this deployer decides, so that
// s.Universe IS the body that gets rendered.
//
// It lives here rather than in Render because a reader has to be able to answer
// "what will the universe have" from the Spec alone. When these were applied at
// render time instead, s.Universe silently disagreed with the CR and
// Deployer.sslMode read the wrong one: it saw no enableClientToNodeEncrypt,
// fell back to the CRD's default of true, and emitted sslmode=require against a
// universe rendered with TLS off.
//
// Every value here depends on nothing outside the spec, which is what makes
// parse time the right moment. The provider's kubeNamespace is the exception
// and is deliberately NOT here: it defaults from s.Namespace, which --namespace
// overrides after Load, so providerSpec fills it in at render time. That is the
// same reason the check on it could not live in Validate.
func (s *Spec) universeDefaults() {
	// YSQL on, YCQL off: every consumer in this repo speaks the Postgres wire
	// protocol, and an interface nothing uses is a listener to secure for no
	// benefit.
	setDefault(s.Universe, "enableYSQL", true)
	setDefault(s.Universe, "enableYCQL", false)

	// TLS off, written rather than left to the CRD, which defaults BOTH to
	// true. Two reasons, one of which is a hard blocker:
	//
	//   - Client-to-node on means the DSN needs sslmode=require. Connect
	//     derives that from this field, so this half is a choice — but it is
	//     the choice that keeps the address contract identical to the yugabyted
	//     path every other consumer is built against.
	//   - Node-to-node on encrypts the tserver RPC port too, and the compute
	//     hot path speaks pggate to 9100 directly. Nothing in the deploy path
	//     can hand those daemons cert material — endpoint_supervisor writes
	//     sslrootcert for the YSQL lane only — so the hot path would fail at
	//     startup with no way to fix it from here.
	//
	// Both are `self == oldSelf` in the CRD: create-time only, so turning
	// either on later REBUILDS the universe rather than editing it. A spec that
	// wants TLS has to say so from the start and solve the RPC cert problem
	// above — tracked in github.com/yugabyte/meko/issues/1241.
	setDefault(s.Universe, "enableNodeToNodeEncrypt", false)
	setDefault(s.Universe, "enableClientToNodeEncrypt", false)

	// Always on, and NOT a request for public exposure: the chart gates its
	// whole serviceEndpoints template on this value, so without it the
	// tserver-conn endpoint below is never created. Public exposure would take
	// a LoadBalancer-type endpoint, and this declares none — and because
	// overriding the endpoint list REPLACES it, the chart's three default
	// LoadBalancer endpoints are dropped too.
	setDefault(s.Universe, "enableLoadBalancer", true)

	overrides, ok := s.Universe["kubernetesOverrides"].(map[string]any)
	if !ok {
		overrides = map[string]any{}
		s.Universe["kubernetesOverrides"] = overrides
	}
	// Only when the spec declares none: overriding the list replaces it, so a
	// spec carrying its own serviceEndpoints has taken responsibility for the
	// tserver-conn endpoint too.
	setDefault(overrides, "serviceEndpoints", []any{map[string]any{
		"name":  TServerEndpointName,
		"type":  "ClusterIP",
		"scope": "Namespaced",
		"app":   "yb-tserver",
		// Both client ports on one Service, so one address serves both compute
		// lanes — see TServerEndpointName.
		"ports": map[string]any{
			"tcp-ysql-port": fmt.Sprint(DefaultYSQLPort),
			"tcp-rpc-port":  fmt.Sprint(TServerRPCPort),
		},
	}})
}

// setDefault fills a key only when the operator left it out, so a passthrough
// value always wins over a default.
func setDefault(m map[string]any, key string, value any) {
	if _, set := m[key]; !set {
		m[key] = value
	}
}

// ProviderName is the provider this foundation's universe references.
func (s *Spec) ProviderName() string {
	name, _ := s.Universe["providerName"].(string)
	return name
}

// UniverseName is the CR's name and metadata.name.
func (s *Spec) UniverseName() string {
	name, _ := s.Universe["universeName"].(string)
	return name
}

// Validate rejects a spec that cannot produce a working foundation.
//
// Deliberately about coherence rather than completeness. Field names, types and
// enums belong to the API server, which rejects them on apply. Everything here
// is something the CRD would happily ACCEPT and YBA or the operator would then
// reject asynchronously — much later, and much less clearly, sometimes with no
// message written back to the CR at all.
func (s *Spec) Validate() error {
	if s.Namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	switch s.ChartSource {
	case ChartRepo, ChartS3, ChartPath:
	default:
		return fmt.Errorf("chartSource %q is not one of repo, s3, path", s.ChartSource)
	}
	if s.ChartSource == ChartPath && s.YBA.Chart == "" {
		return fmt.Errorf("chartSource path needs yba.chart set to the tarball or directory")
	}
	nodes, hasNodes := intField(s.Universe, "numNodes")
	rf, hasRF := intField(s.Universe, "replicationFactor")
	if hasRF {
		switch rf {
		case 1, 3, 5, 7:
		default:
			return fmt.Errorf("universe.replicationFactor %d is not supported (use 1, 3, 5 or 7)", rf)
		}
	}
	if hasNodes && hasRF && nodes < rf {
		return fmt.Errorf("universe.numNodes (%d) is below replicationFactor (%d): the universe cannot place its replicas",
			nodes, rf)
	}

	if len(s.Provider) == 0 && s.ProviderName() == "" {
		return fmt.Errorf("with no provider block, universe.providerName must name a preexisting " +
			"provider: a k8s provider carries the DB image registry, its pull secret and the " +
			"per-zone storage class, none of which the operator's auto-provider can infer")
	}

	// YBA parses this and compares it against its own bundled release, so an
	// EMPTY string is not "use the default" — it is a parse failure. The
	// universe then reports universeState "Error Creating" with no message and
	// a still-queued action, and the only explanation is a RuntimeException in
	// the yugaware log: "Unable to parse YB version strings".
	if universeString(s.Universe, "ybSoftwareVersion") == "" {
		return fmt.Errorf("universe.ybSoftwareVersion is required and must be a release this YBA " +
			"has: an empty value is not \"whatever YBA bundles\", it fails to parse and the " +
			"universe comes back \"Error Creating\" with no reason attached. The bundled release " +
			"is the one under /opt/yugabyte/releases in the yugaware pod, and it must also be a " +
			"tag in the provider's kubernetesImageRegistry")
	}

	if err := s.validateProviderZones(); err != nil {
		return err
	}
	if err := s.validatePlacement(nodes, hasNodes); err != nil {
		return err
	}
	return nil
}

// validatePlacement checks universe.placementInfo against the provider and the
// node count.
//
// Both halves are things YBA rejects, but asynchronously and from inside the
// operator: a placement naming a zone the provider does not have, and a
// per-zone node count that does not add up to numNodes. Either arrives as a
// failed action on the universe tens of minutes into a bring-up, which is a
// bad place to read "you typed the zone code wrong".
func (s *Spec) validatePlacement(nodes int, hasNodes bool) error {
	placement, ok := s.Universe["placementInfo"].(map[string]any)
	if !ok {
		return nil
	}
	known := providerZones(s.Provider)
	total, counted := 0, false
	for _, r := range asList(placement["regions"]) {
		region, _ := r.(map[string]any)
		for _, z := range asList(region["zones"]) {
			zone, _ := z.(map[string]any)
			code, _ := zone["code"].(string)
			// Only when there IS a provider block: with a preexisting provider
			// its zones live in YBA, where nothing here can see them.
			if len(s.Provider) > 0 && code != "" && !known[code] {
				return fmt.Errorf("universe.placementInfo names zone %q, which the provider does not "+
					"have (it has %s): a placement can only use zones its provider defines",
					code, strings.Join(sortedKeys(known), ", "))
			}
			if n, has := intField(zone, "numNodes"); has {
				total += n
				counted = true
			}
		}
	}
	if hasNodes && counted && total != nodes {
		return fmt.Errorf("universe.placementInfo places %d node(s) but universe.numNodes is %d: "+
			"YBA rejects a placement that does not add up", total, nodes)
	}
	return nil
}

// providerZones is every zone code the provider block defines.
func providerZones(provider map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, r := range asList(provider["regions"]) {
		region, _ := r.(map[string]any)
		for _, z := range asList(region["zones"]) {
			zone, _ := z.(map[string]any)
			if code, ok := zone["code"].(string); ok && code != "" {
				out[code] = true
			}
		}
	}
	return out
}

// asList reads a YAML/JSON list out of a passthrough body.
func asList(v any) []any {
	list, _ := v.([]any)
	return list
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// intField reads a YAML-decoded integer, which arrives as int or float64
// depending on how it was written.
func intField(m map[string]any, key string) (int, bool) {
	switch v := m[key].(type) {
	case int:
		return v, true
	case float64:
		return int(v), true
	}
	return 0, false
}

// universeString reads a string field out of the passthrough universe body.
func universeString(universe map[string]any, key string) string {
	v, _ := universe[key].(string)
	return v
}

// universeInt reads an integer field out of the passthrough universe body.
func universeInt(universe map[string]any, key string) int {
	n, _ := intField(universe, key)
	return n
}

// storageClasses is every storage class the spec names, deduplicated and
// ordered so preflight checks them predictably.
//
// There is more than one place to set one: each volume block on the universe,
// each zone on the provider — the provider's wins, per the CRD ("provider
// storage class takes precedence over this since it is at AZ level") — and
// yba.values, for yugaware's own PVC, which is a separate class again. Checking
// them all is the only way preflight cannot pass a spec whose effective class is
// missing.
func storageClasses(s *Spec) []string {
	seen := map[string]bool{}
	add := func(v any) {
		if sc, ok := v.(string); ok && sc != "" {
			seen[sc] = true
		}
	}
	for _, key := range []string{"tserverVolume", "masterVolume", "deviceInfo", "masterDeviceInfo"} {
		if vol, ok := s.Universe[key].(map[string]any); ok {
			add(vol["storageClass"])
		}
	}
	for _, r := range asList(s.Provider["regions"]) {
		region, _ := r.(map[string]any)
		for _, z := range asList(region["zones"]) {
			zone, _ := z.(map[string]any)
			if ci, ok := zone["cloudInfo"].(map[string]any); ok {
				add(ci["kubernetesStorageClass"])
			}
		}
	}
	// yugaware's OWN PVC. Unset, it takes the cluster default — which on a
	// multi-zone cluster is the Immediate-binding class that zone-pins the
	// ~4.5 CPU yugaware pod before it schedules, the failure preflight exists
	// to catch. It is a different class from the universe's and is missed
	// entirely by a walk that only reads the provider and the volumes.
	if yw, ok := s.YBA.Values["yugaware"].(map[string]any); ok {
		add(yw["storageClass"])
	}
	out := make([]string, 0, len(seen))
	for sc := range seen {
		out = append(out, sc)
	}
	sort.Strings(out)
	return out
}

// StorageClasses is every storage class this spec's effective configuration
// names. Exported for callers that must check them against a cluster before an
// apply — the foundation CI does, since an Immediate-binding class zone-pins the
// yugaware pod and it then never schedules.
//
// There are two places to set one and the provider's wins, so a caller that
// read only the universe's would check the wrong class entirely.
func (s *Spec) StorageClasses() []string { return storageClasses(s) }

// PullSecrets is the image-pull Secret this deployer needs present, if any.
//
// Only yugaware's own (yba.values.image.pullSecret), and only because we install
// that chart into a namespace we own. The DB image's pull secret is NOT here:
// YBA derives it from its own and propagates it into whatever namespace it
// deploys universe nodes into, which is the only thing that can work — the
// YBProvider CRD has no field for pull-secret CONTENT (only kubeConfigSecret),
// so a name is all a CR could carry, and a name is useless in a namespace the
// Secret was never copied to.
//
// Nothing here creates one. A missing one is not a deploy error but an
// ImagePullBackOff that only shows up as a wait that never ends.
func (s *Spec) PullSecrets() []string {
	seen := map[string]bool{}
	if img, ok := s.YBA.Values["image"].(map[string]any); ok {
		if name, ok := img["pullSecret"].(string); ok && name != "" {
			seen[name] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// UniverseNamespace is where the DB pods, and so the Service consumers dial,
// actually land.
//
// It is the provider zones' kubeNamespace, not spec.Namespace, because that
// field is what YBA obeys — and when it is unset YBA invents a namespace of its
// own ("yb-<customer code>-<universe>-<suffix>"), which is how a perfectly
// healthy universe ended up somewhere nothing looked for it. providerSpec
// defaults it to spec.Namespace, so co-location stays the default and a spec
// that wants the universe elsewhere says so in the CR rather than in a field
// invented here.
func (s *Spec) UniverseNamespace() string {
	if ns := s.declaredUniverseNamespace(); ns != "" {
		return ns
	}
	return s.Namespace
}

// declaredUniverseNamespace is the kubeNamespace every zone agrees on, or "" if
// none is set. It returns the first when they disagree; validateProviderZones
// is what refuses that, so this stays a plain accessor.
func (s *Spec) declaredUniverseNamespace() string {
	for _, r := range asList(s.Provider["regions"]) {
		region, _ := r.(map[string]any)
		for _, z := range asList(region["zones"]) {
			zone, _ := z.(map[string]any)
			ci, _ := zone["cloudInfo"].(map[string]any)
			if ns, ok := ci["kubeNamespace"].(string); ok && ns != "" {
				return ns
			}
		}
	}
	return ""
}

// validateProviderZones requires every zone to name the same kubeNamespace.
//
// One namespace, because every universe-facing lookup here runs in exactly one:
// the tserver Service, its selector, the pod ysqlsh runs in. Zones spread over
// several namespaces is a genuinely different feature — the address would have
// to be per-zone — and rejecting it beats resolving to whichever zone happened
// to be first.
func (s *Spec) validateProviderZones() error {
	var first, firstCode string
	for _, r := range asList(s.Provider["regions"]) {
		region, _ := r.(map[string]any)
		for _, z := range asList(region["zones"]) {
			zone, _ := z.(map[string]any)
			code, _ := zone["code"].(string)
			ci, _ := zone["cloudInfo"].(map[string]any)
			ns, _ := ci["kubeNamespace"].(string)
			if ns == "" {
				continue
			}
			if first == "" {
				first, firstCode = ns, code
				continue
			}
			if ns != first {
				return fmt.Errorf("provider zones disagree about kubeNamespace: %q says %q but %q "+
					"says %q. Every lookup this deployer makes for the universe runs in one "+
					"namespace, so the zones have to share one",
					firstCode, first, code, ns)
			}
		}
	}
	return nil
}

// DefaultUserPassword is the password for the single YBA customer the operator
// requires, or "" when the spec sets none.
//
// Checked by Preflight rather than Validate: it is a precondition for
// INSTALLING the chart, not a property of a well-formed spec — `render` needs
// no YBA at all, and a GitOps consumer that only ever renders CRs should not
// have to carry a password to do it.
func (s *Spec) DefaultUserPassword() string {
	yw, _ := s.YBA.Values["yugaware"].(map[string]any)
	user, _ := yw["defaultUser"].(map[string]any)
	pw, _ := user["password"].(string)
	return pw
}

// checkDefaultUser requires the password for the YBA customer the operator
// needs.
//
// HelmSets forces yugaware.defaultUser.enabled on, because the operator refuses
// every CR unless YBA has EXACTLY ONE customer and the chart's post-install Job
// is the only thing that creates one. That Job's template hard-fails on an
// empty password — but from inside helm, tens of minutes into a bring-up. The
// other fields have chart defaults; only this one does not.
func (s *Spec) checkDefaultUser() error {
	if s.DefaultUserPassword() == "" {
		return fmt.Errorf("yba.values.yugaware.defaultUser.password is required: the operator " +
			"refuses every CR unless YBA has exactly one customer, and the chart's post-install " +
			"job is what creates it — the deployer enables that job, but the chart rejects an " +
			"empty password")
	}
	return nil
}
