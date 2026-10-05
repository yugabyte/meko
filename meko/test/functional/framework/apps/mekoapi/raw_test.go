package mekoapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRawReturnsARefusalAsAStatusAndStillTellsTheHook(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"data":null,"error":{"code":"INSUFFICIENT_SCOPE","message":"token lacks required scope \"write\""}}`))
	}))
	defer srv.Close()
	var seen []*Error
	c := New(srv.URL, WithToken("tok")).WithErrorHook(func(e *Error) { seen = append(seen, e) })

	resp, err := c.Raw(context.Background(), http.MethodPost, "/datapacks", map[string]string{"name": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusForbidden || resp.Code != "INSUFFICIENT_SCOPE" {
		t.Fatalf("got %d %q", resp.Status, resp.Code)
	}
	if len(seen) != 1 || seen[0].Path != "/datapacks" || seen[0].Status != 403 {
		t.Fatalf("hook saw %+v", seen)
	}
	resp, _ = c.WithToken("").Raw(context.Background(), http.MethodGet, "/datapacks", nil)
	if resp.Status != http.StatusUnauthorized || resp.Code != "" {
		t.Fatalf("got %d %q", resp.Status, resp.Code)
	}
}

func TestLoginAndShareDecodeTheirEnvelopes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			_, _ = w.Write([]byte(`{"data":{"access_token":"a","id_token":"i","refresh_token":"r","expires_in":3600}}`))
		case "/api/v1/datapacks/dp-1/share":
			_, _ = w.Write([]byte(`{"data":{"status":"pending","invite_id":"inv","recipient_email":"b@x.test","grant":"viewer","is_new_user":false}}`))
		case "/api/v1/datapacks/incoming-shares":
			_, _ = w.Write([]byte(`{"data":[{"invite_id":"inv","datapack_id":"dp-1","grant":"viewer"}]}`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL)
	res, err := c.Login(context.Background(), "a@x.test", "pw")
	if err != nil || res.AccessToken != "a" || res.ExpiresIn != 3600 {
		t.Fatalf("login: %+v %v", res, err)
	}
	inv, err := c.ShareDatapack(context.Background(), "dp-1", "b@x.test", GrantViewer)
	if err != nil || inv.InviteID != "inv" || inv.Grant != "viewer" {
		t.Fatalf("share: %+v %v", inv, err)
	}
	in, err := c.IncomingShares(context.Background())
	if err != nil || len(in) != 1 || in[0].DatapackID != "dp-1" {
		t.Fatalf("incoming: %+v %v", in, err)
	}
}

func TestRateLimitIsRetriedBeforeItIsReported(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":"RATE_LIMITED"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	var seen int
	c := New(srv.URL).WithErrorHook(func(*Error) { seen++ })
	if _, err := c.ListDatapacks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n != 3 || seen != 0 {
		t.Fatalf("server saw %d requests and the hook %d errors, want 3 and 0", n, seen)
	}
	resp, err := c.Raw(context.Background(), http.MethodGet, "/datapacks", nil)
	if err != nil || resp.Status != 200 {
		t.Fatalf("raw after the limiter cleared: %+v %v", resp, err)
	}
}
