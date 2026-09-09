package mekoclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func client(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, "tok", srv.Client())
}

// The api-server wraps every payload in {"data":…}. Decoding that straight into
// the payload struct yields zero values instead of an error, which is how the
// agent once enrolled with an empty environment id (meko#399) — so the unwrap is
// asserted, not assumed.
func TestListUnwrapsTheDataEnvelope(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"datapack_id":"id-1","datapack_name":"team-a"}]}`))
	})
	got, err := c.ListDatapacks(t.Context())
	if err != nil {
		t.Fatalf("ListDatapacks: %v", err)
	}
	if len(got) != 1 || got[0].ID != "id-1" || got[0].Name != "team-a" {
		t.Fatalf("ListDatapacks = %+v", got)
	}
}

// An error in the envelope is a failure whatever the status line says. Trusting
// the status alone is the other half of the same trap: a 200 carrying an error
// object would decode into an empty payload and read as an empty world.
func TestEnvelopeErrorOn200IsAnError(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":null,"error":{"code":"NOT_FOUND","message":"nope"}}`))
	})
	_, err := c.ListDatapacks(t.Context())
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *Error", err)
	}
	if apiErr.Code != "NOT_FOUND" {
		t.Errorf("code = %q, want NOT_FOUND", apiErr.Code)
	}
}

// The machine-readable code is what callers branch on, so it has to survive the
// status: a 402 and a 400 are both rejections but only one of them is permanent.
func TestErrorCarriesStatusAndCode(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"code":"FREE_TIER_DATAPACK_LIMIT","message":"upgrade"}}`))
	})
	_, err := c.CreateDatapack(t.Context(), CreateDatapackRequest{Name: "x"})
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *Error", err)
	}
	if apiErr.StatusCode != http.StatusPaymentRequired || apiErr.Code != "FREE_TIER_DATAPACK_LIMIT" {
		t.Errorf("error = %+v", apiErr)
	}
}

// A body that is not the envelope at all — a proxy's HTML error page in front of
// the api-server — must fail rather than decode to nothing.
func TestNonEnvelopeBodyIsAnError(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>502 Bad Gateway</html>`))
	})
	if _, err := c.ListDatapacks(t.Context()); err == nil {
		t.Fatal("a non-JSON body must be an error")
	}
}

// A verb with nothing to return must not demand a payload: PATCH and DELETE
// reply with an empty data field.
func TestVerbsWithNoPayloadSucceedOnAnEmptyEnvelope(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":null}`))
	})
	if err := c.UpdateDatapack(t.Context(), "id-1", UpdateDatapackRequest{Description: "d"}); err != nil {
		t.Errorf("UpdateDatapack: %v", err)
	}
	if err := c.DeleteDatapack(t.Context(), "id-1"); err != nil {
		t.Errorf("DeleteDatapack: %v", err)
	}
}
