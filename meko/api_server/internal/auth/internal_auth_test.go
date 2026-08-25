package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

func runMiddleware(t *testing.T, secret string, header string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	e := echo.New()
	called := false
	mw := RequireInternalSecret(secret)
	h := mw(func(c echo.Context) error {
		called = true
		return c.NoContent(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/test", nil)
	if header != "" {
		req.Header.Set(InternalSecretHeader, header)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h(c); err != nil {
		t.Fatalf("middleware: %v", err)
	}
	return rec, called
}

func TestRequireInternalSecret_DisabledWhenSecretEmpty(t *testing.T) {
	rec, called := runMiddleware(t, "", "anything")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
	if called {
		t.Error("downstream handler should not be invoked when disabled")
	}
}

func TestRequireInternalSecret_RejectsMissingHeader(t *testing.T) {
	rec, called := runMiddleware(t, "s3cret", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if called {
		t.Error("downstream handler should not be invoked on missing header")
	}
}

func TestRequireInternalSecret_RejectsWrongSecret(t *testing.T) {
	rec, called := runMiddleware(t, "s3cret", "wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if called {
		t.Error("downstream handler should not be invoked on wrong secret")
	}
}

func TestRequireInternalSecret_AcceptsCorrectSecret(t *testing.T) {
	rec, called := runMiddleware(t, "s3cret", "s3cret")
	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", rec.Code)
	}
	if !called {
		t.Error("downstream handler should be invoked on correct secret")
	}
}

func TestRequireInternalSecret_RejectsLengthMismatchPrefix(t *testing.T) {
	// A constant-time comparator on different lengths must still
	// fail; this ensures we don't accidentally fall back to a
	// prefix-match shortcut.
	rec, called := runMiddleware(t, "s3cret", "s3cretextra")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if called {
		t.Error("downstream handler should not be invoked on prefix match")
	}
}

// runAPIKeyID drives RequireAPIKeyID with the given header value ("" = absent)
// and reports the response plus the id the handler saw in context.
func runAPIKeyID(t *testing.T, header string) (*httptest.ResponseRecorder, string, bool) {
	t.Helper()
	e := echo.New()
	called := false
	seen := ""
	h := RequireAPIKeyID()(func(c echo.Context) error {
		called = true
		seen = GetAPIKeyID(c)
		return c.NoContent(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/status", nil)
	if header != "" {
		req.Header.Set(APIKeyIDHeader, header)
	}
	rec := httptest.NewRecorder()
	if err := h(e.NewContext(req, rec)); err != nil {
		t.Fatalf("middleware: %v", err)
	}
	return rec, seen, called
}

// An absent key id is an unauthenticated agent call: 401, not 400 — the header IS
// the caller's identity on the agent surface.
func TestRequireAPIKeyID_MissingIs401(t *testing.T) {
	rec, _, called := runAPIKeyID(t, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if called {
		t.Error("downstream handler should not run without a key id")
	}
}

// api_key_id is a UUID column, so a garbled header is refused at the edge (400)
// rather than reaching the query as a 500.
func TestRequireAPIKeyID_MalformedIs400(t *testing.T) {
	rec, _, called := runAPIKeyID(t, "not-a-uuid")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
	if called {
		t.Error("downstream handler should not run on a malformed key id")
	}
}

func TestRequireAPIKeyID_StoresIDInContext(t *testing.T) {
	const id = "b0000000-0000-0000-0000-00000000000a"
	rec, seen, called := runAPIKeyID(t, id)
	if rec.Code != http.StatusNoContent || !called {
		t.Fatalf("expected the handler to run, got %d", rec.Code)
	}
	if seen != id {
		t.Errorf("GetAPIKeyID = %q, want %q", seen, id)
	}
}
