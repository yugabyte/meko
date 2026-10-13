package node

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRemoteAuth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   RemoteAuthStatus
	}{
		{"pending", http.StatusOK, `{"state":"pending","lsn":null}`, RemoteAuthStatus{State: RemoteAuthPending}},
		{"applied", http.StatusOK, `{"state":"applied","lsn":"0/1A2B3C0"}`, RemoteAuthStatus{State: RemoteAuthApplied, LSN: "0/1A2B3C0"}},
		{"applied without an lsn", http.StatusOK, `{"state":"applied","lsn":null}`, RemoteAuthStatus{State: RemoteAuthApplied}},
		{"already set", http.StatusOK, `{"state":"already_set","lsn":null}`, RemoteAuthStatus{State: RemoteAuthAlreadySet}},
		{"unset", http.StatusOK, `{"state":"unset","lsn":null}`, RemoteAuthStatus{State: RemoteAuthUnset}},
		{"failed", http.StatusOK, `{"state":"failed","lsn":null}`, RemoteAuthStatus{State: RemoteAuthFailed}},
		{"lsn omitted", http.StatusOK, `{"state":"unset"}`, RemoteAuthStatus{State: RemoteAuthUnset}},
		// Passed through: the caller decides what an unknown state means.
		{"a state from a newer supervisor", http.StatusOK, `{"state":"deferred","lsn":null,"extra":1}`, RemoteAuthStatus{State: "deferred"}},
		{"an endpoint image older than the route", http.StatusNotFound, `not found`, RemoteAuthStatus{State: RemoteAuthUnsupported}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotMethod string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotMethod = r.URL.Path, r.Method
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			got, err := NewClient().RemoteAuth(t.Context(), srv.URL+"/")
			if err != nil {
				t.Fatalf("RemoteAuth: %v", err)
			}
			if got != tc.want {
				t.Errorf("RemoteAuth = %+v, want %+v", got, tc.want)
			}
			if gotMethod != http.MethodGet || gotPath != "/v1/remote-auth" {
				t.Errorf("request = %s %s, want GET /v1/remote-auth", gotMethod, gotPath)
			}
		})
	}
}

func TestRemoteAuthErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"malformed json", http.StatusOK, `{"state":`},
		{"not json", http.StatusOK, `<html>`},
		{"no state", http.StatusOK, `{"lsn":"0/10"}`},
		{"server error", http.StatusInternalServerError, `{"state":"applied","lsn":"0/10"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			got, err := NewClient().RemoteAuth(t.Context(), srv.URL)
			if err == nil {
				t.Fatalf("RemoteAuth = %+v, want an error", got)
			}
			if errors.Is(err, ErrUnreachable) {
				t.Errorf("err = %v; a supervisor that answered is not unreachable", err)
			}
		})
	}

	t.Run("unreachable", func(t *testing.T) {
		// Port 1 needs root to bind, so nothing is listening.
		if _, err := NewClient().RemoteAuth(t.Context(), "http://127.0.0.1:1"); !errors.Is(err, ErrUnreachable) {
			t.Errorf("err = %v, want ErrUnreachable", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		old := remoteAuthTimeout
		remoteAuthTimeout = 50 * time.Millisecond
		t.Cleanup(func() { remoteAuthTimeout = old })
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		defer srv.Close()
		defer close(release)

		start := time.Now()
		_, err := NewClient().RemoteAuth(context.Background(), srv.URL)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("RemoteAuth took %s, want it bounded by remoteAuthTimeout", elapsed)
		}
	})
}
