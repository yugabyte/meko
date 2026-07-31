package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/config"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/metrics"
)

// TestMetricsEndpoint checks that, with a registry wired in, the server exposes
// /metrics in Prometheus text format carrying the runtime, build-info, and
// per-request series — and that a handled request is reflected in the HTTP
// counter under its route template.
func TestMetricsEndpoint(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := api.NewServer(&config.Config{ListenAddr: ":0"}, core.NewMemBackend(), newFakeTaskService(), api.Provisioning{}, nil, metrics.New(), logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Drive one request so the HTTP middleware records a sample before we scrape.
	if _, _, code := get(t, ts.URL+"/healthz"); code != http.StatusOK {
		t.Fatalf("healthz status = %d", code)
	}

	body, ct, code := get(t, ts.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics status = %d", code)
	}
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("/metrics content-type = %q, want text/plain (Prometheus exposition)", ct)
	}

	// Every series family should be present.
	wantSubstrings := []string{
		`amp_controller_build_info{`,          // build identity
		"go_goroutines",                       // Go runtime collector
		"process_start_time_seconds",          // process collector
		"amp_controller_http_requests_total{", // request counter
		"amp_controller_tasks_inflight",       // task gauge (registered even at 0)
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics output missing %q", want)
		}
	}

	// The /healthz request must be counted under its route template and a 200,
	// and the /metrics scrape itself must be excluded.
	if !strings.Contains(body, `amp_controller_http_requests_total{method="GET",route="/healthz",status="200"}`) {
		t.Errorf("/metrics missing the /healthz request sample:\n%s", grepLines(body, "http_requests_total"))
	}
	if strings.Contains(body, `route="/metrics"`) {
		t.Errorf("the /metrics scrape endpoint should not record metrics about itself")
	}
}

// TestMetricsRecordsErrorStatus checks a failed request is recorded with the
// status the client actually receives, not 200 — exercising the HandleError
// path where slogLogger commits the mapped status before metricsMiddleware
// reads it.
func TestMetricsRecordsErrorStatus(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := api.NewServer(&config.Config{ListenAddr: ":0"}, core.NewMemBackend(), newFakeTaskService(), api.Provisioning{}, nil, metrics.New(), logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// An unmatched path returns 404 through echo's error handler.
	if _, _, code := get(t, ts.URL+"/no/such/path"); code != http.StatusNotFound {
		t.Fatalf("unmatched path status = %d, want 404", code)
	}
	body, _, _ := get(t, ts.URL+"/metrics")
	if !strings.Contains(body, `status="404"`) {
		t.Errorf("expected a 404 sample, got:\n%s", grepLines(body, "http_requests_total"))
	}
	// The failed request must not be misrecorded as 200.
	if strings.Contains(body, `route="unmatched",status="200"`) {
		t.Errorf("failed request misrecorded as 200:\n%s", grepLines(body, "http_requests_total"))
	}
}

// TestMetricsDisabled checks that a nil registry leaves /metrics unmounted
// (404) while the rest of the server keeps serving.
func TestMetricsDisabled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := api.NewServer(&config.Config{ListenAddr: ":0"}, core.NewMemBackend(), newFakeTaskService(), api.Provisioning{}, nil, nil, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	if _, _, code := get(t, ts.URL+"/metrics"); code != http.StatusNotFound {
		t.Fatalf("/metrics status with metrics disabled = %d, want 404", code)
	}
	if _, _, code := get(t, ts.URL+"/healthz"); code != http.StatusOK {
		t.Fatalf("healthz status = %d (server should serve normally with metrics off)", code)
	}
}

func grepLines(body, substr string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
