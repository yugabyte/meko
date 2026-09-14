package foundation

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"meko/foundation/ybuniverse"
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
}

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
	var manifests []Manifest

	// The provider first: the universe references it by name, and YBA cannot
	// place a universe against a provider that does not exist yet.
	if len(s.Provider) > 0 {
		name, _ := s.Provider["name"].(string)
		provider, err := s.providerSpec()
		if err != nil {
			return nil, err
		}
		cr := ybuniverse.Render(ybuniverse.KindProvider, name, s.Namespace, provider)
		body, err := marshal(cr)
		if err != nil {
			return nil, fmt.Errorf("render YBProvider: %w", err)
		}
		manifests = append(manifests, Manifest{Name: "ybprovider.json", JSON: body, Kind: ybuniverse.KindProvider})
	}

	universe := ybuniverse.Render(ybuniverse.KindUniverse, s.UniverseName(), s.Namespace, s.universeSpec())
	body, err := marshal(universe)
	if err != nil {
		return nil, fmt.Errorf("render YBUniverse: %w", err)
	}
	return append(manifests, Manifest{Name: "ybuniverse.json", JSON: body, Kind: ybuniverse.KindUniverse}), nil
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
func (s *Spec) providerSpec() (map[string]any, error) {
	out := make(map[string]any, len(s.Provider))
	for k, v := range s.Provider {
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

// universeSpec is the YBUniverse spec body.
//
// It is s.Universe as-is: the defaults this deployer owns are applied at parse
// time (Spec.universeDefaults), so there is exactly one map and no way for a
// reader of the Spec to disagree with the CR that gets applied.
func (s *Spec) universeSpec() map[string]any { return s.Universe }

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
