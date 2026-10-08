package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// internalError must never put the wrapped error in the client response — Go's
// net/http errors embed the full internal URL (the Langfuse ELB host and the
// /api/public/projects/<id> path), which is exactly what MEKO-812 reported
// leaking out of the datapack delete path.
func TestInternalError_HidesUpstreamErrorFromClient(t *testing.T) {
	leak := errors.New(`Delete "https://langfuse-abc.us-east-2.elb.amazonaws.com:8080` +
		`/api/public/projects/p_123": dial tcp: i/o timeout`)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodDelete, "/api/v1/datapacks/x", nil), rec)

	if err := internalError(c, leak, "delete the datapack", "datapack_id", "x"); err != nil {
		t.Fatalf("internalError returned a raw error: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Could not delete the datapack. Please try again.") {
		t.Errorf("want the generic message, got %s", body)
	}
	for _, leaked := range []string{"elb.amazonaws.com", "/api/public/projects", "dial tcp"} {
		if strings.Contains(body, leaked) {
			t.Errorf("response leaked upstream detail %q: %s", leaked, body)
		}
	}
}
