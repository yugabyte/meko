package metrics

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newEcho(m *Metrics) *echo.Echo {
	e := echo.New()
	e.Use(m.Middleware())
	e.GET("/api/v1/datapacks/:id", func(c echo.Context) error { return c.NoContent(http.StatusOK) })
	e.GET("/forbidden", func(c echo.Context) error { return echo.NewHTTPError(http.StatusForbidden) })
	e.GET("/boom", func(c echo.Context) error { return sql.ErrConnDone })
	return e
}

func serve(e *echo.Echo, path string) {
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
}

func TestMiddleware_LabelsByRouteTemplateAndStatus(t *testing.T) {
	m := New(nil, nil)
	e := newEcho(m)
	serve(e, "/api/v1/datapacks/a")
	serve(e, "/api/v1/datapacks/b")
	serve(e, "/forbidden")
	serve(e, "/boom")
	serve(e, "/no/such/route/123")

	for _, tc := range []struct {
		route, status string
		want          float64
	}{
		// Two ids, one series: the template, never the raw URI.
		{"/api/v1/datapacks/:id", "200", 2},
		{"/forbidden", "403", 1},
		{"/boom", "500", 1},
		{unmatchedRoute, "404", 1},
	} {
		got := testutil.ToFloat64(m.httpRequests.WithLabelValues(http.MethodGet, tc.route, tc.status))
		if got != tc.want {
			t.Errorf("requests_total{route=%q,status=%q} = %v, want %v", tc.route, tc.status, got, tc.want)
		}
	}
	if n := testutil.CollectAndCount(m.httpRequests); n != 4 {
		t.Errorf("requests_total has %d series, want 4 (a raw URI leaked into a label?)", n)
	}
	if v := testutil.ToFloat64(m.httpInflight); v != 0 {
		t.Errorf("requests_inflight = %v after every request returned, want 0", v)
	}
}

// The method is client-controlled: a made-up one must not mint its own series.
func TestMiddleware_ClampsUnknownMethods(t *testing.T) {
	m := New(nil, nil)
	e := newEcho(m)
	for _, method := range []string{"FLOOD0", "FLOOD1", "FLOOD2"} {
		e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, "/no/such/route", nil))
	}
	if got := testutil.ToFloat64(m.httpRequests.WithLabelValues(otherMethod, unmatchedRoute, "404")); got != 3 {
		t.Errorf("requests_total{method=OTHER} = %v, want 3", got)
	}
	if got := testutil.ToFloat64(m.httpRequests.WithLabelValues("FLOOD0", unmatchedRoute, "404")); got != 0 {
		t.Errorf("a made-up method got its own series")
	}
}

func TestNilMetricsIsANoOp(t *testing.T) {
	var m *Metrics
	serve(newEcho(m), "/api/v1/datapacks/a")
}

// The colocated layout passes one pool as both databases; reporting it twice
// would double every connection-count sum.
func TestNew_ColocatedPoolIsReportedOnce(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rec := httptest.NewRecorder()
	New(db, db).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `go_sql_open_connections{db_name="data"}`) {
		t.Fatalf("no data pool stats in /metrics:\n%s", body)
	}
	if strings.Contains(body, `db_name="system"`) {
		t.Error("the shared pool was reported a second time as system")
	}
}

func TestRAGBacklog_CountsByStatusAndCaches(t *testing.T) {
	calls := 0
	c := newRagBacklogCollector(func() (map[string]int64, error) {
		calls++
		return map[string]int64{"COMPLETED": 40, "FAILED": 2}, nil
	})
	now := time.Unix(1_000, 0)
	c.now = func() time.Time { return now }

	if got := testutil.CollectAndCount(c); got != 2 {
		t.Fatalf("collected %d series, want 2", got)
	}
	now = now.Add(30 * time.Second)
	want := `
# HELP meko_api_rag_documents ` + ragDocumentsHelp + `
# TYPE meko_api_rag_documents gauge
meko_api_rag_documents{status="COMPLETED"} 40
meko_api_rag_documents{status="FAILED"} 2
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Error(err)
	}
	if calls != 1 {
		t.Errorf("counted %d times inside the TTL, want 1", calls)
	}
	now = now.Add(time.Minute)
	testutil.CollectAndCount(c)
	if calls != 2 {
		t.Errorf("counted %d times after the TTL, want 2", calls)
	}
}

// A database without dist_rag must not produce zeros that read as "no backlog".
func TestRAGBacklog_EmitsNothingOnError(t *testing.T) {
	c := newRagBacklogCollector(func() (map[string]int64, error) {
		return nil, errors.New(`relation "dist_rag.pipeline_details" does not exist`)
	})
	if got := testutil.CollectAndCount(c); got != 0 {
		t.Errorf("collected %d series from a failed count, want 0", got)
	}
}
