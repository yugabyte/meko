package main

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// The chart sets these from the same values the collectors render from, so the
// agent cannot attest to something the collectors are not doing.
func TestTelemetryFromEnv(t *testing.T) {
	withEnv(t, map[string]string{
		envOtelEnabled:     "true",
		envByocEnabled:     "true",
		envMetricsEndpoint: "https://byoc.cloud.yugabyte.com/api/v1/otlp/metrics",
		envLogsSink:        "datadog",
	})
	tel := telemetryFromEnv()
	if tel == nil {
		t.Fatal("a configured deployment attested nothing")
	}
	if !tel.OtelEnabled || !tel.ByocEnabled || tel.LogsSink != "datadog" {
		t.Errorf("attestation does not match the env: %+v", tel)
	}
	if tel.MetricsEndpoint != "https://byoc.cloud.yugabyte.com/api/v1/otlp/metrics" {
		t.Errorf("endpoint did not survive: %q", tel.MetricsEndpoint)
	}
}

// nil, not a zero value: a deployment whose chart sets none of this is an older
// release, and the control plane treats that as not-yet-reporting rather than as a
// deployment that positively reports telemetry off.
func TestTelemetryFromEnv_UnsetIsNil(t *testing.T) {
	if tel := telemetryFromEnv(); tel != nil {
		t.Errorf("an unconfigured deployment attested %+v", tel)
	}
}

// An honest "off" is different again: it says the collectors really are disabled,
// which the control plane can act on.
func TestTelemetryFromEnv_AttestsOffHonestly(t *testing.T) {
	withEnv(t, map[string]string{envOtelEnabled: "false", envByocEnabled: "false"})
	tel := telemetryFromEnv()
	if tel == nil {
		t.Fatal("a deployment with telemetry off attested nothing")
	}
	if tel.OtelEnabled || tel.ByocEnabled {
		t.Errorf("attested on when configured off: %+v", tel)
	}
}

// Phase 1: a Secret that exists with a blank value is the case a presence check
// misses, and it fails far less legibly later.
func TestRunPreflight_RefusesABlankPassword(t *testing.T) {
	withEnv(t, map[string]string{
		envMetricsEndpoint: "https://byoc.example/api/v1/otlp/metrics",
		envMetricsUser:     "byoc",
		envMetricsPassword: "",
	})
	err := probeTelemetry(context.Background())
	if err == nil || !strings.Contains(err.Error(), "password is empty") {
		t.Fatalf("a blank password passed the preflight: %v", err)
	}
}

// Phase 2: the door answers but rejects the credentials. That is a wrong password,
// not an outage, and the message has to say so.
func TestRunPreflight_RefusesRejectedCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	withEnv(t, map[string]string{
		envMetricsEndpoint: srv.URL,
		envMetricsUser:     "byoc",
		envMetricsPassword: "wrong",
	})
	err := probeTelemetry(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rejected the credentials") {
		t.Fatalf("a rejected password passed the preflight: %v", err)
	}
}

// The door is fronted by an nginx that returns 415 without gzip, so the probe has
// to send what the collectors send — otherwise it would pass against a door the
// collectors cannot actually use.
func TestRunPreflight_ProbeIsGzippedAndAuthenticated(t *testing.T) {
	var gotEncoding, gotUser string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Content-Encoding")
		gotUser, _, _ = r.BasicAuth()
		if zr, err := gzip.NewReader(r.Body); err == nil {
			gotBody, _ = io.ReadAll(zr)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	withEnv(t, map[string]string{
		envMetricsEndpoint: srv.URL,
		envMetricsUser:     "byoc",
		envMetricsPassword: "s3cret",
		envCollectorURLs:   "",
	})
	if err := probeTelemetry(context.Background()); err != nil {
		t.Fatalf("preflight failed against an open door: %v", err)
	}
	if gotEncoding != "gzip" {
		t.Errorf("probe was not gzipped (%q); the BYOC ingress returns 415 for that", gotEncoding)
	}
	if gotUser != "byoc" {
		t.Errorf("probe did not authenticate as the configured user: %q", gotUser)
	}
	if !strings.Contains(string(gotBody), "resourceMetrics") {
		t.Errorf("probe body was not an OTLP payload: %q", gotBody)
	}
}

// Phase 3 is what separates "configured correctly" from "working": the door can be
// open while the collectors never schedule, never scrape, or drop everything.
func TestAwaitExport_PassesOnceTheExporterHasSent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `# HELP otelcol_exporter_sent_metric_points points sent`)
		fmt.Fprintln(w, `otelcol_exporter_sent_metric_points{exporter="otlphttp/byoc",service_name="x"} 412`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := awaitExport(ctx, []string{srv.URL}); err != nil {
		t.Fatalf("a collector that has exported was not accepted: %v", err)
	}
}

// Zero sent and a rising failure count is the credentials-or-ingress case, and the
// message has to distinguish it from "nothing has started yet".
func TestAwaitExport_ReportsFailedSendsSeparately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `otelcol_exporter_sent_metric_points{exporter="otlphttp/byoc"} 0`)
		fmt.Fprintln(w, `otelcol_exporter_send_failed_metric_points{exporter="otlphttp/byoc"} 97`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := awaitExport(ctx, []string{srv.URL})
	if err == nil {
		t.Fatal("a collector exporting nothing was accepted")
	}
	if !strings.Contains(err.Error(), "97 send(s) FAILED") {
		t.Errorf("the failure count did not reach the operator: %v", err)
	}
}

// Another exporter's counters must not be read as the byoc one's — a healthy
// in-cluster remote-write would otherwise mask a byoc door that never worked.
func TestAwaitExport_IgnoresOtherExporters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `otelcol_exporter_sent_metric_points{exporter="prometheus_remote_write"} 9000`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := awaitExport(ctx, []string{srv.URL}); err == nil {
		t.Fatal("another exporter's counter was read as the byoc one's")
	}
}

// A labelled sample with no value after the closing brace yields no fields at all.
// Indexing that unguarded panicked the preflight — and so failed the install — on
// malformed exposition from a collector we do not control.
func TestParseSample_SurvivesMalformedLines(t *testing.T) {
	for _, line := range []string{
		`otelcol_exporter_sent_metric_points{exporter="otlphttp/byoc"}`,
		`otelcol_exporter_sent_metric_points{exporter="otlphttp/byoc"}   `,
		`otelcol_exporter_sent_metric_points{exporter="otlphttp/byoc"} notanumber`,
		`no_braces_at_all 12`,
		`}{`,
		``,
	} {
		if _, _, ok := parseSample(line); ok {
			t.Errorf("parseSample(%q) reported success on a line it cannot parse", line)
		}
	}

	name, v, ok := parseSample(`otelcol_exporter_sent_metric_points{exporter="otlphttp/byoc"} 412`)
	if !ok || name != "otelcol_exporter_sent_metric_points" || v != 412 {
		t.Errorf("a well-formed sample did not parse: (%q, %v, %v)", name, v, ok)
	}
}

// The preflight REPORTS and never fails an install: probeTelemetry finds the
// problem (the tests above), and runPreflight declines to turn it into a non-zero
// exit. There is no flag in between — the two are separate functions precisely so
// making it fatal later is a one-line change here rather than a new value.
func TestRunPreflight_NeverFailsTheInstall(t *testing.T) {
	withEnv(t, map[string]string{
		envMetricsEndpoint: "https://byoc.example/api/v1/otlp/metrics",
		envMetricsUser:     "byoc",
		envMetricsPassword: "", // a failure the probe definitely detects
	})

	// The probe sees it...
	if err := probeTelemetry(context.Background()); err == nil {
		t.Fatal("the probe missed a blank password, so this test proves nothing")
	}
	// ...and the install is not failed for it.
	if err := runPreflight(context.Background()); err != nil {
		t.Errorf("the preflight failed the install: %v", err)
	}
}
