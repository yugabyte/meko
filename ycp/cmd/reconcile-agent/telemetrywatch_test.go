package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"meko/ycp/internal/api"
)

// collectorStub serves the collector's own telemetry with a settable sent counter.
func collectorStub(sent, failed, queued *float64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "otelcol_exporter_sent_metric_points{exporter=\"otlphttp/byoc\"} %v\n", *sent)
		fmt.Fprintf(w, "otelcol_exporter_send_failed_metric_points{exporter=\"otlphttp/byoc\"} %v\n", *failed)
		fmt.Fprintf(w, "otelcol_exporter_queue_size{exporter=\"otlphttp/byoc\"} %v\n", *queued)
	}))
}

// The counter climbing means this environment is still shipping, so the stall
// clock keeps resetting no matter how long the agent runs.
func TestTelemetryWatch_AdvancingCounterNeverStalls(t *testing.T) {
	sent, failed, queued := 10.0, 0.0, 0.0
	srv := collectorStub(&sent, &failed, &queued)
	defer srv.Close()

	w := newTelemetryWatch(srv.URL)
	base := time.Now()
	w.Observe(context.Background(), base)

	sent = 250
	w.Observe(context.Background(), base.Add(10*time.Minute))

	got := w.Report(apiTel(), base.Add(10*time.Minute))
	if got.StalledSeconds != 0 {
		t.Errorf("a climbing counter reported a stall of %ds", got.StalledSeconds)
	}
	if got.MetricsSent != 250 {
		t.Errorf("sent counter not reported: %v", got.MetricsSent)
	}
}

// A flat counter is the whole signal: the collectors scrape on a fixed clock, so
// it climbs even for an idle environment. Flat means broken, not quiet.
func TestTelemetryWatch_FlatCounterStalls(t *testing.T) {
	sent, failed, queued := 99.0, 3.0, 42.0
	srv := collectorStub(&sent, &failed, &queued)
	defer srv.Close()

	w := newTelemetryWatch(srv.URL)
	base := time.Now()
	w.Observe(context.Background(), base)
	w.Observe(context.Background(), base.Add(12*time.Minute)) // same value

	got := w.Report(apiTel(), base.Add(12*time.Minute))
	if got.StalledSeconds < 700 {
		t.Errorf("a flat counter reported only %ds of stall", got.StalledSeconds)
	}
	if got.MetricsQueued != 42 || got.MetricsSendFailed != 3 {
		t.Errorf("diagnostic counters not carried: queued=%v failed=%v", got.MetricsQueued, got.MetricsSendFailed)
	}
}

// The FIRST reading is a baseline, not a stall — the agent may have started long
// after the collector, and cannot know how long the counter had already sat still.
func TestTelemetryWatch_FirstReadingIsABaseline(t *testing.T) {
	sent, failed, queued := 5.0, 0.0, 0.0
	srv := collectorStub(&sent, &failed, &queued)
	defer srv.Close()

	w := newTelemetryWatch(srv.URL)
	now := time.Now()
	w.Observe(context.Background(), now)

	if got := w.Report(apiTel(), now); got.StalledSeconds != 0 {
		t.Errorf("the first reading reported a stall of %ds", got.StalledSeconds)
	}
}

// An unreachable collector is "we cannot see", not "metrics have stopped" — so it
// never starts the stall clock. It is also not reported AT ALL until it persists:
// the control plane degrades an environment on a probe error, and a single
// connection reset against an in-cluster Service is not evidence of anything. A
// collector rolling (its pod restarts on any config change) would otherwise flip the
// environment to DEGRADED for a tick, which the reconcile smoke asserts against
// without a retry.
func TestTelemetryWatch_ProbeFailureIsNotAStall(t *testing.T) {
	srv := collectorStub(new(float64), new(float64), new(float64))
	srv.Close() // refuse connections

	w := newTelemetryWatch(srv.URL)
	base := time.Now()

	// A blip: observed, but not worth reporting.
	w.Observe(context.Background(), base)
	got := w.Report(apiTel(), base)
	if got.ProbeError != "" {
		t.Errorf("a single failed read was reported as a fault: %q", got.ProbeError)
	}
	if got.StalledSeconds != 0 {
		t.Errorf("a failed read started the stall clock (%ds)", got.StalledSeconds)
	}

	// Still failing well past the grace: now it is worth saying.
	w.Observe(context.Background(), base.Add(probeGrace+time.Minute))
	got = w.Report(apiTel(), base.Add(probeGrace+time.Minute))
	if got.ProbeError == "" {
		t.Error("a persistently unreachable collector was never reported")
	}
	if got.StalledSeconds != 0 {
		t.Errorf("an unreachable collector was reported as a %ds stall", got.StalledSeconds)
	}
}

// And the clock resets on recovery, so an intermittent collector does not
// accumulate its way past the grace and report an outage that never happened.
func TestTelemetryWatch_ProbeGraceResetsOnRecovery(t *testing.T) {
	sent, failed, queued := 10.0, 0.0, 0.0
	srv := collectorStub(&sent, &failed, &queued)
	defer srv.Close()

	w := newTelemetryWatch(srv.URL)
	base := time.Now()
	w.Observe(context.Background(), base) // healthy: establishes the baseline

	bad := newTelemetryWatch("http://127.0.0.1:1/metrics") // always refused
	bad.Observe(context.Background(), base)
	bad.Observe(context.Background(), base.Add(30*time.Second))
	if got := bad.Report(apiTel(), base.Add(30*time.Second)); got.ProbeError != "" {
		t.Errorf("reported inside the grace window: %q", got.ProbeError)
	}

	// A good read clears it, so the next failure starts a fresh window.
	sent = 99
	w.Observe(context.Background(), base.Add(time.Minute))
	if got := w.Report(apiTel(), base.Add(time.Minute)); got.ProbeError != "" {
		t.Errorf("a healthy read still reported a probe error: %q", got.ProbeError)
	}
}

// A deployment whose chart wires no collector endpoint reports exactly what it did
// before this existed — no runtime fields, no spurious stall.
func TestTelemetryWatch_NoURLIsInert(t *testing.T) {
	w := newTelemetryWatch("")
	cfg := apiTel()
	got := w.Report(cfg, time.Now())
	if got.StalledSeconds != 0 || got.ProbeError != "" || got.MetricsSent != 0 {
		t.Errorf("an unconfigured watch invented runtime state: %+v", got)
	}
}

// apiTel is the configuration half a real agent would have read from env.
func apiTel() *api.Telemetry {
	return &api.Telemetry{OtelEnabled: true, ByocEnabled: true, MetricsEndpoint: "https://byoc/otlp"}
}
