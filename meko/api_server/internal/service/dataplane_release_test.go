package service

import (
	"strings"
	"testing"
)

// The pinned release is what a caller gets by registering an environment with nothing
// but a name in it, so every component has to be pinned. A blank one is a data plane
// missing a component, discovered as a failed row in the console rather than here.
func TestDefaultDataPlaneReleaseIsComplete(t *testing.T) {
	rel := DefaultDataPlaneRelease(DefaultDataPlaneRegistry)
	if ok, missing := rel.Complete(); !ok {
		t.Fatalf("pinned release has no image for %s", strings.Join(missing, ", "))
	}
	images := []string{
		rel.Database, rel.APIServer, rel.MCPServer,
		rel.RAGWorker, rel.InferenceGateway, rel.UI,
	}
	for _, image := range images {
		repo, ok := strings.CutPrefix(image, DefaultDataPlaneRegistry+"/")
		if !ok {
			t.Errorf("image %q was not qualified by the registry", image)
			continue
		}
		// A reference with no tag resolves to :latest, which is the one thing a pinned
		// release must not be — the data plane would drift under its own feet.
		_, tag, tagged := strings.Cut(repo, ":")
		if !tagged || tag == "" || tag == "latest" {
			t.Errorf("image %q is not pinned to a tag", image)
		}
	}
	// The database has to be Yugabyte's Meko build: mage and pg_dist_rag are in that
	// one, and api_server's own data migrations reference the dist_rag schema.
	if !strings.Contains(rel.Database, "ybdb-meko-release") {
		t.Errorf("database image = %q, want the MAGE-enabled Meko YugabyteDB build", rel.Database)
	}
}

func TestSetImage(t *testing.T) {
	rel := DefaultDataPlaneRelease(DefaultDataPlaneRegistry)
	if err := rel.SetImage("mcp_server", "other.registry/mcp:9"); err != nil {
		t.Fatalf("SetImage: %v", err)
	}
	if rel.MCPServer != "other.registry/mcp:9" {
		t.Errorf("MCPServer = %q, want the override taken as written", rel.MCPServer)
	}
	// A misspelt component must be an error, not a silent no-op: ignoring it deploys
	// the pinned image while the operator believes they replaced it.
	err := rel.SetImage("mcp-server", "x:1")
	if err == nil {
		t.Fatal("SetImage accepted an unknown component")
	}
	// The message has to list the names, since the hyphen/underscore mix-up is the
	// mistake anyone will actually make.
	if !strings.Contains(err.Error(), "mcp_server") {
		t.Errorf("error %q does not name the valid components", err)
	}
}

func TestQualifyImage(t *testing.T) {
	const reg = "161908593886.dkr.ecr.us-east-2.amazonaws.com"
	cases := map[string]string{
		// Bare repository paths get the registry.
		"meko/meko-ui:1.0":                  reg + "/meko/meko-ui:1.0",
		"yugabyte-db/ybdb-meko-release:2.0": reg + "/yugabyte-db/ybdb-meko-release:2.0",
		"postgres:16":                       reg + "/postgres:16",
		// Anything already naming a host is left alone, so one override can point at a
		// different registry entirely.
		"other.example.com/meko/ui:1": "other.example.com/meko/ui:1",
		"localhost/meko/ui:1":         "localhost/meko/ui:1",
		"registry:5000/meko/ui:1":     "registry:5000/meko/ui:1",
		"":                            "",
	}
	for in, want := range cases {
		if got := qualifyImage(reg, in); got != want {
			t.Errorf("qualifyImage(%q) = %q, want %q", in, got, want)
		}
	}
	// No registry configured leaves the reference untouched — a kind cluster with
	// side-loaded images has no registry to name.
	if got := qualifyImage("", "meko/ui:1"); got != "meko/ui:1" {
		t.Errorf("qualifyImage with no registry = %q, want the bare path", got)
	}
}
