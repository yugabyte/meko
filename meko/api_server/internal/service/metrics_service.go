// Package service: metrics_service reads one environment's amp series out of the
// Prometheus-compatible query API the control plane points at (Thanos, in the YBM
// control plane). Read-only, and unauthenticated on the wire — the query API is
// reachable only from inside the cluster, so a NetworkPolicy is the gate and there
// is no credential to hold.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"meko/api_server/internal/models"
)

// ErrMetricsNotConfigured is returned when no query API is configured, which the
// handler maps to 503. Empty is a legitimate deployment state — a data-plane
// api-server, or a control plane brought up before the monitoring wiring — so it
// disables the route rather than failing startup.
var ErrMetricsNotConfigured = errors.New("no metrics query API is configured")

const (
	// maxMetricsPerRequest matches the ceiling YBM apiserver puts on a cluster's
	// metrics. The console asks per chart, and its widest chart names five.
	maxMetricsPerRequest = 6
	// metricsQueryConcurrency bounds how many of those run at once, so a wide
	// request is fast without handing a single caller the whole query pool.
	metricsQueryConcurrency = 4
	// defaultMetricsRange is the window used when the caller names neither end.
	defaultMetricsRange = time.Hour
	// maxMetricsRange and minMetricsRange bound the window, on YBM apiserver's
	// limits. The upper one is inclusive: the console's widest interval is
	// exactly seven days.
	maxMetricsRange = 7 * 24 * time.Hour
	minMetricsRange = 5 * time.Minute
	// metricsQueryTimeout is what we allow the store per range query; the HTTP
	// client is given longer so a slow-but-succeeding response is still read.
	metricsQueryTimeout = 25 * time.Second
	// maxFilterLength caps a caller-supplied label filter.
	maxFilterLength = 200
)

// filterPattern is what a caller may put in the endpoint/pod filters: a name, or
// a simple glob/alternation over names, and nothing that could either escape the
// matcher or hand the store a pathological regex.
var filterPattern = regexp.MustCompile(`^[A-Za-z0-9_.*|-]+$`)

// MetricsService queries the configured Prometheus-compatible store.
type MetricsService struct {
	baseURL string
	http    *http.Client
}

// NewMetricsService returns a service for the query API at baseURL (the API root,
// e.g. "http://thanos-query-frontend.monitoring-core.svc.cluster.local:9090/api/v1"
// — "/query_range" is appended per call). An empty baseURL yields a service whose
// every call reports ErrMetricsNotConfigured.
//
// The client's timeout is deliberately looser than the per-query context deadline,
// so it is the deadline that ends a slow query and the error says so.
func NewMetricsService(baseURL string) *MetricsService {
	return &MetricsService{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http:    &http.Client{Timeout: 3 * metricsQueryTimeout},
	}
}

// Configured reports whether a query API was supplied.
func (s *MetricsService) Configured() bool { return s.baseURL != "" }

// MetricsQuery is what a caller asks for. It deliberately carries no tenancy
// input: the environment those labels come from is resolved separately and passed
// to Range, so nothing here can widen or retarget the series a call can see.
type MetricsQuery struct {
	Names []string
	Start time.Time
	End   time.Time
	// Endpoint and Pod fill the endpoint/pod regex filters some expressions
	// carry. Empty means every one.
	Endpoint string
	Pod      string
}

// MetricSample is one (timestamp, value) point. It serializes as the two-element
// array the console's charting expects, matching YBM's MetricData.
type MetricSample struct {
	Timestamp int64
	Value     float64
}

// MarshalJSON renders the sample as [timestamp, value].
func (s MetricSample) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]any{s.Timestamp, s.Value})
}

// MetricSeries is one series of one metric. Several series share a Name when the
// expression aggregates by a label (by (route), by (pod), …); Labels is what tells
// them apart.
type MetricSeries struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Values []MetricSample    `json:"values"`
}

// MetricsResult is the payload of one metrics call.
type MetricsResult struct {
	Metrics        []MetricSeries `json:"metrics"`
	StartTimestamp int64          `json:"start_timestamp"`
	EndTimestamp   int64          `json:"end_timestamp"`
}

// pinnedLabels are the matchers every expression is scoped by. They are the same
// on every series a call returns, so they are dropped from the per-series labels
// rather than repeated into the response.
var pinnedLabels = map[string]bool{
	"__name__":       true,
	"environment_id": true,
	"account_id":     true,
	"source":         true,
}

// Range answers a metrics request for one environment.
//
// env is the registry row, already resolved against the calling account — that
// resolution is what authorizes the read, and the label values are taken from the
// row rather than from the request so a caller cannot ask for another
// environment's series.
func (s *MetricsService) Range(
	ctx context.Context, env *models.Environment, q MetricsQuery,
) (*MetricsResult, error) {
	if !s.Configured() {
		return nil, ErrMetricsNotConfigured
	}
	names, start, end, err := normalizeMetricsQuery(q)
	if err != nil {
		return nil, err
	}
	endpoint, err := normalizeFilter("endpoint", q.Endpoint)
	if err != nil {
		return nil, err
	}
	pod, err := normalizeFilter("pod", q.Pod)
	if err != nil {
		return nil, err
	}

	step := stepFor(start, end)
	perName := make([][]MetricSeries, len(names))
	errs := make([]error, len(names))

	var wg sync.WaitGroup
	slots := make(chan struct{}, metricsQueryConcurrency)
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			query := buildQuery(metricsCatalog[name], env, endpoint, pod)
			series, qErr := s.queryRange(ctx, query, start, end, step)
			if qErr != nil {
				errs[i] = fmt.Errorf("metric %s: %w", name, qErr)
				return
			}
			for j := range series {
				series[j].Name = name
			}
			perName[i] = series
		}(i, name)
	}
	wg.Wait()

	if joined := errors.Join(errs...); joined != nil {
		return nil, joined
	}

	// Flattened in request order, so the response is deterministic and a caller
	// can line series up with the names it asked for.
	result := &MetricsResult{
		Metrics:        make([]MetricSeries, 0, len(names)),
		StartTimestamp: start.Unix(),
		EndTimestamp:   end.Unix(),
	}
	for _, series := range perName {
		result.Metrics = append(result.Metrics, series...)
	}
	return result, nil
}

// normalizeMetricsQuery validates the names and resolves the window, defaulting to
// the last hour when the caller names neither end.
func normalizeMetricsQuery(q MetricsQuery) ([]string, time.Time, time.Time, error) {
	var names []string
	seen := map[string]bool{}
	for _, raw := range q.Names {
		// A repeated ?metrics= and a comma-separated one are both accepted, so the
		// console can send whichever its query-string builder produces.
		for _, name := range strings.Split(raw, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if _, ok := metricsCatalog[name]; !ok {
				return nil, time.Time{}, time.Time{},
					fmt.Errorf("%w: unknown metric %q", ErrValidation, name)
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, time.Time{}, time.Time{},
			fmt.Errorf("%w: at least one metric is required", ErrValidation)
	}
	if len(names) > maxMetricsPerRequest {
		return nil, time.Time{}, time.Time{},
			fmt.Errorf("%w: at most %d metrics per request, got %d",
				ErrValidation, maxMetricsPerRequest, len(names))
	}

	now := time.Now()
	end := q.End
	if end.IsZero() {
		end = now
	}
	// A clock ahead of ours asks for a window that cannot have data in it yet.
	// Clamped rather than rejected, and the response reports the window it served.
	if end.After(now) {
		end = now
	}
	start := q.Start
	if start.IsZero() {
		start = end.Add(-defaultMetricsRange)
	}
	if !start.Before(end) {
		return nil, time.Time{}, time.Time{},
			fmt.Errorf("%w: start_time must be before end_time", ErrValidation)
	}
	if span := end.Sub(start); span > maxMetricsRange {
		return nil, time.Time{}, time.Time{},
			fmt.Errorf("%w: the window cannot exceed %s, got %s",
				ErrValidation, maxMetricsRange, span.Truncate(time.Second))
	} else if span < minMetricsRange {
		return nil, time.Time{}, time.Time{},
			fmt.Errorf("%w: the window cannot be shorter than %s, got %s",
				ErrValidation, minMetricsRange, span.Truncate(time.Second))
	}
	return names, start, end, nil
}

// normalizeFilter validates a caller-supplied label filter, mapping empty to the
// match-everything regex.
func normalizeFilter(param, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ".*", nil
	}
	if len(trimmed) > maxFilterLength {
		return "", fmt.Errorf("%w: %s filter is longer than %d characters",
			ErrValidation, param, maxFilterLength)
	}
	if !filterPattern.MatchString(trimmed) {
		return "", fmt.Errorf(
			"%w: %s filter may contain only letters, digits and _.*|- characters",
			ErrValidation, param)
	}
	return trimmed, nil
}

// stepFor picks the resolution for a window, on the same schedule YBM apiserver's
// cluster metrics use, so the two products' charts thin out alike. The 15m tier is
// kept for that parity but is unreachable while maxMetricsRange is seven days.
func stepFor(start, end time.Time) string {
	switch span := end.Sub(start); {
	case span > 7*24*time.Hour:
		return "15m"
	case span > 24*time.Hour:
		return "5m"
	case span > time.Hour:
		return "3m"
	default:
		return "1m"
	}
}

// buildQuery substitutes the catalog placeholders. $FLEET is the tenancy matchers,
// built from the resolved registry row; account_id is a regex so that the
// match-everything case also selects the series of a standalone environment, which
// carry no account_id label at all (see MetricsAccountID).
//
// source is not redundant with the pair: names like
// container_memory_working_set_bytes and pg_up exist in the control plane's store
// from other producers, and without it a query would silently mix them in.
func buildQuery(expr string, env *models.Environment, endpoint, pod string) string {
	account := MetricsAccountID(env.AccountID)
	if account == "" {
		account = ".*"
	}
	fleet := fmt.Sprintf(`environment_id="%s",account_id=~"%s",source="%s"`,
		escapeLabelValue(env.ID), escapeLabelValue(account), MetricsSource(env.Kind))

	expr = strings.ReplaceAll(expr, "$FLEET", fleet)
	expr = strings.ReplaceAll(expr, "$ENDPOINT", escapeLabelValue(endpoint))
	return strings.ReplaceAll(expr, "$POD", escapeLabelValue(pod))
}

// escapeLabelValue makes a value safe inside a double-quoted PromQL matcher. The
// ids come from our own registry and the filters are charset-validated, so this is
// belt and braces — but a matcher a value can break out of is a query a caller
// could rewrite, so it is not left to those two guarantees.
func escapeLabelValue(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
}

// promResponse is the subset of the Prometheus query API's envelope we read.
type promResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string    `json:"metric"`
			Values [][2]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// queryRange runs one range query and maps its matrix to series.
func (s *MetricsService) queryRange(
	ctx context.Context, query string, start, end time.Time, step string,
) ([]MetricSeries, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("start", strconv.FormatInt(start.Unix(), 10))
	params.Set("end", strconv.FormatInt(end.Unix(), 10))
	params.Set("step", step)
	params.Set("timeout", metricsQueryTimeout.String())

	// The store is given metricsQueryTimeout; the request gets more, so a large
	// but successful response is still read off the wire rather than cut short.
	ctx, cancel := context.WithTimeout(ctx, 2*metricsQueryTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.baseURL+"/query_range?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		// The store's own error text is the only thing that says why a query was
		// rejected, so it is worth carrying up rather than a bare status.
		return nil, fmt.Errorf("metrics store returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed promResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("metrics store returned an unreadable body: %w", err)
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("metrics store reported %s: %s", parsed.ErrorType, parsed.Error)
	}

	series := make([]MetricSeries, 0, len(parsed.Data.Result))
	for _, raw := range parsed.Data.Result {
		labels := map[string]string{}
		for k, v := range raw.Metric {
			if !pinnedLabels[k] {
				labels[k] = v
			}
		}
		if len(labels) == 0 {
			labels = nil
		}
		series = append(series, MetricSeries{Labels: labels, Values: parseSamples(raw.Values)})
	}
	return series, nil
}

// parseSamples converts the wire form ([<epoch float>, "<value>"]) to samples,
// dropping any point that is not a real number. NaN and ±Inf are how the store
// reports "no value here"; a chart wants a gap, and they are not valid JSON
// numbers, so they cannot be passed through even if it did not.
func parseSamples(raw [][2]json.RawMessage) []MetricSample {
	samples := make([]MetricSample, 0, len(raw))
	for _, point := range raw {
		var at float64
		if err := json.Unmarshal(point[0], &at); err != nil {
			continue
		}
		var text string
		if err := json.Unmarshal(point[1], &text); err != nil {
			continue
		}
		value, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		samples = append(samples, MetricSample{Timestamp: int64(at), Value: value})
	}
	return samples
}
