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
// Every CR is a top-level key naming its KIND, whose value is {name, spec}:
// name is metadata.name and spec is the CR's spec body VERBATIM. YBProvider and
// YBUniverse are two such keys and nothing more, so any kind in the operator's
// bundle is reachable the day its CRD ships. `yba.values:` is yugaware chart
// values verbatim on the same principle. None is a translated subset: this
// package defaults a few keys and validates a few, and does not own the field
// list.
//
// An UPPERCASE-initial key is a kind; everything else must be one of the Spec's
// own fields. That split is what keeps KnownFields on for the settings — with
// free-form keys a mistyped `namespaec:` would become a CR rather than an
// error. Key order is apply order, and teardown is its reverse.
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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"meko/foundation/ybuniverse"
)

// Defaults, shared by the examples and the golden-file tests.
const (
	// The default is the newest PUBLISHED line, because it is the only thing
	// that works with no other field set. A build is deployed by naming its
	// tarball in yba.chart, which is a choice the spec has to make explicitly
	// — see examples/amp.yaml.
	DefaultChartVersion = "2026.1.1"
	DefaultChartSource  = ChartRepo
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

	// TServerAppLabel is the chart Service the endpoint attaches to. The
	// template renders an entry only where $service.label == $endpoint.app, so
	// a typo here produces no Service and no complaint.
	TServerAppLabel = "yb-tserver"

	// KindRelease is the one kind name this package knows beyond the two it is
	// built around, and only because checkDBRelease has to ask whether the
	// spec imports a release of its own.
	KindRelease = "Release"

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
	// ChartPath is whatever helm takes: a tarball, an unpacked chart
	// directory, or a URL.
	//
	// There is deliberately no separate source for the release bucket. An
	// unreleased build lives at a releases.yugabyte.com URL (the path ampqa's
	// apps/ybastack uses), that bucket is public for reads, and helm fetches
	// a URL itself — so it is a path like any other, and a second enum value
	// would only be a second name for the same thing.
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

	// Databases are created after the universe reports Ready. The amp
	// controller's db.Connect targets yb_storage AT STARTUP, so it must exist
	// before the controller rolls out.
	Databases []string `yaml:"databases"`

	// Resources is every operator CR the spec declares, IN DOCUMENT ORDER.
	//
	// A slice rather than a map keyed by kind, because apply order is the order
	// written and delete order is its reverse, and Go's map iteration would
	// lose both. Filled by Load from the uppercase top-level keys; never a yaml
	// field of its own.
	Resources []Resource `yaml:"-"`
}

// Resource is one operator CR: the kind it was declared under, the object's
// name, and a spec body passed through exactly as written.
//
// Name is separate from Spec rather than lifted out of it, because spec.name is
// a REQUIRED field of KMSConfig, PitrConfig and DrConfig and a real field of
// StorageConfig and BackupSchedule. Folding the object name into the body would
// collide with the CR's own.
type Resource struct {
	Kind string         `yaml:"-"`
	Name string         `yaml:"name"`
	Spec map[string]any `yaml:"spec"`

	// specGiven records whether the file actually had a `spec:` key, which
	// Validate needs: the universe's own defaults are written into Spec before
	// Validate runs, so emptiness alone can no longer tell.
	specGiven bool `yaml:"-"`
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
// A top-level key whose first letter is UPPERCASE is an operator CR kind and
// its value is passed through; anything else must be one of the Spec's own
// fields. That split is what lets any CRD in the bundle be declared without
// this package knowing the kind, while keeping KnownFields on for the
// settings — so a typo in `namespace` is still an error rather than a silently
// ignored key that would otherwise become a CR named "namespaec".
//
// It reaches a node at a time rather than decoding the whole document into a
// map, because the map would lose the key order that apply and delete both
// depend on.
func Load(data []byte) (*Spec, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse spec: %w", err)
	}
	root := mappingRoot(&doc)
	if root == nil {
		return nil, fmt.Errorf("parse spec: expected a YAML mapping at the top level")
	}

	settings := &yaml.Node{Kind: yaml.MappingNode}
	var s Spec
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if replacement, ok := renamedKeys[key.Value]; ok {
			return nil, fmt.Errorf("parse spec: %q is no longer a spec field — declare it as the "+
				"CR kind %q, whose value is {name, spec}", key.Value, replacement)
		}
		if !isCRKind(key.Value) {
			settings.Content = append(settings.Content, key, value)
			continue
		}
		entries, err := decodeResources(key.Value, value)
		if err != nil {
			return nil, fmt.Errorf("parse spec: %w", err)
		}
		s.Resources = append(s.Resources, entries...)
	}
	if err := strictDecode(settings, &s); err != nil {
		return nil, fmt.Errorf("parse spec: %w", err)
	}

	s.withDefaults()
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// renamedKeys are the two spec fields that became CR kinds, kept so the
// migration is a message naming the replacement rather than a bare "field not
// found".
var renamedKeys = map[string]string{
	"provider": ybuniverse.KindProvider,
	"universe": ybuniverse.KindUniverse,
}

// mappingRoot unwraps the document node, returning nil for anything that is
// not a mapping — an empty file included.
func mappingRoot(doc *yaml.Node) *yaml.Node {
	node := doc
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	return node
}

// isCRKind reports whether a top-level key names an operator CR rather than a
// setting.
//
// Case is the whole rule: every field of Spec is lowerCamel and every kind in
// the operator's bundle is UpperCamel, so this needs no list of kinds — which
// is the point, since a kind shipped in a future chart has to work with no
// change here.
func isCRKind(key string) bool {
	r, _ := utf8.DecodeRuneInString(key)
	return unicode.IsUpper(r)
}

// decodeResources reads one kind's value: a single entry, or a list of them
// when a spec declares several of the same kind.
func decodeResources(kind string, value *yaml.Node) ([]Resource, error) {
	switch value.Kind {
	case yaml.MappingNode:
		r, err := decodeResource(kind, value)
		if err != nil {
			return nil, err
		}
		return []Resource{r}, nil
	case yaml.SequenceNode:
		out := make([]Resource, 0, len(value.Content))
		for _, item := range value.Content {
			if item.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("%s: every entry must be a mapping of {name, spec}", kind)
			}
			r, err := decodeResource(kind, item)
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s must be a mapping of {name, spec}, or a list of them for "+
			"several objects of one kind", kind)
	}
}

// decodeResource reads one {name, spec} entry, strictly — so a typo in `name`
// is an error here, while the spec body underneath stays passthrough.
func decodeResource(kind string, node *yaml.Node) (Resource, error) {
	var r Resource
	if err := strictDecode(node, &r); err != nil {
		return Resource{}, fmt.Errorf("%s: %w", kind, err)
	}
	r.Kind = kind
	r.specGiven = r.Spec != nil
	if r.Spec == nil {
		r.Spec = map[string]any{}
	}
	return r, nil
}

// strictDecode decodes a node with KnownFields on.
//
// It goes back through Marshal because KnownFields lives on the Decoder and
// Node.Decode has no equivalent — there is no cheaper way to keep unknown-key
// rejection once the document has been walked by hand.
func strictDecode(node *yaml.Node, out any) error {
	raw, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// find returns the single entry of a kind, or nil.
func (s *Spec) find(kind string) *Resource {
	for i := range s.Resources {
		if s.Resources[i].Kind == kind {
			return &s.Resources[i]
		}
	}
	return nil
}

// count returns how many entries of a kind the spec declares.
func (s *Spec) count(kind string) int {
	n := 0
	for i := range s.Resources {
		if s.Resources[i].Kind == kind {
			n++
		}
	}
	return n
}

// body returns a kind's spec body, or nil when the spec does not declare it.
// Callers read through it rather than a field, so a missing YBUniverse is a
// validation error rather than a panic.
func (s *Spec) body(kind string) map[string]any {
	if r := s.find(kind); r != nil {
		return r.Spec
	}
	return nil
}

// DeclaresRelease reports whether the spec imports a DB release itself, which
// is what makes checkDBRelease's probe inconclusive.
func (s *Spec) DeclaresRelease() bool { return s.count(KindRelease) > 0 }

func (s *Spec) provider() map[string]any { return s.body(ybuniverse.KindProvider) }
func (s *Spec) universe() map[string]any { return s.body(ybuniverse.KindUniverse) }

func (s *Spec) withDefaults() {
	if s.ChartVersion == "" {
		s.ChartVersion = DefaultChartVersion
	}
	if s.ChartSource == "" {
		s.ChartSource = DefaultChartSource
	}
	if s.YBA.Chart == "" && s.ChartSource == ChartRepo {
		s.YBA.Chart = DefaultChartRepo
	}
	s.expandPaths()
	// Nil-safe throughout: a spec missing either required kind still has to
	// reach Validate, which is what names the omission.
	provider, universe := s.find(ybuniverse.KindProvider), s.find(ybuniverse.KindUniverse)
	if provider != nil && provider.Name == "" {
		provider.Name = DefaultProviderName
	}
	if universe == nil {
		return
	}
	if universe.Name == "" {
		universe.Name = DefaultUniverseName
	}
	// metadata.name and the body's own universeName are two different fields
	// and YBA reads the second, so the entry's name is what they agree on.
	setDefault(universe.Spec, "universeName", universe.Name)
	if provider != nil {
		setDefault(universe.Spec, "providerName", provider.Name)
	}
}

// expandPaths expands a leading ~ in the two fields that name something on
// disk.
//
// Neither Go, helm nor kubectl does this: a YAML value is not a shell word, so
// "~/yb/yugaware.tgz" is looked up as a directory literally named "~" and the
// failure is a confusing "no such file" naming a path that visibly exists.
//
// Safe to run unconditionally. Under chartSource repo, yba.chart is a chart
// NAME ("yugabytedb/yugaware") and cannot start with ~; a URL cannot either.
func (s *Spec) expandPaths() {
	s.YBA.Chart = expandHome(s.YBA.Chart)
	s.CRDPath = expandHome(s.CRDPath)
}

// expandHome resolves a leading ~ against the user's home directory, leaving
// everything else — including a bare "~foo" username form this does not
// support — untouched.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// Nothing else about the universe is filled in here. The deployer applies only
// what the spec says, so the CRD's own defaults decide anything it omits — and
// they are not all benign: enableLoadBalancer defaults FALSE, which renders no
// serviceEndpoints at all, and both encryption flags default TRUE, which is an
// encrypted universe nobody asked for. All of them are create-time only, so the
// cost of inheriting the wrong one is a rebuild.
//
// So this stays a parse step with no opinions, and validateAddressContract does
// the talking: what a spec leaves out is named at parse time, before anything is
// applied, rather than papered over with a value the file never mentions.
//
// The two that remain above are not universe policy: universeName and
// providerName are filled from the entry's own metadata.name, which IS in the
// spec — YBA reads the body field and kubectl addresses the object, so they have
// to agree or the object is created under one name and known by another.

// setDefault fills a key only when the operator left it out, so a passthrough
// value always wins over a default.
func setDefault(m map[string]any, key string, value any) {
	if _, set := m[key]; !set {
		m[key] = value
	}
}

// ProviderName is the provider this foundation's universe references.
// It is the ENTRY's name, not the universe body's providerName: kubectl
// addresses the object by metadata.name, which Render takes from the entry.
// Validate keeps the two from disagreeing.
func (s *Spec) ProviderName() string {
	if r := s.find(ybuniverse.KindProvider); r != nil {
		return r.Name
	}
	// No provider of our own — the universe names a preexisting one, and that
	// is the only name there is.
	name, _ := s.universe()["providerName"].(string)
	return name
}

// UniverseName is the CR's metadata.name, which is what every kubectl lookup
// here addresses — NOT the body's universeName, which is a different field
// that YBA reads. Validate keeps the two from disagreeing.
func (s *Spec) UniverseName() string {
	if r := s.find(ybuniverse.KindUniverse); r != nil {
		return r.Name
	}
	return ""
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
	case ChartRepo, ChartPath:
	case "s3":
		// There was an s3 source once. It only ever built a
		// releases.yugabyte.com URL, which helm fetches as an ordinary path.
		return fmt.Errorf("chartSource s3 is gone: the release bucket is public over HTTPS and " +
			"helm fetches a URL like any other chart, so use chartSource: path with yba.chart " +
			"set to https://releases.yugabyte.com/<build>/helm/yugaware-<line>.tgz")
	default:
		return fmt.Errorf("chartSource %q is not one of repo, path", s.ChartSource)
	}
	if s.ChartSource == ChartPath && s.YBA.Chart == "" {
		return fmt.Errorf("chartSource path needs yba.chart set to the tarball or directory")
	}
	if s.ChartSource == ChartRepo && s.ChartVersion == "" {
		return fmt.Errorf("chartVersion is required with chartSource repo: it is the published " +
			"version helm resolves, and an empty one is a failed lookup rather than \"latest\"")
	}
	if err := s.validateResources(); err != nil {
		return err
	}
	nodes, hasNodes := intField(s.universe(), "numNodes")
	rf, hasRF := intField(s.universe(), "replicationFactor")
	if hasNodes && hasRF && nodes < rf {
		return fmt.Errorf("universe.numNodes (%d) is below replicationFactor (%d): the universe cannot place its replicas",
			nodes, rf)
	}

	// YBA parses this and compares it against its own bundled release, so an
	// EMPTY string is not "use the default" — it is a parse failure. The
	// universe then reports universeState "Error Creating" with no message and
	// a still-queued action, and the only explanation is a RuntimeException in
	// the yugaware log: "Unable to parse YB version strings".
	if universeString(s.universe(), "ybSoftwareVersion") == "" {
		return fmt.Errorf("universe.ybSoftwareVersion is required and must be a release this YBA " +
			"has: an empty value is not \"whatever YBA bundles\", it fails to parse and the " +
			"universe comes back \"Error Creating\" with no reason attached. The bundled release " +
			"is the one under /opt/yugabyte/releases in the yugaware pod, and it must also be a " +
			"tag in the provider's kubernetesImageRegistry")
	}

	if err := s.validateProviderZones(); err != nil {
		return err
	}
	if err := s.validatePlacement(); err != nil {
		return err
	}
	if err := s.validateRootCA(); err != nil {
		return err
	}
	if err := s.validateAddressContract(); err != nil {
		return err
	}
	return nil
}

// validateRootCA checks the two halves of a cert-manager universe against each
// other. Both are things the CRD accepts and YBA only complains about later,
// from inside a task, which is this function's whole remit.
//
// rootCA names a YBCertificate for YBA to sign node certificates from. Empty
// means YBA invents its own CA, which is a working universe — just not one
// anybody can verify, which is why sslMode answers `require` for it.
func (s *Spec) validateRootCA() error {
	u := s.universe()
	ca, _ := u["rootCA"].(string)
	if ca == "" {
		return nil
	}
	n2n := s.encryptionOn("enableNodeToNodeEncrypt")
	c2n := s.encryptionOn("enableClientToNodeEncrypt")
	if !n2n && !c2n {
		return fmt.Errorf("universe.rootCA is %q but both encryption flags are false: YBA ignores "+
			"the certificate and builds a plaintext universe, so the CA is a statement of intent "+
			"that nothing acts on. Set enableClientToNodeEncrypt (and enableNodeToNodeEncrypt for "+
			"the tserver RPC port), or drop rootCA", ca)
	}
	// The CA says what to trust; the issuer is what actually signs. YBA reads
	// it off the zone, so a spec with one and not the other builds a universe
	// whose node certificates never appear.
	if s.provider() != nil && !s.anyZoneNamesAnIssuer() {
		return fmt.Errorf("universe.rootCA is %q but no provider zone sets "+
			"cloudInfo.certManagerIssuerName: YBA has a CA to trust and nothing to sign node "+
			"certificates with. Add certManagerIssuerName (with certManagerIssuerKind, which the "+
			"CRD requires alongside it) to the zone", ca)
	}
	return nil
}

// anyZoneNamesAnIssuer reports whether some provider zone points YBA at a
// cert-manager issuer. Per zone, not per region: that is where the CRD puts the
// three certManagerIssuer* fields.
func (s *Spec) anyZoneNamesAnIssuer() bool {
	for _, r := range asList(s.provider()["regions"]) {
		region, _ := r.(map[string]any)
		for _, z := range asList(region["zones"]) {
			zone, _ := z.(map[string]any)
			ci, _ := zone["cloudInfo"].(map[string]any)
			if name, _ := ci["certManagerIssuerName"].(string); name != "" {
				return true
			}
		}
	}
	return false
}

// TLSInfo is the universe's transport security, as a deploy script needs it:
// enough to decide what has to exist BEFORE the universe is created. A
// cert-manager universe needs its issuer and its CA in place first, and that is
// knowable from the spec alone — which is what keeps TLS from becoming a second
// input that can disagree with this file.
type TLSInfo struct {
	ClientToNode bool   `json:"clientToNode"`
	NodeToNode   bool   `json:"nodeToNode"`
	RootCA       string `json:"rootCA,omitempty"`
	SSLMode      string `json:"sslMode"`
}

// TLS reports that shape.
func (s *Spec) TLS() TLSInfo {
	ca, _ := s.universe()["rootCA"].(string)
	return TLSInfo{
		ClientToNode: s.encryptionOn("enableClientToNodeEncrypt"),
		NodeToNode:   s.encryptionOn("enableNodeToNodeEncrypt"),
		RootCA:       ca,
		SSLMode:      s.SSLMode(),
	}
}

// CRDEncryptionDefault is what the YBUniverse CRD gives
// enableClientToNodeEncrypt and enableNodeToNodeEncrypt when a spec omits them.
//
// Mirrored here — the one place this module copies a CRD default — because the
// DSN has to answer for the universe that WILL exist, not for the fields the
// file happens to mention. Reading an absent flag as false gave a plaintext DSN
// against an encrypted universe, which is the
// `no pg_hba.conf entry ... no encryption` crash-loop.
//
// Being a mirror it can go stale, so it is checked on a chart bump like the
// other things a bump moves (see ARCHITECTURE.md):
//
//	kubectl explain ybuniverse.spec.enableClientToNodeEncrypt
const CRDEncryptionDefault = true

// encryptionOn reports what the universe will actually have, spec or CRD.
func (s *Spec) encryptionOn(field string) bool {
	if v, ok := s.universe()[field].(bool); ok {
		return v
	}
	return CRDEncryptionDefault
}

// InheritedEncryption names the encryption flags the spec did not state, in the
// order a reader would want them.
//
// Absent is a decision, just not one the file records: the CRD turns both on and
// both are create-time only, so a spec that meant plaintext has exactly one
// chance to notice before the universe is built. Preflight is where it is said.
func (s *Spec) InheritedEncryption() []string {
	var inherited []string
	for _, f := range []string{"enableClientToNodeEncrypt", "enableNodeToNodeEncrypt"} {
		if _, set := s.universe()[f]; !set {
			inherited = append(inherited, f)
		}
	}
	return inherited
}

// SSLMode is the sslmode this universe implies. See Deployer.SSLMode, which is
// where the reasoning lives and where a consumer's override is applied.
func (s *Spec) SSLMode() string {
	if !s.encryptionOn("enableClientToNodeEncrypt") {
		return "disable"
	}
	if ca, _ := s.universe()["rootCA"].(string); ca != "" {
		return "verify-full"
	}
	return "require"
}

// validateAddressContract requires the spec to say what every consumer depends
// on, because the deployer no longer says it for them.
//
// All of this is something the CRD accepts and the chart then quietly declines
// to render, so the universe comes up healthy and serves no address: nothing
// carries TServerServiceLabels, connstr resolves nothing, and `foundation up`
// reaches its databases step before failing — tens of minutes in, pointing at
// the cluster rather than at the file. There is no version of that worth
// letting through, which is why these are errors and not advice: this deployer
// cannot itself work without the Service.
//
// Reported together rather than one at a time. A spec arriving from before the
// deployer stopped defaulting is missing the entry AND the flag that gates it,
// and finding that out one run at a time helps nobody.
//
// Deliberately NOT checked: the endpoint's `type`, because ClusterIP and an
// internal LoadBalancer are both real choices; `exportService`, which is inert
// rather than wrong on a cluster with no multi-cluster services; any other
// entry, which is the author's business; and field names, types and enums,
// which the API server rejects on apply.
func (s *Spec) validateAddressContract() error {
	u := s.universe()
	if u == nil {
		return nil
	}
	var bad []string

	overrides, _ := u["kubernetesOverrides"].(map[string]any)
	var entry map[string]any
	matches := 0
	for _, e := range asList(overrides["serviceEndpoints"]) {
		ep, _ := e.(map[string]any)
		if name, _ := ep["name"].(string); name == TServerEndpointName {
			matches++
			if entry == nil {
				entry = ep
			}
		}
	}
	if entry == nil {
		bad = append(bad, fmt.Sprintf("universe.kubernetesOverrides.serviceEndpoints declares no %q "+
			"entry: it is the single address this foundation hands consumers, carrying YSQL on "+
			"%d and the tserver RPC the compute hot path derives on %d. Without it the chart "+
			"creates no Service carrying %s and there is no address at all",
			TServerEndpointName, DefaultYSQLPort, TServerRPCPort, TServerServiceLabels))
		// enableLoadBalancer does not depend on the entry, and a spec from
		// before this deployer stopped defaulting is missing both — so it is
		// reported now rather than on the next run.
		return joinProblems(append(bad, missingLoadBalancer(u)...))
	}
	if matches > 1 {
		bad = append(bad, fmt.Sprintf("serviceEndpoints declares %d entries named %q: a "+
			"Namespaced-scope endpoint is one Service shared by every release in the namespace, so "+
			"the duplicates fight over it and the address becomes a coin flip", matches,
			TServerEndpointName))
	}
	bad = append(bad, missingLoadBalancer(u)...)
	if scope, _ := entry["scope"].(string); scope != "Namespaced" {
		bad = append(bad, fmt.Sprintf("serviceEndpoints %q has scope %q, want Namespaced: an "+
			"AZ-scope endpoint is one Service per zone release under another name, so %s matches "+
			"nothing and the address stops surviving a full move", TServerEndpointName, scope,
			TServerServiceLabels))
	}
	if app, _ := entry["app"].(string); app != TServerAppLabel {
		bad = append(bad, fmt.Sprintf("serviceEndpoints %q has app %q, want %q: the chart renders an "+
			"endpoint only for the Service whose label matches, so nothing is created and nothing "+
			"says why", TServerEndpointName, app, TServerAppLabel))
	}
	ports, _ := entry["ports"].(map[string]any)
	for _, p := range []struct {
		key  string
		want int
	}{{"tcp-ysql-port", DefaultYSQLPort}, {"tcp-rpc-port", TServerRPCPort}} {
		if got, ok := portValue(ports[p.key]); !ok || got != p.want {
			bad = append(bad, fmt.Sprintf("serviceEndpoints %q sets %s to %v, want %d. Both client "+
				"ports belong on this one Service: the YSQL pool dials the host as given and the "+
				"direct-RPC lane DERIVES its address by appending %d to the same host, which "+
				"nothing in the deploy path can configure otherwise. A missing or moved port "+
				"passes every other check and then compute fails to open its table handles at "+
				"startup", TServerEndpointName, p.key, ports[p.key], p.want, TServerRPCPort))
		}
	}
	return joinProblems(bad)
}

// missingLoadBalancer is the gate the whole serviceEndpoints template hangs on.
func missingLoadBalancer(u map[string]any) []string {
	if on, _ := u["enableLoadBalancer"].(bool); on {
		return nil
	}
	return []string{fmt.Sprintf("universe.enableLoadBalancer is not true (the CRD defaults it "+
		"FALSE), so the chart renders no serviceEndpoints at all and the %q entry is never "+
		"applied. It is not a request for public exposure — the chart gates the whole template "+
		"on it", TServerEndpointName)}
}

// joinProblems renders several findings as one error, numbered when there is
// more than one so a reader can see how many there are before reading them.
func joinProblems(bad []string) error {
	switch len(bad) {
	case 0:
		return nil
	case 1:
		return errors.New(bad[0])
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d problems with this spec:", len(bad))
	for i, p := range bad {
		fmt.Fprintf(&b, "\n  %d. %s", i+1, p)
	}
	return errors.New(b.String())
}

// portValue reads a port written either as a YAML number or as a string. The
// chart takes both, so a spec that quotes it is not wrong and must not warn.
func portValue(v any) (int, bool) {
	switch p := v.(type) {
	case int:
		return p, true
	case string:
		n, err := strconv.Atoi(p)
		return n, err == nil
	}
	return 0, false
}

// validateResources checks the shape of the declared CR set — what the API
// server either accepts happily or rejects in a way that names nothing the
// author can find in their file.
//
// It deliberately checks nothing ABOUT a kind beyond the two this module is
// built around. Which fields a KMSConfig needs is the CRD's business, and a
// table of kinds here would be the thing that goes stale on the next chart.
func (s *Spec) validateResources() error {
	if s.count(ybuniverse.KindUniverse) != 1 {
		// connstr, ensureDatabases and the tserver-Service lookup all resolve
		// one universe in one namespace.
		return fmt.Errorf("exactly one %s is required; this spec declares %d",
			ybuniverse.KindUniverse, s.count(ybuniverse.KindUniverse))
	}
	// A provider is OPTIONAL: the operator has an auto-provider, and a universe
	// can also name one somebody else registered. Declaring one is how you
	// control the DB image registry, its pull secret and the per-zone storage
	// class — which the auto-provider cannot infer and the universe cannot
	// override — but that is a reason to want one, not a reason to require it.
	if s.count(ybuniverse.KindProvider) > 1 {
		return fmt.Errorf("%s is declared more than once, and a universe references exactly one",
			ybuniverse.KindProvider)
	}
	// Apply order is document order, and YBA rejects a universe naming a
	// provider that has not registered yet — which arrives asynchronously, as a
	// failed action on the universe, tens of minutes in.
	if s.count(ybuniverse.KindProvider) == 1 &&
		s.indexOf(ybuniverse.KindProvider) > s.indexOf(ybuniverse.KindUniverse) {
		return fmt.Errorf("%s must be declared above %s: keys are applied top to bottom, and YBA "+
			"rejects a universe whose provider has not registered",
			ybuniverse.KindProvider, ybuniverse.KindUniverse)
	}
	// Same reason for the CA the universe's rootCA names: the reference is
	// resolved asynchronously, so a certificate below the universe is a failed
	// action rather than a parse error.
	if s.count(ybuniverse.KindCertificate) == 1 &&
		s.indexOf(ybuniverse.KindCertificate) > s.indexOf(ybuniverse.KindUniverse) {
		return fmt.Errorf("%s must be declared above %s: keys are applied top to bottom, and the "+
			"universe's rootCA names a certificate YBA has to know about first",
			ybuniverse.KindCertificate, ybuniverse.KindUniverse)
	}

	// metadata.name and the body's own name field are different fields, and
	// nothing outside this check couples them: Render names the object from
	// the entry while every cluster lookup here reads... the entry too, so a
	// spec that spells the body field differently would have the object
	// created under one name and YBA know it under another. That is the
	// natural mistake when migrating from the old provider:/universe: blocks,
	// where the name lived in the body — and it costs the full universe
	// timeout, reporting "waiting for universe X to appear" while X is
	// healthy under the other name.
	if u := s.find(ybuniverse.KindUniverse); u != nil {
		if body, ok := u.Spec["universeName"].(string); ok && body != u.Name {
			return fmt.Errorf("YBUniverse is named %q but spec.universeName says %q; they are "+
				"different fields and both name this universe, so they have to agree",
				u.Name, body)
		}
	}
	if p := s.find(ybuniverse.KindProvider); p != nil {
		if body, ok := s.universe()["providerName"].(string); ok && body != p.Name {
			return fmt.Errorf("YBProvider is named %q but the universe's providerName says %q; "+
				"the universe would be placed against a provider that does not exist",
				p.Name, body)
		}
	}

	seen := map[string]bool{}
	for _, r := range s.Resources {
		if r.Name == "" {
			return fmt.Errorf("%s: name is required — it is the object's metadata.name, and the "+
				"API server's own complaint about an empty one names no key in this file", r.Kind)
		}
		// The universe's own defaults are written into Spec before Validate
		// runs, so emptiness cannot be the test.
		if !r.specGiven {
			return fmt.Errorf("%s/%s: spec is required — it is the CR's spec body, passed through "+
				"verbatim", r.Kind, r.Name)
		}
		// The API server accepts both and treats the second as a server-side
		// apply UPDATE of the first, so one entry silently does nothing.
		if key := r.Kind + "/" + r.Name; seen[key] {
			return fmt.Errorf("%s is declared twice: the second would be a server-side apply of "+
				"the first, so one of them would silently have no effect", key)
		} else {
			seen[key] = true
		}
	}
	return nil
}

// indexOf is where a kind first appears in document order, or len(Resources)
// when it does not appear at all.
func (s *Spec) indexOf(kind string) int {
	for i := range s.Resources {
		if s.Resources[i].Kind == kind {
			return i
		}
	}
	return len(s.Resources)
}

// validatePlacement checks universe.placementInfo against the provider.
//
// A placement naming a zone the provider does not have is a cross-reference
// between two halves of THIS file — so it is checked only when both halves are
// here, i.e. when the spec declares a YBProvider rather than naming somebody
// else's. YBA
// rejects it asynchronously, from inside the operator, as a failed action on
// the universe tens of minutes into a bring-up. That is a bad place to read
// "you typed the zone code wrong".
//
// It deliberately does not check that the per-zone counts add up to numNodes,
// nor that replicationFactor is one YBA supports. Both are YBA's own rules,
// and which values it accepts moves between releases — a copy here goes stale
// and starts rejecting specs YBA would take. Same reason the CRD schema check
// and the preview-gflag check are not here either.
func (s *Spec) validatePlacement() error {
	placement, ok := s.universe()["placementInfo"].(map[string]any)
	if !ok {
		return nil
	}
	// Only when this spec declares the provider. With a preexisting one the
	// zones live in YBA, where nothing here can see them — checking against
	// an empty set would reject every zone in the placement.
	provider := s.provider()
	if provider == nil {
		return nil
	}
	known := providerZones(provider)
	for _, r := range asList(placement["regions"]) {
		region, _ := r.(map[string]any)
		for _, z := range asList(region["zones"]) {
			zone, _ := z.(map[string]any)
			code, _ := zone["code"].(string)
			if code != "" && !known[code] {
				return fmt.Errorf("universe.placementInfo names zone %q, which the provider does not "+
					"have (it has %s): a placement can only use zones its provider defines",
					code, strings.Join(sortedKeys(known), ", "))
			}
		}
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
		if vol, ok := s.universe()[key].(map[string]any); ok {
			add(vol["storageClass"])
		}
	}
	for _, r := range asList(s.provider()["regions"]) {
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

// TelemetryProviderSecrets is every Secret a declared TelemetryProvider names,
// which must already exist for the provider to reconcile.
//
// Credentials reach YBA by reference on this kind — a Datadog apiKeySecret, an
// OTLP basicAuth passwordSecret, a bearer tokenSecret — so the CR is safe to
// keep in a spec file, and the Secret is somebody else's job. The install hint
// creates the BYOC one (byoc-metrics-auth) from a password only the operator
// has, which is also why telemetry is added to a spec AFTER the universe is up
// rather than with it.
//
// Nothing here creates one. Missing, the operator leaves the provider in Error
// with the reason only in YBA's own log, and the universe's telemetry section
// silently never reconciles — so Preflight checks instead.
//
// The walk is over spec bodies passed through verbatim, so it reads whatever
// shape a `*Secret` key happens to have rather than modelling each provider:
// a kind shipped in a future chart works the day its CRD does, which is the
// same reason Resources is untyped.
func (s *Spec) TelemetryProviderSecrets() []string {
	seen := map[string]bool{}
	for i := range s.Resources {
		if s.Resources[i].Kind != ybuniverse.KindTelemetryProvider {
			continue
		}
		collectSecretRefs(s.Resources[i].Spec, seen)
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// collectSecretRefs walks a CR body for {name, key} references under any key
// ending in "Secret" — the shape every telemetry provider's credential uses.
func collectSecretRefs(node any, seen map[string]bool) {
	switch v := node.(type) {
	case map[string]any:
		for key, child := range v {
			if strings.HasSuffix(key, "Secret") {
				if ref, ok := child.(map[string]any); ok {
					if name, ok := ref["name"].(string); ok && name != "" {
						seen[name] = true
					}
				}
			}
			collectSecretRefs(child, seen)
		}
	case []any:
		for _, child := range v {
			collectSecretRefs(child, seen)
		}
	}
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
	for _, r := range asList(s.provider()["regions"]) {
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
	for _, r := range asList(s.provider()["regions"]) {
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
