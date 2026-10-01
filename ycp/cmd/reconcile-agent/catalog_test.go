package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"meko/ycp/internal/api"
)

type fakeCatalogSource struct {
	cat []api.Extension
	err error
}

func (f *fakeCatalogSource) ExtensionCatalog(context.Context) ([]api.Extension, error) {
	return f.cat, f.err
}

type fakeCatalogSink struct {
	puts []api.ExtensionCatalog
	err  error
}

func (f *fakeCatalogSink) PutExtensions(_ context.Context, cat api.ExtensionCatalog) error {
	if f.err != nil {
		return f.err
	}
	f.puts = append(f.puts, cat)
	return nil
}

func relayOn(src *fakeCatalogSource, dst *fakeCatalogSink, now *time.Time) *catalogRelay {
	r := newCatalogRelay(src, dst, func(string, ...any) {})
	r.now = func() time.Time { return *now }
	return r
}

func TestCatalogRelaySendsOnlyWhatChanged(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	src := &fakeCatalogSource{cat: []api.Extension{{Name: "pgvector", Builtin: true}}}
	dst := &fakeCatalogSink{}
	r := relayOn(src, dst, &now)

	r.Sync(ctx)
	if len(dst.puts) != 1 {
		t.Fatalf("first sync: %d puts, want 1", len(dst.puts))
	}

	now = now.Add(time.Minute)
	r.Sync(ctx)
	if len(dst.puts) != 1 {
		t.Fatalf("unchanged catalog: %d puts, want still 1", len(dst.puts))
	}

	src.cat = append(src.cat, api.Extension{Name: "my_ext", Preload: true})
	now = now.Add(time.Minute)
	r.Sync(ctx)
	if len(dst.puts) != 2 || len(dst.puts[1].Extensions) != 2 {
		t.Fatalf("changed catalog: puts %+v, want a second put with both entries", dst.puts)
	}
}

// Change detection lives in memory, so a control plane that lost its copy would
// otherwise stay blank until the agent restarted.
func TestCatalogRelayResendsAnUnchangedCatalogHourly(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	dst := &fakeCatalogSink{}
	r := relayOn(&fakeCatalogSource{cat: []api.Extension{{Name: "age"}}}, dst, &now)

	r.Sync(ctx)
	now = now.Add(catalogResendEvery)
	r.Sync(ctx)
	if len(dst.puts) != 2 {
		t.Fatalf("%d puts, want the unchanged catalog re-sent after %s", len(dst.puts), catalogResendEvery)
	}
}

// A control plane (or YBM passthrough) without the route 404s every time; retrying
// each tick would add a request per tick for nothing.
func TestCatalogRelayBacksOffAfterAFailedPut(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	dst := &fakeCatalogSink{err: errors.New("ycp: extensions -> HTTP 404")}
	r := relayOn(&fakeCatalogSource{cat: []api.Extension{{Name: "age"}}}, dst, &now)

	r.Sync(ctx)
	dst.err = nil
	now = now.Add(catalogRetryAfter - time.Second)
	r.Sync(ctx)
	if len(dst.puts) != 0 {
		t.Fatalf("put inside the backoff window: %+v", dst.puts)
	}
	now = now.Add(time.Second)
	r.Sync(ctx)
	if len(dst.puts) != 1 {
		t.Fatalf("%d puts after the backoff, want the retry to land", len(dst.puts))
	}
}

// An unreadable catalog sends nothing: the control plane keeps its last copy, or
// none, rather than being told the environment has no extensions. And it backs off,
// since a controller predating GET /extensions 404s on every tick.
func TestCatalogRelaySendsNothingWhenTheControllerFails(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	src := &fakeCatalogSource{err: errors.New("HTTP 404")}
	dst := &fakeCatalogSink{}
	r := relayOn(src, dst, &now)
	r.Sync(ctx)
	if len(dst.puts) != 0 {
		t.Fatalf("put %+v from a failed read", dst.puts)
	}

	src.err, src.cat = nil, []api.Extension{{Name: "age"}}
	now = now.Add(catalogRetryAfter - time.Second)
	r.Sync(ctx)
	if len(dst.puts) != 0 {
		t.Fatalf("re-read the controller inside the backoff window: %+v", dst.puts)
	}
	now = now.Add(time.Second)
	r.Sync(ctx)
	if len(dst.puts) != 1 {
		t.Fatalf("%d puts after the backoff, want the catalog relayed", len(dst.puts))
	}
}
