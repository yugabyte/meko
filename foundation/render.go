package foundation

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"meko/foundation/ybuniverse"
)

// The label every rendered object carries, and the one `down
// --delete-namespace` already checks before it will touch a namespace.
//
// It is what makes pruning safe: the delete set is drawn from a labelled
// query, so nothing this deployer did not create can be in it.
const (
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "meko-foundation"
)

// Manifest is one rendered Kubernetes object, ready to apply.
type Manifest struct {
	// Name is the file `render` writes it to, and the label an apply logs.
	Name string
	// JSON is the object. JSON rather than YAML because it is what
	// `kubectl apply` consumes over stdin, and because a map marshalled to JSON
	// is byte-stable, which is what makes the golden tests meaningful.
	JSON []byte
	// Kind is what the object is, so an apply can order and report by it.
	Kind string
	// ObjectName is metadata.name, so a log line and an error can name the
	// object rather than just its kind.
	ObjectName string
}

// String is how every log line and error names an object.
//
// Worth having because these objects are addressed on the command line by
// their manifest on stdin ("kubectl delete -f -"), which is kind-agnostic —
// SupportBundle's singular really is "support-bundle", and deriving a resource
// name from a kind string is per-kind knowledge this package does not have. So
// the argv says nothing about what is being deleted and the message has to.
func (m Manifest) String() string { return m.Kind + "/" + m.ObjectName }

// Render builds every object a foundation needs, in apply order.
//
// It performs no I/O and reaches no cluster, which is the point: the output is
// the reviewable artifact. `foundation render` writes these to files, GitOps
// applies them, and the unit tests compare them against golden files — so what
// CI checks and what a cluster receives are the same bytes.
//
// The bodies are NOT checked against the CRD's schema here. `kubectl apply`
// sends strict field validation, so the API server rejects an undeclared field,
// a wrong type and a bad enum itself — and a copy of that schema in this repo
// would have to be regenerated on every chart bump to say the same thing
// earlier. What Spec.Validate does check is the part no schema can express:
// YBA's own rules, which it enforces asynchronously inside the operator with
// nothing written back to the CR.
//
// Not included, and each for a different reason: the CRDs are cluster-scoped,
// shared by every foundation on the cluster, and fetched by version rather than
// rendered; the yugaware release is rendered by helm from its own chart; and the
// tserver Service is rendered by the chart too, from the CR's
// kubernetesOverrides.serviceEndpoints entry, selector included — so it is
// asked for here (see TServerEndpointName) but never built here.
func (s *Spec) Render() ([]Manifest, error) {
	manifests := make([]Manifest, 0, len(s.Resources))
	for i, r := range s.Resources {
		body := r.Spec
		if r.Kind == ybuniverse.KindProvider {
			var err error
			if body, err = s.providerSpec(r); err != nil {
				return nil, err
			}
		}
		cr := ybuniverse.Render(r.Kind, r.Name, s.Namespace, body)
		// The ownership boundary for pruning. Only a labelled object is a
		// candidate for deletion, so a CR somebody else put in this namespace
		// is never touched — and neither is one created before the label
		// existed, which is a gap rather than a feature: anything the spec
		// still declares is re-labelled by the next apply, but an object
		// dropped in the same edit that introduced the label never is. See
		// ARCHITECTURE.md's Pruning section.
		md := cr["metadata"].(map[string]any)
		md["labels"] = map[string]any{ManagedByLabel: ManagedByValue}
		doc, err := marshal(cr)
		if err != nil {
			return nil, fmt.Errorf("render %s %s: %w", r.Kind, r.Name, err)
		}
		manifests = append(manifests, Manifest{
			Name:       resourceFile(i, r),
			JSON:       doc,
			Kind:       r.Kind,
			ObjectName: r.Name,
		})
	}
	return manifests, nil
}

// resourceFile names the file `render -o DIR` writes and the golden the tests
// compare.
//
// Index first, because declaration order IS apply order and `ls` would
// otherwise sort alphabetically and lie about it. Name included because two
// objects of one kind are legal.
func resourceFile(i int, r Resource) string {
	return fmt.Sprintf("%d-%s-%s.json", i+1, strings.ToLower(r.Kind), r.Name)
}

// providerSpec is the provider body with this deployer's one default filled in:
// every zone gets kubeNamespace, so the universe lands in the namespace the
// spec names.
//
// Not cosmetic. With kubeNamespace unset, YBA generates a namespace of its own
// ("yb-<customer code>-<universe>-<suffix>") and puts the DB pods there — so
// the tserver Service consumers dial is in a namespace nothing predicted, and
// the single-namespace story the rest of this module documents is simply false.
// Observed on a real run: universe Ready in yb-operator-foundation-1954916679,
// and nothing that looked for it in the spec's namespace found anything.
//
// A zone that names its own is honoured: the DB then lives there, and the
// deployer follows it (Spec.UniverseNamespace). The default is co-location,
// which is what every consumer in this repo wants — amp's controller and
// endpoints share the namespace with their database.
func (s *Spec) providerSpec(r Resource) (map[string]any, error) {
	out := make(map[string]any, len(r.Spec))
	for k, v := range r.Spec {
		out[k] = v
	}
	regions := deepCopyList(asList(out["regions"]))
	for _, r := range regions {
		region, ok := r.(map[string]any)
		if !ok {
			continue
		}
		for _, z := range asList(region["zones"]) {
			zone, ok := z.(map[string]any)
			if !ok {
				continue
			}
			ci, ok := zone["cloudInfo"].(map[string]any)
			if !ok {
				ci = map[string]any{}
				zone["cloudInfo"] = ci
			}
			// A zone that names its own namespace keeps it, and the deployer
			// then does every universe-facing lookup there — see
			// Spec.UniverseNamespace. The zones have to agree on one, which
			// Validate enforces.
			setDefault(ci, "kubeNamespace", s.Namespace)
		}
	}
	if len(regions) > 0 {
		out["regions"] = regions
	}
	return out, nil
}

// deepCopyList copies a rendered list of maps deeply enough that filling in a
// default cannot mutate the caller's spec — Render must be callable twice with
// the same result, and the CLI renders then applies the same Spec.
func deepCopyList(list []any) []any {
	out := make([]any, 0, len(list))
	for _, item := range list {
		out = append(out, deepCopyValue(item))
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		copied := make(map[string]any, len(t))
		for k, val := range t {
			copied[k] = deepCopyValue(val)
		}
		return copied
	case []any:
		return deepCopyList(t)
	}
	return v
}

// marshal renders obj as indented JSON with sorted keys.
//
// encoding/json sorts map keys, which is what makes a rendered manifest
// byte-stable across runs and therefore comparable to a golden file. A gflag
// map that reordered between renders would show up as a spurious diff in
// review and in GitOps.
func marshal(obj map[string]any) ([]byte, error) {
	b, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// CRDSource is where this spec's CRD bundle comes from: the local or remote
// override when set, else the bundle for the chart version.
//
// Exported because `render` has to print the apply command it is deliberately
// not running, and printing CRDURL directly there ignored crdPath — telling
// the reader to apply a bundle the spec had overridden.
func (s *Spec) CRDSource() string {
	if s.CRDPath != "" {
		return s.CRDPath
	}
	return CRDURL(s.ChartVersion)
}

// CRDURL is where the CRD bundle for a chart version lives.
//
// raw.githubusercontent.com, not the docs' raw.github.com, which is a redirect
// kubectl will not follow. chartVersion doubles as a git ref here and as the
// helm chart version, so it has to resolve as both — a published release like
// 2026.1.1 does; a `-bNNN` build tag is a git ref only and needs chartSource
// s3 or path.
func CRDURL(chartVersion string) string {
	return fmt.Sprintf("https://raw.githubusercontent.com/yugabyte/charts/%s/crds/concatenated_crd.yaml", chartVersion)
}

// HelmSets is the --set list for the yugaware install, sorted so the argv is
// stable and diffable.
//
// These are applied AFTER yba.values' file, so they win: helm gives --set
// precedence over -f. Each is load-bearing rather than a preference, which is
// why none is left to the values file to switch off by accident:
//
//   - kubernetesOperatorEnabled is what makes this a CR-driven foundation at
//     all, and kubernetesOperatorNamespace points the operator at the release
//     namespace rather than a separate one — everything here shares one
//     namespace, which is what amp/deploy/up-gke.sh already does.
//   - defaultUser.enabled gates the chart's customer-create post-install Job,
//     and the chart defaults it OFF. Without it YBA comes up with zero
//     customers and the operator refuses every CR with "Customer list does not
//     have exactly one customer" — while leaving the CR's status EMPTY, since
//     it throws before it can write one. Setting the defaultUser fields without
//     this does nothing at all: the whole Job template is behind the flag.
//
// The values go over --set-string, so the chart sees the string "true" —
// truthy in the Go template that tests it. Note a string "false" would be
// truthy too, so this pattern only ever expresses "on".
//
// rbac.create is deliberately NOT here, though the operator does need the
// ClusterRoles it makes. It is the chart's own default, the chart already
// fails loudly when the operator is enabled with neither rbac.create nor a
// yugaware.serviceAccount, and forcing it would block the legitimate case that
// rule exists for: a cluster where you cannot create cluster-scoped RBAC and
// supply a pre-made ServiceAccount instead. So it belongs in yba.values, where
// the spec sets it explicitly and can override it.
func (s *Spec) HelmSets() map[string]string {
	return map[string]string{
		"yugaware.kubernetesOperatorEnabled":   "true",
		"yugaware.kubernetesOperatorNamespace": s.Namespace,
		"yugaware.defaultUser.enabled":         "true",
	}
}

// SortedSetArgs renders sets as helm --set-string arguments, sorted by key.
//
// --set-string rather than --set, and commas escaped: helm splits an unescaped
// comma into another assignment, so a value containing one would silently
// become two settings.
func SortedSetArgs(sets map[string]string) []string {
	keys := make([]string, 0, len(sets))
	for k := range sets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	argv := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		argv = append(argv, "--set-string", k+"="+strings.ReplaceAll(sets[k], ",", `\,`))
	}
	return argv
}
