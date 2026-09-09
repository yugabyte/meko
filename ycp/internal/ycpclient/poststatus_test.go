package ycpclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"meko/ycp/internal/api"
)

// TestPostStatusReportsWhetherTasksLanded covers the signal the two-phase task
// commit depends on. The agent retires a delta only when this says the control plane
// stored it; a wrong answer here either loses transitions for good (false positive)
// or stalls the watermark forever (false negative).
func TestPostStatusReportsWhetherTasksLanded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		want    bool
		wantErr bool
		why     string
	}{
		{
			name: "204 (the common path)", status: http.StatusNoContent, body: "", want: true,
			why: "a 204 is a clean accept; holding the delta would stall the watermark",
		},
		{
			name:   "200 with tasks_recorded=false",
			status: http.StatusOK, body: `{"data":{"tasks_recorded":false}}`, want: false,
			why: "the control plane took the report but stored no tasks, so the delta is still ours",
		},
		{
			name:   "200 with tasks_recorded=true",
			status: http.StatusOK, body: `{"data":{"tasks_recorded":true}}`, want: true,
			why: "an explicit accept",
		},
		{
			name:   "200 with no such field (a control plane predating task reporting)",
			status: http.StatusOK, body: `{"data":{}}`, want: true,
			why: "defaulting to hold would wedge the agent against a control plane that will " +
				"never store tasks: it would re-send the same delta forever",
		},
		{
			name:   "500",
			status: http.StatusInternalServerError, body: `{"error":{"code":"INTERNAL_ERROR"}}`,
			want: false, wantErr: true,
			why: "the report did not land at all",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.body != "" {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := New(srv.URL, "", nil)
			accepted, err := c.PostStatus(context.Background(), api.StatusReport{})
			if tc.wantErr && err == nil {
				t.Fatal("PostStatus returned nil error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("PostStatus: %v", err)
			}
			if accepted != tc.want {
				t.Errorf("tasksAccepted = %v, want %v — %s", accepted, tc.want, tc.why)
			}
		})
	}
}

// A 409 is a kind mismatch only when the control plane says so. The status alone
// is ambiguous on the path an agent's call takes — a proxy or YBM's pass-through
// can answer 409 too — and the agent treats a mismatch as permanently fatal, so
// matching on the bare status turns a transient failure into a dead pod.
func TestEnrollTreatsOnlyTheKindMismatchCodeAsFatal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		wantKind bool
	}{
		{"the control plane's own refusal", `{"error":{"code":"KIND_MISMATCH","message":"registered as meko"}}`, true},
		{"someone else's 409", `{"error":{"code":"CONFLICT","message":"upstream"}}`, false},
		{"a body that is not the envelope", `<html>409</html>`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := New(srv.URL, "key", srv.Client()).Enroll(t.Context(), api.EnrollRequest{Name: "acme"})
			if got := errors.Is(err, ErrKindMismatch); got != tc.wantKind {
				t.Errorf("ErrKindMismatch = %v, want %v (err = %v)", got, tc.wantKind, err)
			}
			if err == nil {
				t.Error("a 409 must always be an error")
			}
		})
	}
}
