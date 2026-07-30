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
