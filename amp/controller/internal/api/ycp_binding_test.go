package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/amp/controller/internal/ycp"
)

// fakeBindingStore is an in-memory write-once binding: the first Bind wins, and
// a later differing Bind reports the incumbent with created=false, mirroring the
// SQL store's ON CONFLICT DO NOTHING.
type fakeBindingStore struct {
	cur *ycp.Binding
}

func (f *fakeBindingStore) Load(context.Context) (*ycp.Binding, error) { return f.cur, nil }

func (f *fakeBindingStore) Bind(_ context.Context, url, envID string) (*ycp.Binding, bool, error) {
	if f.cur == nil {
		f.cur = &ycp.Binding{YCPURL: url, EnvironmentID: envID}
		return f.cur, true, nil
	}
	return f.cur, false, nil
}

func bindingServer(store ycpBindingStore) *Server {
	s := &Server{echo: echo.New()}
	s.RegisterYCPBinding(store)
	return s
}

func TestYCPBinding_FirstBindWinsAndIsIdempotent(t *testing.T) {
	s := bindingServer(&fakeBindingStore{})

	// First bind succeeds.
	if rec := post(t, s, `{"ycp_url":"https://ycp.example","environment_id":"amp-x"}`); rec.Code != http.StatusOK {
		t.Fatalf("first bind: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	// Re-binding the same (url, env) is idempotent.
	if rec := post(t, s, `{"ycp_url":"https://ycp.example","environment_id":"amp-x"}`); rec.Code != http.StatusOK {
		t.Fatalf("idempotent re-bind: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestYCPBinding_ConflictOnDifferentYCP(t *testing.T) {
	s := bindingServer(&fakeBindingStore{cur: &ycp.Binding{YCPURL: "https://ycp.a", EnvironmentID: "amp-x"}})

	rec := post(t, s, `{"ycp_url":"https://ycp.b","environment_id":"amp-x"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflicting bind: got %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	// The incumbent binding is returned, not the rejected claim.
	if !strings.Contains(rec.Body.String(), "ycp.a") {
		t.Fatalf("conflict body should carry the incumbent binding, got %s", rec.Body.String())
	}
}

func TestYCPBinding_GetUnboundIs404(t *testing.T) {
	s := bindingServer(&fakeBindingStore{})

	req := httptest.NewRequest(http.MethodGet, apiBaseURL+"/ycp/binding", nil)
	rec := httptest.NewRecorder()
	s.echo.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get while unbound: got %d, want 404", rec.Code)
	}
}

func TestYCPBinding_RejectsIncompleteBody(t *testing.T) {
	s := bindingServer(&fakeBindingStore{})

	if rec := post(t, s, `{"ycp_url":"https://ycp.example"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing environment_id: got %d, want 400", rec.Code)
	}
}

func post(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, apiBaseURL+"/ycp/binding", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	s.echo.ServeHTTP(rec, req)
	return rec
}
