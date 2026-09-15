package service

import (
	"strings"
)

// The Meko release a data plane runs, and the one place it is pinned.
//
// The control plane decides the version, not the operator. A data plane's workloads
// come from its chart, so this is not written into desired state; it is what the
// install hint on GET /environments/{id} pins the chart's per-component image tags to,
// so the operator installs a set the control plane says goes together rather than six
// tags they had to pick themselves.
//
// To ship a new data-plane release, bump the tags in pinnedDataPlaneRelease below.
// Hints rendered afterwards carry the new set; an already-installed release keeps what
// it was installed with until someone upgrades the chart.

// DefaultDataPlaneRegistry is the Meko ECR, kept for the deprecated
// --dataplane-registry flag and for tests. It qualifies nothing in the pin any more:
// every reference there is host-qualified on Quay, so mirroring the images elsewhere
// means editing the pin rather than passing a registry.
const DefaultDataPlaneRegistry = "161908593886.dkr.ecr.us-east-2.amazonaws.com"

// DataPlaneRelease is one image reference per data-plane component. The JSON tags are
// the retired meko-stack spec's flat component names, not chart values — the chart
// nests image.repository/image.tag under a differently-named key per component. The
// install hint maps between them.
type DataPlaneRelease struct {
	Database         string `json:"database,omitempty"`
	APIServer        string `json:"api_server,omitempty"`
	MCPServer        string `json:"mcp_server,omitempty"`
	RAGWorker        string `json:"rag_worker,omitempty"`
	InferenceGateway string `json:"inference_gateway,omitempty"`
	UI               string `json:"ui,omitempty"`
}

// pinnedDataPlaneRelease is the release this control plane hands out.
//
// The database is Yugabyte's Meko build rather than a public YugabyteDB: MAGE and
// pg_dist_rag are in this one, and the stack is built on both — api_server's own data
// migrations reference the dist_rag schema, so a data plane on a stock image fails its
// first migration.
//
// Only APIServer, MCPServer and UI are rendered: the meko install hint prints one tag
// each, and a BYOC release runs nothing else — values-byoc.yaml disables the inference
// gateway, the RAG worker, langfuse and the provisioner. RAGWorker and InferenceGateway
// are carried at the same release for when it does not, and Database is read by nothing
// at all, the chart owning that image. Wiring those up or deleting them is
// https://github.com/yugabyte/meko/issues/1319.
var pinnedDataPlaneRelease = DataPlaneRelease{
	// Mirrored to Quay by meko/jenkins/scripts/mirror-database-image.sh rather than
	// built: it is a YugabyteDB release artifact carrying MAGE and pg_dist_rag, and a
	// stock image fails the first data migration. Its tag follows YugabyteDB's cadence,
	// not this repo's, so it moves independently of the release below.
	Database: "quay.io/yugabyte/meko-yugabytedb:2026.1.0.0-b1-meko-2",

	// One release, built by meko/jenkins on 2026-09-12 from 59d52fac1c. Host-qualified
	// like the database above, so qualifyImage leaves every reference alone and the
	// registry argument decides nothing.
	APIServer:        "quay.io/yugabyte/meko-api-server:0.260912.0-59d52fac1",
	MCPServer:        "quay.io/yugabyte/meko-mcp-server:0.260912.0-59d52fac1",
	UI:               "quay.io/yugabyte/meko-ui:0.260912.0-59d52fac1",
	RAGWorker:        "quay.io/yugabyte/meko-rag-agent:0.260912.0-59d52fac1",
	InferenceGateway: "quay.io/yugabyte/meko-inference-gateway:0.260912.0-59d52fac1",
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

// DefaultDataPlaneRelease is the pinned release, with any bare reference qualified by
// registry. Every reference is host-qualified today, so this is a no-op for all of
// them; it stays because a bare path is still legal in the pin and would otherwise
// render as a Docker Hub reference. The one production caller passes "".
func DefaultDataPlaneRelease(registry string) DataPlaneRelease {
	out := pinnedDataPlaneRelease
	for _, field := range out.dataPlaneComponents() {
		*field = qualifyImage(registry, *field)
	}
	return out
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
