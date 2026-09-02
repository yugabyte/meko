package service

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"meko/api_server/internal/models"
)

const (
	metricsAcct = "a0000000-0000-0000-0000-000000000001"
	metricsEnv  = "amp-acme-prod"
)

// fixedEnd anchors the window cases in the past, clear of the future-end clamp.
var fixedEnd = time.Unix(1_756_600_000, 0)

// overTheCeiling is maxMetricsPerRequest+1 catalog names, so one fixture serves the
// over-the-ceiling case and, truncated, the at-the-ceiling one. Derived from the
// constant so moving the ceiling cannot leave either case on the wrong side of it.
var overTheCeiling = slices.Sorted(maps.Keys(metricsCatalog))[:maxMetricsPerRequest+1]

func testEnv(accountID string) *models.Environment {
	return &models.Environment{ID: metricsEnv, AccountID: accountID}
}

// Every selector in every expression must be scoped. One that is not reads the
// whole fleet's series, which is a cross-account leak rather than a wrong chart,
// so it is worth a test that cannot be satisfied by eyeballing the catalog.
func TestCatalog_ScopesEverySelector(t *testing.T) {
	for name, expr := range metricsCatalog {
		if !strings.Contains(expr, "$FLEET") {
			t.Errorf("%s: expression has no $FLEET matchers", name)
			continue
		}
		// Each "{" opens a label matcher, and $FLEET has to be the first thing in it.
		for i, selector := range strings.Split(expr, "{")[1:] {
			if !strings.HasPrefix(selector, "$FLEET") {
				t.Errorf("%s: selector %d does not start with $FLEET: {%.40s", name, i, selector)
			}
		}
	}
}

// The console's two Endpoint-logs charts are LogsQL against VictoriaLogs. They are
// deliberately absent, so a request for one is an unknown metric rather than a
// query the store cannot parse.
func TestCatalog_OmitsLogQueries(t *testing.T) {
	for _, name := range []string{"AMP_ENDPOINT_LOG_VOLUME_PER_SEC", "AMP_ENDPOINT_ERROR_COUNT"} {
		if _, ok := metricsCatalog[name]; ok {
			t.Errorf("%s is LogsQL and must not be in the PromQL catalog", name)
		}
	}
}

func TestBuildQuery_PinsResolvedIdentity(t *testing.T) {
	got := buildQuery(metricsCatalog["AMP_CONTROLLER_TASKS_INFLIGHT"], testEnv(metricsAcct), ".*", ".*")
	want := `amp_controller_tasks_inflight{environment_id="amp-acme-prod",` +
		`account_id=~"a0000000-0000-0000-0000-000000000001",source="amp"}`
	if got != want {
		t.Errorf("query =\n%s\nwant\n%s", got, want)
	}
}

// A standalone environment's series carry no account_id at all, because the install
// hint omits otel.accountId for the sentinel. An exact match on the zeros would
// select nothing; the regex has to stay open so those series are still found.
func TestBuildQuery_SentinelAccountMatchesEverything(t *testing.T) {
	got := buildQuery(metricsCatalog["AMP_CONTROLLER_TASKS_INFLIGHT"], testEnv(SentinelAccountID), ".*", ".*")
	if !strings.Contains(got, `account_id=~".*"`) {
		t.Errorf("sentinel account must not pin account_id: %s", got)
	}
}

func TestBuildQuery_SubstitutesFilters(t *testing.T) {
	got := buildQuery(metricsCatalog["AMP_POSTGRES_UP"], testEnv(metricsAcct), "yb-ep-7", ".*")
	if !strings.Contains(got, `endpoint=~"yb-ep-7"`) {
		t.Errorf("endpoint filter missing: %s", got)
	}
	if strings.Contains(got, "$ENDPOINT") {
		t.Errorf("placeholder left unsubstituted: %s", got)
	}
}

// The ids come from our own registry, but a matcher a value can break out of is a
// query the caller rewrites, so the escaping is not left to that.
func TestBuildQuery_EscapesLabelValues(t *testing.T) {
	env := &models.Environment{ID: `a" or source="ybm`, AccountID: metricsAcct}
	got := buildQuery(metricsCatalog["AMP_CONTROLLER_TASKS_INFLIGHT"], env, ".*", ".*")
	if !strings.Contains(got, `environment_id="a\" or source=\"ybm"`) {
		t.Errorf("quotes not escaped: %s", got)
	}
}

func TestStepFor(t *testing.T) {
	end := time.Unix(1_756_600_000, 0)
	cases := []struct {
		span time.Duration
		want string
	}{
		{30 * time.Minute, "1m"},
		{6 * time.Hour, "3m"},
		{3 * 24 * time.Hour, "5m"},
		{30 * 24 * time.Hour, "15m"},
	}
	for _, tc := range cases {
		if got := stepFor(end.Add(-tc.span), end); got != tc.want {
			t.Errorf("stepFor(%s) = %s, want %s", tc.span, got, tc.want)
		}
	}
}

func TestNormalizeMetricsQuery(t *testing.T) {
	// Comma-separated and repeated forms both arrive, and duplicates collapse.
	names, _, _, err := normalizeMetricsQuery(MetricsQuery{
		Names: []string{"AMP_POSTGRES_UP,AMP_POSTGRES_CONNECTIONS", "AMP_POSTGRES_UP"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(names) != 2 || names[0] != "AMP_POSTGRES_UP" {
		t.Errorf("names = %v, want the two distinct names in request order", names)
	}

	for _, tc := range []struct {
		name  string
		query MetricsQuery
	}{
		{"no metrics", MetricsQuery{}},
		{"unknown metric", MetricsQuery{Names: []string{"NOT_A_METRIC"}}},
		{"end before start", MetricsQuery{
			Names: []string{"AMP_POSTGRES_UP"},
			Start: time.Unix(200, 0),
			End:   time.Unix(100, 0),
		}},
		{"end equals start", MetricsQuery{
			Names: []string{"AMP_POSTGRES_UP"},
			Start: time.Unix(100, 0),
			End:   time.Unix(100, 0),
		}},
		{"too many metrics", MetricsQuery{Names: overTheCeiling}},
		{"window too long", MetricsQuery{
			Names: []string{"AMP_POSTGRES_UP"},
			Start: fixedEnd.Add(-maxMetricsRange - time.Second),
			End:   fixedEnd,
		}},
		{"window too short", MetricsQuery{
			Names: []string{"AMP_POSTGRES_UP"},
			Start: fixedEnd.Add(-minMetricsRange + time.Second),
			End:   fixedEnd,
		}},
	} {
		if _, _, _, err := normalizeMetricsQuery(tc.query); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: error = %v, want ErrValidation", tc.name, err)
		}
	}
}

// The limits are YBM apiserver's, so a request the cluster metrics tab makes is one
// this route also serves. Pinned to the literals because their justification is in
// the other product, not in this file: bumping one here silently un-matches them.
func TestMetricsLimits_MatchYBM(t *testing.T) {
	if maxMetricsPerRequest != 6 {
		t.Errorf("maxMetricsPerRequest = %d, want 6", maxMetricsPerRequest)
	}
	if maxMetricsRange != 7*24*time.Hour {
		t.Errorf("maxMetricsRange = %s, want 168h", maxMetricsRange)
	}
	if minMetricsRange != 5*time.Minute {
		t.Errorf("minMetricsRange = %s, want 5m", minMetricsRange)
	}
}

// The bounds are inclusive at both ends, which is load-bearing on the upper one:
// the console's widest interval is exactly seven days, so an exclusive limit would
// reject its own default request.
func TestNormalizeMetricsQuery_BoundsAreInclusive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query MetricsQuery
	}{
		{"the metric ceiling", MetricsQuery{Names: overTheCeiling[:maxMetricsPerRequest]}},
		{"the widest window", MetricsQuery{
			Names: []string{"AMP_POSTGRES_UP"},
			Start: fixedEnd.Add(-maxMetricsRange),
			End:   fixedEnd,
		}},
		{"the narrowest window", MetricsQuery{
			Names: []string{"AMP_POSTGRES_UP"},
			Start: fixedEnd.Add(-minMetricsRange),
			End:   fixedEnd,
		}},
	} {
		if _, _, _, err := normalizeMetricsQuery(tc.query); err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
		}
	}
}

// A browser clock running ahead asks for a window partly in the future. Clamping
// beats rejecting: the response's end_timestamp reports the window served.
func TestNormalizeMetricsQuery_ClampsFutureEnd(t *testing.T) {
	before := time.Now()
	start := before.Add(-time.Hour)
	_, gotStart, gotEnd, err := normalizeMetricsQuery(MetricsQuery{
		Names: []string{"AMP_POSTGRES_UP"},
		Start: start,
		End:   before.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotEnd.Before(before) || gotEnd.After(time.Now()) {
		t.Errorf("end = %s, want it clamped to now", gotEnd)
	}
	if !gotStart.Equal(start) {
		t.Errorf("start = %s, want the requested %s", gotStart, start)
	}
}

func TestNormalizeFilter(t *testing.T) {
	if got, err := normalizeFilter("pod", ""); err != nil || got != ".*" {
		t.Errorf(`empty filter = (%q, %v), want (".*", nil)`, got, err)
	}
	for _, bad := range []string{`yb" or source="ybm`, "pod name", strings.Repeat("a", maxFilterLength+1)} {
		if _, err := normalizeFilter("pod", bad); !errors.Is(err, ErrValidation) {
			t.Errorf("filter %.20q: error = %v, want ErrValidation", bad, err)
		}
	}
}

func TestRange_NotConfigured(t *testing.T) {
	svc := NewMetricsService("")
	_, err := svc.Range(context.Background(), testEnv(metricsAcct),
		MetricsQuery{Names: []string{"AMP_POSTGRES_UP"}})
	if !errors.Is(err, ErrMetricsNotConfigured) {
		t.Errorf("error = %v, want ErrMetricsNotConfigured", err)
	}
}

func TestRange_ParsesMatrix(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{"__name__":"x","route":"/a","environment_id":"amp-acme-prod","source":"amp"},
	   "values":[[1756600000,"0.5"],[1756600060,"NaN"],[1756600120,"1.5"]]},
	  {"metric":{"route":"/b"},"values":[[1756600000,"2"]]}]}}`

	var gotQuery, gotStep string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		gotStep = r.URL.Query().Get("step")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer stub.Close()

	svc := NewMetricsService(stub.URL)
	end := time.Unix(1_756_600_120, 0)
	got, err := svc.Range(context.Background(), testEnv(metricsAcct), MetricsQuery{
		Names: []string{"AMP_CONTROLLER_HTTP_REQUESTS_BY_ROUTE"},
		Start: end.Add(-10 * time.Minute),
		End:   end,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(gotQuery, `environment_id="amp-acme-prod"`) {
		t.Errorf("query is not scoped: %s", gotQuery)
	}
	if gotStep != "1m" {
		t.Errorf("step = %s, want 1m", gotStep)
	}
	if len(got.Metrics) != 2 {
		t.Fatalf("series = %d, want 2", len(got.Metrics))
	}
	first := got.Metrics[0]
	if first.Name != "AMP_CONTROLLER_HTTP_REQUESTS_BY_ROUTE" {
		t.Errorf("name = %s, want the requested metric name", first.Name)
	}
	// NaN is the store saying "no value here": it drops out so the chart gaps,
	// and because it is not a JSON number it could not be passed through anyway.
	if len(first.Values) != 2 {
		t.Errorf("values = %v, want the NaN sample dropped", first.Values)
	}
	// The pinned matchers are the same on every series, so they are not repeated
	// back; what is left is what tells the series apart.
	if _, pinned := first.Labels["environment_id"]; pinned {
		t.Errorf("labels still carry the pinned matchers: %v", first.Labels)
	}
	if first.Labels["route"] != "/a" {
		t.Errorf("labels = %v, want route=/a", first.Labels)
	}
	if got.StartTimestamp != end.Add(-10*time.Minute).Unix() || got.EndTimestamp != end.Unix() {
		t.Errorf("window = (%d, %d), want the requested one", got.StartTimestamp, got.EndTimestamp)
	}

	// The samples serialize as [timestamp, value] pairs, which is what the
	// console's charting reads.
	encoded, err := json.Marshal(first.Values[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != "[1756600000,0.5]" {
		t.Errorf("sample = %s, want [1756600000,0.5]", encoded)
	}
}

// The store's own message is the only thing that says why a query was rejected, so
// it has to survive into the error rather than being flattened to a status code.
func TestRange_ReportsStoreError(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"execution","error":"query timed out"}`))
	}))
	defer stub.Close()

	_, err := NewMetricsService(stub.URL).Range(context.Background(), testEnv(metricsAcct),
		MetricsQuery{Names: []string{"AMP_POSTGRES_UP"}})
	if err == nil || !strings.Contains(err.Error(), "query timed out") {
		t.Errorf("error = %v, want the store's message", err)
	}
	if err == nil || !strings.Contains(err.Error(), "AMP_POSTGRES_UP") {
		t.Errorf("error = %v, want the metric name", err)
	}
}
