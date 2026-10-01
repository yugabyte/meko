package ampbackend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"meko/ycp/internal/api"
)

func catalogBackend(t *testing.T, status int, body string) *Backend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/extensions" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	b, err := New(srv.URL, "", nil, noCtl())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestExtensionCatalogMapsTheConsoleFields(t *testing.T) {
	b := catalogBackend(t, http.StatusOK, `{"total":2,"count":2,"extensions":[
		{"name":"pgvector","image":"quay.io/x/pgvector:1","builtin":true,"preload":false,"version":"0.8.0","description":"vector search"},
		{"name":"my_ext","image":"quay.io/acme/my_ext:2","builtin":false,"preload":true,"library":"my_ext"}]}`)
	got, err := b.ExtensionCatalog(context.Background())
	if err != nil {
		t.Fatalf("ExtensionCatalog: %v", err)
	}
	want := []api.Extension{
		{Name: "pgvector", Builtin: true, Version: "0.8.0", Description: "vector search"},
		{Name: "my_ext", Preload: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// An empty catalog is a fact to relay, so it must come back non-nil: nil is what
// the wire reserves for "unknown".
func TestExtensionCatalogEmptyIsNotNil(t *testing.T) {
	b := catalogBackend(t, http.StatusOK, `{"total":0,"count":0,"extensions":[]}`)
	got, err := b.ExtensionCatalog(context.Background())
	if err != nil {
		t.Fatalf("ExtensionCatalog: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got %#v, want an empty non-nil slice", got)
	}
}

// A controller that cannot serve its catalog must not read as one with no
// extensions — the console would then offer nothing instead of falling back.
func TestExtensionCatalogUnavailableIsAnError(t *testing.T) {
	b := catalogBackend(t, http.StatusServiceUnavailable,
		`{"error":{"code":"UNAVAILABLE","message":"catalog store not ready"}}`)
	got, err := b.ExtensionCatalog(context.Background())
	if err == nil {
		t.Fatalf("got %+v and no error, want an error", got)
	}
	if got != nil {
		t.Fatalf("got %+v alongside the error, want nil", got)
	}
}
