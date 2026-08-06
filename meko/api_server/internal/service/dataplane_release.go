package service

import (
	"fmt"
	"sort"
	"strings"
)

// The Meko release a data plane runs, and the one place it is pinned.
//
// The control plane decides the version, not the caller. Registering an environment is
// a `POST /api/v1/environments` with a name in it; everything a data plane needs to
// come up — six image references, the Secret names to reach them and to configure them
// — is resolved here and written into that environment's desired state. A caller who
// had to supply image tags would need to know which six are compatible with each other,
// which is precisely the knowledge a control plane exists to hold.
//
// To ship a new data-plane release, bump the tags in pinnedDataPlaneRelease below. That
// is the whole change: every environment registered afterwards gets the new set, and
// existing ones keep what they were registered with until an operator replaces their
// desired state.

// DefaultDataPlaneRegistry is where the Meko images live. Every reference in the
// pinned release is a bare repository path qualified by this, so a deployment mirroring
// the images somewhere else changes one flag instead of six.
const DefaultDataPlaneRegistry = "161908593886.dkr.ecr.us-east-2.amazonaws.com"

// DataPlaneRelease is one image reference per data-plane component. The JSON tags are
// the agent's meko-stack `images` object (ycp/cmd/agent/mekostack.go), so this struct
// is serialized into desired state as-is.
type DataPlaneRelease struct {
	Database         string `json:"database,omitempty"`
	APIServer        string `json:"api_server,omitempty"`
	MCPServer        string `json:"mcp_server,omitempty"`
	RAGWorker        string `json:"rag_worker,omitempty"`
	InferenceGateway string `json:"inference_gateway,omitempty"`
	UI               string `json:"ui,omitempty"`
}

// pinnedDataPlaneRelease is the release this control plane installs.
//
// The database is Yugabyte's Meko build rather than a public YugabyteDB: MAGE and
// pg_dist_rag are in this one, and the stack is built on both — api_server's own data
// migrations reference the dist_rag schema, so a data plane on a stock image fails its
// first migration.
//
// The api-server tag is a commit build because nothing in the repo publishes
// meko/meko-api-server from main — it is the one YCP component whose image is built by
// hand, tracked in https://github.com/yugabyte/meko/issues/572. Until it publishes on
// merge, this pin is the closest thing to a release and has to be bumped by hand with
// the others, and every environment registered here depends on a tag no CI job produced.
var pinnedDataPlaneRelease = DataPlaneRelease{
	Database:         "yugabyte-db/ybdb-meko-release:2026.1.0.0-b53",
	APIServer:        "meko/meko-api-server:console-943437724",
	MCPServer:        "meko/meko-mcp-server:2.1.0-20260731-7b3fdd3",
	RAGWorker:        "meko/rag-agent:1.0.7-20260703-2669cba",
	InferenceGateway: "meko/inference-gateway:0.1.0-20260730-ae46721",
	UI:               "meko/meko-ui:1.0.0-20260729-bf56db7",
}

// dataPlaneComponents maps the name an operator writes in --dataplane-image to the
// field it sets. Keyed by the JSON name, so the flag, the desired state and the agent's
// error messages all use one spelling.
func (r *DataPlaneRelease) dataPlaneComponents() map[string]*string {
	return map[string]*string{
		"database":          &r.Database,
		"api_server":        &r.APIServer,
		"mcp_server":        &r.MCPServer,
		"rag_worker":        &r.RAGWorker,
		"inference_gateway": &r.InferenceGateway,
		"ui":                &r.UI,
	}
}

// DefaultDataPlaneRelease is the pinned release with every reference qualified by
// registry. An empty registry leaves the bare repository paths alone, which is what a
// test or a side-loaded kind cluster wants.
func DefaultDataPlaneRelease(registry string) DataPlaneRelease {
	out := pinnedDataPlaneRelease
	for _, field := range out.dataPlaneComponents() {
		*field = qualifyImage(registry, *field)
	}
	return out
}

// SetImage overrides one component's image. The component is a JSON name from the
// struct above; the image is a full reference, taken as written.
func (r *DataPlaneRelease) SetImage(component, image string) error {
	fields := r.dataPlaneComponents()
	field, ok := fields[component]
	if !ok {
		names := make([]string, 0, len(fields))
		for name := range fields {
			names = append(names, name)
		}
		sort.Strings(names)
		return fmt.Errorf("unknown data-plane component %q (want one of %s)", component, strings.Join(names, ", "))
	}
	*field = image
	return nil
}

// Complete reports whether every component has an image, and names the ones that do
// not. A data plane missing one is still worth registering — the agent brings up what
// it can and reports the rest as failed — but it is worth saying so at boot rather than
// leaving it to be discovered as an absent component in the console.
func (r DataPlaneRelease) Complete() (bool, []string) {
	var missing []string
	for name, field := range r.dataPlaneComponents() {
		if strings.TrimSpace(*field) == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return len(missing) == 0, missing
}

// qualifyImage prefixes registry onto a bare repository path.
//
// A reference that already names a host is left alone — anything with a dot or a colon
// before its first slash, plus the special case `localhost`, which is how Docker itself
// distinguishes a registry host from the first path segment of a Docker Hub repository.
// That is what lets a single override point at an entirely different registry while the
// rest of the release stays where it was.
func qualifyImage(registry, ref string) string {
	registry = strings.TrimSuffix(strings.TrimSpace(registry), "/")
	if ref == "" || registry == "" {
		return ref
	}
	// No slash at all is a bare name like "postgres"; Docker Hub's implicit library/
	// namespace is not something to guess at, so it gets qualified like any other.
	first, _, hasSlash := strings.Cut(ref, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return ref
	}
	return registry + "/" + ref
}
