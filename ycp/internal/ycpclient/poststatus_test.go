package ycpclient

import (
	"context"
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
