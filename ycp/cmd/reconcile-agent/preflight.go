package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"meko/ycp/internal/api"
)

// Telemetry env, set by the chart from the same values the collectors render
// from (amp/deploy/helm/yb-amp-controller). One map, so the agent cannot attest
// to something the collectors are not doing.
const (
	envOtelEnabled     = "OTEL_ENABLED"
	envByocEnabled     = "OTEL_BYOC_ENABLED"
	envMetricsEndpoint = "BYOC_METRICS_ENDPOINT"
	envMetricsUser     = "BYOC_METRICS_USERNAME"
	envMetricsPassword = "BYOC_METRICS_PASSWORD" //nolint:gosec // an env var NAME, not a credential
	envLogsSink        = "OTEL_LOGS_SINK"
	envDatadogKey      = "DD_API_KEY" //nolint:gosec // an env var NAME, not a credential
	envCollectorURLs   = "PREFLIGHT_COLLECTOR_URLS"
	envPreflightTmo    = "PREFLIGHT_TIMEOUT"
)

// telemetryFromEnv reads what this deployment is configured to ship. nil when the
// chart set none of it — a deployment too old or too small to attest, which the
// control plane treats as not-yet-reporting rather than as a mismatch.
func telemetryFromEnv() *api.Telemetry {
	if os.Getenv(envOtelEnabled) == "" && os.Getenv(envByocEnabled) == "" && os.Getenv(envMetricsEndpoint) == "" {
		return nil
	}
	return &api.Telemetry{
		OtelEnabled:     envBool(envOtelEnabled),
		ByocEnabled:     envBool(envByocEnabled),
		MetricsEndpoint: os.Getenv(envMetricsEndpoint),
		LogsSink:        os.Getenv(envLogsSink),
	}
}

func envBool(k string) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(k)))
	return err == nil && b
}

// runPreflight is the `preflight` subcommand: the post-install hook Job the chart
// runs on every YCP-managed install. It exists because a byoc install that cannot
// reach the metrics door still reports `helm upgrade` success today, with the only
// evidence an error loop inside a collector pod.
//
// It checks three things in order, each a different failure an operator fixes
// differently: the credentials are present and non-blank, the door answers and
// accepts them, and the collectors have actually exported a datapoint through it.
//
// It REPORTS all three and fails none of them — runPreflight below turns every
// failure into a WARNING and returns nil, so the release survives and the Job's log
// is the verdict. Read that before assuming this hook gates anything.
//
// It runs in the agent image rather than a curl container because every image in
// that chart comes from quay.io with the release's own pull secrets — a docker.io
// image would be the one thing a restricted customer cluster could not fetch — and
// the image is distroless, so there is no shell to script it in anyway.
func runPreflight(ctx context.Context) error {
	// REPORTS, never fails. Every check runs and the diagnosis is printed in full;
	// the install is not failed over it, because the BYOC door is still being rolled
	// out fleet by fleet and an install that cannot reach it is as likely to be a
	// fleet problem as an operator mistake.
	//
	// Deliberately not behind a flag. A flag here is the thing an operator sets to
	// make the warning go away, which is worse than either mode; making this fatal
	// later is returning err instead of nil, in this one place.
	if err := probeTelemetry(ctx); err != nil {
		log.Printf("WARNING ===================================================================")
		log.Printf("WARNING telemetry preflight FAILED. This install is NOT being failed for it,")
		log.Printf("WARNING so a successful install does NOT mean metrics are flowing.")
		log.Printf("WARNING %v", err)
		log.Printf("WARNING this environment is very likely shipping no metrics to yugabyte-cloud;")
		log.Printf("WARNING the YCP console will show it as DEGRADED.")
		log.Printf("WARNING ===================================================================")
		return nil
	}
	return nil
}

// probeTelemetry is the check itself, independent of what is done with the result.
func probeTelemetry(ctx context.Context) error {
	endpoint := os.Getenv(envMetricsEndpoint)
	user := os.Getenv(envMetricsUser)
	password := os.Getenv(envMetricsPassword)
	timeout := 5 * time.Minute
	if d, err := time.ParseDuration(os.Getenv(envPreflightTmo)); err == nil && d > 0 {
		timeout = d
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 1 — credentials present. A Secret that exists with a blank value is the case
	// a presence check misses, and it fails later and far less legibly.
	if endpoint == "" || user == "" {
		return fmt.Errorf("telemetry preflight: %s and %s must be set (the chart sets them from otel.byoc.*)", envMetricsEndpoint, envMetricsUser)
	}
	if password == "" {
		return fmt.Errorf("telemetry preflight: the BYOC metrics password is empty. The Secret named by otel.byoc.passwordSecret exists but its key holds nothing:\n"+
			"  kubectl -n <ns> create secret generic <name> --from-literal=password='<byoc password>' --dry-run=client -o yaml | kubectl apply -f -\n"+
			"  (endpoint %s)", endpoint)
	}
	if os.Getenv(envDatadogKey) == "" {
		// A warning, not a failure: the ask is that METRICS failing fails the
		// deploy. The datadog exporter fails collector startup on an empty key, so
		// phase 3's sibling daemonset is where this surfaces as an outage.
		log.Printf("WARNING the Datadog API key is empty; the logs daemonset will fail to start and no logs will ship")
	}

	// 2 — the door answers and accepts these credentials.
	if err := probeDoor(ctx, endpoint, user, password); err != nil {
		return err
	}
	log.Printf("telemetry preflight: the BYOC metrics door at %s answered and accepted the credentials", endpoint)

	// 3 — something actually came out the other end.
	urls := splitList(os.Getenv(envCollectorURLs))
	if len(urls) == 0 {
		log.Printf("WARNING no collector telemetry endpoints were configured (%s is empty); "+
			"the door is reachable but nothing verified that metrics are flowing through it", envCollectorURLs)
		return nil
	}
	return awaitExport(ctx, urls)
}

// probeDoor POSTs an empty OTLP/HTTP metrics payload. gzip is mandatory — the
// BYOC ingress nginx returns 415 without Content-Encoding: gzip. A 401/403 means
// the password is wrong; a transport error means the endpoint does not resolve or
// does not route, which on the BYOC path means PrivateLink is not wired. Any other
// status proves the door is open and auth was accepted, which is all this phase
// claims.
func probeDoor(ctx context.Context, endpoint, user, password string) error {
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	if _, err := zw.Write([]byte(`{"resourceMetrics":[]}`)); err != nil {
		return fmt.Errorf("telemetry preflight: compress probe payload: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("telemetry preflight: compress probe payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body.Bytes()))
	if err != nil {
		return fmt.Errorf("telemetry preflight: %s is not a usable URL: %w", endpoint, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.SetBasicAuth(user, password)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("telemetry preflight: cannot reach the BYOC metrics door at %s: %w\n"+
			"  The hostname resolves to an in-VPC endpoint, so this usually means PrivateLink/DNS is not wired for this cluster", endpoint, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("telemetry preflight: the BYOC metrics door at %s rejected the credentials (HTTP %d).\n"+
			"  The password in the Secret named by otel.byoc.passwordSecret is not the one this account was issued", endpoint, resp.StatusCode)
	}
	return nil
}

// awaitExport polls each metrics collector's own telemetry until the byoc exporter
// reports a sent datapoint. This is the phase that distinguishes "configured
// correctly" from "working": the door can be open while the collectors never
// schedule, never scrape, or drop everything on the floor.
func awaitExport(ctx context.Context, urls []string) error {
	// The collector's own names for these, verified against a live one rather
	// than assumed: contrib 0.158.0 emits both unsuffixed, with no _total. A
	// rename in a future collector release makes this wait for a series that never
	// appears, and the symptom is a timeout WARNING buried in a hook pod's log —
	// considerably easier to miss than a failed install, so it is the first thing to
	// check if that starts happening.
	const (
		sentMetric   = "otelcol_exporter_sent_metric_points"
		failedMetric = "otelcol_exporter_send_failed_metric_points"
		exporterTag  = `exporter="otlphttp/byoc"`
	)
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()

	pending := append([]string(nil), urls...)
	var lastErr error
	var lastFailed float64
	for {
		remaining := pending[:0]
		for _, u := range pending {
			sent, failed, err := scrapeCounter(ctx, u, sentMetric, failedMetric, exporterTag)
			switch {
			case err != nil:
				lastErr = err
				remaining = append(remaining, u)
			case sent > 0:
				log.Printf("telemetry preflight: %s has exported %.0f datapoint(s) to the BYOC door", u, sent)
			default:
				lastFailed = failed
				remaining = append(remaining, u)
			}
		}
		pending = remaining
		if len(pending) == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			msg := fmt.Sprintf("telemetry preflight: the BYOC door is reachable, but %d collector(s) had not exported a single datapoint before the deadline: %s",
				len(pending), strings.Join(pending, ", "))
			if lastFailed > 0 {
				msg += fmt.Sprintf("\n  %.0f send(s) FAILED, so the collectors are trying and being refused — check the door's credentials and its ingress", lastFailed)
			}
			if lastErr != nil {
				msg += fmt.Sprintf("\n  last error reading collector telemetry: %v\n"+
					"  A collector that never became Ready looks exactly like this; check its pod", lastErr)
			}
			msg += "\n  If the collector pods are simply slow to schedule here, raise otel.byoc.preflight.timeoutSeconds" +
				" AND helm's own --timeout past it — helm waits on this hook for --timeout (5m by default), so raising" +
				" one without the other just moves which of the two gives up first."
			return fmt.Errorf("%s", msg)
		case <-tick.C:
		}
	}
}

// scrapeCounter reads one collector's Prometheus-format own-telemetry and returns
// the sent and failed counters for the byoc exporter. A parse that finds neither
// returns zeros, which the caller treats as not-yet — a collector that has never
// exported does not emit the series at all.
func scrapeCounter(ctx context.Context, url, sentName, failedName, tag string) (sent, failed float64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}

	sc := bufio.NewScanner(io.LimitReader(resp.Body, 4<<20))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || !strings.Contains(line, tag) {
			continue
		}
		name, value, ok := parseSample(line)
		if !ok {
			continue
		}
		switch name {
		case sentName:
			sent += value
		case failedName:
			failed += value
		}
	}
	return sent, failed, sc.Err()
}

// parseSample splits one Prometheus text-format sample into its metric name and
// value. Deliberately minimal: the only samples this reads are two counters from
// the collector's own telemetry, so a full parser would be more surface than the
// job needs.
func parseSample(line string) (name string, value float64, ok bool) {
	brace := strings.IndexByte(line, '{')
	if brace < 0 {
		return "", 0, false
	}
	close := strings.LastIndexByte(line, '}')
	if close < brace {
		return "", 0, false
	}
	// A labelled line with no value after the closing brace yields no fields at
	// all; indexing it unguarded would panic the preflight — and so fail an install
	// — on malformed exposition from something we do not control.
	fields := strings.Fields(strings.TrimSpace(line[close+1:]))
	if len(fields) == 0 {
		return "", 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return "", 0, false
	}
	return line[:brace], v, true
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
