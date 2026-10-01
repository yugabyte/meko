package ycpclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"meko/ycp/internal/api"
)

// A nil catalog must still go out as [] — null is the control plane's "never
// reported", which it rejects on this route.
func TestPutExtensionsSendsAnEmptyCatalogAsAnArray(t *testing.T) {
	var got map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/agent/extensions" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("decode body %q: %v", raw, err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := New(srv.URL, "k", nil).PutExtensions(context.Background(), api.ExtensionCatalog{}); err != nil {
		t.Fatalf("PutExtensions: %v", err)
	}
	if string(got["extensions"]) != "[]" {
		t.Fatalf("extensions = %s, want []", got["extensions"])
	}
}

func TestPutExtensionsErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusNotFound, nil}, // any error; a route this control plane lacks
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
		}))
		err := New(srv.URL, "k", nil).PutExtensions(context.Background(), api.ExtensionCatalog{})
		srv.Close()
		if err == nil {
			t.Fatalf("HTTP %d: no error", tc.status)
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Fatalf("HTTP %d: got %v, want %v", tc.status, err, tc.want)
		}
	}
}
