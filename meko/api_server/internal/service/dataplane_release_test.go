package service

import (
	"strings"
	"testing"
)

// The pinned release is where the install hint gets the stack's image tags. A field
// with no tag is not silently wrong — tagOf renders the literal "<tag>" — but that is
// a placeholder an operator pastes and only diagnoses from an InvalidImageName pod,
// which is the class of mistake the hint exists to avoid.
func TestDefaultDataPlaneReleaseIsComplete(t *testing.T) {
	rel := DefaultDataPlaneRelease(DefaultDataPlaneRegistry)
	images := []string{
		rel.Database, rel.APIServer, rel.MCPServer,
		rel.RAGWorker, rel.InferenceGateway, rel.UI,
	}
	for _, image := range images {
		// Every reference is host-qualified today, so the qualified branch below is not
		// what fires — but a bare path stays legal in the pin, and this is what would
		// catch one. Either way the invariant is the same: the reference names a
		// registry, because an unqualified one resolves against Docker Hub and fails as
		// ImagePullBackOff minutes into a deploy.
		repo, ok := strings.CutPrefix(image, DefaultDataPlaneRegistry+"/")
		if !ok {
			host, rest, hasSlash := strings.Cut(image, "/")
			if !hasSlash || !strings.ContainsAny(host, ".:") {
				t.Errorf("image %q names no registry: it is neither qualified by %s nor host-qualified itself",
					image, DefaultDataPlaneRegistry)
				continue
			}
			repo = rest
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
	if !strings.Contains(rel.Database, "meko-yugabytedb") {
		t.Errorf("database image = %q, want the MAGE-enabled Meko YugabyteDB build", rel.Database)
	}
}

// All six are blank-checked, not just the three the hint prints (API_TAG, MCP_TAG,
// UI_TAG). The other three have no consumer today, and this is what would notice if
// the pinned release lost one before something started reading it — the check the
// deleted Complete() used to provide.
func TestDefaultDataPlaneReleaseHasNoBlankComponent(t *testing.T) {
	rel := DefaultDataPlaneRelease("")
	for name, image := range map[string]string{
		"database": rel.Database, "api_server": rel.APIServer, "mcp_server": rel.MCPServer,
		"rag_worker": rel.RAGWorker, "inference_gateway": rel.InferenceGateway, "ui": rel.UI,
	} {
		if strings.TrimSpace(image) == "" {
			t.Errorf("pinned release has no image for %s", name)
		}
	}
}
