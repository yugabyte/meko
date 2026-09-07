package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// promMatrix is an empty-but-valid range response, for the cases that care about
// the query that was sent rather than what came back.
const promMatrix = `{"status":"success","data":{"resultType":"matrix","result":[]}}`

// newMetricsHandler builds the metrics handler over a mocked DB and a stub metrics
// store. queries receives every PromQL expression the store is asked for, so a test
// can assert on what the handler decided to ask — the point of the account scoping
// is what ends up in that string.
func newMetricsHandler(t *testing.T, configured bool) (
	h *EnvironmentMetricsHandler, mock sqlmock.Sqlmock, queries *[]string, cleanup func(),
) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	// Guarded: a multi-metric request queries the store from several goroutines.
	var mu sync.Mutex
	sent := []string{}
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = append(sent, r.URL.Query().Get("query"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(promMatrix))
	}))

	storeURL := ""
	if configured {
		storeURL = store.URL
	}
	envSvc := service.NewEnvironmentService(repository.NewEnvironmentRepo(db), repository.NewEnvironmentTaskRepo(db), service.BootstrapDefaults{})
	return NewEnvironmentMetricsHandler(envSvc, service.NewMetricsService(storeURL)),
		mock, &sent, func() { store.Close(); db.Close() }
}

func metricsTarget(params url.Values) string {
	return "/api/v1/environments/acme-prod/metrics?" + params.Encode()
}

// Another account's environment is a 404, exactly as it is on the sibling fleet
// routes — and the store is never asked, so a 404 cannot be told apart from a
// miss by whether any series came back.
func TestMetrics_OtherAccountIsNotFound(t *testing.T) {
	h, mock, queries, cleanup := newMetricsHandler(t, true)
	defer cleanup()

	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctB, "acme-prod").
		WillReturnRows(sqlmock.NewRows(scopeRowCols))

	params := url.Values{"metrics": {"AMP_CONTROLLER_TASKS_INFLIGHT"}}
	rec := fleetRequest(t, h.Get, http.MethodGet, metricsTarget(params), acctB, "", "acme-prod")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if len(*queries) != 0 {
		t.Errorf("the store was queried for an environment the caller cannot see: %v", *queries)
	}
}

// The tenancy matchers come from the resolved registry row. A caller passing its
// own environment_id/account_id must change nothing about what is read — that is
// what makes the account check an authorization check and not a lookup hint.
func TestMetrics_QueryIsScopedToResolvedRow(t *testing.T) {
	h, mock, queries, cleanup := newMetricsHandler(t, true)
	defer cleanup()

	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctA, "acme-prod").
		WillReturnRows(scopeRow("acme-prod", acctA))

	params := url.Values{
		"metrics":        {"AMP_CONTROLLER_TASKS_INFLIGHT"},
		"environment_id": {"someone-elses-env"},
		"account_id":     {acctB},
	}
	rec := fleetRequest(t, h.Get, http.MethodGet, metricsTarget(params), acctA, "", "acme-prod")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(*queries) != 1 {
		t.Fatalf("queries = %v, want exactly one", *queries)
	}
	query := (*queries)[0]
	for _, want := range []string{
		`environment_id="acme-prod"`,
		`account_id=~"` + acctA + `"`,
		`source="amp"`,
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query %s\nis missing %s", query, want)
		}
	}
	for _, unwanted := range []string{"someone-elses-env", acctB} {
		if strings.Contains(query, unwanted) {
			t.Errorf("query %s\nhonored a caller-supplied identity (%s)", query, unwanted)
		}
	}
}

func TestMetrics_UnknownMetricIsBadRequest(t *testing.T) {
	h, mock, _, cleanup := newMetricsHandler(t, true)
	defer cleanup()

	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctA, "acme-prod").
		WillReturnRows(scopeRow("acme-prod", acctA))

	params := url.Values{"metrics": {"AMP_ENDPOINT_LOG_VOLUME_PER_SEC"}}
	rec := fleetRequest(t, h.Get, http.MethodGet, metricsTarget(params), acctA, "", "acme-prod")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestMetrics_BadStartTimeIsBadRequest(t *testing.T) {
	h, mock, _, cleanup := newMetricsHandler(t, true)
	defer cleanup()

	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctA, "acme-prod").
		WillReturnRows(scopeRow("acme-prod", acctA))

	params := url.Values{"metrics": {"AMP_CONTROLLER_TASKS_INFLIGHT"}, "start_time": {"yesterday"}}
	rec := fleetRequest(t, h.Get, http.MethodGet, metricsTarget(params), acctA, "", "acme-prod")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// A control plane with no metrics store keeps the route and says so, rather than
// 404ing as though the environment had no metrics tab at all.
func TestMetrics_UnconfiguredStoreIsUnavailable(t *testing.T) {
	h, mock, _, cleanup := newMetricsHandler(t, false)
	defer cleanup()

	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctA, "acme-prod").
		WillReturnRows(scopeRow("acme-prod", acctA))

	params := url.Values{"metrics": {"AMP_CONTROLLER_TASKS_INFLIGHT"}}
	rec := fleetRequest(t, h.Get, http.MethodGet, metricsTarget(params), acctA, "", "acme-prod")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}
