package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/repository"
)

// The URL is handed to an out-of-process caller, so it has to be absolute,
// right behind the ingress, escaped, and — under --base-path — carry the
// same prefix the route it names is actually mounted at. Hardcoding
// /api/v1/... (no mountPrefix) sent that caller back to a 404 on any
// base-path deployment, the same regression class as
// TestStartOAuthLogin_RedirectURIFollowsMount in oauth_handler_test.go.
func TestDocumentStatusURL(t *testing.T) {
	const routePath = "/api/v1/datapacks/:datapack_id/knowledge-bases/upload-url"
	for _, tc := range []struct {
		name  string
		route string
		want  string
	}{
		{"root-mounted", routePath, "https://api.example.com/api/v1/datapacks/dp-1/knowledge-bases/document-status"},
		{
			"under --base-path", "/ycp" + routePath,
			"https://api.example.com/ycp/api/v1/datapacks/dp-1/knowledge-bases/document-status",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			req := httptest.NewRequest(http.MethodPost, tc.route, nil)
			req.Host = "internal:8080"
			req.Header.Set("X-Forwarded-Proto", "https")
			req.Header.Set("X-Forwarded-Host", "api.example.com")
			c := e.NewContext(req, httptest.NewRecorder())
			c.SetPath(tc.route)

			got := documentStatusURL(c, routePath, "dp-1", "u-1/dp-1/abc123/my report.pdf")
			want := tc.want + "?document_key=u-1%2Fdp-1%2Fabc123%2Fmy+report.pdf"
			if got != want {
				t.Errorf("documentStatusURL =\n  %s\nwant\n  %s", got, want)
			}
			if strings.Contains(got, "internal:8080") {
				t.Error("URL points at the pod, not the ingress")
			}
		})
	}
}

// document-status returned a bare PipelineDetail before it reported upload
// progress, so a client already reading document_status must still find it at
// the top level whenever dist_rag answers. The fields only appear then: before
// registration the old route answered 404, so there is no old shape to keep.
func TestDocumentStatusResponse_KeepsLegacyFields(t *testing.T) {
	chunks := 3
	withDocument := documentStatusResponse{
		PipelineDetail: &repository.PipelineDetail{
			IndexName:       "datapacks.knowledge_bases",
			SourceURI:       "s3://bucket/u/dp/",
			DocumentName:    "u/dp/h/a.pdf",
			DocumentStatus:  "COMPLETED",
			ChunksProcessed: &chunks,
		},
		DocumentKey: "u/dp/h/a.pdf",
		State:       "completed",
	}
	raw, err := json.Marshal(withDocument)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if uerr := json.Unmarshal(raw, &got); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	for _, field := range []string{"index_name", "source_uri", "document_name", "document_status", "chunks_processed"} {
		if _, ok := got[field]; !ok {
			t.Errorf("legacy field %q missing from %s", field, raw)
		}
	}

	raw, err = json.Marshal(documentStatusResponse{DocumentKey: "u/dp/h/a.pdf", State: "awaiting_upload"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "document_status") {
		t.Errorf("legacy fields present before registration: %s", raw)
	}
}
