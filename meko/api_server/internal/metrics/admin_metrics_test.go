package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// gatherAdmin registers c in a fresh registry and returns a gather function that
// collects it on demand, keyed "name" or "name{label=value}" → value.
func gatherAdmin(t *testing.T, c prometheus.Collector) func() map[string]float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	return func() map[string]float64 {
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatalf("gather: %v", err)
		}
		out := map[string]float64{}
		for _, mf := range mfs {
			for _, m := range mf.Metric {
				key := mf.GetName()
				for _, l := range m.Label {
					key += "{" + l.GetName() + "=" + l.GetValue() + "}"
				}
				out[key] = m.GetGauge().GetValue()
			}
		}
		return out
	}
}

// syncRefresh runs one refresh synchronously and marks the cache fresh, so a
// following gather serves it without triggering the background path.
func syncRefresh(c *adminMetricsCollector) {
	c.runProbes(context.Background())
	c.mu.Lock()
	c.at = c.now()
	c.mu.Unlock()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAdminMetrics_RefreshEmitsAllAndCaches(t *testing.T) {
	var userCalls, dpCalls, tsCalls, memCalls, kbCalls int
	c := newAdminMetricsCollector(AdminBusinessSources{
		UsersByTier: func(context.Context) (int64, int64, int64, error) {
			userCalls++
			return 10, 5, 3, nil
		},
		Datapacks: func(context.Context) (int64, error) {
			dpCalls++
			return 42, nil
		},
		TokenSavings: func(context.Context) (int64, int64, error) {
			tsCalls++
			return 100, 9999, nil
		},
		Memories: func(context.Context) (int64, int64, error) {
			memCalls++
			return 7, 4, nil
		},
		KnowledgeBase: func(context.Context) (int64, int64, map[string]int64, error) {
			kbCalls++
			return 30, 480, map[string]int64{"application/pdf": 20, "text/markdown": 10}, nil
		},
	})
	now := time.Unix(1_000, 0)
	c.now = func() time.Time { return now }
	gather := gatherAdmin(t, c)

	syncRefresh(c) // one refresh populates every series

	want := map[string]float64{
		"meko_api_admin_users{tier=free}":                           10,
		"meko_api_admin_users{tier=standard}":                       5,
		"meko_api_admin_users{tier=pro}":                            3,
		"meko_api_admin_datapacks":                                  42,
		"meko_api_admin_token_savings_requests":                     100,
		"meko_api_admin_token_savings_tokens":                       9999,
		"meko_api_admin_memories{kind=private}":                     7,
		"meko_api_admin_memories{kind=promoted}":                    4,
		"meko_api_admin_kb_documents":                               30,
		"meko_api_admin_kb_chunks":                                  480,
		"meko_api_admin_kb_documents_by_type{type=application/pdf}": 20,
		"meko_api_admin_kb_documents_by_type{type=text/markdown}":   10,
	}
	got := gather()
	if len(got) != len(want) {
		t.Fatalf("got %d series, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if userCalls != 1 || dpCalls != 1 || tsCalls != 1 || memCalls != 1 || kbCalls != 1 {
		t.Errorf("one refresh should call each source once: users=%d datapacks=%d savings=%d memories=%d kb=%d",
			userCalls, dpCalls, tsCalls, memCalls, kbCalls)
	}

	// A scrape on a fresh cache serves cached values and starts no refresh.
	now = now.Add(30 * time.Second)
	gather()
	if userCalls != 1 || dpCalls != 1 || tsCalls != 1 || memCalls != 1 || kbCalls != 1 {
		t.Errorf("a fresh-cache scrape re-ran a source: users=%d datapacks=%d savings=%d memories=%d kb=%d",
			userCalls, dpCalls, tsCalls, memCalls, kbCalls)
	}
}

// A stale scrape must not run queries itself: it returns at once and refreshes in
// the background, so a slow datastore can never stall the :9464 endpoint.
func TestAdminMetrics_StaleScrapeRefreshesInBackground(t *testing.T) {
	called := make(chan struct{}, 1)
	c := newAdminMetricsCollector(AdminBusinessSources{
		Datapacks: func(context.Context) (int64, error) {
			called <- struct{}{}
			return 7, nil
		},
	})
	gather := gatherAdmin(t, c)

	// Cache is empty and stale: this scrape serves nothing and kicks off a refresh.
	if got := gather(); len(got) != 0 {
		t.Fatalf("first scrape should serve an empty cache, got %v", got)
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("a stale scrape did not start a background refresh")
	}
	waitFor(t, "the background refresh to commit", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return !c.refreshing && c.probes[0].ok
	})
	if got := gather()["meko_api_admin_datapacks"]; got != 7 {
		t.Fatalf("after the background refresh, datapacks = %v, want 7", got)
	}
}

// One datastore failing must drop only its own series — a zero would read as a
// real count, and the other probes must still report.
func TestAdminMetrics_FailedProbeIsolated(t *testing.T) {
	c := newAdminMetricsCollector(AdminBusinessSources{
		UsersByTier: func(context.Context) (int64, int64, int64, error) {
			return 10, 5, 3, nil
		},
		Datapacks: func(context.Context) (int64, error) {
			return 0, errors.New(`relation "meko_system.datapacks" does not exist`)
		},
		Memories: func(context.Context) (int64, int64, error) {
			return 7, 4, nil
		},
	})
	syncRefresh(c)
	got := gatherAdmin(t, c)()
	if _, ok := got["meko_api_admin_datapacks"]; ok {
		t.Error("a failed datapacks probe emitted a series instead of staying absent")
	}
	if got["meko_api_admin_users{tier=free}"] != 10 || got["meko_api_admin_memories{kind=promoted}"] != 4 {
		t.Errorf("a sibling probe's failure dropped healthy series: %v", got)
	}
}

// Only the non-nil sources become probes; an all-nil set registers nothing.
func TestAdminMetrics_OnlyNonNilSources(t *testing.T) {
	if n := len(newAdminMetricsCollector(AdminBusinessSources{}).probes); n != 0 {
		t.Errorf("empty sources made %d probes, want 0", n)
	}
	c := newAdminMetricsCollector(AdminBusinessSources{
		Datapacks: func(context.Context) (int64, error) { return 1, nil },
	})
	if n := len(c.probes); n != 1 {
		t.Fatalf("one source made %d probes, want 1", n)
	}
}

// WithAdminBusiness is nil-safe and registers nothing when no source is set.
func TestWithAdminBusiness_NilAndEmpty(t *testing.T) {
	var m *Metrics
	if m.WithAdminBusiness(AdminBusinessSources{}) != nil {
		t.Error("nil *Metrics should stay nil")
	}
	real := New(nil, nil)
	if real.WithAdminBusiness(AdminBusinessSources{}) != real {
		t.Error("WithAdminBusiness should return the receiver")
	}
}
