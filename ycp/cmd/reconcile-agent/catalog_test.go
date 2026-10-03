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
	posts []api.ExtensionCatalog
	err   error
}

func (f *fakeCatalogSink) PostExtensions(_ context.Context, cat api.ExtensionCatalog) error {
	if f.err != nil {
		return f.err
	}
	f.posts = append(f.posts, cat)
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
	if len(dst.posts) != 1 {
		t.Fatalf("first sync: %d posts, want 1", len(dst.posts))
	}

	now = now.Add(time.Minute)
	r.Sync(ctx)
	if len(dst.posts) != 1 {
		t.Fatalf("unchanged catalog: %d posts, want still 1", len(dst.posts))
	}

	src.cat = append(src.cat, api.Extension{Name: "my_ext", Preload: true})
	now = now.Add(time.Minute)
	r.Sync(ctx)
	if len(dst.posts) != 2 || len(dst.posts[1].Extensions) != 2 {
		t.Fatalf("changed catalog: posts %+v, want a second post with both entries", dst.posts)
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
	if len(dst.posts) != 2 {
		t.Fatalf("%d posts, want the unchanged catalog re-sent after %s", len(dst.posts), catalogResendEvery)
	}
}

// A control plane that serves the route only as PUT 405s every time, and one without
// it 404s; retrying each tick would add a request per tick for nothing.
func TestCatalogRelayBacksOffAfterAFailedPost(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	dst := &fakeCatalogSink{err: errors.New("ycp: extensions -> HTTP 405")}
	r := relayOn(&fakeCatalogSource{cat: []api.Extension{{Name: "age"}}}, dst, &now)

	r.Sync(ctx)
	dst.err = nil
	now = now.Add(catalogRetryAfter - time.Second)
	r.Sync(ctx)
	if len(dst.posts) != 0 {
		t.Fatalf("post inside the backoff window: %+v", dst.posts)
	}
	now = now.Add(time.Second)
	r.Sync(ctx)
	if len(dst.posts) != 1 {
		t.Fatalf("%d posts after the backoff, want the retry to land", len(dst.posts))
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
	if len(dst.posts) != 0 {
		t.Fatalf("post %+v from a failed read", dst.posts)
	}

	src.err, src.cat = nil, []api.Extension{{Name: "age"}}
	now = now.Add(catalogRetryAfter - time.Second)
	r.Sync(ctx)
	if len(dst.posts) != 0 {
		t.Fatalf("re-read the controller inside the backoff window: %+v", dst.posts)
	}
	now = now.Add(time.Second)
	r.Sync(ctx)
	if len(dst.posts) != 1 {
		t.Fatalf("%d posts after the backoff, want the catalog relayed", len(dst.posts))
	}
}
