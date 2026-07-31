package metrics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakeStatsSource struct {
	branches int64
	err      error
}

func (f fakeStatsSource) RegistryCounts(context.Context) (int64, error) {
	return f.branches, f.err
}

func TestRegistryCollector_EmitsGauges(t *testing.T) {
	c := newRegistryCollector(fakeStatsSource{branches: 3}, discardLogger())
	want := `
# HELP amp_controller_registry_branches Branches currently recorded in the registry.
# TYPE amp_controller_registry_branches gauge
amp_controller_registry_branches 3
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Fatalf("unexpected metrics: %v", err)
	}
}

func TestRegistryCollector_SkipsScrapeOnError(t *testing.T) {
	c := newRegistryCollector(fakeStatsSource{err: errors.New("db down")}, discardLogger())
	if n := testutil.CollectAndCount(c); n != 0 {
		t.Fatalf("expected 0 metrics when the count query fails, got %d", n)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
