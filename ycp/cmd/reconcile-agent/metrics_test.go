package main

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"meko/ycp/reconcile"
)

func TestAgentMetricsRecordATick(t *testing.T) {
	m := newAgentMetrics("meko")
	k := reconcile.ResourceKey{Kind: "datapack", Name: "a/b"}
	m.tick(reconcile.Result{
		Created: 2, Deleted: 1, Deferred: 3,
		Failed: map[reconcile.ResourceKey]error{k: errors.New("x")},
		Actual: []reconcile.Actual{{Key: k}},
	}, nil, 50*time.Millisecond)
	m.tick(reconcile.Result{}, errors.New("list failed"), time.Second)

	for _, tc := range []struct {
		name string
		got  float64
		want float64
	}{
		{"ticks ok", testutil.ToFloat64(m.ticks.WithLabelValues("ok")), 1},
		{"ticks error", testutil.ToFloat64(m.ticks.WithLabelValues("error")), 1},
		{"creates", testutil.ToFloat64(m.actions.WithLabelValues("create")), 2},
		{"deletes", testutil.ToFloat64(m.actions.WithLabelValues("delete")), 1},
		// A failed tick leaves the last good reading, not zeros.
		{"failed", testutil.ToFloat64(m.resources.WithLabelValues("failed")), 1},
		{"deferred", testutil.ToFloat64(m.resources.WithLabelValues("deferred")), 3},
		{"observed", testutil.ToFloat64(m.observed), 1},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	if testutil.ToFloat64(m.lastTickOK) == 0 {
		t.Error("the successful tick did not stamp last_successful_tick_timestamp_seconds")
	}
}

func TestAgentMetricsRecordFetchesAndReports(t *testing.T) {
	m := newAgentMetrics("amp")
	m.fetched("ok")
	m.fetched("unauthorized")
	m.reported(nil)
	m.reported(errors.New("503"))
	if testutil.ToFloat64(m.desiredFetch.WithLabelValues("unauthorized")) != 1 ||
		testutil.ToFloat64(m.reports.WithLabelValues("error")) != 1 ||
		testutil.ToFloat64(m.lastReportOK) == 0 {
		t.Error("fetch/report outcomes not recorded")
	}

	// The non-converged conditions exist from the first scrape, so "none" is 0
	// rather than absent.
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(newAgentMetrics("meko").reg, promhttp.HandlerOpts{}).
		ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `ycp_agent_resources{condition="stuck"} 0`) {
		t.Errorf("stuck condition absent before any tick:\n%s", rec.Body.String())
	}
}

// Only a tick that issued something has a dispatch to measure; an idle tick's
// report would drown the boot path in near-zero samples.
func TestAgentMetricsSpanOnlyTicksThatDispatched(t *testing.T) {
	m := newAgentMetrics("amp")
	m.dispatchReported(reconcile.Result{NoOp: 4}, time.Second)
	m.dispatchReported(reconcile.Result{Created: 1}, 3*time.Second)
	m.dispatchReported(reconcile.Result{Updated: 1}, 2*time.Second)
	if got := testutil.CollectAndCount(m.dispatchSpan); got != 1 {
		t.Fatalf("dispatch span series = %d, want 1", got)
	}
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{}).
		ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "ycp_agent_dispatch_to_report_seconds_count 2") {
		t.Errorf("want 2 observations (the create and the update):\n%s", rec.Body.String())
	}
}

func TestNilAgentMetricsIsANoOp(_ *testing.T) {
	var m *agentMetrics
	m.tick(reconcile.Result{}, nil, 0)
	m.dispatchReported(reconcile.Result{Created: 1}, time.Second)
	m.fetched("ok")
	m.reported(nil)
}
