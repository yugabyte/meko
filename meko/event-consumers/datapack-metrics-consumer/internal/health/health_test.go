package health

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, srv *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestHeartbeatGoesStale(t *testing.T) {
	hb := NewHeartbeat(50 * time.Millisecond)
	if !hb.Alive() {
		t.Fatal("a new heartbeat should be alive")
	}
	time.Sleep(80 * time.Millisecond)
	if hb.Alive() {
		t.Fatal("the heartbeat should have gone stale")
	}
	hb.Beat()
	if !hb.Alive() {
		t.Fatal("a beat should revive it")
	}
}

func TestHealthzAndMetricsAreServed(t *testing.T) {
	hb := NewHeartbeat(time.Minute)
	srv := httptest.NewServer(Handler(hb))
	defer srv.Close()

	if code, _ := get(t, srv, "/healthz"); code != 200 {
		t.Fatalf("/healthz = %d", code)
	}
	code, body := get(t, srv, "/metrics")
	if code != 200 || !strings.Contains(body, "meko_events_consumer_lag_seconds") ||
		!strings.Contains(body, "meko_events_consumer_dead_letters_total") {
		t.Fatalf("/metrics = %d, missing series", code)
	}
	if code, _ := get(t, srv, "/nope"); code != 404 {
		t.Fatalf("/nope = %d", code)
	}

	stale := NewHeartbeat(-time.Second)
	srv2 := httptest.NewServer(Handler(stale))
	defer srv2.Close()
	if code, _ := get(t, srv2, "/healthz"); code != 503 {
		t.Fatalf("a stalled loop should be 503, got %d", code)
	}
}

func TestPortZeroServesNothing(t *testing.T) {
	shutdown, err := Serve(0, NewHeartbeat(time.Minute))
	if err != nil || shutdown == nil {
		t.Fatalf("shutdown nil = %v, err = %v", shutdown == nil, err)
	}
}
