package ycpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The hold is sent as since/wait, wait in whole seconds, and the client's own timeout is
// stretched by the wait: a 50ms client asked to hold 2s must not give up on a server
// holding 100ms.
func TestDesiredSince_StretchesTheTimeoutByTheWait(t *testing.T) {
	var gotSince, gotWait string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSince, gotWait = r.URL.Query().Get("since"), r.URL.Query().Get("wait")
		time.Sleep(100 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"change_token": 8, "long_poll": true, "resources": []any{}},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "k", &http.Client{Timeout: 50 * time.Millisecond})
	ds, err := c.DesiredSince(context.Background(), 7, 2500*time.Millisecond)
	if err != nil {
		t.Fatalf("DesiredSince: %v", err)
	}
	if gotSince != "7" || gotWait != "2" {
		t.Errorf("sent since=%q wait=%q, want 7 and 2", gotSince, gotWait)
	}
	if ds.ChangeToken != 8 || !ds.LongPoll {
		t.Errorf("decoded %+v, want change_token 8 with long_poll", ds)
	}
	// The stretch is per call: the client's other requests keep their own timeout.
	if _, err := c.Desired(context.Background()); err == nil {
		t.Error("a plain Desired outlived the client's 50ms timeout")
	}
}
